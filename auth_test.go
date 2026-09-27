package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSignInNeedsTheClassList(t *testing.T) {
	s := classServer(t, testConfig(t), newFakeCloud("127.0.0.1"), "22", studentEmail)
	h := s.routes()

	if rec := request(h, "POST", "/api/login", `{"credential":"stranger@elsewhere.com"}`, nil); rec.Code != http.StatusForbidden {
		t.Errorf("someone off the list signed in: %d", rec.Code)
	}
	if rec := request(h, "POST", "/api/login", `{"credential":"forged"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("a bad Google token was accepted: %d", rec.Code)
	}

	var anon statusView
	json.Unmarshal(request(h, "GET", "/api/status", "", nil).Body.Bytes(), &anon)
	if anon.User != nil || anon.SignIn == nil || anon.Project != "" {
		t.Errorf("a visitor should see only the sign-in button, got %+v", anon)
	}
	if rec := request(h, "GET", "/api/vms", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("a visitor listed VMs: %d", rec.Code)
	}

	cookie := signIn(t, h, "Ana@School.edu") // emails match whatever their case
	var st statusView
	json.Unmarshal(request(h, "GET", "/api/status", "", cookie).Body.Bytes(), &st)
	if st.User == nil || st.User.Email != studentEmail || st.User.Admin {
		t.Errorf("signed-in status user = %+v", st.User)
	}
}

func TestSessionCookieIsLockedDown(t *testing.T) {
	s := classServer(t, testConfig(t), newFakeCloud("127.0.0.1"), "22", studentEmail)
	req := httptest.NewRequest("POST", "https://blink.example/api/login", strings.NewReader(`{"credential":"`+studentEmail+`"}`))
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	var c *http.Cookie
	for _, k := range rec.Result().Cookies() {
		if k.Name == sessionCookie {
			c = k
		}
	}
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie = %+v, want HttpOnly, Secure, SameSite=Lax", c)
	}
}

func TestTamperedOrRevokedSessionsAreRejected(t *testing.T) {
	s := classServer(t, testConfig(t), newFakeCloud("127.0.0.1"), "22", studentEmail)
	h := s.routes()
	cookie := signIn(t, h, studentEmail)

	forged := *cookie
	payload, sig, _ := strings.Cut(forged.Value, ".")
	forged.Value = payload + "x." + sig
	if rec := request(h, "GET", "/api/vms", "", &forged); rec.Code != http.StatusUnauthorized {
		t.Errorf("a tampered cookie worked: %d", rec.Code)
	}

	admin := signIn(t, h, adminEmail)
	if rec := request(h, "PUT", "/api/class", `{"roster":"someone@else.edu"}`, admin); rec.Code != http.StatusOK {
		t.Fatalf("saving the class list: %d %s", rec.Code, rec.Body)
	}
	if rec := request(h, "GET", "/api/vms", "", cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("a student taken off the list kept access: %d", rec.Code)
	}
}

func TestOnlyAdminsManageTheClass(t *testing.T) {
	s := classServer(t, testConfig(t), newFakeCloud("127.0.0.1"), "22", studentEmail)
	h := s.routes()
	student, admin := signIn(t, h, studentEmail), signIn(t, h, adminEmail)

	if rec := request(h, "GET", "/api/class", "", student); rec.Code != http.StatusForbidden {
		t.Errorf("a student read the class list: %d", rec.Code)
	}
	if rec := request(h, "PUT", "/api/class", `{"roster":"me@school.edu"}`, student); rec.Code != http.StatusForbidden {
		t.Errorf("a student edited the class list: %d", rec.Code)
	}
	if rec := request(h, "PUT", "/api/class", `{"roster":"new@school.edu\n@partner.edu\n\n"}`, admin); rec.Code != http.StatusOK {
		t.Fatalf("admin save: %d %s", rec.Code, rec.Body)
	}
	if rec := request(h, "PUT", "/api/class", `{"roster":"not an email"}`, admin); rec.Code != http.StatusBadRequest {
		t.Errorf("a junk class list was saved: %d", rec.Code)
	}
	if !s.auth.allowed("new@school.edu") || !s.auth.allowed("anyone@partner.edu") || s.auth.allowed(studentEmail) {
		t.Error("the saved class list doesn't match what the admin wrote")
	}
}

func TestStudentsOnlySeeAndDeleteTheirOwnVMs(t *testing.T) {
	fc := newFakeCloud("127.0.0.1")
	fc.put(machine{Name: "blink-ana001", Zone: "us-central1-a", Status: "RUNNING", Owner: ownerID(studentEmail), blink: true})
	fc.put(machine{Name: "blink-ben001", Zone: "us-central1-a", Status: "RUNNING", Owner: ownerID(otherEmail), blink: true})
	s := classServer(t, testConfig(t), fc, "22", studentEmail, otherEmail)
	h := s.routes()
	ana, admin := signIn(t, h, studentEmail), signIn(t, h, adminEmail)

	names := func(cookie *http.Cookie) []string {
		var body struct{ VMs []machine }
		json.Unmarshal(request(h, "GET", "/api/vms", "", cookie).Body.Bytes(), &body)
		var out []string
		for _, m := range body.VMs {
			out = append(out, m.Name)
		}
		return out
	}
	if got := names(ana); len(got) != 1 || got[0] != "blink-ana001" {
		t.Errorf("Ana sees %v, want only her own VM", got)
	}
	if got := names(admin); len(got) != 2 {
		t.Errorf("the admin sees %v, want both VMs", got)
	}

	if rec := request(h, "DELETE", "/api/vms/us-central1-a/blink-ben001", "", ana); rec.Code != http.StatusForbidden {
		t.Errorf("Ana deleted Ben's VM: %d", rec.Code)
	}
	if rec := request(h, "DELETE", "/api/vms/us-central1-a/blink-ana001", "", ana); rec.Code != http.StatusNoContent {
		t.Errorf("Ana couldn't delete her own VM: %d %s", rec.Code, rec.Body)
	}
	if d := fc.deletedVMs(); len(d) != 1 || d[0] != "blink-ana001" {
		t.Errorf("deleted %v", d)
	}
}

func TestLimitsStopNewVMs(t *testing.T) {
	ctx, now := context.Background(), time.Now()
	small := menu[0]
	student := user{Email: studentEmail, owner: ownerID(studentEmail)}
	admin := user{Email: adminEmail, Admin: true, owner: ownerID(adminEmail)}

	t.Run("one VM each", func(t *testing.T) {
		fc := newFakeCloud("127.0.0.1")
		fc.put(machine{Name: "blink-ana001", Zone: "us-central1-a", Status: "RUNNING", Owner: student.owner, blink: true})
		s := testServer(t, fc, "22")
		s.ledger.add(usage{VM: "blink-ana001", Zone: "us-central1-a", Owner: student.owner, Hourly: small.Hourly, Start: now, TTL: 1800})
		_, no := s.admitStart(ctx, fc, student, small, 30*time.Minute)
		if no == nil || no.code != http.StatusConflict || no.VM == nil {
			t.Fatalf("second VM for one student: %+v", no)
		}
	})

	t.Run("VMs at once", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.MaxVMs = 1
		s := serverFor(t, cfg, newFakeCloud("127.0.0.1"), "22")
		s.ledger.add(usage{VM: "blink-ben001", Owner: ownerID(otherEmail), Hourly: small.Hourly, Start: now, TTL: 1800})
		_, no := s.admitStart(ctx, s.currentCloud(), student, small, 30*time.Minute)
		if no == nil || !strings.Contains(no.Error, "All 1 VMs are in use") {
			t.Fatalf("over the VM cap: %+v", no)
		}
	})

	t.Run("budget", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.Budget = 1
		s := serverFor(t, cfg, newFakeCloud("127.0.0.1"), "22")
		s.ledger.add(usage{VM: "blink-big001", Owner: ownerID(otherEmail), Hourly: 0.14, Start: now.Add(-3 * time.Hour), TTL: 2 * 3600})
		s.ledger.end("blink-big001", now.Add(-time.Hour)) // spent $0.28
		if _, no := s.admitStart(ctx, s.currentCloud(), student, small, 30*time.Minute); no != nil {
			t.Fatalf("a cheap VM under budget was refused: %s", no.Error)
		}
		s.admit.Lock()
		delete(s.starting, student.owner)
		s.admit.Unlock()
		_, no := s.admitStart(ctx, s.currentCloud(), admin, menu[2], 6*time.Hour) // $0.84 more
		if no == nil || !strings.Contains(no.Error, "budget") {
			t.Fatalf("over budget: %+v", no)
		}
	})

	t.Run("weekly hours", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.WeeklyHours = 1
		s := serverFor(t, cfg, newFakeCloud("127.0.0.1"), "22")
		s.ledger.add(usage{VM: "blink-ana000", Owner: student.owner, Hourly: small.Hourly, Start: now.Add(-2 * time.Hour), TTL: 3600})
		_, no := s.admitStart(ctx, s.currentCloud(), student, small, 30*time.Minute)
		if no == nil || !strings.Contains(no.Error, "your 1 h a week") {
			t.Fatalf("over weekly hours: %+v", no)
		}
		if _, no := s.admitStart(ctx, s.currentCloud(), admin, small, 30*time.Minute); no != nil {
			t.Errorf("admins shouldn't have an hour limit: %s", no.Error)
		}
	})
}

func TestRequestingAccess(t *testing.T) {
	s := classServer(t, testConfig(t), newFakeCloud("127.0.0.1"), "22", studentEmail)
	h := s.routes()
	stranger := "newcomer@school.edu"

	rec := request(h, "POST", "/api/login", `{"credential":"`+stranger+`"}`, nil)
	var refused apiError
	json.Unmarshal(rec.Body.Bytes(), &refused)
	if rec.Code != http.StatusForbidden || !refused.CanRequest {
		t.Fatalf("login off the list: %d %+v, want 403 offering a request", rec.Code, refused)
	}
	if rec := request(h, "POST", "/api/access", `{"credential":"`+stranger+`"}`, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("asking for access: %d %s", rec.Code, rec.Body)
	}
	request(h, "POST", "/api/access", `{"credential":"`+stranger+`"}`, nil) // asking twice is fine
	request(h, "POST", "/api/access", `{"credential":"pest@else.com"}`, nil)

	admin := signIn(t, h, adminEmail)
	var class struct{ Requests []accessRequest }
	json.Unmarshal(request(h, "GET", "/api/class", "", admin).Body.Bytes(), &class)
	if len(class.Requests) != 2 {
		t.Fatalf("requests = %+v, want the newcomer and the pest once each", class.Requests)
	}
	if rec := request(h, "POST", "/api/class/answer", `{"email":"`+stranger+`","approve":true}`, signIn(t, h, studentEmail)); rec.Code != http.StatusForbidden {
		t.Errorf("a student answered a request: %d", rec.Code)
	}
	request(h, "POST", "/api/class/answer", `{"email":"`+stranger+`","approve":true}`, admin)
	request(h, "POST", "/api/class/answer", `{"email":"pest@else.com","approve":false}`, admin)

	signIn(t, h, stranger) // approved, so this works
	if rec := request(h, "POST", "/api/login", `{"credential":"pest@else.com"}`, nil); rec.Code != http.StatusForbidden {
		t.Errorf("a dismissed request got in: %d", rec.Code)
	}
	json.Unmarshal(request(h, "GET", "/api/class", "", admin).Body.Bytes(), &class)
	if len(class.Requests) != 0 {
		t.Errorf("answered requests are still waiting: %+v", class.Requests)
	}
	if !strings.Contains(s.auth.roster(), studentEmail) {
		t.Error("approving someone dropped an existing entry from the list")
	}
}
