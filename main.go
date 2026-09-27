// Blink starts temporary Linux VMs on Google Cloud and opens their terminals
// in the browser. Each VM is deleted when its lifetime runs out.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"golang.org/x/oauth2/google"
)

// config is set by flags, which default to BLINK_* environment variables so
// Blink can be configured on Cloud Run.
type config struct {
	Addr        string
	Project     string
	Zone        string
	Image       string
	Network     string
	Open        bool
	State       string        // keys, ledger, class list
	ClientID    string        // Google OAuth client ID; turns on sign-in
	Admins      string        // emails, comma separated
	Budget      float64       // dollars; 0 is no limit
	MaxVMs      int           // across everyone
	WeeklyHours float64       // per person; 0 is no limit
	Sizes       string        // menu entries to offer
	MaxTTL      time.Duration // longest lifetime to offer
}

func main() {
	home, _ := os.UserHomeDir() // may be unset in a container, where -state is set anyway
	port := os.Getenv("PORT")   // set on Cloud Run
	addr := "localhost:8080"
	if port != "" {
		addr = ":" + port
	}

	var cfg config
	flag.StringVar(&cfg.Addr, "addr", addr, "address to serve the page on")
	flag.StringVar(&cfg.Project, "project", "", "Google Cloud project ID (default: your gcloud project)")
	flag.StringVar(&cfg.Zone, "zone", env("BLINK_ZONE", "us-central1-a"), "zone to create VMs in")
	flag.StringVar(&cfg.Image, "image", env("BLINK_IMAGE", defaultImage), "boot disk image (default: the pre-built image if you've made one, else Debian 12)")
	flag.StringVar(&cfg.Network, "network", env("BLINK_NETWORK", "default"), "VPC network for the VMs")
	flag.BoolVar(&cfg.Open, "open", port == "", "open the page in your browser")
	flag.StringVar(&cfg.State, "state", env("BLINK_STATE", filepath.Join(home, ".blink")), "directory for keys, the usage ledger and the class list")
	flag.StringVar(&cfg.ClientID, "client-id", env("BLINK_CLIENT_ID", ""), "Google OAuth client ID; turns on sign-in")
	flag.StringVar(&cfg.Admins, "admins", env("BLINK_ADMINS", ""), "emails that manage the class list, comma separated")
	flag.Float64Var(&cfg.Budget, "budget", envNumber("BLINK_BUDGET", 0), "stop starting VMs once estimated spending reaches this many dollars (0: no limit)")
	flag.IntVar(&cfg.MaxVMs, "max-vms", int(envNumber("BLINK_MAX_VMS", 10)), "most VMs running at once, across everyone")
	flag.Float64Var(&cfg.WeeklyHours, "weekly-hours", envNumber("BLINK_WEEKLY_HOURS", 0), "VM hours each person gets per week (0: no limit)")
	flag.StringVar(&cfg.Sizes, "sizes", env("BLINK_SIZES", "small,medium,large"), "sizes to offer: small, medium, large")
	flag.DurationVar(&cfg.MaxTTL, "max-ttl", envDuration("BLINK_MAX_TTL", 2*time.Hour), "longest lifetime to offer")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg.Project = findProject(ctx, cfg.Project)
	s, err := newServer(cfg)
	if err != nil {
		log.Fatalf("blink: %v", err)
	}
	st := s.preflight(ctx)
	if st.Ready {
		s.pruneKeys(ctx)
		go s.recoverStarts(ctx)
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Fatalf("blink: can't listen on %s: %v (try -addr localhost:8081)", cfg.Addr, err)
	}
	url := "http://" + cfg.Addr
	if strings.HasPrefix(cfg.Addr, ":") {
		url = "http://localhost" + cfg.Addr
	}
	printBanner(url, cfg, st)

	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if cfg.Open {
		openBrowser(url)
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("blink: %v", err)
	}
	fmt.Println("\n  Stopped. Running VMs are still deleted when their lifetime runs out.")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envNumber(key string, def float64) float64 {
	if n, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return n
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return def
}

// findProject looks where gcloud users would expect: the flag, the usual
// environment variables, gcloud's own config, then whatever project the
// Application Default Credentials carry (on Cloud Run, the service's own).
func findProject(ctx context.Context, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	for _, key := range []string{"BLINK_PROJECT", "GOOGLE_CLOUD_PROJECT", "CLOUDSDK_CORE_PROJECT"} {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "gcloud", "config", "get-value", "project").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" && p != "(unset)" {
			return p
		}
	}
	if creds, err := google.FindDefaultCredentials(ctx, compute.DefaultAuthScopes()...); err == nil {
		return creds.ProjectID
	}
	return ""
}

func printBanner(url string, cfg config, st status) {
	fmt.Printf("\n  blink  %s\n", url)
	if st.Project != "" {
		fmt.Printf("  %s · %s (%s) · sizes %s · up to %s\n", st.Project, st.Zone, st.Place, cfg.Sizes, formatTTL(cfg.MaxTTL))
	}
	var limits []string
	if cfg.ClientID != "" {
		limits = append(limits, "sign-in on")
	}
	if cfg.Budget > 0 {
		limits = append(limits, fmt.Sprintf("budget $%.2f", cfg.Budget))
	}
	if cfg.WeeklyHours > 0 {
		limits = append(limits, fmt.Sprintf("%s per person per week", hoursText(cfg.WeeklyHours)))
	}
	if len(limits) > 0 {
		fmt.Printf("  %s\n", strings.Join(limits, " · "))
	}
	switch {
	case st.Problem != "":
		fmt.Printf("\n  ! %s\n", st.Problem)
	case st.Warning != "":
		fmt.Printf("\n  ! %s\n", st.Warning)
	}
	if st.Fix != "" {
		fmt.Printf("    %s\n", st.Fix)
	}
	fmt.Println()
}

func formatTTL(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return fmt.Sprintf("%dm", d/time.Minute)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
