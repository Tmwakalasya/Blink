package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"slices"
	"time"

	"golang.org/x/crypto/ssh"
)

// Starting a VM: check the limits, create it, wait for it to boot, and prove
// SSH works, streaming each step to the page as it happens.

// event is one line of the NDJSON stream the page reads while a VM starts.
type event struct {
	Step  string   `json:"step,omitempty"` // requested, creating, booting, ssh
	At    float64  `json:"at"`             // seconds since the click
	Note  string   `json:"note,omitempty"`
	VM    *machine `json:"vm,omitempty"` // set on the final event
	Error string   `json:"error,omitempty"`
	Fix   string   `json:"fix,omitempty"`
}

// How long each step may take before Blink gives up and deletes the VM.
const (
	createTimeout = 3 * time.Minute
	bootTimeout   = 3 * time.Minute
	sshTimeout    = 2 * time.Minute
)

var errHostKey = errors.New("host key doesn't match the one Google published")

type startRequest struct {
	Size       string `json:"size"`
	TTLSeconds int    `json:"ttlSeconds"`
}

// refusal is why a VM can't start right now.
type refusal struct {
	code int
	apiError
}

func (s *server) handleSummon(w http.ResponseWriter, r *http.Request, u user) {
	start := time.Now()
	c := s.currentCloud()
	if c == nil {
		writeJSON(w, http.StatusServiceUnavailable, apiError{Error: errNotConnected.Error()})
		return
	}
	var req startRequest
	if json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req) != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "Couldn't read the request."})
		return
	}
	i := slices.IndexFunc(s.sizes, func(sz size) bool { return sz.ID == req.Size })
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if i < 0 || !slices.Contains(s.lifetimes, ttl) {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "Pick one of the sizes and lifetimes on the page."})
		return
	}

	// Keep going if the page is closed or reloaded mid-start. The VM still
	// comes up, and the page reconnects to it when it loads again.
	ctx := context.WithoutCancel(r.Context())
	sp, no := s.admitStart(ctx, c, u, s.sizes[i], ttl)
	if no != nil {
		writeJSON(w, no.code, no.apiError)
		return
	}
	defer func() {
		s.admit.Lock()
		delete(s.starting, u.owner)
		s.admit.Unlock()
	}()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	s.summon(ctx, start, sp, u, func(e event) {
		if enc.Encode(e) == nil {
			rc.Flush()
		}
	})
}

// admitStart checks everything that could stop u from starting a VM of size
// sz for ttl, and records it in the ledger if not. It holds the admission
// lock throughout, so two requests can't both slip under a limit.
func (s *server) admitStart(ctx context.Context, c cloud, u user, sz size, ttl time.Duration) (spec, *refusal) {
	s.admit.Lock()
	defer s.admit.Unlock()
	no := func(code int, format string, args ...any) *refusal {
		return &refusal{code: code, apiError: apiError{Error: fmt.Sprintf(format, args...)}}
	}
	if s.starting[u.owner] {
		return spec{}, no(http.StatusConflict, "Your VM is already starting.")
	}
	// The ledger knows which VM this person may still have; checking just that
	// one is much quicker than listing every VM in the project.
	if prev, ok := s.ledger.activeFor(u.owner, time.Now()); ok {
		gctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		m, err := c.Get(gctx, cmp.Or(prev.Zone, s.cfg.Zone), prev.VM)
		cancel()
		switch {
		case err == nil:
			s.decorate(&m, u)
			return spec{}, &refusal{code: http.StatusConflict, apiError: apiError{Error: m.Name + " still exists. End that session before starting another.", VM: &m}}
		case errors.Is(err, errNotFound):
			if s.keys.has(prev.VM) && !s.keys.pinned(prev.VM) {
				return spec{}, no(http.StatusConflict, "%s has an unresolved start. Its usage is still reserved; check it before starting another workspace.", prev.VM)
			}
			if err := s.ledger.end(prev.VM, time.Now()); err != nil {
				return spec{}, no(http.StatusInternalServerError, "Couldn't update the previous VM's usage: %v", err)
			}
		default:
			problem, fix := s.explain(err)
			return spec{}, &refusal{code: http.StatusBadGateway, apiError: apiError{Error: problem, Fix: fix}}
		}
	}

	now := time.Now()
	t := s.ledger.totals(u.owner, now)
	cost := sz.cost(ttl)
	switch {
	case t.Active >= s.cfg.MaxVMs:
		return spec{}, no(http.StatusConflict, "All %d VMs are in use. Try again when one frees up.", s.cfg.MaxVMs)
	case s.cfg.Budget > 0 && t.Spent+cost > s.cfg.Budget:
		return spec{}, no(http.StatusForbidden, "That would go over the $%.2f budget ($%.2f used). Try a smaller size or a shorter lifetime.", s.cfg.Budget, t.Spent)
	case s.cfg.WeeklyHours > 0 && !u.Admin && t.Hours+ttl.Hours() > s.cfg.WeeklyHours:
		return spec{}, no(http.StatusForbidden, "That would go over your %s a week (%s used). Try a shorter lifetime.",
			hoursText(s.cfg.WeeklyHours), hoursText(t.Hours))
	}

	sp := spec{
		Name:    "blink-" + randomSuffix(),
		Zone:    s.cfg.Zone,
		Machine: sz.Machine,
		Size:    sz.ID,
		Image:   s.cfg.Image,
		Network: s.cfg.Network,
		TTL:     ttl,
		Owner:   u.owner,
		Editor:  sz.Editor,
	}
	err := s.ledger.add(usage{
		VM: sp.Name, Zone: sp.Zone, Owner: u.owner, Email: u.Email, Size: sz.ID, Hourly: sz.Hourly,
		Start: now, TTL: int64(ttl / time.Second),
	})
	if err != nil {
		return spec{}, no(http.StatusInternalServerError, "Couldn't record the VM: %v", err)
	}
	s.starting[u.owner] = true
	return sp, nil
}

