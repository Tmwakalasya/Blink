package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// The terminal is a WebSocket between xterm.js in the page and a shell on the
// VM. The page sends JSON: {"type":"stdin","data":"..."} for keystrokes and
// {"type":"resize","cols":N,"rows":N} when its size changes. Blink sends the
// shell's output back as binary frames.

type termMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// statusCantConnect closes the socket when the shell couldn't be opened; the
// close reason says why.
const statusCantConnect websocket.StatusCode = 4000

func (s *server) handleTerminal(w http.ResponseWriter, r *http.Request, u user) {
	conn, err := websocket.Accept(w, r, nil) // refuses other origins
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20) // room for big pastes
	ctx := r.Context()

	m, err := s.ownedVM(ctx, r.PathValue("zone"), r.PathValue("name"), u)
	if err == nil && (m.Status != "RUNNING" || m.IP == "") {
		err = errors.New(m.Name + " isn't running.")
	}
	var sh *shell
	if err == nil {
		sh, err = s.shells.get(m.Name, func() (*shell, error) {
			return s.openShell(ctx, m, dimension(r, "cols", 80, 10, 500), dimension(r, "rows", 24, 5, 200))
		})
	}
	if err == nil && !sh.attach(conn) {
		err = errors.New("That session just ended. Reconnect to start a new one.")
	}
	if err != nil {
		if errors.Is(err, errNotFound) {
			err = errors.New(r.PathValue("name") + " is gone.")
		}
		conn.Close(statusCantConnect, closeReason(err))
		return
	}
	defer sh.detach(conn)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg termMessage
		if json.Unmarshal(data, &msg) == nil {
			sh.input(msg)
		}
	}
}

// dialVM opens an SSH connection to the VM with its key and pinned host key.
func (s *server) dialVM(ctx context.Context, m machine) (*ssh.Client, error) {
	signer, err := s.keys.signer(m.Name)
	if err != nil {
		return nil, errors.New("Blink doesn't have the key for " + m.Name + ".")
	}
	checkHost, err := s.keys.hostKeyCallback(m.Name)
	if err != nil {
		return nil, errors.New(m.Name + " is still starting.")
	}
	client, err := dialSSH(ctx, net.JoinHostPort(m.IP, s.sshPort), &ssh.ClientConfig{
		User:            sshUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: checkHost,
	})
	if err != nil {
		return nil, errors.New("Couldn't reach " + m.Name + " over SSH.")
	}
	return client, nil
}

// openShell connects to the VM and starts a shell on a PTY of the given size.
func (s *server) openShell(ctx context.Context, m machine, cols, rows int) (*shell, error) {
	client, err := s.dialVM(ctx, m)
	if err != nil {
		return nil, err
	}
	sess, err := client.NewSession()
	if err == nil {
		modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
		err = sess.RequestPty("xterm-256color", rows, cols, modes)
	}
	if err != nil {
		client.Close()
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		client.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		client.Close()
		return nil, err
	}
	if err := sess.Shell(); err != nil {
		client.Close()
		return nil, err
	}
	return startShell(client, sess, stdout, stdin, s.timing.shellIdle), nil
}

// keepAlive pings the VM every 20 seconds and hangs up if it stops answering,
// which is how a VM that Google deleted on schedule usually shows up.
func keepAlive(ctx context.Context, c *ssh.Client) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		reply := make(chan error, 1)
		go func() {
			_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
			reply <- err
		}()
		select {
		case err := <-reply:
			if err != nil {
				return
			}
		case <-time.After(15 * time.Second):
			c.Close()
			return
		case <-ctx.Done():
			return
		}
	}
}

func dimension(r *http.Request, key string, def, lo, hi int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return def
	}
	return min(max(n, lo), hi)
}

// closeReason fits an error into a WebSocket close frame, which allows 123 bytes.
func closeReason(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}
