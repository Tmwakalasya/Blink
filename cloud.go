package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2/apierror"
	"golang.org/x/crypto/ssh"
	"google.golang.org/api/googleapi"
	"google.golang.org/protobuf/proto"
)

// Blink labels and tags every VM it creates and refuses to touch a VM without
// the label, so it can never delete a machine it didn't make.
const (
	blinkLabel = "blink"
	sshUser    = "blink"
	diskGB     = 10

	defaultImage = "projects/debian-cloud/global/images/family/debian-12"
	// imageFamily is the pre-built image deploy/build-image.sh makes: Debian 12
	// with VS Code and dev tools installed, so VMs are ready in seconds.
	imageFamily = "blink"
)

var errNotFound = errors.New("not found")

// machine is what Blink knows about one VM.
type machine struct {
	Name      string    `json:"name"`
	Zone      string    `json:"zone"`
	Status    string    `json:"status"`
	IP        string    `json:"ip,omitempty"`
	Size      string    `json:"size,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitzero"`

	// Filled in by server.decorate.
	HasKey bool   `json:"hasKey"`
	Ready  bool   `json:"ready"`           // running, with a verified host key
	SSH    string `json:"ssh,omitempty"`   // how to connect from your own terminal
	Email  string `json:"email,omitempty"` // whose it is, for admins
	Editor bool   `json:"editor"`          // VS Code in the browser is installed

	Owner string `json:"-"` // label value naming who started it
	blink bool
}

func (m machine) alive() bool {
	switch m.Status {
	case "PROVISIONING", "STAGING", "RUNNING":
		return true
	}
	return false
}

// spec is everything needed to create one VM.
type spec struct {
	Name, Zone, Machine, Image, Network string
	Size                                string // menu entry, e.g. small
	Owner                               string // label value from ownerID
	Editor                              bool   // install VS Code in the browser at boot
	TTL                                 time.Duration
	PublicKey                           string // authorized_keys format
}

// cloud is the part of Compute Engine that Blink uses. gce is the real one;
// tests substitute a fake.
type cloud interface {
	// Insert asks for a VM and returns once Google has accepted the request.
	// The returned wait blocks until the VM has been created.
	Insert(ctx context.Context, s spec) (wait func(context.Context) error, err error)
	Get(ctx context.Context, zone, name string) (machine, error)
	List(ctx context.Context) ([]machine, error)
	// HostKeys returns the SSH host keys the VM's guest agent published, or
	// none if it hasn't published them yet.
	HostKeys(ctx context.Context, zone, name string) ([]ssh.PublicKey, error)
	Delete(ctx context.Context, zone, name string) error
}

type gce struct {
	project   string
	images    *compute.ImagesClient
	instances *compute.InstancesClient
	networks  *compute.NetworksClient
	firewalls *compute.FirewallsClient
}

func newGCE(ctx context.Context, project string) (*gce, error) {
	instances, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return nil, err
	}
	networks, err := compute.NewNetworksRESTClient(ctx)
	if err != nil {
		instances.Close()
		return nil, err
	}
	firewalls, err := compute.NewFirewallsRESTClient(ctx)
	if err != nil {
		instances.Close()
		networks.Close()
		return nil, err
	}
	images, err := compute.NewImagesRESTClient(ctx)
	if err != nil {
		instances.Close()
		networks.Close()
		firewalls.Close()
		return nil, err
	}
	return &gce{project: project, images: images, instances: instances, networks: networks, firewalls: firewalls}, nil
}

func (g *gce) Close() {
	g.images.Close()
	g.instances.Close()
	g.networks.Close()
	g.firewalls.Close()
}

