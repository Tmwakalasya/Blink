package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// size is one entry on the machine menu. Hourly is an estimate for
// us-central1 that includes the public IP and the boot disk, and ignores the
// free tier, so the budget errs on the high side.
type size struct {
	ID      string  `json:"id"`
	Label   string  `json:"label"`
	Machine string  `json:"machine"`
	CPU     string  `json:"cpu"`
	Memory  string  `json:"memory"`
	Hourly  float64 `json:"hourly"`
	Editor  bool    `json:"editor"` // gets VS Code in the browser; 1 GB is too little for it
}

var menu = []size{
	{ID: "small", Label: "Small", Machine: "e2-micro", CPU: "2 shared vCPUs", Memory: "1 GB", Hourly: 0.015},
	{ID: "medium", Label: "Medium", Machine: "e2-medium", CPU: "2 shared vCPUs", Memory: "4 GB", Hourly: 0.040, Editor: true},
	{ID: "large", Label: "Large", Machine: "e2-standard-4", CPU: "4 vCPUs", Memory: "16 GB", Hourly: 0.141, Editor: true},
}

func sizeByID(id string) (size, bool) {
	for _, s := range menu {
		if s.ID == id {
			return s, true
		}
	}
	return size{}, false
}

var lifetimes = []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour}

// pickSizes returns the menu entries named in a list like "small,medium".
func pickSizes(list string) ([]size, error) {
	var out []size
	for _, id := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' }) {
		i := slices.IndexFunc(menu, func(s size) bool { return s.ID == id })
		if i < 0 {
			return nil, fmt.Errorf("unknown size %q (choose from small, medium, large)", id)
		}
		out = append(out, menu[i])
	}
	if len(out) == 0 {
		return nil, errors.New("no sizes to offer")
	}
	return out, nil
}

// pickLifetimes returns the lifetimes no longer than max.
func pickLifetimes(max time.Duration) ([]time.Duration, error) {
	var out []time.Duration
	for _, d := range lifetimes {
		if d <= max {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-max-ttl must be at least %s", lifetimes[0])
	}
	return out, nil
}

// cost estimates what running s for d costs, billed per second with a
// one-minute minimum, as Compute Engine does.
func (s size) cost(d time.Duration) float64 {
	return s.Hourly * max(d, time.Minute).Hours()
}
