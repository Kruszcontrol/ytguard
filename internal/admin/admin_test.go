package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"ytguard/internal/auth"
	"ytguard/internal/core"
	"ytguard/internal/rules"
	"ytguard/internal/store"
)

type env struct {
	t   *testing.T
	srv *httptest.Server
	c   *http.Client
	app *core.App
	au  *auth.Auth
	kid store.Kid
}

func setup(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := core.New(st)
	au := auth.New(st)
	if err := au.SetPassword("parent", "longenough1"); err != nil {
		t.Fatal(err)
	}
	k := store.Kid{LinuxUser: "kiddo", Name: "Kiddo"}
	if err := st.SaveKid(&k); err != nil {
		t.Fatal(err)
	}
	ui, err := New(app, au)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(ui.Handler())
	t.Cleanup(srv.Close)
	c := srv.Client()
	c.Jar, _ = cookiejar.New(nil)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &env{t: t, srv: srv, c: c, app: app, au: au, kid: k}
}

func (e *env) get(path string) (int, string) {
	e.t.Helper()
	resp, err := e.c.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *env) post(path string, form url.Values) (int, string) {
	e.t.Helper()
	resp, err := e.c.PostForm(e.srv.URL+path, form)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b) + resp.Header.Get("Location")
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (e *env) login() string {
	e.t.Helper()
	code, _ := e.post("/login", url.Values{"username": {"parent"}, "password": {"longenough1"}, "remember": {"on"}, "device": {"test"}})
	if code != http.StatusSeeOther {
		e.t.Fatalf("login: %d", code)
	}
	_, body := e.get("/")
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		e.t.Fatal("no csrf token on dashboard")
	}
	return m[1]
}

func TestLoginRequired(t *testing.T) {
	e := setup(t)
	if code, loc := e.post("/login", url.Values{"username": {"parent"}, "password": {"wrong"}}); code != http.StatusUnauthorized {
		t.Fatalf("bad login: %d %s", code, loc)
	}
	resp, _ := e.c.Get(e.srv.URL + "/settings")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("want redirect to login, got %d", resp.StatusCode)
	}
}

func TestPagesRender(t *testing.T) {
	e := setup(t)
	e.login()
	// Some data so lists aren't empty.
	r := rules.Rule{Tier: rules.TierHide, List: rules.ListDeny, Type: rules.TypeKeyword, Value: "creepy", Match: rules.MatchWord, Fields: []string{"title"}}
	e.app.St.AddRule(&r)
	r2 := rules.Rule{Tier: rules.TierBlock, List: rules.ListAllow, Type: rules.TypeChannel, Value: "UCxxxxxxxxxxxxxxxxxxxxxx", Label: "Chan", KidID: e.kid.ID}
	e.app.St.AddRule(&r2)
	e.app.St.AddWatch(e.kid.ID, "2026-10-02", rules.Meta{VideoID: "abcdefghijk", Title: "T"}, 60, 1)
	req := store.Request{KidID: e.kid.ID, VideoID: "abcdefghijk", Title: "Req"}
	e.app.St.CreateRequest(&req)

	for _, p := range []string{"/", "/kids", "/kids/1", "/filters", "/filters?tier=block&scope=all", "/filters/overview",
		"/tester", "/history", "/history?kid=1&day=2026-10-02", "/settings", "/security", "/login", "/report?kid=1&day=2026-10-02"} {
		code, body := e.get(p)
		if code != 200 {
			t.Errorf("%s: %d %s", p, code, body)
			continue
		}
		if strings.Contains(body, "<no value>") || strings.Contains(body, "ZgotmplZ") {
			t.Errorf("%s: template problem in output", p)
		}
	}
}

