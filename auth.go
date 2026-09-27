package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/idtoken"
)

// Sign-in is on when Blink has a Google OAuth client ID. People sign in with
// Google, and only emails on the list (or admins) get in. Without a
// client ID Blink is a single-user tool on localhost, and that user is the
// admin.

// user is whoever is making a request.
type user struct {
	Email string `json:"email"`
	Admin bool   `json:"admin"`
	owner string // label value that marks this person's VMs
}

var localUser = user{Admin: true, owner: "local"}

// ownerID turns an email into a label value, since labels can't hold '@'.
func ownerID(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(email)))
	return "u" + hex.EncodeToString(sum[:10])
}

const (
	sessionCookie = "blink_session"
	sessionLength = 12 * time.Hour
	rosterLimit   = 64 << 10
)

type auth struct {
	mu       sync.Mutex // guards the files below while they're rewritten
	clientID string
	admins   map[string]bool
	dir      string // holds roster.txt and session.key
	key      []byte // signs session cookies
	// verify checks a Google ID token and returns its verified email.
	verify func(ctx context.Context, credential string) (string, error)
}

func newAuth(clientID, admins, dir string) (*auth, error) {
	a := &auth{clientID: clientID, admins: map[string]bool{}, dir: dir}
	for _, email := range strings.FieldsFunc(admins, func(r rune) bool { return r == ',' || r == ' ' }) {
		a.admins[strings.ToLower(email)] = true
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := os.ReadFile(filepath.Join(dir, "session.key"))
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		rand.Read(key)
		err = os.WriteFile(filepath.Join(dir, "session.key"), key, 0o600)
	}
	if err != nil {
		return nil, err
	}
	a.key = key
	a.verify = func(ctx context.Context, credential string) (string, error) {
		p, err := idtoken.Validate(ctx, credential, clientID)
		if err != nil {
			return "", err
		}
		email, _ := p.Claims["email"].(string)
		if verified, _ := p.Claims["email_verified"].(bool); email == "" || !verified {
			return "", errors.New("Google didn't confirm that email")
		}
		return strings.ToLower(email), nil
	}
	return a, nil
}

func (a *auth) rosterPath() string { return filepath.Join(a.dir, "roster.txt") }

// roster returns the list of people who can sign in, as the admin wrote it.
func (a *auth) roster() string {
	b, _ := os.ReadFile(a.rosterPath())
	return string(b)
}

// allowed reports whether email may sign in: admins always, others if the
// list names them or their domain (a line like @school.edu).
func (a *auth) allowed(email string) bool {
	email = strings.ToLower(email)
	if a.admins[email] {
		return true
	}
	_, domain, _ := strings.Cut(email, "@")
	for _, line := range strings.Split(a.roster(), "\n") {
		entry := strings.ToLower(strings.TrimSpace(line))
		if entry == email || entry == "@"+domain {
			return true
		}
	}
	return false
}

func (a *auth) userFor(email string) user {
	return user{Email: email, Admin: a.admins[email], owner: ownerID(email)}
}

// signIn sets a session cookie for email.
func (a *auth) signIn(w http.ResponseWriter, r *http.Request, email string) {
	payload, _ := json.Marshal(struct {
		Email   string `json:"e"`
		Expires int64  `json:"x"`
	}{email, time.Now().Add(sessionLength).Unix()})
	value := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(a.sign(payload))
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/", MaxAge: int(sessionLength / time.Second),
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
}

func (a *auth) signOut(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
}

// session returns the signed-in user, who must still be allowed in, so
// removing someone from the list signs them out.
func (a *auth) session(r *http.Request) (user, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return user{}, false
	}
	encoded, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return user{}, false
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(encoded)
	mac, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil || !hmac.Equal(mac, a.sign(payload)) {
		return user{}, false
	}
	var s struct {
		Email   string `json:"e"`
		Expires int64  `json:"x"`
	}
	if json.Unmarshal(payload, &s) != nil || time.Now().Unix() > s.Expires || !a.allowed(s.Email) {
		return user{}, false
	}
	return a.userFor(s.Email), true
}

func (a *auth) sign(payload []byte) []byte {
	m := hmac.New(sha256.New, a.key)
	m.Write(payload)
	return m.Sum(nil)
}

// saveRoster replaces the list, keeping one entry per line.
func (a *auth) saveRoster(text string) (int, error) {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		entry := strings.ToLower(strings.TrimSpace(line))
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "@") || strings.ContainsAny(entry, " \t,;<>") {
			return 0, errors.New(entry + " doesn't look like an email or @domain")
		}
		lines = append(lines, entry)
	}
	var buf bytes.Buffer
	for _, line := range lines {
		buf.WriteString(line + "\n")
	}
	return len(lines), os.WriteFile(a.rosterPath(), buf.Bytes(), 0o600)
}

// replaceRoster saves text as the list of people who can sign in.
func (a *auth) replaceRoster(text string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveRoster(text)
}

// accessRequest is someone asking to be let in.
type accessRequest struct {
	Email string    `json:"email"`
	At    time.Time `json:"at"`
}

const maxRequests = 200

func (a *auth) requestsPath() string { return filepath.Join(a.dir, "requests.json") }

func (a *auth) requests() []accessRequest {
	var rs []accessRequest
	if b, err := os.ReadFile(a.requestsPath()); err == nil {
		json.Unmarshal(b, &rs)
	}
	return rs
}

func (a *auth) saveRequests(rs []accessRequest) error {
	b, err := json.Marshal(rs)
	if err != nil {
		return err
	}
	return os.WriteFile(a.requestsPath(), b, 0o600)
}

// ask records that email wants in. Asking twice is harmless.
func (a *auth) ask(email string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	rs := a.requests()
	if slices.ContainsFunc(rs, func(r accessRequest) bool { return r.Email == email }) {
		return nil
	}
	if len(rs) >= maxRequests {
		return errors.New("Too many requests are waiting. Try again later.")
	}
	return a.saveRequests(append(rs, accessRequest{Email: email, At: time.Now()}))
}

// answer clears email's request, adding them to the list if approved.
func (a *auth) answer(email string, approve bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if approve && !a.allowed(email) {
		roster := strings.TrimRight(a.roster(), "\n")
		if roster != "" {
			roster += "\n"
		}
		if _, err := a.saveRoster(roster + email); err != nil {
			return err
		}
	}
	return a.saveRequests(slices.DeleteFunc(a.requests(), func(r accessRequest) bool { return r.Email == email }))
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}
