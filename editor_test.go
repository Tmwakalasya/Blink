package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEditor stands in for code-server on the VM and remembers what reached it.
type fakeEditor struct {
	mu      sync.Mutex
	paths   []string
	cookies []string
}

func startFakeEditor(t *testing.T) (*fakeEditor, string) {
	t.Helper()
	fe := &fakeEditor{}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fe.mu.Lock()
		fe.paths = append(fe.paths, r.URL.Path)
		fe.cookies = append(fe.cookies, r.Header.Get("Cookie"))
		fe.mu.Unlock()
		io.WriteString(w, "editor ok")
	}))
	t.Cleanup(hs.Close)
	return fe, strings.TrimPrefix(hs.URL, "http://")
}

func (fe *fakeEditor) seen() ([]string, []string) {
	fe.mu.Lock()
	defer fe.mu.Unlock()
	return append([]string(nil), fe.paths...), append([]string(nil), fe.cookies...)
}

func TestEditorIsProxiedOnlyToItsOwner(t *testing.T) {
	sshd := startSSHServer(t)
	fc := newFakeCloud("127.0.0.1")
	s := classServer(t, testConfig(t), fc, sshd.port, studentEmail, otherEmail)
	fe, addr := startFakeEditor(t)
	s.editorAddr = addr
	readyVM(t, s, fc, sshd, ownerID(studentEmail))
	h := s.routes()
	path := "/editor/us-central1-a/" + testVM + "/healthz"

	rec := request(h, "GET", path, "", signIn(t, h, studentEmail))
	if rec.Code != http.StatusOK || rec.Body.String() != "editor ok" {
		t.Fatalf("owner got %d %q", rec.Code, rec.Body)
	}
	paths, cookies := fe.seen()
	if len(paths) != 1 || paths[0] != "/healthz" {
		t.Errorf("the editor saw %v, want [/healthz] with Blink's prefix stripped", paths)
	}
	if strings.Contains(cookies[0], sessionCookie) {
		t.Errorf("Blink's session cookie reached the VM: %q", cookies[0])
	}

	for _, email := range []string{otherEmail, adminEmail} {
		if rec := request(h, "GET", path, "", signIn(t, h, email)); rec.Code == http.StatusOK {
			t.Errorf("%s opened someone else's editor", email)
		}
	}
	if rec := request(h, "GET", path, "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("a visitor reached the editor: %d", rec.Code)
	}
	if paths, _ := fe.seen(); len(paths) != 1 {
		t.Errorf("refused requests still reached the VM: %v", paths)
	}
}

func TestEditorSaysSoWhileItInstalls(t *testing.T) {
	sshd := startSSHServer(t)
	fc := newFakeCloud("127.0.0.1")
	s := testServer(t, fc, sshd.port)
	s.editorAddr = "127.0.0.1:1" // nothing listens here yet
	readyVM(t, s, fc, sshd, "local")

	rec := request(s.routes(), "GET", "/editor/us-central1-a/"+testVM+"/healthz", "", nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "isn't running") {
		t.Fatalf("got %d %q, want 502 saying VS Code isn't running yet", rec.Code, rec.Body)
	}
}

func TestOnlyBiggerSizesInstallTheEditor(t *testing.T) {
	base := spec{Name: "blink-abc123", Zone: "us-central1-a", Machine: "e2-medium", TTL: time.Hour}
	meta := func(s spec, key string) string {
		for _, item := range newInstance(s).GetMetadata().GetItems() {
			if item.GetKey() == key {
				return item.GetValue()
			}
		}
		return ""
	}
	script := func(s spec) string { return meta(s, "startup-script") }
	withEditor := base
	withEditor.Editor = true
	if got := script(withEditor); !strings.Contains(got, "code-server --bind-addr 127.0.0.1:8080 --auth none") {
		t.Errorf("editor VM's startup script doesn't run code-server on localhost:\n%s", got)
	}
	if got := script(base); got != "" {
		t.Errorf("a VM without the editor got a startup script")
	}
	// The pre-built image starts VS Code at boot only on VMs with this flag.
	if meta(withEditor, "blink-editor") != "TRUE" || meta(base, "blink-editor") != "" {
		t.Error("the blink-editor flag should be set exactly on editor VMs")
	}
	if !strings.Contains(editorUnit, "ExecCondition=") {
		t.Error("the VS Code unit must check the flag before starting")
	}
	for _, sz := range menu {
		if want := sz.ID != "small"; sz.Editor != want {
			t.Errorf("%s: editor = %v, want %v", sz.ID, sz.Editor, want)
		}
	}
}
