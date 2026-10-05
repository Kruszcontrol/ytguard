package console

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ytguard/internal/admin"
	"ytguard/internal/auth"
	"ytguard/internal/core"
	"ytguard/internal/filterlist"
	"ytguard/internal/pairing"
	"ytguard/internal/rules"
	"ytguard/internal/store"
	"ytguard/internal/timekeeper"
)

// kidPC is a real YTGuard admin server with one kid.
type kidPC struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store
	app *core.App
	kid store.Kid
	c   *http.Client
}

func newKidPC(t *testing.T, name, kidName string) *kidPC {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s, _ := st.Settings()
	s.PCName = name
	if err := st.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	app := core.New(st)
	au := auth.New(st)
	if err := au.SetPassword("parent", "pw1234"); err != nil {
		t.Fatal(err)
	}
	k := store.Kid{LinuxUser: strings.ToLower(kidName), Name: kidName}
	if err := st.SaveKid(&k); err != nil {
		t.Fatal(err)
	}
	ui, err := admin.New(app, au)
	if err != nil {
		t.Fatal(err)
	}
	ui.PCID = "id-" + name
	srv := httptest.NewTLSServer(ui.Handler())
	t.Cleanup(srv.Close)
	ui.CertFingerprint = pairing.Fingerprint(srv.Certificate().Raw)
	c := srv.Client()
	c.Jar, _ = cookiejar.New(nil)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &kidPC{t: t, srv: srv, st: st, app: app, kid: k, c: c}
}

func (k *kidPC) addr() string { return strings.TrimPrefix(k.srv.URL, "https://") }

var (
	csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	codeRe = regexp.MustCompile(`class="pair-code mono"[^>]*>(\d{3} \d{3})<`)
)

// pairingCode logs in to the PC's web UI and starts pairing, like a parent.
func (k *kidPC) pairingCode() string {
	k.t.Helper()
	resp, err := k.c.PostForm(k.srv.URL+"/login", url.Values{"username": {"parent"}, "password": {"pw1234"}})
	if err != nil {
		k.t.Fatal(err)
	}
	resp.Body.Close()
	body := get(k.t, k.c, k.srv.URL+"/security")
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		k.t.Fatal("no csrf on security page")
	}
	resp, err = k.c.PostForm(k.srv.URL+"/security/pair", url.Values{"csrf": {m[1]}})
	if err != nil {
		k.t.Fatal(err)
	}
	resp.Body.Close()
	body = get(k.t, k.c, k.srv.URL+"/security/pair")
	cm := codeRe.FindStringSubmatch(body)
	if cm == nil {
		k.t.Fatalf("no code on pairing page:\n%s", body)
	}
	if !strings.Contains(body, pairing.Short(pairing.Fingerprint(k.srv.Certificate().Raw))) {
		k.t.Fatal("pairing page doesn't show the certificate")
	}
	return cm[1]
}