// newInstance describes the VM Blink asks for. The scheduling block matters
// most: Google itself deletes the VM when the TTL runs out, even if Blink has
// crashed or your laptop is shut.
func newInstance(s spec) *computepb.Instance {
	inst := &computepb.Instance{
		Name:        proto.String(s.Name),
		MachineType: proto.String(fmt.Sprintf("zones/%s/machineTypes/%s", s.Zone, s.Machine)),
		Labels:      map[string]string{blinkLabel: "true", "blink-owner": s.Owner, "blink-size": s.Size},
		Tags:        &computepb.Tags{Items: []string{blinkLabel}},
		Disks: []*computepb.AttachedDisk{{
			Boot:       proto.Bool(true),
			AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{
				SourceImage: proto.String(s.Image),
				DiskSizeGb:  proto.Int64(diskGB),
				DiskType:    proto.String(fmt.Sprintf("zones/%s/diskTypes/pd-balanced", s.Zone)), // pd-standard is far too slow to boot from
			},
		}},
		NetworkInterfaces: []*computepb.NetworkInterface{{
			Network: proto.String("global/networks/" + s.Network),
			AccessConfigs: []*computepb.AccessConfig{{
				Name: proto.String("External NAT"),
				Type: proto.String(computepb.AccessConfig_ONE_TO_ONE_NAT.String()),
			}},
		}},
		Metadata: &computepb.Metadata{Items: []*computepb.Items{
			{Key: proto.String("ssh-keys"), Value: proto.String(sshUser + ":" + s.PublicKey)},
			// Honor the key above even if the project turns on OS Login, and
			// only that key: project-wide keys don't get onto students' VMs.
			{Key: proto.String("enable-oslogin"), Value: proto.String("FALSE")},
			{Key: proto.String("block-project-ssh-keys"), Value: proto.String("TRUE")},
			// Lets the guest agent publish the VM's host keys so Blink can check them.
			{Key: proto.String("enable-guest-attributes"), Value: proto.String("TRUE")},
		}},
		Scheduling: &computepb.Scheduling{
			MaxRunDuration:            &computepb.Duration{Seconds: proto.Int64(int64(s.TTL / time.Second))},
			InstanceTerminationAction: proto.String(computepb.Scheduling_DELETE.String()),
		},
	}
	if s.Editor {
		inst.Metadata.Items = append(inst.Metadata.Items,
			&computepb.Items{Key: proto.String("blink-editor"), Value: proto.String("TRUE")},
			&computepb.Items{Key: proto.String("startup-script"), Value: proto.String(editorScript)})
	}
	return inst
}

func (g *gce) Insert(ctx context.Context, s spec) (func(context.Context) error, error) {
	op, err := g.instances.Insert(ctx, &computepb.InsertInstanceRequest{
		Project:          g.project,
		Zone:             s.Zone,
		InstanceResource: newInstance(s),
	})
	if err != nil {
		return nil, err
	}
	// Poll twice a second instead of using op.Wait, whose backoff can
	// oversleep by several seconds, which would defeat the point of Blink.
	return func(ctx context.Context) error {
		for !op.Done() {
			if err := sleep(ctx, 500*time.Millisecond); err != nil {
				return err
			}
			if err := op.Poll(ctx); err != nil {
				return err
			}
		}
		if errs := op.Proto().GetError().GetErrors(); len(errs) > 0 {
			return fmt.Errorf("%s: %s", errs[0].GetCode(), errs[0].GetMessage())
		}
		return nil
	}, nil
}

func (g *gce) Get(ctx context.Context, zone, name string) (machine, error) {
	inst, err := g.instances.Get(ctx, &computepb.GetInstanceRequest{Project: g.project, Zone: zone, Instance: name})
	if err != nil {
		if httpCode(err) == 404 {
			return machine{}, errNotFound
		}
		return machine{}, err
	}
	return machineFrom(inst), nil
}

// List returns every Blink VM in the project, in any zone.
func (g *gce) List(ctx context.Context) ([]machine, error) {
	var ms []machine
	it := g.instances.AggregatedList(ctx, &computepb.AggregatedListInstancesRequest{
		Project:              g.project,
		ReturnPartialSuccess: proto.Bool(true),
	})
	for pair, err := range it.All() {
		if err != nil {
			return nil, err
		}
		for _, inst := range pair.Value.GetInstances() {
			if m := machineFrom(inst); m.blink {
				ms = append(ms, m)
			}
		}
	}
	return ms, nil
}

