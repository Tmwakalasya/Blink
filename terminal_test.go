package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testVM = "blink-abc123"

// readyVM sets up a running VM for owner whose key and host key Blink holds.
func readyVM(t *testing.T, s *server, fc *fakeCloud, sshd *sshServer, owner string) {
	t.Helper()
	fc.put(machine{Name: testVM, Zone: "us-central1-a", Status: "RUNNING", IP: "127.0.0.1", Owner: owner, blink: true})
	_, pub, err := s.keys.create(testVM)
	if err != nil {
		t.Fatal(err)
	}
	sshd.allow(pub)
	if err := s.keys.pin(testVM, sshd.addr, sshd.hostKey.PublicKey()); err != nil {
		t.Fatal(err)
	}
}

func localShell(t *testing.T) (*server, *sshServer, *httptest.Server) {
	t.Helper()
	sshd := startSSHServer(t)
	fc := newFakeCloud("127.0.0.1")
	s := testServer(t, fc, sshd.port)
	readyVM(t, s, fc, sshd, "local")
	hs := httptest.NewServer(s.routes())
	t.Cleanup(hs.Close)
	return s, sshd, hs
}

func dialTerminal(t *testing.T, hs *httptest.Server, name string, cookie *http.Cookie) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/api/vms/us-central1-a/" + name + "/terminal?cols=100&rows=30"
	opts := &websocket.DialOptions{}
	if cookie != nil {
		opts.HTTPHeader = http.Header{"Cookie": {cookie.String()}}
	}
	conn, _, err := websocket.Dial(ctx, url, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func typeLine(t *testing.T, conn *websocket.Conn, line string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"stdin","data":"`+line+`\n"}`)); err != nil {
		t.Fatal(err)
	}
}

// readUntil reads output until it contains want.
func readUntil(t *testing.T, conn *websocket.Conn, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out strings.Builder
	for !strings.Contains(out.String(), want) {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v (got %q)", want, err, out.String())
		}
		out.Write(data)
	}
}

func TestTerminalBridgesKeystrokesAndResizes(t *testing.T) {
	_, sshd, hs := localShell(t)
	conn := dialTerminal(t, hs, testVM, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-sshd.resized:
		if got != [2]uint32{120, 40} {
			t.Errorf("VM saw resize %v, want 120x40", got)
		}
	case <-ctx.Done():
		t.Fatal("resize never reached the VM")
	}

	typeLine(t, conn, "hello from blink")
	readUntil(t, conn, "hello from blink")
}

func TestTerminalReattachesToTheSameShell(t *testing.T) {
	_, _, hs := localShell(t)
	first := dialTerminal(t, hs, testVM, nil)
	typeLine(t, first, "still here")
	readUntil(t, first, "still here")
	first.Close(websocket.StatusGoingAway, "reload")

	// A reload or a dropped connection comes back to the same shell and
	// sees what it printed.
	second := dialTerminal(t, hs, testVM, nil)
	readUntil(t, second, "still here")
}

func TestTerminalMovesToTheNewestTab(t *testing.T) {
	_, _, hs := localShell(t)
	older := dialTerminal(t, hs, testVM, nil)
	typeLine(t, older, "tab one")
	readUntil(t, older, "tab one")

	dialTerminal(t, hs, testVM, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := older.Read(ctx); err != nil {
			if websocket.CloseStatus(err) != statusElsewhere {
				t.Fatalf("older tab closed with %v, want %d", err, statusElsewhere)
			}
			return
		}
	}
}

func TestTerminalRefusesVMsBlinkDidNotMake(t *testing.T) {
	s, _, hs := localShell(t)
	s.currentCloud().(*fakeCloud).put(machine{Name: "blink-other1", Zone: "us-central1-a", Status: "RUNNING", IP: "127.0.0.1"})

	conn := dialTerminal(t, hs, "blink-other1", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)

	if websocket.CloseStatus(err) != statusCantConnect {
		t.Fatalf("want close %d, got %v", statusCantConnect, err)
	}
	if !strings.Contains(err.Error(), "wasn't made by Blink") {
		t.Errorf("close reason should say why, got %v", err)
	}
}

func TestTerminalKeepsStudentsOutOfEachOthersVMs(t *testing.T) {
	sshd := startSSHServer(t)
	fc := newFakeCloud("127.0.0.1")
	s := classServer(t, testConfig(t), fc, sshd.port, studentEmail, otherEmail)
	readyVM(t, s, fc, sshd, ownerID(studentEmail))
	hs := httptest.NewServer(s.routes())
	t.Cleanup(hs.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := dialTerminal(t, hs, testVM, signIn(t, s.routes(), otherEmail))
	if _, _, err := conn.Read(ctx); !strings.Contains(err.Error(), "belongs to someone else") {
		t.Fatalf("another student got in, or the reason is wrong: %v", err)
	}

	own := dialTerminal(t, hs, testVM, signIn(t, s.routes(), studentEmail))
	typeLine(t, own, "mine")
	readUntil(t, own, "mine")
}
