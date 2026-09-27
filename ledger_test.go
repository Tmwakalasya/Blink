package main

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestLedgerChargesForWhatRan(t *testing.T) {
	l, err := openLedger(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Hour)
	l.add(usage{VM: "blink-a", Owner: "u1", Hourly: 0.12, Start: start, TTL: 3600})

	if got := l.totals("u1", start).Spent; !near(got, 0.12) {
		t.Errorf("a running VM should count its whole lifetime: $%.4f", got)
	}
	l.end("blink-a", start.Add(15*time.Minute))
	if got := l.totals("u1", start).Spent; !near(got, 0.03) {
		t.Errorf("after ending at 15 min: $%.4f, want $0.03", got)
	}
	l.end("blink-a", start.Add(30*time.Minute)) // later news can't extend it
	if got := l.totals("u1", start).Spent; !near(got, 0.03) {
		t.Errorf("a later end changed the charge: $%.4f", got)
	}

	l.add(usage{VM: "blink-b", Owner: "u2", Hourly: 0.12, Start: start, TTL: 3600})
	l.end("blink-b", start.Add(time.Second))
	if got := l.totals("u2", start).Spent - 0.03; !near(got, 0.002) {
		t.Errorf("a VM that ran a second costs $%.4f, want the one-minute minimum", got)
	}
}

func TestLedgerCountsActiveVMsAndWeeklyHours(t *testing.T) {
	l, _ := openLedger(filepath.Join(t.TempDir(), "ledger.jsonl"))
	now := time.Now()
	l.add(usage{VM: "blink-live", Owner: "u1", Hourly: 0.01, Start: now.Add(-10 * time.Minute), TTL: 1800})
	l.add(usage{VM: "blink-done", Owner: "u1", Hourly: 0.01, Start: now.Add(-3 * time.Hour), TTL: 3600})
	l.add(usage{VM: "blink-old", Owner: "u1", Hourly: 0.01, Start: now.Add(-8 * 24 * time.Hour), TTL: 3600})
	l.add(usage{VM: "blink-other", Owner: "u2", Hourly: 0.01, Start: now.Add(-time.Minute), TTL: 1800})

	got := l.totals("u1", now)
	if got.Active != 2 {
		t.Errorf("active = %d, want 2", got.Active)
	}
	if !near(got.Hours, 1.5) {
		t.Errorf("hours this week = %.2f, want 1.5 (last week's VM doesn't count)", got.Hours)
	}
}

func TestLedgerSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, _ := openLedger(path)
	start := time.Now().Add(-time.Hour)
	l.add(usage{VM: "blink-a", Owner: "u1", Email: "ana@school.edu", Hourly: 0.12, Start: start, TTL: 3600})
	l.end("blink-a", start.Add(30*time.Minute))

	again, err := openLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.totals("u1", start).Spent; !near(got, 0.06) {
		t.Errorf("after reopening: $%.4f, want $0.06", got)
	}
	if again.email("blink-a") != "ana@school.edu" {
		t.Error("the ledger forgot whose VM it was")
	}
}