func (g *gce) HostKeys(ctx context.Context, zone, name string) ([]ssh.PublicKey, error) {
	attrs, err := g.instances.GetGuestAttributes(ctx, &computepb.GetGuestAttributesInstanceRequest{
		Project:   g.project,
		Zone:      zone,
		Instance:  name,
		QueryPath: proto.String("hostkeys/"),
	})
	if err != nil {
		if httpCode(err) == 404 {
			return nil, nil // not published yet
		}
		return nil, err
	}
	var keys []ssh.PublicKey
	for _, item := range attrs.GetQueryValue().GetItems() {
		line := item.GetValue()
		if !strings.Contains(line, " ") {
			line = item.GetKey() + " " + line // published as key type + base64 blob
		}
		if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err == nil {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (g *gce) Delete(ctx context.Context, zone, name string) error {
	_, err := g.instances.Delete(ctx, &computepb.DeleteInstanceRequest{Project: g.project, Zone: zone, Instance: name})
	if httpCode(err) == 404 {
		return nil
	}
	return err
}

// prebuilt returns the pre-built image's path if the project has one.
func (g *gce) prebuilt(ctx context.Context) (string, bool) {
	_, err := g.images.GetFromFamily(ctx, &computepb.GetFromFamilyImageRequest{Project: g.project, Family: imageFamily})
	if err != nil {
		return "", false
	}
	return "projects/" + g.project + "/global/images/family/" + imageFamily, true
}

func (g *gce) networkExists(ctx context.Context, network string) error {
	_, err := g.networks.Get(ctx, &computepb.GetNetworkRequest{Project: g.project, Network: network})
	if httpCode(err) == 404 {
		return errNotFound
	}
	return err
}

// sshOpen reports whether any firewall rule on the network lets SSH reach
// Blink's VMs from the internet.
func (g *gce) sshOpen(ctx context.Context, network string) (bool, error) {
	it := g.firewalls.List(ctx, &computepb.ListFirewallsRequest{Project: g.project})
	for fw, err := range it.All() {
		if err != nil {
			return false, err
		}
		if path.Base(fw.GetNetwork()) == network && allowsSSH(fw) {
			return true, nil
		}
	}
	return false, nil
}

func allowsSSH(fw *computepb.Firewall) bool {
	if fw.GetDisabled() || fw.GetDirection() != "INGRESS" ||
		len(fw.GetSourceRanges()) == 0 || len(fw.GetTargetServiceAccounts()) > 0 {
		return false
	}
	if tags := fw.GetTargetTags(); len(tags) > 0 && !slices.Contains(tags, blinkLabel) {
		return false
	}
	for _, a := range fw.GetAllowed() {
		switch a.GetIPProtocol() {
		case "all":
			return true
		case "tcp", "6":
			if len(a.GetPorts()) == 0 {
				return true
			}
			for _, p := range a.GetPorts() {
				if portIn(p, 22) {
					return true
				}
			}
		}
	}
	return false
}

// portIn reports whether port falls in a firewall port spec like "22" or "20-30".
func portIn(spec string, port int) bool {
	lo, hi, isRange := strings.Cut(spec, "-")
	if !isRange {
		hi = lo
	}
	l, err1 := strconv.Atoi(lo)
	h, err2 := strconv.Atoi(hi)
	return err1 == nil && err2 == nil && l <= port && port <= h
}

func machineFrom(inst *computepb.Instance) machine {
	labels := inst.GetLabels()
	m := machine{
		Name:   inst.GetName(),
		Zone:   path.Base(inst.GetZone()),
		Status: inst.GetStatus(),
		Size:   labels["blink-size"],
		Owner:  cmp.Or(labels["blink-owner"], localUser.owner), // VMs from before sign-in
		blink:  labels[blinkLabel] == "true",
	}
	for _, ni := range inst.GetNetworkInterfaces() {
		for _, ac := range ni.GetAccessConfigs() {
			if ip := ac.GetNatIP(); ip != "" && m.IP == "" {
				m.IP = ip
			}
		}
	}
	if t, err := time.Parse(time.RFC3339, inst.GetResourceStatus().GetScheduling().GetTerminationTimestamp()); err == nil {
		m.ExpiresAt = t
	} else if start, err := time.Parse(time.RFC3339, inst.GetLastStartTimestamp()); err == nil {
		if d := inst.GetScheduling().GetMaxRunDuration().GetSeconds(); d > 0 {
			m.ExpiresAt = start.Add(time.Duration(d) * time.Second)
		}
	}
	return m
}

func httpCode(err error) int {
	var ae *apierror.APIError
	if errors.As(err, &ae) {
		return ae.HTTPCode()
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

var places = map[string]string{
	"us-central1": "Iowa", "us-east1": "South Carolina", "us-east4": "Virginia", "us-east5": "Ohio",
	"us-south1": "Dallas", "us-west1": "Oregon", "us-west2": "Los Angeles", "us-west3": "Salt Lake City",
	"us-west4": "Las Vegas", "northamerica-northeast1": "Montréal", "northamerica-northeast2": "Toronto",
	"southamerica-east1": "São Paulo", "europe-west1": "Belgium", "europe-west2": "London",
	"europe-west3": "Frankfurt", "europe-west4": "Netherlands", "europe-north1": "Finland",
	"asia-northeast1": "Tokyo", "asia-southeast1": "Singapore", "asia-south1": "Mumbai",
	"australia-southeast1": "Sydney", "africa-south1": "Johannesburg",
}

var imageNames = map[string]string{
	imageFamily: "Debian 12 + dev tools",
	"debian-11": "Debian 11", "debian-12": "Debian 12", "debian-13": "Debian 13",
	"ubuntu-2204-lts": "Ubuntu 22.04", "ubuntu-2404-lts-amd64": "Ubuntu 24.04",
	"rocky-linux-9": "Rocky Linux 9",
}

// imageName turns an image path like .../images/family/debian-12 into Debian 12.
func imageName(image string) string {
	base := path.Base(image)
	if name, ok := imageNames[base]; ok {
		return name
	}
	return base
}

// place turns a zone like us-central1-a into a place name like Iowa.
func place(zone string) string {
	region := zone
	if i := strings.LastIndex(zone, "-"); i > 0 {
		region = zone[:i]
	}
	if p, ok := places[region]; ok {
		return p
	}
	return region
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
