package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func runSummon(t *testing.T, s *server) []event {
	t.Helper()
	var events []event
	s.summon(context.Background(), time.Now(), func(e event) { events = append(events, e) })
	if len(events) == 0 {
		t.Fatal("summon emitted nothing")
	}
	return events
}

func completedSteps(events []event) []string {
	var steps []string
	for _, e := range events {
		if e.Step != "" && e.Error == "" {
			steps = append(steps, e.Step)
		}
	}
	return steps
}

func TestSummonReachesAVerifiedShell(t *testing.T) {
	sshd := startSSHServer(t)
	fc := newFakeCloud("127.0.0.1")
	fc.hostKeys = []ssh.PublicKey{sshd.hostKey.PublicKey()}
	fc.onInsert = func(sp spec) { sshd.allow(sp.PublicKey) }
	s := testServer(t, fc, sshd.port)

	events := runSummon(t, s)

	if got, want := completedSteps(events), []string{"requested", "creating", "booting", "ssh"}; !slices.Equal(got, want) {
		t.Fatalf("steps = %v, want %v (events %+v)", got, want, events)
	}
	if note := events[3].Note; note != "Host key verified" {
		t.Errorf("ssh step note = %q, want Host key verified", note)
	}
	last := events[len(events)-1]
	if last.VM == nil || !last.VM.Ready || last.VM.SSH == "" {
		t.Fatalf("last event should carry a ready VM with an ssh command, got %+v", last)
	}
	if d := fc.deletedVMs(); len(d) != 0 {
		t.Errorf("a successful summon deleted %v", d)
	}

	// The pinned host key must be the one the VM actually has.
	check, err := s.keys.hostKeyCallback(last.VM.Name)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(sshd.port)
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}
	if err := check(sshd.addr, remote, sshd.hostKey.PublicKey()); err != nil {
		t.Errorf("pinned key rejects the VM's own host key: %v", err)
	}
	if err := check(sshd.addr, remote, newSigner(t).PublicKey()); err == nil {
		t.Error("pinned key accepts a stranger's host key")
	}
}

func TestSummonHangsUpOnUnpublishedHostKey(t *testing.T) {
	sshd := startSSHServer(t)
	fc := newFakeCloud("127.0.0.1")
	fc.hostKeys = []ssh.PublicKey{newSigner(t).PublicKey()} // not the key sshd presents
	fc.onInsert = func(sp spec) { sshd.allow(sp.PublicKey) }
	s := testServer(t, fc, sshd.port)

	events := runSummon(t, s)
	last := events[len(events)-1]

	if last.Error == "" || last.Step != "ssh" {
		t.Fatalf("want a failure at the ssh step, got %+v", last)
	}
	if !strings.Contains(last.Error, "host key") {
		t.Errorf("error should explain the host key mismatch, got %q", last.Error)
	}
	name := events[0].Note
	if d := fc.deletedVMs(); !slices.Equal(d, []string{name}) {
		t.Errorf("deleted = %v, want [%s]", d, name)
	}
	if s.keys.has(name) {
		t.Error("keys for the failed VM were left behind")
	}
}

func TestSummonTrustsFirstKeyWhenNonePublished(t *testing.T) {
	sshd := startSSHServer(t)
	fc := newFakeCloud("127.0.0.1")
	fc.onInsert = func(sp spec) { sshd.allow(sp.PublicKey) }
	s := testServer(t, fc, sshd.port)

	events := runSummon(t, s)
	last := events[len(events)-1]

	if last.VM == nil {
		t.Fatalf("want a ready VM, got %+v", last)
	}
	if note := events[3].Note; !strings.Contains(note, "trusted on first use") {
		t.Errorf("ssh step note = %q, want trusted on first use", note)
	}
}

func TestSummonDeletesTheVMWhenCreationFails(t *testing.T) {
	fc := newFakeCloud("127.0.0.1")
	fc.waitErr = errors.New("ZONE_RESOURCE_POOL_EXHAUSTED: The zone does not have enough resources available")
	s := testServer(t, fc, "22")

	events := runSummon(t, s)
	last := events[len(events)-1]

	if last.Step != "creating" || !strings.Contains(last.Error, "capacity") {
		t.Fatalf("want a capacity failure while creating, got %+v", last)
	}
	if last.Fix == "" {
		t.Error("a capacity failure should suggest another zone")
	}
	if d := fc.deletedVMs(); len(d) != 1 {
		t.Errorf("deleted = %v, want the half-made VM", d)
	}
	if s.keys.has(events[0].Note) {
		t.Error("keys for the failed VM were left behind")
	}
}

func TestSummonRejectedRequestLeavesNothingBehind(t *testing.T) {
	fc := newFakeCloud("127.0.0.1")
	fc.insertErr = errors.New("Compute Engine API has not been used in project test-project before or it is disabled")
	s := testServer(t, fc, "22")

	events := runSummon(t, s)
	last := events[len(events)-1]

	if last.Step != "requested" || !strings.Contains(last.Fix, "gcloud services enable compute.googleapis.com") {
		t.Fatalf("want a disabled-API failure with the fix, got %+v", last)
	}
	if d := fc.deletedVMs(); len(d) != 0 {
		t.Errorf("nothing was created, but Blink deleted %v", d)
	}
	if entries, _ := filepathGlob(s.keys.dir); len(entries) != 0 {
		t.Errorf("keys left behind: %v", entries)
	}
}

func TestSummonRefusesASecondVM(t *testing.T) {
	fc := newFakeCloud("127.0.0.1")
	fc.vms["blink-abc123"] = machine{Name: "blink-abc123", Zone: "us-central1-a", Status: "RUNNING", IP: "127.0.0.1", blink: true}
	s := testServer(t, fc, "22")

	req := httptest.NewRequest("POST", "http://localhost:8080/api/vms", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var body apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.VM == nil || body.VM.Name != "blink-abc123" {
		t.Errorf("409 should point at the running VM, got %+v", body)
	}
}
