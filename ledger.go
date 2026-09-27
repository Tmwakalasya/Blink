package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// The ledger records every VM Blink starts so that it can keep spending under
// the budget. Google bills after the fact and its budgets only send email,
// so this is what actually stops new VMs.

// usage is one VM's line in the ledger.
type usage struct {
	VM     string    `json:"vm"`
	Zone   string    `json:"zone,omitempty"`
	Owner  string    `json:"owner"`
	Email  string    `json:"email,omitempty"`
	Size   string    `json:"size"`
	Hourly float64   `json:"hourly"`
	Start  time.Time `json:"start"`
	TTL    int64     `json:"ttlSeconds"`
	End    time.Time `json:"end,omitzero"`
}

// until is when the VM stopped costing money as far as Blink knows: when
// Blink saw it end, or else when Google deletes it on schedule.
func (u usage) until() time.Time {
	stop := u.Start.Add(time.Duration(u.TTL) * time.Second)
	if !u.End.IsZero() && u.End.Before(stop) {
		stop = u.End
	}
	return stop
}

func (u usage) cost() float64 {
	return u.Hourly * max(u.until().Sub(u.Start), time.Minute).Hours()
}

func (u usage) active(now time.Time) bool { return now.Before(u.until()) }

type ledger struct {
	path string
	mu   sync.Mutex
	rows map[string]usage
}

// openLedger loads the ledger at path. The file is JSON lines, and a later
// line for the same VM replaces an earlier one.
func openLedger(path string) (*ledger, error) {
	l := &ledger{path: path, rows: map[string]usage{}}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		var u usage
		if err := json.Unmarshal(sc.Bytes(), &u); err != nil {
			return nil, fmt.Errorf("ledger line %d is invalid; refusing to undercount usage: %w", line, err)
		}
		if u.VM == "" {
			return nil, fmt.Errorf("ledger line %d has no VM; refusing to undercount usage", line)
		}
		l.rows[u.VM] = u
	}
	return l, sc.Err()
}

func (l *ledger) record(u usage) error {
	b, err := json.Marshal(u)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	l.rows[u.VM] = u
	return nil
}

// add records a VM Blink is about to start.
func (l *ledger) add(u usage) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.record(u)
}

// end records that a VM stopped at t, if it hadn't already.
func (l *ledger) end(vm string, t time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	u, ok := l.rows[vm]
	if !ok || !t.Before(u.until()) {
		return nil
	}
	u.End = t
	return l.record(u)
}

// totals is what the ledger knows at one moment.
type totals struct {
	Spent  float64 // estimated dollars, counting running VMs in full
	Active int     // VMs that may still be running
	Hours  float64 // hours used by one owner in the last week
}

func (l *ledger) totals(owner string, now time.Time) totals {
	l.mu.Lock()
	defer l.mu.Unlock()
	var t totals
	weekAgo := now.Add(-7 * 24 * time.Hour)
	for _, u := range l.rows {
		t.Spent += u.cost()
		if u.active(now) {
			t.Active++
		}
		if u.Owner == owner && u.Start.After(weekAgo) {
			t.Hours += u.until().Sub(u.Start).Hours()
		}
	}
	return t
}

// person is one row of the usage table admins see.
type person struct {
	Email string  `json:"email"`
	VMs   int     `json:"vms"`
	Hours float64 `json:"hours"`
	Cost  float64 `json:"cost"`
}

// week sums each person's use over the last seven days, busiest first.
func (l *ledger) week(now time.Time) []person {
	l.mu.Lock()
	defer l.mu.Unlock()
	byOwner := map[string]*person{}
	weekAgo := now.Add(-7 * 24 * time.Hour)
	for _, u := range l.rows {
		if !u.Start.After(weekAgo) {
			continue
		}
		p := byOwner[u.Owner]
		if p == nil {
			p = &person{Email: u.Email}
			if p.Email == "" {
				p.Email = u.Owner
			}
			byOwner[u.Owner] = p
		}
		p.VMs++
		p.Hours += u.until().Sub(u.Start).Hours()
		p.Cost += u.cost()
	}
	people := make([]person, 0, len(byOwner))
	for _, p := range byOwner {
		people = append(people, *p)
	}
	sort.Slice(people, func(i, j int) bool { return people[i].Hours > people[j].Hours })
	return people
}

// activeFor returns the newest VM of owner's that may still be running.
func (l *ledger) activeFor(owner string, now time.Time) (usage, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var found usage
	for _, u := range l.rows {
		if u.Owner == owner && u.active(now) && u.Start.After(found.Start) {
			found = u
		}
	}
	return found, found.VM != ""
}

func (l *ledger) email(vm string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rows[vm].Email
}

// activeVMs includes reservations whose Insert result may still be unknown.
func (l *ledger) activeVMs(now time.Time) []usage {
	l.mu.Lock()
	defer l.mu.Unlock()
	var rows []usage
	for _, u := range l.rows {
		if u.active(now) {
			rows = append(rows, u)
		}
	}
	return rows
}
