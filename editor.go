package main

import (
	"context"
	_ "embed"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// The editor is VS Code in the browser (code-server). Medium and large VMs
// install it at boot and run it on localhost only; Blink proxies to it
// through the VM's SSH connection, so it sits behind Blink's sign-in and is
// never exposed to the internet.

// editorUnit runs VS Code at boot. The pre-built image already has it; on
// other images the startup script below installs everything first.
//
//go:embed deploy/blink-editor.service
var editorUnit string

// editorScript is the startup script for VMs that get the editor. On the
// pre-built image it only makes sure VS Code is running.
var editorScript = `#!/bin/bash
set -euo pipefail
export HOME=/root # startup scripts run without one, and the installer needs it
if [ ! -f /etc/systemd/system/blink-editor.service ]; then
  id blink >/dev/null 2>&1 || useradd --create-home --shell /bin/bash blink
  command -v code-server >/dev/null || curl -fsSL https://code-server.dev/install.sh | sh
  cat >/etc/systemd/system/blink-editor.service <<'UNIT'
` + editorUnit + `UNIT
  systemctl daemon-reload
fi
systemctl enable --now blink-editor
`

// tunnel is a standing SSH connection to one VM that editor requests ride on.
type tunnel struct {
	client *ssh.Client
	proxy  *httputil.ReverseProxy
	stop   context.CancelFunc
}

type tunnels struct {
	mu   sync.Mutex
	byVM map[string]*tunnel
}

// get returns the VM's tunnel, dialing one with dial if there isn't one.
func (t *tunnels) get(vm string, dial func() (*tunnel, error)) (*tunnel, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tn := t.byVM[vm]; tn != nil {
		return tn, nil
	}
	tn, err := dial()
	if err != nil {
		return nil, err
	}
	t.byVM[vm] = tn
	return tn, nil
}

// drop forgets tn if it's still the VM's tunnel, so the next request redials.
func (t *tunnels) drop(vm string, tn *tunnel) {
	t.mu.Lock()
	if t.byVM[vm] == tn {
		delete(t.byVM, vm)
	}
	t.mu.Unlock()
	tn.stop()
	tn.client.Close()
}

func (t *tunnels) end(vm string) {
	t.mu.Lock()
	tn := t.byVM[vm]
	t.mu.Unlock()
	if tn != nil {
		t.drop(vm, tn)
	}
}

func (s *server) handleEditor(w http.ResponseWriter, r *http.Request, u user) {
	zone, name := r.PathValue("zone"), r.PathValue("name")
	m, err := s.ownedVM(r.Context(), zone, name, u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// Not even admins: the editor runs whatever the VM's owner put in it, and
	// it would run with the admin's session in this origin.
	if m.Owner != u.owner {
		http.Error(w, "Only the person who started "+name+" can open its editor.", http.StatusForbidden)
		return
	}
	if m.Status != "RUNNING" || m.IP == "" {
		http.Error(w, name+" isn't running.", http.StatusConflict)
		return
	}
	tn, err := s.tunnels.get(name, func() (*tunnel, error) { return s.openTunnel(r.Context(), m) })
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	tn.proxy.ServeHTTP(w, r)
}

// openTunnel connects to the VM and builds the proxy to its editor.
func (s *server) openTunnel(ctx context.Context, m machine) (*tunnel, error) {
	client, err := s.dialVM(ctx, m)
	if err != nil {
		return nil, err
	}
	kctx, stop := context.WithCancel(context.Background())
	go keepAlive(kctx, client)
	tn := &tunnel{client: client, stop: stop}
	prefix := "/editor/" + m.Zone + "/" + m.Name
	tn.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = s.editorAddr
			pr.Out.URL.Path = "/" + strings.TrimPrefix(strings.TrimPrefix(pr.In.URL.Path, prefix), "/")
			pr.Out.URL.RawPath = ""
			pr.Out.Host = pr.In.Host // code-server checks that Origin matches Host
			pr.SetXForwarded()
			dropCookie(pr.Out.Header, sessionCookie) // the VM has no business with Blink's session
		},
		Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return client.Dial("tcp", s.editorAddr)
			},
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var refused *ssh.OpenChannelError
			if !errors.As(err, &refused) {
				s.tunnels.drop(m.Name, tn) // the SSH connection itself failed
			}
			http.Error(w, "VS Code isn't running on "+m.Name+" yet.", http.StatusBadGateway)
		},
	}
	return tn, nil
}

func dropCookie(h http.Header, name string) {
	var keep []string
	for _, line := range h.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			if k, _, _ := strings.Cut(strings.TrimSpace(part), "="); k != name && strings.TrimSpace(part) != "" {
				keep = append(keep, strings.TrimSpace(part))
			}
		}
	}
	h.Del("Cookie")
	if len(keep) > 0 {
		h.Set("Cookie", strings.Join(keep, "; "))
	}
}
