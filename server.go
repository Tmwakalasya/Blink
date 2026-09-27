package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"golang.org/x/oauth2/google"
)

//go:embed web
var webFS embed.FS

type server struct {
	cfg       config
	keys      keyStore
	sizes     []size
	lifetimes []time.Duration
	auth      *auth // nil when Blink is a single-user tool on localhost
	ledger    *ledger
	shells    *shells
	sshPort   string // 22, except in tests
	timing    timing

	mu     sync.Mutex
	cloud  cloud // nil until Blink can reach Google Cloud
	status status

	admit    sync.Mutex      // held through the checks before a VM starts
	starting map[string]bool // owners with a VM on its way
}

// timing holds the waits that tests shrink.
type timing struct {
	poll         time.Duration // how often to re-check while starting a VM
	hostKeyGrace time.Duration // how long to wait for published host keys before trusting on first use
	keyChurn     time.Duration // how long to tolerate a host key mismatch while the VM sets itself up
	shellIdle    time.Duration // how long a shell with no page attached stays open
}

// status is the result of the last check that Blink can reach Google Cloud.
type status struct {
	Ready   bool   `json:"ready"`
	Problem string `json:"problem,omitempty"`
	Warning string `json:"warning,omitempty"`
	Fix     string `json:"fix,omitempty"`
	Project string `json:"project,omitempty"`
	Zone    string `json:"zone,omitempty"`
	Place   string `json:"place,omitempty"`
	Image   string `json:"image,omitempty"`
	Network string `json:"network,omitempty"`
	DiskGB  int    `json:"diskGb,omitempty"`
}

type apiError struct {
	Error string   `json:"error"`
	Fix   string   `json:"fix,omitempty"`
	VM    *machine `json:"vm,omitempty"`
}

func newServer(cfg config) (*server, error) {
	sizes, err := pickSizes(cfg.Sizes)
	if err != nil {
		return nil, err
	}
	lts, err := pickLifetimes(cfg.MaxTTL)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.State, 0o700); err != nil {
		return nil, err
	}
	l, err := openLedger(filepath.Join(cfg.State, "ledger.jsonl"))
	if err != nil {
		return nil, err
	}
	s := &server{
		cfg:       cfg,
		keys:      keyStore{dir: filepath.Join(cfg.State, "vms")},
		sizes:     sizes,
		lifetimes: lts,
		ledger:    l,
		shells:    &shells{byVM: map[string]*shell{}},
		sshPort:   "22",
		starting:  map[string]bool{},
		timing: timing{
			poll:         time.Second,
			hostKeyGrace: 45 * time.Second,
			keyChurn:     20 * time.Second,
			shellIdle:    10 * time.Minute,
		},
	}
	if cfg.ClientID != "" {
		if s.auth, err = newAuth(cfg.ClientID, cfg.Admins, cfg.State); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *server) routes() http.Handler {
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", noCache(http.FileServerFS(static)))
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/status", s.admin(s.handleRecheck))
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/vms", s.signedIn(s.handleList))
	mux.HandleFunc("POST /api/vms", s.signedIn(s.handleSummon))
	mux.HandleFunc("DELETE /api/vms/{zone}/{name}", s.signedIn(s.handleDelete))
	mux.HandleFunc("GET /api/vms/{zone}/{name}/terminal", s.signedIn(s.handleTerminal))
	mux.HandleFunc("GET /api/class", s.admin(s.handleClass))
	mux.HandleFunc("PUT /api/class", s.admin(s.handleSaveClass))
	h := http.NewCrossOriginProtection().Handler(mux)
	if s.auth == nil {
		h = localOnly(h) // no sign-in, so nothing but this machine may reach it
	}
	return h
}

