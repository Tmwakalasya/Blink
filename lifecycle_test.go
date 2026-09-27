package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestFailedProvisioningRetainsReservationWhenCleanupFails(t *testing.T) {
	fc := newFakeCloud("127.0.0.1")
	fc.waitErr = errors.New("provisioning interrupted")
	fc.deleteErr = errors.New("delete unavailable")
	s := testServer(t, fc, "22")
	events := runSummon(t, s)
	last := events[len(events)-1]
	if !strings.Contains(last.Error, "Cleanup failed") || strings.Contains(last.Error, "was deleted") {
		t.Fatalf("cleanup must report uncertainty: %+v", last)
	}
	u, ok := s.ledger.activeFor(localUser.owner, time.Now())
	if !ok || !s.keys.has(u.VM) {
		t.Fatal("failed deletion discarded the reservation or access key")
	}
	if got := s.ledger.totals(localUser.owner, time.Now()).Spent; !near(got, s.sizes[0].cost(30*time.Minute)) {
		t.Fatalf("failed deletion released reserved spending: %v", got)
	}
	// A subsequent explicit deletion can finish cleanup, including accounting.
	fc.deleteErr = nil
	rec := request(s.routes(), "DELETE", "/api/vms/"+u.Zone+"/"+u.VM, "", nil)
	if rec.Code != http.StatusNoContent || s.keys.has(u.VM) || s.ledger.totals(localUser.owner, time.Now()).Active != 0 {
		t.Fatalf("retry did not finish cleanup: %d %s", rec.Code, rec.Body)
	}
}

func TestDeleteWaitsForObservedAbsence(t *testing.T) {
	fc := newFakeCloud("127.0.0.1")
	fc.deletePending = true
	s := testServer(t, fc, "22")
	sp, no := s.admitStart(context.Background(), fc, localUser, s.sizes[0], time.Hour)
	if no != nil {
		t.Fatal(no.Error)
	}
	if _, _, err := s.keys.create(sp.Name); err != nil {
		t.Fatal(err)
	}
	fc.put(machine{Name: sp.Name, Zone: sp.Zone, Owner: sp.Owner, Status: "RUNNING", blink: true})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err := s.deleteVM(ctx, fc, sp.Zone, sp.Name)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("accepted deletion was mistaken for completion: %v", err)
	}
	if !s.keys.has(sp.Name) || s.ledger.totals(sp.Owner, time.Now()).Active != 1 {
		t.Fatal("pending deletion discarded its key or reservation")
	}
}

func TestInsertTimeoutRetainsReservationEvenWhenVMNotVisibleYet(t *testing.T) {
	fc := newFakeCloud("127.0.0.1")
	fc.insertErr = context.DeadlineExceeded
	s := testServer(t, fc, "22")
	events := runSummon(t, s)
	if !strings.Contains(events[len(events)-1].Error, "could not be ruled out") {
		t.Fatalf("lost Insert response was reported as a definite rejection: %+v", events)
	}
	u, ok := s.ledger.activeFor(localUser.owner, time.Now())
	if !ok || !s.keys.has(u.VM) {
		t.Fatal("uncertain creation discarded its key or reservation")
	}
	// A restart or retry must not undo that conservative reservation.
	s.pruneKeys(context.Background())
	if !s.keys.has(u.VM) {
		t.Fatal("startup pruning discarded an unresolved start's key")
	}
	s.admit.Lock()
	delete(s.starting, localUser.owner) // handleSummon's deferred cleanup
	s.admit.Unlock()
	if _, no := s.admitStart(context.Background(), fc, localUser, s.sizes[0], time.Hour); no == nil || !strings.Contains(no.Error, "unresolved start") {
		t.Fatalf("a retry silently released the unresolved reservation: %+v", no)
	}
}

func TestRestartRecoversUnfinishedConnectionWithoutCreatingAnotherVM(t *testing.T) {
	sshd := startSSHServer(t)
	fc := newFakeCloud("127.0.0.1")
	fc.hostKeys = []ssh.PublicKey{sshd.hostKey.PublicKey()}
	cfg := testConfig(t)
	s := serverFor(t, cfg, fc, sshd.port)
	sp, no := s.admitStart(context.Background(), fc, localUser, s.sizes[0], time.Hour)
	if no != nil {
		t.Fatal(no.Error)
	}
	_, pub, err := s.keys.create(sp.Name)
	if err != nil {
		t.Fatal(err)
	}
	sshd.allow(pub)
	fc.put(machine{Name: sp.Name, Zone: sp.Zone, Owner: sp.Owner, Size: sp.Size, Status: "RUNNING", IP: "127.0.0.1", blink: true})
	// Reopen from disk as a fresh process, before known_hosts was written.
	restarted := serverFor(t, cfg, fc, sshd.port)
	restarted.recoverStarts(context.Background())
	m, _ := fc.Get(context.Background(), sp.Zone, sp.Name)
	restarted.decorate(&m, localUser)
	if !m.Ready {
		t.Fatal("restart left a running VM inaccessible")
	}
	client, err := restarted.dialVM(context.Background(), m)
	if err != nil {
		t.Fatalf("recovered VM cannot actually be reached with the pinned key: %v", err)
	}
	client.Close()
	if len(fc.vms) != 1 || len(fc.deletedVMs()) != 0 || restarted.ledger.totals(sp.Owner, time.Now()).Active != 1 {
		t.Fatal("recovery changed VM count or spending reservation")
	}
}

func TestHostKeyAPIFailureDoesNotDowngradeToTrustOnFirstUse(t *testing.T) {
	fc := newFakeCloud("127.0.0.1")
	fc.hostKeysErr = errors.New("permission denied")
	s := testServer(t, fc, "22")
	s.timing.hostKeyGrace = -time.Second
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, trusted, err := s.untilSSH(ctx, fc, machine{Name: "blink-test", Zone: "us-central1-a"}, "127.0.0.1:1", newSigner(t))
	if err == nil || !strings.Contains(err.Error(), "permission denied") || trusted {
		t.Fatalf("host-key lookup failure wasn't preserved: trusted=%v error=%v", trusted, err)
	}
}

func TestLedgerRejectsCorruptionInsteadOfDroppingCharges(t *testing.T) {
	for _, contents := range []string{"{broken\n", "{}\n"} {
		path := filepath.Join(t.TempDir(), "ledger.jsonl")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := openLedger(path); err == nil || !strings.Contains(err.Error(), "refusing to undercount") {
			t.Fatalf("corrupt ledger was silently accepted: %v", err)
		}
	}
}
