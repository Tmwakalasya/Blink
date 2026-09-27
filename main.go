// Blink puts a real Google Cloud VM, and a shell into it, one click away.
// Every VM it creates deletes itself when its timer runs out.
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
	"strings"
	"syscall"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"golang.org/x/oauth2/google"
)

type config struct {
	Addr    string
	Project string
	Zone    string
	Machine string
	Image   string
	Network string
	TTL     time.Duration
	Open    bool
}

func main() {
	var cfg config
	flag.StringVar(&cfg.Addr, "addr", "localhost:8080", "address to serve the page on")
	flag.StringVar(&cfg.Project, "project", "", "Google Cloud project ID (default: your gcloud project)")
	flag.StringVar(&cfg.Zone, "zone", "us-central1-a", "zone to create VMs in")
	flag.StringVar(&cfg.Machine, "machine", "e2-micro", "machine type")
	flag.StringVar(&cfg.Image, "image", "projects/debian-cloud/global/images/family/debian-12", "boot disk image")
	flag.StringVar(&cfg.Network, "network", "default", "VPC network for the VMs")
	flag.DurationVar(&cfg.TTL, "ttl", 30*time.Minute, "how long a VM lives before Google deletes it")
	flag.BoolVar(&cfg.Open, "open", true, "open the page in your browser")
	flag.Parse()

	if cfg.TTL < time.Minute || cfg.TTL > 24*time.Hour {
		log.Fatal("blink: -ttl must be between 1m and 24h")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("blink: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg.Project = findProject(ctx, cfg.Project)
	s := newServer(cfg, keyStore{dir: filepath.Join(home, ".blink")})
	st := s.preflight(ctx)
	if st.Ready {
		s.pruneKeys(ctx)
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Fatalf("blink: can't listen on %s: %v (try -addr localhost:8081)", cfg.Addr, err)
	}
	url := "http://" + cfg.Addr
	if strings.HasPrefix(cfg.Addr, ":") {
		url = "http://localhost" + cfg.Addr
	}
	printBanner(url, st)

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
	fmt.Println("\n  Stopped. Running VMs are still deleted when their timer runs out.")
}

// findProject looks where gcloud users would expect: the flag, the usual
// environment variables, gcloud's own config, then whatever project the
// Application Default Credentials carry.
func findProject(ctx context.Context, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	for _, env := range []string{"BLINK_PROJECT", "GOOGLE_CLOUD_PROJECT", "CLOUDSDK_CORE_PROJECT"} {
		if v := os.Getenv(env); v != "" {
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

func printBanner(url string, st status) {
	fmt.Printf("\n  blink  %s\n", url)
	if st.Project != "" {
		fmt.Printf("  %s · %s (%s) · %s · deleted after %s\n",
			st.Project, st.Zone, st.Place, st.Machine, formatTTL(time.Duration(st.TTL)*time.Second))
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
