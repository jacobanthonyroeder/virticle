package api

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"virticle/internal/webaccess"
)

func proxyMode(trusted string) webaccess.Settings {
	return webaccess.Settings{Mode: "proxy", Listen: "127.0.0.1:8080", PublicURL: "https://vm.example.org", TrustedProxies: []string{trusted}}.Normalize()
}

// In proxy mode the configured list decides who may set X-Forwarded-*.
func TestWebProxyModeTrust(t *testing.T) {
	e := newEnv(t)
	e.srv.UseWebSettings(proxyMode("192.0.2.0/24"))
	e.user("alice", "admin")

	// The test peer 192.0.2.7 is trusted: its headers count.
	w := e.do("POST", "/api/login", credentials{"alice", testPW},
		header("X-Forwarded-For", "9.9.9.9, 203.0.113.5"), header("X-Forwarded-Proto", "https"), header("Origin", "https://vm.example.org"))
	if w.Code != 200 {
		t.Fatalf("login via proxy: %d %s", w.Code, w.Body)
	}
	if got := lastAudit(t, e).IP; got != "203.0.113.5" {
		t.Errorf("audit IP %s", got)
	}
	if c := w.Result().Cookies(); len(c) == 0 || !c[0].Secure {
		t.Error("cookie must be Secure behind an https proxy")
	}

	// The same headers from an address outside the list are ignored.
	e.srv.UseWebSettings(proxyMode("10.99.0.0/16"))
	w = e.do("POST", "/api/login", credentials{"alice", testPW}, header("X-Forwarded-For", "9.9.9.9"), header("X-Forwarded-Proto", "https"))
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	if got := lastAudit(t, e).IP; got != "192.0.2.7" {
		t.Errorf("spoofed X-Forwarded-For accepted: audit IP %s", got)
	}
	if c := w.Result().Cookies(); len(c) == 0 || c[0].Secure {
		t.Error("spoofed X-Forwarded-Proto made the cookie Secure")
	}

	// Leaving proxy mode stops trusting the list.
	e.srv.UseWebSettings(webaccess.Settings{Mode: "off"}.Normalize())
	w = e.do("POST", "/api/login", credentials{"alice", testPW}, header("X-Forwarded-For", "9.9.9.9"))
	if got := lastAudit(t, e).IP; got != "192.0.2.7" {
		t.Errorf("after leaving proxy mode: %s", got)
	}
}

// The flag-configured list still works when the stored settings are not in proxy mode.
func TestWebFlagProxiesSurviveOtherModes(t *testing.T) {
	e := newEnv(t)
	e.srv.TrustedProxies = mustProxies(t, "192.0.2.0/24")
	e.srv.UseWebSettings(webaccess.Settings{Mode: "own"}.Normalize())
	e.user("alice", "admin")
	e.do("POST", "/api/login", credentials{"alice", testPW}, header("X-Forwarded-For", "203.0.113.5"))
	if got := lastAudit(t, e).IP; got != "203.0.113.5" {
		t.Errorf("flag proxies lost: %s", got)
	}
}

// The configured public URL is an accepted Origin even when the proxy rewrites Host.
func TestWebPublicURLOrigin(t *testing.T) {
	e := newEnv(t)
	e.user("alice", "admin")
	post := func() int {
		return e.do("POST", "/api/logout", nil, cookieOf(e, "alice"), header("Origin", "https://vm.example.org")).Code
	}
	if c := post(); c != 403 {
		t.Fatalf("unknown origin accepted: %d", c)
	}
	e.srv.UseWebSettings(webaccess.Settings{Mode: "own", PublicURL: "https://vm.example.org"}.Normalize())
	if c := post(); c == 403 {
		t.Error("the public URL origin was refused")
	}
	if c := e.do("POST", "/api/logout", nil, cookieOf(e, "alice"), header("Origin", "https://evil.example")).Code; c != 403 {
		t.Errorf("foreign origin accepted: %d", c)
	}
}

func TestWebHSTSToggle(t *testing.T) {
	e := newEnv(t)
	hsts := func() string {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "198.51.100.1:1"
		r.TLS = &tls.ConnectionState{}
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		return w.Header().Get("Strict-Transport-Security")
	}
	if hsts() != "" {
		t.Error("HSTS must be off by default")
	}
	e.srv.UseWebSettings(webaccess.Settings{Mode: "own", HSTS: true}.Normalize())
	if hsts() == "" {
		t.Error("HSTS missing when enabled")
	}
}