// preflight connects to Google Cloud and checks what would otherwise make the
// first click fail. A problem disables the button. A missing firewall rule is
// only a warning, because an organization firewall policy might still let SSH in.
func (s *server) preflight(ctx context.Context) status {
	st := status{
		Project: s.cfg.Project,
		Zone:    s.cfg.Zone,
		Place:   place(s.cfg.Zone),
		Image:   imageName(s.cfg.Image),
		Network: s.cfg.Network,
		DiskGB:  diskGB,
	}
	defer func() {
		s.mu.Lock()
		s.status = st
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Setup usually happens while Blink is running, so look for the project
	// again. Nothing else reads the config until Blink is connected.
	if st.Project == "" && s.currentCloud() == nil {
		s.cfg.Project = findProject(ctx, "")
		st.Project = s.cfg.Project
	}
	if _, err := exec.LookPath("gcloud"); err != nil && st.Project == "" {
		st.Problem = "The gcloud CLI isn't installed."
		st.Fix = "brew install --cask gcloud-cli"
		if runtime.GOOS != "darwin" {
			st.Fix = "https://cloud.google.com/sdk/docs/install"
		}
		return st
	}
	if _, err := google.FindDefaultCredentials(ctx, compute.DefaultAuthScopes()...); err != nil {
		st.Problem, st.Fix = s.explain(err)
		return st
	}
	if st.Project == "" {
		st.Problem = "Blink doesn't know which Google Cloud project to use. gcloud projects list shows yours."
		st.Fix = "gcloud config set project YOUR_PROJECT_ID"
		return st
	}

	g, err := newGCE(ctx, st.Project)
	if err != nil {
		st.Problem, st.Fix = s.explain(err)
		return st
	}
	if err := g.networkExists(ctx, s.cfg.Network); err != nil {
		g.Close()
		if errors.Is(err, errNotFound) {
			st.Problem = fmt.Sprintf("There's no %q network in %s.", s.cfg.Network, st.Project)
			st.Fix = fmt.Sprintf("gcloud compute networks create %s --subnet-mode=auto --project %s", s.cfg.Network, st.Project)
		} else {
			st.Problem, st.Fix = s.explain(err)
		}
		return st
	}
	if open, err := g.sshOpen(ctx, s.cfg.Network); err == nil && !open {
		st.Warning = fmt.Sprintf("No firewall rule lets SSH into the %q network, so VMs will boot but you won't be able to connect.", s.cfg.Network)
		st.Fix = s.firewallFix()
	}
	st.Ready = true
	s.mu.Lock()
	s.cloud = g
	s.mu.Unlock()
	return st
}

func (s *server) firewallFix() string {
	return fmt.Sprintf("gcloud compute firewall-rules create blink-ssh --network %s --allow tcp:22 --target-tags blink --project %s",
		s.cfg.Network, s.cfg.Project)
}

// pruneKeys forgets keys for VMs that no longer exist, such as ones Google
// deleted on schedule while Blink wasn't running.
func (s *server) pruneKeys(ctx context.Context) {
	if c := s.currentCloud(); c != nil {
		if ms, err := c.List(ctx); err == nil {
			s.keys.prune(ms)
		}
	}
}

func (s *server) currentCloud() cloud {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cloud
}

func (s *server) currentUser(r *http.Request) (user, bool) {
	if s.auth == nil {
		return localUser, true
	}
	return s.auth.session(r)
}

func (s *server) signedIn(h func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.currentUser(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, apiError{Error: "Sign in first."})
			return
		}
		h(w, r, u)
	}
}

func (s *server) admin(h func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return s.signedIn(func(w http.ResponseWriter, r *http.Request, u user) {
		if !u.Admin {
			writeJSON(w, http.StatusForbidden, apiError{Error: "Only admins can do that."})
			return
		}
		h(w, r, u)
	})
}

// decorate fills in what only this server knows about m: whether it holds
// the VM's key and has verified its host key, and, for admins, whose it is.
func (s *server) decorate(m *machine, u user) {
	m.HasKey = s.keys.has(m.Name)
	m.Ready = m.Status == "RUNNING" && m.IP != "" && m.HasKey && s.keys.pinned(m.Name)
	if m.Ready && s.auth == nil {
		// The key lives on this computer, so the command only works here.
		m.SSH = s.keys.command(m.Name, m.IP)
	}
	if u.Admin {
		m.Email = s.ledger.email(m.Name)
	}
}

var (
	errNotBlink     = errors.New("That isn't a Blink VM.")
	errNotConnected = errors.New("Blink isn't connected to Google Cloud yet.")
)

