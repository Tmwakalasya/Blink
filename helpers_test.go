package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeCloud stands in for Compute Engine. Its VMs "run" at ip, where a test
// SSH server plays the part of the guest.
type fakeCloud struct {
	mu        sync.Mutex
	ip        string
	vms       map[string]machine
	hostKeys  []ssh.PublicKey
	insertErr error
	waitErr   error
	onInsert  func(spec)
	deleted   []string
}

func newFakeCloud(ip string) *fakeCloud {
	return &fakeCloud{ip: ip, vms: map[string]machine{}}
}

func (f *fakeCloud) Insert(_ context.Context, s spec) (func(context.Context) error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return nil, f.insertErr
	}
	f.vms[s.Name] = machine{Name: s.Name, Zone: s.Zone, Status: "PROVISIONING", Size: s.Size, Owner: s.Owner, blink: true}
	if f.onInsert != nil {
		f.onInsert(s)
	}
	return func(context.Context) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.waitErr != nil {
			return f.waitErr
		}
		m := f.vms[s.Name]
		m.Status, m.IP, m.ExpiresAt = "RUNNING", f.ip, time.Now().Add(s.TTL)
		f.vms[s.Name] = m
		return nil
	}, nil
}

func (f *fakeCloud) Get(_ context.Context, _, name string) (machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.vms[name]
	if !ok {
		return machine{}, errNotFound
	}
	return m, nil
}

func (f *fakeCloud) List(context.Context) ([]machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ms []machine
	for _, m := range f.vms {
		ms = append(ms, m)
	}
	return ms, nil
}

func (f *fakeCloud) HostKeys(context.Context, string, string) ([]ssh.PublicKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hostKeys, nil
}

func (f *fakeCloud) Delete(_ context.Context, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	delete(f.vms, name)
	return nil
}

func (f *fakeCloud) deletedVMs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func (f *fakeCloud) put(m machine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vms[m.Name] = m
}

// sshServer is a minimal sshd whose shell echoes its input.
type sshServer struct {
	addr    string
	port    string
	hostKey ssh.Signer
	resized chan [2]uint32

	mu      sync.Mutex
	allowed []ssh.PublicKey
}

func startSSHServer(t *testing.T) *sshServer {
	t.Helper()
	srv := &sshServer{hostKey: newSigner(t), resized: make(chan [2]uint32, 8)}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			srv.mu.Lock()
			defer srv.mu.Unlock()
			for _, k := range srv.allowed {
				if meta.User() == sshUser && bytes.Equal(k.Marshal(), key.Marshal()) {
					return nil, nil
				}
			}
			return nil, errors.New("key not allowed")
		},
	}
	cfg.AddHostKey(srv.hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv.addr = ln.Addr().String()
	_, srv.port, _ = net.SplitHostPort(srv.addr)
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.serve(nc, cfg)
		}
	}()
	return srv
}

// allow installs a client key, as the guest agent does with metadata keys.
func (s *sshServer) allow(authorized string) {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorized))
	if err != nil {
		panic(err)
	}
	s.mu.Lock()
	s.allowed = append(s.allowed, k)
	s.mu.Unlock()
}

func (s *sshServer) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		nc.Close()
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "sessions only")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range chReqs {
				switch req.Type {
				case "pty-req", "shell":
					req.Reply(true, nil)
				case "window-change":
					var wc struct{ Cols, Rows, Width, Height uint32 }
					if ssh.Unmarshal(req.Payload, &wc) == nil {
						s.resized <- [2]uint32{wc.Cols, wc.Rows}
					}
				default:
					req.Reply(false, nil)
				}
			}
		}()
		go func() {
			io.Copy(ch, ch)
			ch.Close()
		}()
	}
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func testConfig(t *testing.T) config {
	return config{
		Project: "test-project",
		Zone:    "us-central1-a",
		Image:   "projects/debian-cloud/global/images/family/debian-12",
		Network: "default",
		State:   t.TempDir(),
		MaxVMs:  10,
		Sizes:   "small,medium,large",
		MaxTTL:  2 * time.Hour,
	}
}

// testServer is Blink without sign-in, the way it runs on a laptop.
func testServer(t *testing.T, c cloud, sshPort string) *server {
	t.Helper()
	return serverFor(t, testConfig(t), c, sshPort)
}

func serverFor(t *testing.T, cfg config, c cloud, sshPort string) *server {
	t.Helper()
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.cloud = c
	s.sshPort = sshPort
	s.timing = timing{
		poll:         10 * time.Millisecond,
		hostKeyGrace: 100 * time.Millisecond,
		keyChurn:     100 * time.Millisecond,
		shellIdle:    time.Minute,
	}
	s.status = status{Ready: true, Project: cfg.Project, Zone: cfg.Zone}
	return s
}

const (
	adminEmail   = "teacher@school.edu"
	studentEmail = "ana@school.edu"
	otherEmail   = "ben@school.edu"
)

// classServer is Blink with sign-in on. Google is faked: a credential is
// accepted as the email it names.
func classServer(t *testing.T, cfg config, c cloud, sshPort string, roster ...string) *server {
	t.Helper()
	cfg.ClientID = "test-client"
	cfg.Admins = adminEmail
	s := serverFor(t, cfg, c, sshPort)
	s.auth.verify = func(_ context.Context, credential string) (string, error) {
		if !strings.Contains(credential, "@") {
			return "", errors.New("bad token")
		}
		return credential, nil
	}
	if _, err := s.auth.saveRoster(strings.Join(roster, "\n")); err != nil {
		t.Fatal(err)
	}
	return s
}

// signIn logs email in and returns its session cookie.
func signIn(t *testing.T, h http.Handler, email string) *http.Cookie {
	t.Helper()
	rec := request(h, "POST", "/api/login", `{"credential":"`+email+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("signing in %s: %d %s", email, rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("signing in %s set no session cookie", email)
	return nil
}

func request(h http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://localhost:8080"+path, strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