func (s *server) summon(ctx context.Context, start time.Time, sp spec, u user, emit func(event)) {
	at := func() float64 { return math.Round(time.Since(start).Seconds()*10) / 10 }
	c := s.currentCloud()
	step := "requested"
	fail := func(err error, created bool) {
		problem, fix := s.explain(err)
		if errors.Is(err, context.DeadlineExceeded) {
			problem, fix = s.timedOut(step)
		}
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		// A response can be lost after Insert succeeded. Check the named VM
		// before deciding there is nothing to clean up.
		if !created {
			m, lookupErr := c.Get(dctx, sp.Zone, sp.Name)
			if lookupErr == nil {
				if !m.blink || m.Owner != sp.Owner {
					emit(event{Step: step, At: at(), Error: problem + " The VM name is already in use; no existing VM was deleted.", Fix: fix})
					return
				}
				created = true
			} else {
				var networkErr net.Error
				if !errors.Is(lookupErr, errNotFound) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.As(err, &networkErr) {
					emit(event{Step: step, At: at(), Error: problem + " Creation could not be ruled out. The key and full usage reservation have been kept; check the VM before retrying.", Fix: fix})
					return
				}
			}
		}
		if created {
			if cleanupErr := s.deleteVM(dctx, c, sp.Zone, sp.Name); cleanupErr != nil {
				problem += " Cleanup failed: " + cleanupErr.Error()
			} else {
				problem += " The VM was deleted."
			}
		} else if cleanupErr := s.forgetVM(sp.Name); cleanupErr != nil {
			problem += " Cleanup failed: " + cleanupErr.Error()
		}
		emit(event{Step: step, At: at(), Error: problem, Fix: fix})
	}

	signer, pub, err := s.keys.create(sp.Name)
	if err != nil {
		fail(err, false)
		return
	}
	sp.PublicKey = pub
	ictx, cancel := context.WithTimeout(ctx, time.Minute)
	wait, err := c.Insert(ictx, sp)
	cancel()
	if err != nil {
		fail(err, false)
		return
	}
	emit(event{Step: "requested", At: at(), Note: sp.Name})

	step = "creating"
	cctx, cancel := context.WithTimeout(ctx, createTimeout)
	m, err := s.untilRunning(cctx, c, wait, sp)
	cancel()
	if err != nil {
		fail(err, true)
		return
	}
	emit(event{Step: "creating", At: at(), Note: m.IP})

	step = "booting"
	addr := net.JoinHostPort(m.IP, s.sshPort)
	bctx, cancel := context.WithTimeout(ctx, bootTimeout)
	err = s.untilListening(bctx, addr)
	cancel()
	if err != nil {
		fail(err, true)
		return
	}
	emit(event{Step: "booting", At: at(), Note: "Port 22 open"})

	step = "ssh"
	sctx, cancel := context.WithTimeout(ctx, sshTimeout)
	hostKey, verified, err := s.untilSSH(sctx, c, m, addr, signer)
	cancel()
	if err == nil {
		err = s.keys.pin(sp.Name, addr, hostKey)
	}
	if err != nil {
		fail(err, true)
		return
	}
	note := "Host key verified"
	if !verified {
		note = "Host key not published; trusted on first use"
	}
	emit(event{Step: "ssh", At: at(), Note: note})

	s.decorate(&m, u)
	emit(event{At: at(), VM: &m})
}