func TestWebEndpoints(t *testing.T) {
	e := newEnv(t)
	e.user("root", "admin")
	e.user("op", "operator")
	if w := e.do("GET", "/api/web", nil, cookieOf(e, "root")); w.Code != 503 {
		t.Errorf("without a manager: %d", w.Code)
	}
	m := &webaccess.Manager{Store: e.srv.Store, DataDir: t.TempDir(), Handler: e.h, Window: 300 * time.Millisecond, OnChange: e.srv.UseWebSettings}
	e.srv.Web = m
	if err := m.Start(webaccess.Settings{Mode: "off", Listen: "127.0.0.1:0"}.Normalize()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path string }{{"GET", "/api/web"}, {"POST", "/api/web/check"}, {"PUT", "/api/web"}, {"POST", "/api/web/confirm"}, {"POST", "/api/web/revert"}, {"POST", "/api/web/renew"}, {"POST", "/api/web/snippets"}} {
		if w := e.do(c.method, c.path, map[string]any{}, cookieOf(e, "op")); w.Code != http.StatusForbidden {
			t.Errorf("%s %s as operator: %d", c.method, c.path, w.Code)
		}
	}
	var st struct {
		Status   webaccess.Status    `json:"status"`
		Snippets []webaccess.Snippet `json:"snippets"`
	}
	w := e.do("GET", "/api/web", nil, cookieOf(e, "root"))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	decode(t, w, &st)
	if st.Status.Settings.Mode != "off" || len(st.Snippets) != 5 {
		t.Errorf("%+v", st)
	}

	// A refused proposal comes back with the check list.
	w = e.do("PUT", "/api/web", map[string]any{"settings": map[string]any{"mode": "acme", "httpsListen": "127.0.0.1:0"}}, cookieOf(e, "root"))
	var bad struct {
		Error  string            `json:"error"`
		Checks []webaccess.Check `json:"checks"`
	}
	decode(t, w, &bad)
	if w.Code != 400 || len(bad.Checks) == 0 {
		t.Errorf("%d %s", w.Code, w.Body)
	}

	// Nothing pending: confirm fails.
	if w := e.do("POST", "/api/web/confirm", map[string]any{}, cookieOf(e, "root")); w.Code != 400 {
		t.Errorf("confirm without change: %d", w.Code)
	}
	// Snippets for a draft.
	w = e.do("POST", "/api/web/snippets", map[string]any{"settings": map[string]any{"publicUrl": "https://draft.example"}}, cookieOf(e, "root"))
	var sn struct{ Snippets []webaccess.Snippet }
	decode(t, w, &sn)
	if len(sn.Snippets) != 5 || !strings.Contains(sn.Snippets[0].Text, "draft.example") {
		t.Errorf("snippets: %s", w.Body)
	}
}

// The sign-in page may learn that a web access change waits and until when, and nothing else about it.
func TestStatusShowsPendingWebChange(t *testing.T) {
	e := newEnv(t)
	e.user("root", "admin")
	const window = 1500 * time.Millisecond
	m := &webaccess.Manager{Store: e.srv.Store, DataDir: t.TempDir(), Handler: e.h, Window: window, OnChange: e.srv.UseWebSettings}
	e.srv.Web = m
	if err := m.Start(webaccess.Settings{Mode: "off", Listen: "127.0.0.1:0"}.Normalize()); err != nil {
		t.Fatal(err)
	}
	status := func() (map[string]any, string) {
		w := e.do("GET", "/api/status", nil) // signed out
		var st map[string]any
		decode(t, w, &st)
		return st, w.Body.String()
	}
	if st, _ := status(); st["webChangeUntil"] != nil {
		t.Fatalf("nothing pending, yet: %v", st)
	}
	before := time.Now()
	w := e.do("PUT", "/api/web", map[string]any{"settings": proxyMode("127.0.0.1")}, cookieOf(e, "root"))
	if w.Code != 200 {
		t.Fatalf("apply: %d %s", w.Code, w.Body)
	}
	after := time.Now()
	st, body := status()
	until, _ := st["webChangeUntil"].(string)
	d, err := time.Parse(time.RFC3339, until)
	// The deadline is the time of the change plus the window, in whole seconds (cut, not rounded). Checked against
	// the clock around the change, not against "now": on a loaded machine the requests themselves take a while.
	if err != nil || d.Before(before.Add(window).Truncate(time.Second)) || d.After(after.Add(window)) {
		t.Fatalf("deadline %q (change made between %s and %s): %v", until, before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano), err)
	}
	for _, leak := range []string{"vm.example.org", "127.0.0.1"} {
		if strings.Contains(body, leak) {
			t.Errorf("status leaks %q: %s", leak, body)
		}
	}
	// The window ends and the change is undone; the undo runs in the background, so it is waited for (no fixed
	// sleep: under load it may come a little later than the window).
	time.Sleep(window)
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, _ := status()
		if st["webChangeUntil"] == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still shown long after the window: %v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
