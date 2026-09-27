package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testVM = "blink-abc123"

// readyVM sets up a running VM whose key and host key Blink already holds.
func readyVM(t *testing.T) (*server, *sshServer, *httptest.Server) {
	t.Helper()
	sshd := startSSHServer(t)
	fc := newFakeCloud("127.0.0.1")
	fc.vms[testVM] = machine{Name: testVM, Zone: "us-central1-a", Status: "RUNNING", IP: "127.0.0.1", blink: true}
	s := testServer(t, fc, sshd.port)
	_, pub, err := s.keys.create(testVM)
	if err != nil {
		t.Fatal(err)
	}
	sshd.allow(pub)
	if err := s.keys.pin(testVM, sshd.addr, sshd.hostKey.PublicKey()); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.routes())
	t.Cleanup(hs.Close)
	return s, sshd, hs
}

func dialTerminal(t *testing.T, hs *httptest.Server, name string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/api/vms/us-central1-a/" + name + "/terminal?cols=100&rows=30"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func TestTerminalBridgesKeystrokesAndResizes(t *testing.T) {
	_, sshd, hs := readyVM(t)
	conn := dialTerminal(t, hs, testVM)
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

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"stdin","data":"hello from blink\n"}`)); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for !strings.Contains(out.String(), "hello from blink") {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("reading shell output: %v (so far %q)", err, out.String())
		}
		out.Write(data)
	}
}

func TestTerminalRefusesVMsBlinkDidNotMake(t *testing.T) {
	s, _, hs := readyVM(t)
	fc := s.cloud.(*fakeCloud)
	fc.vms["blink-other1"] = machine{Name: "blink-other1", Zone: "us-central1-a", Status: "RUNNING", IP: "127.0.0.1"}

	conn := dialTerminal(t, hs, "blink-other1")
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
