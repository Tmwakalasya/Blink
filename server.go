package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"golang.org/x/oauth2/google"
)

//go:embed web
var webFS embed.FS

// maxLive is how many VMs Blink runs at once: one button, one VM.
const maxLive = 1

type server struct {
	cfg     config
	keys    keyStore
	sshPort string // 22, except in tests
	timing  timing

	mu     sync.Mutex
	cloud  cloud // nil until Blink can reach Google Cloud
	status status

	summoning atomic.Bool
}

// timing holds the waits that tests shrink.
type timing struct {
	poll         time.Duration // how often to re-check while summoning
	hostKeyGrace time.Duration // how long to wait for published host keys before trusting on first use
	keyChurn     time.Duration // how long to tolerate a host key mismatch while the VM sets itself up
}

// status is what the page needs before the first click.
type status struct {
	Ready   bool   `json:"ready"`
	Problem string `json:"problem,omitempty"`
	Warning string `json:"warning,omitempty"`
	Fix     string `json:"fix,omitempty"`
	Project string `json:"project"`
	Zone    string `json:"zone"`
	Place   string `json:"place"`
	Machine string `json:"machine"`
	Image   string `json:"image"`
	Network string `json:"network"`
	DiskGB  int    `json:"diskGb"`
	TTL     int    `json:"ttlSeconds"`
}

type apiError struct {
	Error string   `json:"error"`
	Fix   string   `json:"fix,omitempty"`
	VM    *machine `json:"vm,omitempty"`
}

func newServer(cfg config, keys keyStore) *server {
	return &server{
		cfg:     cfg,
		keys:    keys,
		sshPort: "22",
		timing:  timing{poll: time.Second, hostKeyGrace: 45 * time.Second, keyChurn: 20 * time.Second},
	}
}

func (s *server) routes() http.Handler {
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", noCache(http.FileServerFS(static)))
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/status", s.handleRecheck)
	mux.HandleFunc("GET /api/vms", s.handleList)
	mux.HandleFunc("POST /api/vms", s.handleSummon)
	mux.HandleFunc("DELETE /api/vms/{zone}/{name}", s.handleDelete)
	mux.HandleFunc("GET /api/vms/{zone}/{name}/terminal", s.handleTerminal)
	return localOnly(http.NewCrossOriginProtection().Handler(mux))
}

// preflight connects to Google Cloud and checks what would otherwise make the
// first click fail. A problem disables the button. A missing firewall rule is
// only a warning, because an organization firewall policy might still let SSH in.
func (s *server) preflight(ctx context.Context) status {
	st := status{
		Project: s.cfg.Project,
		Zone:    s.cfg.Zone,
		Place:   place(s.cfg.Zone),
		Machine: s.cfg.Machine,
		Image:   imageName(s.cfg.Image),
		Network: s.cfg.Network,
		DiskGB:  diskGB,
		TTL:     int(s.cfg.TTL / time.Second),
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

// decorate fills in what only this computer knows: whether it holds the VM's
// key, and whether the VM's host key has been verified.
func (s *server) decorate(m *machine) {
	m.HasKey = s.keys.has(m.Name)
	m.Ready = m.Status == "RUNNING" && m.IP != "" && m.HasKey && s.keys.pinned(m.Name)
	if m.Ready {
		m.SSH = s.keys.command(m.Name, m.IP)
	}
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	st := s.status
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, st)
}

func (s *server) handleRecheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.preflight(r.Context()))
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	c := s.currentCloud()
	if c == nil {
		writeJSON(w, http.StatusServiceUnavailable, apiError{Error: "Blink isn't connected to Google Cloud yet."})
		return
	}
	ms, err := c.List(r.Context())
	if err != nil {
		problem, fix := s.explain(err)
		writeJSON(w, http.StatusBadGateway, apiError{Error: problem, Fix: fix})
		return
	}
	for i := range ms {
		s.decorate(&ms[i])
	}
	if ms == nil {
		ms = []machine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"vms": ms})
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	zone, name := r.PathValue("zone"), r.PathValue("name")
	if !validZone(zone) || !validName(name) {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "That isn't a Blink VM."})
		return
	}
	c := s.currentCloud()
	if c == nil {
		writeJSON(w, http.StatusServiceUnavailable, apiError{Error: "Blink isn't connected to Google Cloud yet."})
		return
	}
	m, err := c.Get(r.Context(), zone, name)
	switch {
	case errors.Is(err, errNotFound):
		// Already gone, perhaps on schedule.
	case err != nil:
		problem, fix := s.explain(err)
		writeJSON(w, http.StatusBadGateway, apiError{Error: problem, Fix: fix})
		return
	case !m.blink:
		writeJSON(w, http.StatusForbidden, apiError{Error: name + " wasn't made by Blink, so Blink won't delete it."})
		return
	default:
		if err := c.Delete(r.Context(), zone, name); err != nil {
			problem, fix := s.explain(err)
			writeJSON(w, http.StatusBadGateway, apiError{Error: problem, Fix: fix})
			return
		}
	}
	s.keys.remove(name)
	w.WriteHeader(http.StatusNoContent)
}

// explain turns an error into a sentence and, when there is one, the fix.
func (s *server) explain(err error) (problem, fix string) {
	p := s.cfg.Project
	msg := err.Error()
	switch {
	case errors.Is(err, errHostKey):
		return "The VM answered SSH with a host key Google never published for it, so Blink hung up and deleted the VM. Something may be intercepting your connection.", ""
	case strings.Contains(msg, "could not find default credentials"):
		return "Blink isn't signed in to Google Cloud.", "gcloud auth login --update-adc"
	case strings.Contains(msg, "invalid_grant"), strings.Contains(msg, "reauth"):
		return "Your Google Cloud sign-in has expired.", "gcloud auth login --update-adc"
	case strings.Contains(msg, "SERVICE_DISABLED"), strings.Contains(msg, "has not been used in project"):
		return "The Compute Engine API is turned off in " + p + ".", "gcloud services enable compute.googleapis.com --project " + p
	case strings.Contains(msg, "BILLING_DISABLED"), strings.Contains(strings.ToLower(msg), "billing"):
		return "Billing isn't enabled for " + p + ".", "https://console.cloud.google.com/billing/linkedaccount?project=" + p
	case strings.Contains(msg, "ZONE_RESOURCE_POOL_EXHAUSTED"), strings.Contains(msg, "does not have enough resources"):
		return fmt.Sprintf("Google has no spare %s capacity in %s right now.", s.cfg.Machine, s.cfg.Zone), "go run . -zone us-central1-b"
	case strings.Contains(msg, "QUOTA_EXCEEDED"), strings.Contains(msg, "Quota '"):
		return "That would go over a Compute Engine quota: " + shorten(msg), ""
	case httpCode(err) == 403:
		return "Your account isn't allowed to create VMs in " + p + ".", ""
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

// localOnly refuses requests addressed to any host but this machine. Blink
// can create VMs and open shells, so a page you visit must not reach it
// through DNS rebinding. Cross-site requests are stopped by
// http.CrossOriginProtection, and the terminal's WebSocket checks its Origin.
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
