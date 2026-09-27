package main

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// A shell outlives the WebSocket showing it. When the page reloads or the
// connection drops (Cloud Run ends every request after an hour), the next
// socket reattaches to the same shell and gets its recent output replayed,
// so whatever is running in it keeps running. A shell left with no socket
// for its idle time is closed.

const (
	scrollback = 64 << 10
	// statusElsewhere closes a socket whose shell was opened in another tab.
	statusElsewhere websocket.StatusCode = 4001
)

type shell struct {
	client *ssh.Client
	sess   *ssh.Session
	stdin  io.Writer
	idle   time.Duration
	done   chan struct{}
	once   sync.Once

	mu     sync.Mutex
	scroll []byte          // recent output, replayed when a socket attaches
	conn   *websocket.Conn // the attached socket, if any
	timer  *time.Timer     // closes the shell once it has been detached too long
}

func startShell(client *ssh.Client, sess *ssh.Session, stdout io.Reader, stdin io.Writer, idle time.Duration) *shell {
	sh := &shell{client: client, sess: sess, stdin: stdin, idle: idle, done: make(chan struct{})}
	sh.timer = time.AfterFunc(idle, sh.close)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-sh.done
		cancel()
	}()
	go keepAlive(ctx, client)
	go sh.pump(stdout)
	go func() {
		sess.Wait()
		sh.close()
	}()
	return sh
}

func (sh *shell) pump(stdout io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			sh.mu.Lock()
			sh.scroll = append(sh.scroll, buf[:n]...)
			if over := len(sh.scroll) - scrollback; over > 0 {
				sh.scroll = append([]byte(nil), sh.scroll[over:]...)
			}
			if sh.conn != nil {
				send(sh.conn, buf[:n])
			}
			sh.mu.Unlock()
		}
		if err != nil {
			sh.close()
			return
		}
	}
}

// attach shows the shell on conn, replaying recent output, and takes it away
// from any other socket. It reports false if the shell has already ended.
func (sh *shell) attach(conn *websocket.Conn) bool {
	sh.mu.Lock()
	if sh.ended() {
		sh.mu.Unlock()
		return false
	}
	old := sh.conn
	sh.conn = conn
	sh.timer.Stop()
	if len(sh.scroll) > 0 {
		send(conn, sh.scroll)
	}
	sh.mu.Unlock()
	if old != nil {
		go old.Close(statusElsewhere, "Opened in another tab.")
	}
	return true
}

// detach lets go of conn and starts the idle clock.
func (sh *shell) detach(conn *websocket.Conn) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.conn == conn {
		sh.conn = nil
		sh.timer.Reset(sh.idle)
	}
}

// input applies one message from the page: keystrokes or a resize.
func (sh *shell) input(msg termMessage) {
	switch msg.Type {
	case "stdin":
		io.WriteString(sh.stdin, msg.Data)
	case "resize":
		if msg.Cols > 0 && msg.Rows > 0 {
			sh.sess.WindowChange(msg.Rows, msg.Cols)
		}
	}
}

func (sh *shell) ended() bool {
	select {
	case <-sh.done:
		return true
	default:
		return false
	}
}

func (sh *shell) close() {
	sh.once.Do(func() {
		close(sh.done)
		sh.mu.Lock()
		conn := sh.conn
		sh.conn = nil
		sh.timer.Stop()
		sh.mu.Unlock()
		if conn != nil {
			go conn.Close(websocket.StatusNormalClosure, "Session ended.")
		}
		sh.sess.Close()
		sh.client.Close()
	})
}

// shells holds the live shell for each VM.
type shells struct {
	mu   sync.Mutex
	byVM map[string]*shell
}

// get returns the VM's live shell, starting one with open if there isn't one.
func (h *shells) get(vm string, open func() (*shell, error)) (*shell, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sh := h.byVM[vm]; sh != nil && !sh.ended() {
		return sh, nil
	}
	sh, err := open()
	if err != nil {
		return nil, err
	}
	h.byVM[vm] = sh
	go func() {
		<-sh.done
		h.mu.Lock()
		if h.byVM[vm] == sh {
			delete(h.byVM, vm)
		}
		h.mu.Unlock()
	}()
	return sh, nil
}

// end closes the VM's shell, if it has one.
func (h *shells) end(vm string) {
	h.mu.Lock()
	sh := h.byVM[vm]
	h.mu.Unlock()
	if sh != nil {
		sh.close()
	}
}

func send(conn *websocket.Conn, p []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn.Write(ctx, websocket.MessageBinary, p)
}