func get(t *testing.T, c *http.Client, u string) string {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// con is the console under test, logged in.
type con struct {
	t    *testing.T
	s    *Server
	srv  *httptest.Server
	c    *http.Client
	csrf string
}

func newConsole(t *testing.T) *con {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s, err := New(st, auth.New(st))
	if err != nil {
		t.Fatal(err)
	}
	s.DataDir = t.TempDir()
	t.Cleanup(s.Close)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	c := srv.Client()
	c.Jar, _ = cookiejar.New(nil)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cn := &con{t: t, s: s, srv: srv, c: c}

	// First run: everything leads to setup.
	resp, _ := c.Get(srv.URL + "/")
	if loc := resp.Header.Get("Location"); loc != "/setup" {
		t.Fatalf("first run should go to setup, got %q", loc)
	}
	code, _ := cn.post("/setup", url.Values{"name": {"Parents' laptop"}, "username": {"mum"}, "password": {"abcd"}, "confirm": {"abcd"}})
	if code != http.StatusSeeOther {
		t.Fatalf("setup: %d", code)
	}
	m := csrfRe.FindStringSubmatch(cn.get("/pcs"))
	if m == nil {
		t.Fatal("not logged in after setup")
	}
	cn.csrf = m[1]
	return cn
}

func (c *con) get(path string) string { return get(c.t, c.c, c.srv.URL+path) }

func (c *con) post(path string, form url.Values) (int, string) {
	c.t.Helper()
	if c.csrf != "" {
		form.Set("csrf", c.csrf)
	}
	resp, err := c.c.PostForm(c.srv.URL+path, form)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	loc, _ := url.QueryUnescape(resp.Header.Get("Location"))
	return resp.StatusCode, string(b) + loc
}

func (c *con) pair(k *kidPC) {
	c.t.Helper()
	code := k.pairingCode()
	_, body := c.post("/pcs/probe", url.Values{"addr": {k.addr()}})
	fp := pairing.Fingerprint(k.srv.Certificate().Raw)
	if !strings.Contains(body, `value="`+fp+`"`) || !strings.Contains(body, pairing.Short(fp)) {
		c.t.Fatalf("probe page doesn't show the PC's certificate:\n%s", body)
	}
	st, body := c.post("/pcs/pair", url.Values{"addr": {k.addr()}, "fingerprint": {fp}, "code": {code}})
	if st != http.StatusSeeOther || !strings.Contains(body, "Connected to") {
		c.t.Fatalf("pair: %d %s", st, body)
	}
}

func TestConsole(t *testing.T) {
	pc1 := newKidPC(t, "Den PC", "Alice")
	pc2 := newKidPC(t, "Bedroom PC", "Bob")
	c := newConsole(t)

	// A wrong code is refused and the PC isn't added.
	pc1.pairingCode()
	fp := pairing.Fingerprint(pc1.srv.Certificate().Raw)
	st, body := c.post("/pcs/pair", url.Values{"addr": {pc1.addr()}, "fingerprint": {fp}, "code": {"000000"}})
	if st != http.StatusBadRequest || !strings.Contains(body, "wrong pairing code") {
		t.Fatalf("wrong code: %d %s", st, body)
	}
	// A pinned certificate that isn't the PC's is refused too.
	code := pc1.pairingCode()
	other := pairing.Fingerprint([]byte("some other certificate")) // test servers all share one certificate
	if _, body := c.post("/pcs/pair", url.Values{"addr": {pc1.addr()}, "fingerprint": {other}, "code": {code}}); !strings.Contains(body, "certificate changed") {
		t.Fatalf("wrong pin: %s", body)
	}
	if len(c.s.pcs()) != 0 {
		t.Fatal("PC added despite failures")
	}

	c.pair(pc1)
	c.pair(pc2)
	pcs := c.s.pcs()
	if len(pcs) != 2 || pcs[0].Name != "Bedroom PC" || pcs[1].PCID != "id-Den PC" {
		t.Fatalf("pcs: %+v", pcs)
	}
	toks, _ := pc1.st.Tokens()
	if len(toks) != 1 || toks[0].Name != "Console: Parents' laptop" || len(toks[0].Scopes) != 3 {
		t.Fatalf("token on PC: %+v", toks)
	}
	// Pairing again replaces the entry instead of adding a second one.
	c.pair(pc1)
	if len(c.s.pcs()) != 2 {
		t.Fatal("re-pairing duplicated the PC")
	}
	if toks, _ := pc1.st.Tokens(); len(toks) != 1 {
		t.Fatalf("re-pairing left the old token: %+v", toks)
	}
	idOf := func(k *kidPC) string {
		for _, p := range c.s.pcs() {
			if p.Addr == k.addr() {
				return p.ID
			}
		}
		t.Fatal("no PC")
		return ""
	}

	// Dashboard: both kids and a request from Alice.
	rq := store.Request{KidID: pc1.kid.ID, VideoID: "dQw4w9WgXcQ", Title: "Lego castle build", ChannelID: "UCaaaaaaaaaaaaaaaaaaaaaa", ChannelName: "Bricks", Reason: "Block default"}
	if _, err := pc1.st.CreateRequest(&rq); err != nil {
		t.Fatal(err)
	}
	dash := c.get("/")
	for _, want := range []string{"Alice", "Bob", "Den PC", "Bedroom PC", "Lego castle build", fmt.Sprintf("/pcs/%s/requests/%d/approve", idOf(pc1), rq.ID)} {
		if !strings.Contains(dash, want) {
			t.Fatalf("dashboard lacks %q", want)
		}
	}
	if _, body := c.post(fmt.Sprintf("/pcs/%s/requests/%d/approve", idOf(pc1), rq.ID), url.Values{"scope": {"channel"}}); !strings.Contains(body, "Allowed channel Bricks for Alice") {
		t.Fatalf("approve: %s", body)
	}
	if got, _ := pc1.st.Request(rq.ID); got.Status != store.RequestApproved {
		t.Fatalf("request status %q", got.Status)
	}

	// Bonus time for Bob.
	if _, body := c.post(fmt.Sprintf("/pcs/%s/kids/%d/bonus", idOf(pc2), pc2.kid.ID), url.Values{"minutes": {"30"}}); !strings.Contains(body, "Gave Bob (Bedroom PC) +30 minutes") {
		t.Fatalf("bonus: %s", body)
	}
	if n, _ := pc2.st.BonusMinutes(pc2.kid.ID, timekeeper.Day(time.Now())); n != 30 {
		t.Fatalf("bonus minutes %d", n)
	}

	// History.
	if h := c.get(fmt.Sprintf("/history?who=%s:%d", idOf(pc2), pc2.kid.ID)); !strings.Contains(h, "Bob on Bedroom PC") {
		t.Fatalf("history:\n%s", h)
	}

	// Filters: Alice's channel approval exists only on the Den PC.
	f := c.get("/filters")
	if !strings.Contains(f, "Bricks") || !strings.Contains(f, "no kid") {
		t.Fatalf("filters page:\n%s", f)
	}
	// An all-kids rule added to both PCs.
	if _, body := c.post("/filters/add", url.Values{"tier": {"hide"}, "list": {"deny"}, "type": {"keyword"}, "value": {"creepypasta"}, "kid": {"all"}, "pcs": {"all"}}); !strings.Contains(body, "Added on") {
		t.Fatalf("add: %s", body)
	}
	for _, k := range []*kidPC{pc1, pc2} {
		rs, _ := k.st.Rules(store.RuleFilter{Type: rules.TypeKeyword, KidID: -1})
		if len(rs) != 1 || rs[0].Value != "creepypasta" || rs[0].Match != rules.MatchWord {
			t.Fatalf("keyword rule: %+v", rs)
		}
	}
	// A rule only on one PC can be copied to the other.
	ru := rules.Rule{Tier: rules.TierBlock, List: rules.ListDeny, Type: rules.TypeCategory, Value: "Gaming"}
	if _, err := pc2.st.AddRule(&ru); err != nil {
		t.Fatal(err)
	}
	key := ruleKey(ru, "")
	if !strings.Contains(c.get("/filters?diff=1"), "Gaming") {
		t.Fatal("differences filter misses the Gaming rule")
	}
	if _, body := c.post("/filters/sync", url.Values{"key": {key}}); !strings.Contains(body, "Copied on Den PC") {
		t.Fatalf("sync: %s", body)
	}
	if rs, _ := pc1.st.Rules(store.RuleFilter{Type: rules.TypeCategory, KidID: -1}); len(rs) != 1 {
		t.Fatalf("copied rule: %+v", rs)
	}
	if _, body := c.post("/filters/remove", url.Values{"key": {key}}); !strings.Contains(body, "Removed on") {
		t.Fatalf("remove: %s", body)
	}
	for _, k := range []*kidPC{pc1, pc2} {
		if rs, _ := k.st.Rules(store.RuleFilter{Type: rules.TypeCategory, KidID: -1}); len(rs) != 0 {
			t.Fatalf("rule not removed: %+v", rs)
		}
	}
	// Kid-scoped rules go only where that kid exists.
	if _, body := c.post("/filters/add", url.Values{"tier": {"block"}, "list": {"deny"}, "type": {"attribute"}, "value": {"longer_than"}, "minutes": {"20"}, "kid": {"Bob"}, "pcs": {"all"}}); !strings.Contains(body, "Skipped Den PC (no kid named Bob)") {
		t.Fatalf("kid-scoped add: %s", body)
	}
	if rs, _ := pc2.st.Rules(store.RuleFilter{Type: rules.TypeAttribute, KidID: pc2.kid.ID}); len(rs) != 1 || rs[0].Value != "longer_than:20" {
		t.Fatalf("kid rule: %+v", rs)
	}

	// Shared lists.
	testLists(t, c, pc1, pc2)

	// A changed certificate is noticed and the PC isn't trusted.
	p := pcs[0]
	for _, x := range c.s.pcs() {
		if x.Addr == pc1.addr() {
			p = x
		}
	}
	good := p.Fingerprint
	p.Fingerprint = other
	if err := c.s.savePC(p); err != nil {
		t.Fatal(err)
	}
	if page := c.get("/pcs"); !strings.Contains(page, "certificate changed") || !strings.Contains(page, "trust the new certificate") {
		t.Fatalf("pcs page should show the changed certificate:\n%s", page)
	}
	if _, body := c.post("/pcs/"+p.ID+"/trust", url.Values{"fingerprint": {good}}); !strings.Contains(body, "Trusting") {
		t.Fatalf("trust: %s", body)
	}
	if !strings.Contains(c.get("/"), "Alice") {
		t.Fatal("PC not back after trusting")
	}

	// Removing a PC revokes the console's token on it.
	if _, body := c.post("/pcs/"+p.ID+"/remove", url.Values{}); !strings.Contains(body, "revoked the console's token") {
		t.Fatalf("remove: %s", body)
	}
	if toks, _ := pc1.st.Tokens(); len(toks) != 0 {
		t.Fatalf("token left on PC: %+v", toks)
	}
	if len(c.s.pcs()) != 1 {
		t.Fatal("PC not removed")
	}
}

func testLists(t *testing.T, c *con, pc1, pc2 *kidPC) {
	filterlist.AllowHTTP = true
	t.Cleanup(func() { filterlist.AllowHTTP = false })
	var hits atomic.Int32
	ls := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "! Title: Scary stuff\n[hide deny]\nkeyword: creepy\n")
	}))
	t.Cleanup(ls.Close)
	u := ls.URL + "/scary.txt"
	if _, body := c.post("/lists/apply", url.Values{"url": {u}, "mode": {"block"}, "kid": {"all"}}); !strings.Contains(body, "Set to block for all kids on") {
		t.Fatalf("subscribe: %s", body)
	}
	for _, k := range []*kidPC{pc1, pc2} {
		l, err := k.st.ListByURL(u)
		if err != nil || l.Mode != store.ListModeBlock || l.Title != "Scary stuff" || len(l.Kids) != 0 || !l.Enabled {
			t.Fatalf("list on %s: %+v %v", k.srv.URL, l, err)
		}
	}
	if page := c.get("/lists"); !strings.Contains(page, "Scary stuff") || !strings.Contains(page, "all kids") {
		t.Fatalf("lists page:\n%s", page)
	}
	// Just Alice, hidden: Bob's PC stops using it.
	_, body := c.post("/lists/apply", url.Values{"url": {u}, "mode": {"hide"}, "kid": {"Alice"}})
	if !strings.Contains(body, "Set to hide for Alice on Den PC") || !strings.Contains(body, "Turned off on Bedroom PC (no kid named Alice)") {
		t.Fatalf("alice only: %s", body)
	}
	if l, _ := pc1.st.ListByURL(u); l.Mode != store.ListModeHide || len(l.Kids) != 1 || l.Kids[0] != pc1.kid.ID || !l.Enabled {
		t.Fatalf("Den PC list: %+v", l)
	}
	if l, _ := pc2.st.ListByURL(u); l.Enabled {
		t.Fatalf("Bedroom PC list should be off: %+v", l)
	}
	if page := c.get("/lists"); !strings.Contains(page, "<b>Hide</b> · Alice") {
		t.Fatalf("lists page should name the kid:\n%s", page)
	}
	// Both kids by name: back on for Bob.
	if _, body := c.post("/lists/apply", url.Values{"url": {u}, "mode": {"mixed"}, "kid": {"Alice", "Bob"}}); !strings.Contains(body, "Set to mixed for Alice, Bob on") {
		t.Fatalf("both: %s", body)
	}
	if l, _ := pc2.st.ListByURL(u); !l.Enabled || len(l.Kids) != 1 || l.Kids[0] != pc2.kid.ID || l.Mode != store.ListModeMixed {
		t.Fatalf("Bedroom PC list: %+v", l)
	}
	// A kid-only subscription on a PC without that kid isn't created.
	u2 := ls.URL + "/other.txt"
	if _, body := c.post("/lists/apply", url.Values{"url": {u2}, "kid": {"Bob"}}); !strings.Contains(body, "Skipped Den PC (no kid named Bob)") {
		t.Fatalf("bob only: %s", body)
	}
	if _, err := pc1.st.ListByURL(u2); err == nil {
		t.Fatal("list subscribed on a PC without Bob")
	}
	if _, body := c.post("/lists/remove", url.Values{"url": {u}}); !strings.Contains(body, "Unsubscribed on") {
		t.Fatalf("remove: %s", body)
	}
	if _, err := pc1.st.ListByURL(u); err == nil {
		t.Fatal("list not removed")
	}
}