// ownedVM fetches a Blink VM that u may use: their own, or any for admins.
func (s *server) ownedVM(ctx context.Context, zone, name string, u user) (machine, error) {
	if !validZone(zone) || !validName(name) {
		return machine{}, errNotBlink
	}
	c := s.currentCloud()
	if c == nil {
		return machine{}, errNotConnected
	}
	m, err := c.Get(ctx, zone, name)
	switch {
	case err != nil:
		return machine{}, err
	case !m.blink:
		return machine{}, errors.New(name + " wasn't made by Blink.")
	case !u.Admin && m.Owner != u.owner:
		return machine{}, errors.New(name + " belongs to someone else.")
	}
	return m, nil
}

// statusView is what the page loads first.
type statusView struct {
	status
	SignIn    *signInInfo `json:"signIn,omitempty"`
	User      *user       `json:"user"`
	Sizes     []size      `json:"sizes,omitempty"`
	Lifetimes []int       `json:"lifetimes,omitempty"`
	Budget    *meter      `json:"budget,omitempty"`
	Week      *meter      `json:"week,omitempty"`
	Running   int         `json:"running"`
	MaxVMs    int         `json:"maxVMs"`
}

type signInInfo struct {
	ClientID string `json:"clientId"`
}

type meter struct {
	Used  float64 `json:"used"`
	Limit float64 `json:"limit"`
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	st := s.status
	s.mu.Unlock()
	var v statusView
	if s.auth != nil {
		v.SignIn = &signInInfo{ClientID: s.auth.clientID}
	}
	u, ok := s.currentUser(r)
	if !ok {
		v.status = status{Ready: st.Ready} // visitors only learn whether Blink is up
		writeJSON(w, http.StatusOK, v)
		return
	}
	if !st.Ready && !u.Admin {
		st = status{Problem: "Blink can't reach Google Cloud right now."}
	}
	v.status, v.User, v.Sizes = st, &u, s.sizes
	for _, d := range s.lifetimes {
		v.Lifetimes = append(v.Lifetimes, int(d/time.Second))
	}
	t := s.ledger.totals(u.owner, time.Now())
	if s.cfg.Budget > 0 {
		v.Budget = &meter{Used: t.Spent, Limit: s.cfg.Budget}
	}
	if s.cfg.WeeklyHours > 0 && !u.Admin {
		v.Week = &meter{Used: t.Hours, Limit: s.cfg.WeeklyHours}
	}
	v.Running, v.MaxVMs = t.Active, s.cfg.MaxVMs
	writeJSON(w, http.StatusOK, v)
}