// timedOut explains which step ran out of time.
func (s *server) timedOut(step string) (problem, fix string) {
	switch step {
	case "creating":
		return "Google took more than 3 minutes to start the VM, so Blink stopped setup.", ""
	case "booting":
		return "The VM started, but nothing answered on port 22. Usually a firewall rule is missing. Some campus and office networks also block outgoing SSH.", s.firewallFix()
	case "ssh":
		return "SSH answered but never accepted Blink's key. If your organization enforces OS Login, it ignores keys like Blink's.", ""
	}
	return "Blink ran out of time.", ""
}

// untilRunning waits for the create operation, then for the VM to be RUNNING
// with an external IP.
func (s *server) untilRunning(ctx context.Context, c cloud, wait func(context.Context) error, sp spec) (machine, error) {
	if err := wait(ctx); err != nil {
		return machine{}, err
	}
	for {
		m, err := c.Get(ctx, sp.Zone, sp.Name)
		switch {
		case errors.Is(err, errNotFound):
		case err != nil:
			return machine{}, err
		case m.Status == "RUNNING" && m.IP != "":
			return m, nil
		case !m.alive():
			return machine{}, errors.New("the VM stopped while starting (" + m.Status + ")")
		}
		if err := sleep(ctx, s.timing.poll); err != nil {
			return machine{}, err
		}
	}
}

// untilListening waits for the VM's OS to boot far enough to accept TCP on addr.
func (s *server) untilListening(ctx context.Context, addr string) error {
	var d net.Dialer
	for {
		dctx, cancel := context.WithTimeout(ctx, time.Second)
		conn, err := d.DialContext(dctx, "tcp", addr)
		cancel()
		if err == nil {
			conn.Close()
			return nil
		}
		if err := sleep(ctx, s.timing.poll/2); err != nil {
			return err
		}
	}
}

// untilSSH retries until the VM accepts Blink's key. The host key has to match
// one the VM's guest agent published through the Compute API, an
// authenticated channel, so nothing in the middle can pose as the VM. If no
// keys appear within the grace period, it trusts the first key it sees, as
// ssh does.
func (s *server) untilSSH(ctx context.Context, c cloud, m machine, addr string, signer ssh.Signer) (ssh.PublicKey, bool, error) {
	began := time.Now()
	var mismatchSince time.Time
	for {
		published, err := c.HostKeys(ctx, m.Zone, m.Name)
		if err != nil {
			return nil, false, fmt.Errorf("couldn't verify the VM's published host key: %w", err)
		}
		tofu := len(published) == 0 && time.Since(began) > s.timing.hostKeyGrace
		if len(published) > 0 || tofu {
			key, err := handshake(ctx, addr, signer, published)
			if err == nil {
				return key, !tofu, nil
			}
			if errors.Is(err, errHostKey) {
				// Keys can change once while the guest agent sets the VM up. A
				// mismatch that lasts means something else is answering.
				if mismatchSince.IsZero() {
					mismatchSince = time.Now()
				}
				if time.Since(mismatchSince) > s.timing.keyChurn {
					return nil, false, errHostKey
				}
			}
		}
		if err := sleep(ctx, s.timing.poll); err != nil {
			return nil, false, err
		}
	}
}

// handshake opens and closes one SSH connection and returns the host key the
// VM presented. When keys were published, any other key is refused.
func handshake(ctx context.Context, addr string, signer ssh.Signer, published []ssh.PublicKey) (ssh.PublicKey, error) {
	var presented ssh.PublicKey
	client, err := dialSSH(ctx, addr, &ssh.ClientConfig{
		User: sshUser,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			presented = key
			if len(published) == 0 {
				return nil
			}
			for _, p := range published {
				if bytes.Equal(p.Marshal(), key.Marshal()) {
					return nil
				}
			}
			return errHostKey
		},
		HostKeyAlgorithms: algorithmsFor(published),
	})
	if err != nil {
		return nil, err
	}
	client.Close()
	return presented, nil
}

// algorithmsFor asks the server for a host key of a type Google published, so
// the handshake can't settle on one Blink has no way to check.
func algorithmsFor(keys []ssh.PublicKey) []string {
	var algs []string
	for _, k := range keys {
		if k.Type() == ssh.KeyAlgoRSA {
			algs = append(algs, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
		} else {
			algs = append(algs, k.Type())
		}
	}
	return algs
}

// dialSSH is ssh.Dial with a context and a deadline on the handshake.
func dialSSH(ctx context.Context, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

func randomSuffix() string {
	b := make([]byte, 3)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hoursText(h float64) string {
	if h == math.Trunc(h) {
		return fmt.Sprintf("%.0f h", h)
	}
	return fmt.Sprintf("%.1f h", h)
}