func TestPhoneAccess(t *testing.T) {
	c := newConsole(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if _, body := c.post("/settings/phone", url.Values{"enabled": {"on"}, "port": {fmt.Sprint(port)}}); !strings.Contains(body, "Phone access is on") {
		t.Fatalf("enable: %s", body)
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := hc.Get(fmt.Sprintf("https://127.0.0.1:%d/login", port))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.TLS == nil {
		t.Fatalf("phone listener: %d", resp.StatusCode)
	}
	if page := c.get("/settings"); !strings.Contains(page, "<b>On.</b>") || !strings.Contains(page, pairing.Short(c.s.rfp)) {
		t.Fatalf("settings should show phone access on:\n%s", page)
	}
	if _, body := c.post("/settings/phone", url.Values{"port": {fmt.Sprint(port)}}); !strings.Contains(body, "Phone access is off") {
		t.Fatalf("disable: %s", body)
	}
	if _, err := hc.Get(fmt.Sprintf("https://127.0.0.1:%d/login", port)); err == nil {
		t.Fatal("phone listener still running")
	}
	if _, body := c.post("/settings/phone", url.Values{"enabled": {"on"}, "port": {"80"}}); !strings.Contains(body, "between 1024 and 65535") {
		t.Fatalf("bad port: %s", body)
	}
}

func TestHomeNetwork(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "192.168.1.5": true, "10.0.0.8": true, "172.20.1.1": true,
		"100.101.102.103": true, "fd7a:115c:a1e0::1": true, "fe80::1": true,
		"8.8.8.8": false, "100.128.0.1": false, "2001:4860:4860::8888": false,
	} {
		if got := homeNetwork(net.ParseIP(ip)); got != want {
			t.Errorf("%s: got %v", ip, got)
		}
	}
	c := newConsole(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/login", nil)
	r.RemoteAddr = "203.0.113.9:4000"
	c.s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("internet address: %d", w.Code)
	}
}

func TestConsoleGuards(t *testing.T) {
	c := newConsole(t)
	// DNS rebinding: another host name is refused.
	req, _ := http.NewRequest("GET", c.srv.URL+"/login", nil)
	req.Host = "evil.example"
	resp, err := c.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("foreign Host: %d", resp.StatusCode)
	}
	// Forms need the CSRF token.
	c2 := *c
	c2.csrf = ""
	if st, _ := c2.post("/pcs/probe", url.Values{"addr": {"127.0.0.1:1"}}); st != http.StatusForbidden {
		t.Fatalf("no csrf: %d", st)
	}

	// The first login can only be created at this computer.
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	s, _ := New(st, auth.New(st))
	defer s.Close()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/setup", strings.NewReader("username=x&password=abcd&confirm=abcd"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "192.168.1.50:5000"
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || s.hasLogin() {
		t.Fatalf("remote setup: %d", w.Code)
	}
}

func TestNormalizeAddr(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.20":                 "192.168.1.20:8443",
		" https://kids-pc.local:9000/": "kids-pc.local:9000",
		"kids-pc":                      "kids-pc:8443",
		"[fe80::1]":                    "[fe80::1]:8443",
	} {
		if got, err := NormalizeAddr(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "http://192.168.1.20", "user@host"} {
		if _, err := NormalizeAddr(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
