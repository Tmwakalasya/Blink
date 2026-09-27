package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// runSummon starts a small VM for the local user and returns every event.
func runSummon(t *testing.T, s *server) []event {
	t.Helper()
	ctx := context.Background()
	sp, no := s.admitStart(ctx, s.currentCloud(), localUser, s.sizes[0], 30*time.Minute)
	if no != nil {
		t.Fatalf("start refused: %s", no.Error)
	}
	var events []event
	s.summon(ctx, time.Now(), sp, localUser, func(e event) { events = append(events, e) })
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
		t.Errorf("a successful start deleted %v", d)
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
	if d := fc.deletedVMs(); len(d) != 1 {
		t.Errorf("deleted = %v, want the half-made VM", d)
	}
	if s.keys.has(events[0].Note) {
		t.Error("keys for the failed VM were left behind")
	}
	if spent := s.ledger.totals("local", time.Now()).Spent; spent > 0.01 {
		t.Errorf("a failed start still counts $%.4f against the budget", spent)
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

func TestStartRefusesASecondVM(t *testing.T) {
	fc := newFakeCloud("127.0.0.1")
	fc.put(machine{Name: "blink-abc123", Zone: "us-central1-a", Status: "RUNNING", IP: "127.0.0.1", Owner: "local", blink: true})
	s := testServer(t, fc, "22")
	s.ledger.add(usage{VM: "blink-abc123", Zone: "us-central1-a", Owner: "local", Hourly: 0.015, Start: time.Now(), TTL: 1800})

	rec := request(s.routes(), "POST", "/api/vms", `{"size":"small","ttlSeconds":1800}`, nil)

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

func TestStartForgetsVMsThatAreGone(t *testing.T) {
	fc := newFakeCloud("127.0.0.1")
	s := testServer(t, fc, "22")
	// The ledger thinks this VM is running, but it was deleted in the console.
	s.ledger.add(usage{VM: "blink-gone01", Zone: "us-central1-a", Owner: "local", Hourly: 0.015, Start: time.Now(), TTL: 1800})

	if _, no := s.admitStart(context.Background(), fc, localUser, s.sizes[0], 30*time.Minute); no != nil {
		t.Fatalf("a VM that no longer exists blocked a new one: %s", no.Error)
	}
	if _, ok := s.ledger.activeFor("local", time.Now().Add(time.Minute)); !ok {
		t.Fatal("the new VM wasn't recorded")
	}
	if u, _ := s.ledger.activeFor("local", time.Now()); u.VM == "blink-gone01" {
		t.Error("the deleted VM still counts as running")
	}
}

func TestStartOnlyOffersTheMenu(t *testing.T) {
	cfg := testConfig(t)
	cfg.Sizes, cfg.MaxTTL = "small,medium", time.Hour
	s := serverFor(t, cfg, newFakeCloud("127.0.0.1"), "22")
	h := s.routes()

	for _, body := range []string{
		`{"size":"large","ttlSeconds":1800}`, // not offered
		`{"size":"small","ttlSeconds":7200}`, // longer than -max-ttl
		`{"size":"small","ttlSeconds":999}`,  // not a lifetime on the menu
		`{"size":"huge","ttlSeconds":1800}`,  // not a size at all
	} {
		if rec := request(h, "POST", "/api/vms", body, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
	}
}
