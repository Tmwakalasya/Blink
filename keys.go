package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// keyStore keeps a fresh SSH keypair for every VM under ~/.blink/<vm>/, plus a
// known_hosts file with the VM's verified host key. The browser terminal uses
// them, and so can plain ssh from your own terminal.
type keyStore struct{ dir string }

var (
	nameRE = regexp.MustCompile(`^blink-[a-z0-9]{1,20}$`)
	zoneRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,40}$`)
)

func validName(s string) bool { return nameRE.MatchString(s) }
func validZone(s string) bool { return zoneRE.MatchString(s) }

func (k keyStore) file(name, file string) string { return filepath.Join(k.dir, name, file) }

// create makes a keypair for the VM and returns its signer and the public key
// in authorized_keys form.
func (k keyStore) create(name string) (ssh.Signer, string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "blink "+name)
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(filepath.Join(k.dir, name), 0o700); err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(k.file(name, "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, "", err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	return signer, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + sshUser, nil
}

func (k keyStore) signer(name string) (ssh.Signer, error) {
	b, err := os.ReadFile(k.file(name, "id_ed25519"))
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(b)
}

func (k keyStore) has(name string) bool    { return exists(k.file(name, "id_ed25519")) }
func (k keyStore) pinned(name string) bool { return exists(k.file(name, "known_hosts")) }

// pin records the VM's verified host key for addr (host:port).
func (k keyStore) pin(name, addr string, key ssh.PublicKey) error {
	line := knownhosts.Line([]string{addr}, key) + "\n"
	return os.WriteFile(k.file(name, "known_hosts"), []byte(line), 0o600)
}

// hostKeyCallback accepts only the host key pinned for this VM.
func (k keyStore) hostKeyCallback(name string) (ssh.HostKeyCallback, error) {
	return knownhosts.New(k.file(name, "known_hosts"))
}

// command is how to reach the VM from your own terminal.
func (k keyStore) command(name, ip string) string {
	return fmt.Sprintf("ssh -i %s -o UserKnownHostsFile=%s %s@%s",
		shellQuote(k.file(name, "id_ed25519")), shellQuote(k.file(name, "known_hosts")), sshUser, ip)
}

func (k keyStore) remove(name string) {
	if validName(name) {
		os.RemoveAll(filepath.Join(k.dir, name))
	}
}

// prune deletes keys for VMs that no longer exist.
func (k keyStore) prune(live []machine) {
	entries, err := os.ReadDir(k.dir)
	if err != nil {
		return
	}
	keep := map[string]bool{}
	for _, m := range live {
		keep[m.Name] = true
	}
	for _, e := range entries {
		if e.IsDir() && !keep[e.Name()] {
			k.remove(e.Name())
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func shellQuote(s string) string {
	safe := strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+=:@", r))
	}) < 0
	if s != "" && safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
