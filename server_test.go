package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func filepathGlob(dir string) ([]string, error) { return filepath.Glob(filepath.Join(dir, "*")) }

func TestOnlyLocalPagesCanDriveBlink(t *testing.T) {
	s := testServer(t, newFakeCloud("127.0.0.1"), "22")
	h := s.routes()

	tests := []struct {
		name   string
		method string
		url    string
		header map[string]string
		want   int
	}{
		{"the page itself", "GET", "http://localhost:8080/api/status", nil, http.StatusOK},
		{"127.0.0.1 works too", "GET", "http://127.0.0.1:8080/api/status", nil, http.StatusOK},
		{"DNS rebinding", "GET", "http://evil.example:8080/api/status", nil, http.StatusForbidden},
		{"cross-site summon", "POST", "http://localhost:8080/api/vms", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"cross-origin delete", "DELETE", "http://localhost:8080/api/vms/us-central1-a/blink-abc123", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.url, nil)
			for k, v := range tt.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestDeleteRefusesVMsBlinkDidNotMake(t *testing.T) {
	fc := newFakeCloud("127.0.0.1")
	fc.vms["blink-theirs"] = machine{Name: "blink-theirs", Zone: "us-central1-a", Status: "RUNNING"}
	s := testServer(t, fc, "22")

	req := httptest.NewRequest("DELETE", "http://localhost:8080/api/vms/us-central1-a/blink-theirs", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if d := fc.deletedVMs(); len(d) != 0 {
		t.Errorf("deleted %v", d)
	}
}

func TestNewInstanceSelfDestructs(t *testing.T) {
	inst := newInstance(spec{
		Name:      "blink-abc123",
		Zone:      "us-central1-a",
		Machine:   "e2-micro",
		Image:     "projects/debian-cloud/global/images/family/debian-12",
		Network:   "default",
		TTL:       30 * time.Minute,
		PublicKey: "ssh-ed25519 AAAAC3Nz blink",
	})

	if got := inst.GetScheduling().GetMaxRunDuration().GetSeconds(); got != 1800 {
		t.Errorf("maxRunDuration = %ds, want 1800", got)
	}
	if got := inst.GetScheduling().GetInstanceTerminationAction(); got != "DELETE" {
		t.Errorf("instanceTerminationAction = %q, want DELETE", got)
	}
	if inst.GetLabels()[blinkLabel] != "true" || !slices.Contains(inst.GetTags().GetItems(), blinkLabel) {
		t.Error("VM must carry the blink label and network tag")
	}
	meta := map[string]string{}
	for _, item := range inst.GetMetadata().GetItems() {
		meta[item.GetKey()] = item.GetValue()
	}
	if meta["ssh-keys"] != "blink:ssh-ed25519 AAAAC3Nz blink" {
		t.Errorf("ssh-keys = %q", meta["ssh-keys"])
	}
	if meta["enable-oslogin"] != "FALSE" || meta["enable-guest-attributes"] != "TRUE" {
		t.Errorf("metadata = %v", meta)
	}
	if !inst.GetDisks()[0].GetAutoDelete() {
		t.Error("boot disk must be deleted with the VM")
	}

	// The REST API sees this JSON, so check the field names it expects.
	b, err := protojson.Marshal(inst)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"maxRunDuration":{"seconds":"1800"}`, `"instanceTerminationAction":"DELETE"`, `"type":"ONE_TO_ONE_NAT"`} {
		if !strings.Contains(strings.ReplaceAll(string(b), " ", ""), want) {
			t.Errorf("request JSON is missing %s:\n%s", want, b)
		}
	}
}

func TestAllowsSSH(t *testing.T) {
	rule := func(edit func(*computepb.Firewall)) *computepb.Firewall {
		fw := &computepb.Firewall{
			Direction:    proto.String("INGRESS"),
			SourceRanges: []string{"0.0.0.0/0"},
			Allowed:      []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: []string{"22"}}},
		}
		if edit != nil {
			edit(fw)
		}
		return fw
	}
	tests := []struct {
		name string
		fw   *computepb.Firewall
		want bool
	}{
		{"default-allow-ssh", rule(nil), true},
		{"port range", rule(func(f *computepb.Firewall) { f.Allowed[0].Ports = []string{"20-30"} }), true},
		{"all tcp", rule(func(f *computepb.Firewall) { f.Allowed[0].Ports = nil }), true},
		{"all protocols", rule(func(f *computepb.Firewall) { f.Allowed[0].IPProtocol = proto.String("all") }), true},
		{"tagged for blink", rule(func(f *computepb.Firewall) { f.TargetTags = []string{"blink"} }), true},
		{"tagged for others", rule(func(f *computepb.Firewall) { f.TargetTags = []string{"http-server"} }), false},
		{"web only", rule(func(f *computepb.Firewall) { f.Allowed[0].Ports = []string{"80", "443"} }), false},
		{"disabled", rule(func(f *computepb.Firewall) { f.Disabled = proto.Bool(true) }), false},
		{"egress", rule(func(f *computepb.Firewall) { f.Direction = proto.String("EGRESS") }), false},
		{"service accounts only", rule(func(f *computepb.Firewall) { f.TargetServiceAccounts = []string{"sa@x"} }), false},
	}
	for _, tt := range tests {
		if got := allowsSSH(tt.fw); got != tt.want {
			t.Errorf("%s: allowsSSH = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestPruneForgetsDeletedVMs(t *testing.T) {
	k := keyStore{dir: t.TempDir()}
	for _, name := range []string{"blink-alive1", "blink-gone22"} {
		if _, _, err := k.create(name); err != nil {
			t.Fatal(err)
		}
	}
	stray := filepath.Join(k.dir, "not-blinks")
	os.Mkdir(stray, 0o700)

	k.prune([]machine{{Name: "blink-alive1"}})

	if !k.has("blink-alive1") || k.has("blink-gone22") {
		t.Errorf("after prune: alive=%v gone=%v", k.has("blink-alive1"), k.has("blink-gone22"))
	}
	if _, err := os.Stat(stray); err != nil {
		t.Error("prune removed a directory Blink didn't create")
	}
}

func TestPlace(t *testing.T) {
	for zone, want := range map[string]string{"us-central1-a": "Iowa", "europe-west2-b": "London", "mars-north1-a": "mars-north1"} {
		if got := place(zone); got != want {
			t.Errorf("place(%q) = %q, want %q", zone, got, want)
		}
	}
}