func TestCSRFAndActions(t *testing.T) {
	e := setup(t)
	csrf := e.login()
	if code, _ := e.post("/filters/add", url.Values{"tier": {"hide"}, "list": {"deny"}, "type": {"keyword"}, "value": {"zombie"}}); code != http.StatusForbidden {
		t.Fatalf("missing csrf accepted: %d", code)
	}
	code, loc := e.post("/filters/add", url.Values{"csrf": {csrf}, "tier": {"hide"}, "list": {"deny"}, "type": {"keyword"}, "value": {"zombie"}, "kid": {"all"}})
	if code != http.StatusSeeOther || strings.Contains(loc, "err=") {
		t.Fatalf("add rule: %d %s", code, loc)
	}
	rs, _ := e.app.St.Rules(store.RuleFilter{KidID: -1, Search: "zombie"})
	if len(rs) != 1 || rs[0].Tier != "hide" || rs[0].Match != "word" {
		t.Fatalf("rules = %+v", rs)
	}
	// Move it to Block allow.
	code, _ = e.post("/filters/"+itoa(rs[0].ID)+"/move", url.Values{"csrf": {csrf}, "to": {"block:allow"}})
	if r, _ := e.app.St.Rule(rs[0].ID); code != http.StatusSeeOther || r.Tier != "block" || r.List != "allow" {
		t.Fatalf("move: %d %+v", code, r)
	}
	// Dashboard bonus.
	if code, _ := e.post("/kids/1/bonus", url.Values{"csrf": {csrf}, "minutes": {"30"}}); code != http.StatusSeeOther {
		t.Fatalf("bonus: %d", code)
	}
	if st, _ := e.app.Status(e.kid.ID); st.LimitSec != (60+30)*60 {
		t.Fatalf("limit = %d", st.LimitSec)
	}
	// Bad regex is rejected.
	_, loc = e.post("/filters/add", url.Values{"csrf": {csrf}, "tier": {"hide"}, "list": {"deny"}, "type": {"keyword"}, "value": {"("}, "match": {"regex"}})
	if !strings.Contains(loc, "err=") {
		t.Fatalf("bad regex accepted: %s", loc)
	}
}

func TestAPITokens(t *testing.T) {
	e := setup(t)
	tok, err := e.au.NewToken("HA", []string{auth.ScopeRead})
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, token string) int {
		req, _ := http.NewRequest(method, e.srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := e.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := do("GET", "/api/v1/state", tok); c != 200 {
		t.Fatalf("state: %d", c)
	}
	if c := do("GET", "/api/v1/state", ""); c != 401 {
		t.Fatalf("no token: %d", c)
	}
	if c := do("POST", "/api/v1/kids/Kiddo/bonus", tok); c != 403 {
		t.Fatalf("read token did control: %d", c)
	}
	if c := do("GET", "/api/v1/rules", tok); c != 403 {
		t.Fatalf("read token did admin: %d", c)
	}
	ctl, _ := e.au.NewToken("HA2", []string{auth.ScopeControl})
	if c := do("POST", "/api/v1/kids/Kiddo/bonus", ctl); c != 200 {
		t.Fatalf("control bonus: %d", c)
	}
	// A token isn't a UI session.
	req, _ := http.NewRequest("GET", e.srv.URL+"/settings", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
	resp, _ := e.c.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("token used as cookie: %d", resp.StatusCode)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := setup(t)
	resp, err := e.c.Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers: %v", resp.Header)
	}
	s, _ := e.app.St.Settings()
	s.EmbedOrigins = "https://ha.local:8123"
	e.app.St.SaveSettings(s)
	resp, _ = e.c.Get(e.srv.URL + "/login")
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'self' https://ha.local:8123") {
		t.Fatalf("embed csp: %s", resp.Header.Get("Content-Security-Policy"))
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{"/filters": "/filters", "//evil.com": "/", "https://evil.com": "/", "": "/", "/\\evil": "/"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q", in, got)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestGuessDevice(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Mobile Safari/537.36":                       "Chrome on Android",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1": "Safari on iPhone",
		"Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0":                                                                  "Firefox on Linux",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Safari/537.36 Edg/129.0":                   "Edge on Windows",
		"Mozilla/5.0 (Linux; Android 14) Home Assistant/2024.10":                                                                                  "Home Assistant app on Android",
	} {
		if got := guessDevice(ua); got != want {
			t.Errorf("%q -> %q, want %q", ua[:40], got, want)
		}
	}
	if lanHostname(context.Background(), "127.0.0.1") != "" {
		t.Error("loopback named")
	}
}
