package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// The terminal is a WebSocket between xterm.js in the page and an SSH session
// on the VM. The page sends JSON: {"type":"stdin","data":"..."} for keystrokes
// and {"type":"resize","cols":N,"rows":N} when its size changes. Blink sends
// the shell's output back as binary frames.

type termMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// statusCantConnect closes the socket when the shell couldn't be opened; the
// close reason says why.
const statusCantConnect websocket.StatusCode = 4000

func (s *server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil) // refuses other origins
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20) // room for big pastes

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	client, sess, stdin, err := s.openShell(ctx, r)
	if err != nil {
		conn.Close(statusCantConnect, closeReason(err))
		return
	}
	defer client.Close()
	defer sess.Close()

	out := wsWriter{ctx: ctx, conn: conn}
	sess.Stdout, sess.Stderr = out, out
	if err := sess.Shell(); err != nil {
		conn.Close(statusCantConnect, closeReason(err))
		return
	}
	go keepAlive(ctx, client)
	go func() {
		defer sess.Close()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var msg termMessage
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			switch msg.Type {
			case "stdin":
				io.WriteString(stdin, msg.Data)
			case "resize":
				if msg.Cols > 0 && msg.Rows > 0 {
					sess.WindowChange(msg.Rows, msg.Cols)
				}
			}
		}
	}()
	sess.Wait()
	conn.Close(websocket.StatusNormalClosure, "session ended")
}

// openShell connects to the VM named in the request and starts a PTY on it.
func (s *server) openShell(ctx context.Context, r *http.Request) (*ssh.Client, *ssh.Session, io.Writer, error) {
	zone, name := r.PathValue("zone"), r.PathValue("name")
	if !validZone(zone) || !validName(name) {
		return nil, nil, nil, errors.New("That isn't a Blink VM.")
	}
	c := s.currentCloud()
	if c == nil {
		return nil, nil, nil, errors.New("Blink isn't connected to Google Cloud yet.")
	}
	m, err := c.Get(ctx, zone, name)
	switch {
	case errors.Is(err, errNotFound):
		return nil, nil, nil, errors.New(name + " is gone.")
	case err != nil:
		problem, _ := s.explain(err)
		return nil, nil, nil, errors.New(problem)
	case !m.blink:
		return nil, nil, nil, errors.New(name + " wasn't made by Blink.")
	case m.Status != "RUNNING" || m.IP == "":
		return nil, nil, nil, errors.New(name + " isn't running.")
	}
	signer, err := s.keys.signer(name)
	if err != nil {
		return nil, nil, nil, errors.New("This computer doesn't have the key for " + name + ".")
	}
	checkHost, err := s.keys.hostKeyCallback(name)
	if err != nil {
		return nil, nil, nil, errors.New(name + " is still starting.")
	}
	client, err := dialSSH(ctx, net.JoinHostPort(m.IP, s.sshPort), &ssh.ClientConfig{
		User:            sshUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: checkHost,
	})
	if err != nil {
		return nil, nil, nil, errors.New("Couldn't reach " + name + " over SSH.")
	}
	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, nil, nil, err
	}
	cols, rows := dimension(r, "cols", 80, 10, 500), dimension(r, "rows", 24, 5, 200)
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		client.Close()
		return nil, nil, nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		client.Close()
		return nil, nil, nil, err
	}
	return client, sess, stdin, nil
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

type wsWriter struct {
	ctx  context.Context
	conn *websocket.Conn
}

func (w wsWriter) Write(p []byte) (int, error) {
	if err := w.conn.Write(w.ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
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