func (s *server) handleRecheck(w http.ResponseWriter, r *http.Request, u user) {
	s.preflight(r.Context())
	s.handleStatus(w, r)
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeJSON(w, http.StatusNotFound, apiError{Error: "Sign-in isn't turned on."})
		return
	}
	var body struct {
		Credential string `json:"credential"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body) != nil || body.Credential == "" {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "Google didn't send a sign-in token."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	email, err := s.auth.verify(ctx, body.Credential)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, apiError{Error: "Google sign-in didn't go through. Try again."})
		return
	}
	email = strings.ToLower(strings.TrimSpace(email)) // one person, one owner, whatever the case
	if !s.auth.allowed(email) {
		writeJSON(w, http.StatusForbidden, apiError{Error: email + " isn't on the class list. Ask whoever runs this Blink to add it."})
		return
	}
	s.auth.signIn(w, r, email)
	writeJSON(w, http.StatusOK, s.auth.userFor(email))
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil {
		s.auth.signOut(w, r)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request, u user) {
	c := s.currentCloud()
	if c == nil {
		writeJSON(w, http.StatusServiceUnavailable, apiError{Error: errNotConnected.Error()})
		return
	}
	ms, err := c.List(r.Context())
	if err != nil {
		problem, fix := s.explain(err)
		writeJSON(w, http.StatusBadGateway, apiError{Error: problem, Fix: fix})
		return
	}
	vms := []machine{}
	for _, m := range ms {
		if u.Admin || m.Owner == u.owner {
			s.decorate(&m, u)
			vms = append(vms, m)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"vms": vms})
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request, u user) {
	zone, name := r.PathValue("zone"), r.PathValue("name")
	_, err := s.ownedVM(r.Context(), zone, name, u)
	switch {
	case errors.Is(err, errNotFound):
		// Already gone, perhaps on schedule.
	case err != nil:
		problem, fix := s.explain(err)
		writeJSON(w, http.StatusForbidden, apiError{Error: problem, Fix: fix})
		return
	default:
		if err := s.currentCloud().Delete(r.Context(), zone, name); err != nil {
			problem, fix := s.explain(err)
			writeJSON(w, http.StatusBadGateway, apiError{Error: problem, Fix: fix})
			return
		}
	}
	s.ledger.end(name, time.Now())
	s.shells.end(name)
	s.keys.remove(name)
	w.WriteHeader(http.StatusNoContent)
}

// handleClass shows admins the class list and this week's usage.
func (s *server) handleClass(w http.ResponseWriter, r *http.Request, u user) {
	resp := struct {
		SignIn bool     `json:"signIn"`
		Roster string   `json:"roster"`
		Week   []person `json:"week"`
	}{Week: s.ledger.week(time.Now())}
	if s.auth != nil {
		resp.SignIn, resp.Roster = true, s.auth.roster()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleSaveClass(w http.ResponseWriter, r *http.Request, u user) {
	if s.auth == nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "Sign-in is off, so there's no class list."})
		return
	}
	var body struct {
		Roster string `json:"roster"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, rosterLimit)).Decode(&body) != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "Couldn't read the class list."})
		return
	}
	n, err := s.auth.saveRoster(body.Roster)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"count": n})
}

// explain turns an error into a sentence and, when there is one, the fix.
func (s *server) explain(err error) (problem, fix string) {
	p := s.cfg.Project
	msg := err.Error()
	switch {
	case errors.Is(err, errHostKey):
		return "The VM answered SSH with a host key Google never published for it, so Blink hung up and deleted the VM. Something may be intercepting the connection.", ""
	case strings.Contains(msg, "could not find default credentials"):
		return "Blink isn't signed in to Google Cloud.", "gcloud auth login --update-adc"
	case strings.Contains(msg, "invalid_grant"), strings.Contains(msg, "reauth"):
		return "Blink's Google Cloud sign-in has expired.", "gcloud auth login --update-adc"
	case strings.Contains(msg, "SERVICE_DISABLED"), strings.Contains(msg, "has not been used in project"):
		return "The Compute Engine API is turned off in " + p + ".", "gcloud services enable compute.googleapis.com --project " + p
	case strings.Contains(msg, "BILLING_DISABLED"), strings.Contains(strings.ToLower(msg), "billing"):
		return "Billing isn't enabled for " + p + ".", "https://console.cloud.google.com/billing/linkedaccount?project=" + p
	case strings.Contains(msg, "ZONE_RESOURCE_POOL_EXHAUSTED"), strings.Contains(msg, "does not have enough resources"):
		return fmt.Sprintf("Google has no spare capacity for that size in %s right now. Try another size, or try again later.", s.cfg.Zone), ""
	case strings.Contains(msg, "QUOTA_EXCEEDED"), strings.Contains(msg, "Quota '"):
		return "That would go over a Compute Engine quota: " + shorten(msg), ""
	case httpCode(err) == 403:
		return "Blink isn't allowed to create VMs in " + p + ".", ""
	}
	return shorten(msg), ""
}

func shorten(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 280 {
		s = s[:277] + "..."
	}
	return s
}

// localOnly refuses requests addressed to any host but this machine. Without
// sign-in, anyone who can reach Blink can create VMs and open shells, so a
// web page you visit must not reach it through DNS rebinding. Cross-site
// requests are stopped by http.CrossOriginProtection, and the terminal's
// WebSocket checks its Origin.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		switch strings.Trim(host, "[]") {
		case "localhost", "127.0.0.1", "::1":
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "blink only answers on localhost", http.StatusForbidden)
		}
	})
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
