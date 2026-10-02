package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"ytguard/internal/filterlist"
	"ytguard/internal/rules"
	"ytguard/internal/store"
)

const testList = `! Title: Scary stuff
! Ages: 0-9
! Description: test list
[hide deny]
keyword: creepypasta
channel: UCaaaaaaaaaaaaaaaaaaaaaa Scary channel
[block allow]
keyword: lego
[hide deny]
widget: nonsense
`

func listServer(t *testing.T, body *atomic.Value, fail *atomic.Bool) *httptest.Server {
	filterlist.AllowHTTP = true
	t.Cleanup(func() { filterlist.AllowHTTP = false })
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog.json":
			fmt.Fprintf(w, `{"lists":[{"title":"Scary stuff","ages":"0-9","description":"d","url":"%s/scary.txt"},{"title":"Other","url":"%s/other.txt"}]}`, srv.URL, srv.URL)
		case "/scary.txt":
			if fail.Load() {
				http.Error(w, "down", 500)
				return
			}
			b := body.Load().(string)
			etag := fmt.Sprintf(`"%d"`, len(b))
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			fmt.Fprint(w, b)
		case "/page.html":
			fmt.Fprint(w, "<html>nope</html>")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFilterLists(t *testing.T) {
	e := setup(t)
	csrf := e.login()
	var body atomic.Value
	body.Store(testList)
	var fail atomic.Bool
	srv := listServer(t, &body, &fail)
	other := store.Kid{LinuxUser: "other", Name: "Other"}
	e.app.St.SaveKid(&other)
	s, _ := e.app.St.Settings()
	s.ListCatalogURL = srv.URL + "/catalog.json"
	e.app.St.SaveSettings(s)

	// Not a list: refused, nothing kept.
	_, loc := e.post("/filters/lists/subscribe", url.Values{"csrf": {csrf}, "url": {srv.URL + "/page.html"}, "kid": {"all"}})
	if !strings.Contains(loc, "err=") {
		t.Fatalf("html page accepted: %s", loc)
	}
	// Subscribe for Kiddo only.
	_, loc = e.post("/filters/lists/subscribe", url.Values{"csrf": {csrf}, "url": {srv.URL + "/scary.txt"}, "kid": {fmt.Sprint(e.kid.ID)}})
	if strings.Contains(loc, "err=") {
		t.Fatalf("subscribe: %s", loc)
	}
	ls, _ := e.app.St.Lists()
	if len(ls) != 1 || ls[0].RuleCount != 3 || ls[0].AllowCount != 1 || len(ls[0].Warnings) != 1 || ls[0].Ages != "0-9" {
		t.Fatalf("list: %+v", ls)
	}
	l := ls[0]

	decide := func(k store.Kid, m rules.Meta) string {
		d, _, err := e.app.Decide(context.Background(), k, m, false)
		if err != nil {
			t.Fatal(err)
		}
		return d.Outcome
	}
	creepy := rules.Meta{VideoID: "v1", Title: "Creepypasta story", Full: true}
	if got := decide(e.kid, creepy); got != rules.Hide {
		t.Fatalf("subscribed kid: %s", got)
	}
	if got := decide(other, creepy); got != rules.Play {
		t.Fatalf("unsubscribed kid: %s", got)
	}
	// Parent's own entry of the same type wins.
	own := rules.Rule{Tier: rules.TierHide, List: rules.ListAllow, Type: rules.TypeKeyword, Value: "creepypasta", Match: rules.MatchWord, Fields: []string{"title"}}
	e.app.St.AddRule(&own)
	if got := decide(e.kid, creepy); got != rules.Play {
		t.Fatalf("parent override: %s", got)
	}
	// List rules don't show up among (or get deleted with) the parent's rules.
	mine, _ := e.app.St.Rules(store.RuleFilter{KidID: -1})
	if len(mine) != 1 {
		t.Fatalf("parent rules: %+v", mine)
	}
	listRules, _ := e.app.St.Rules(store.RuleFilter{KidID: -1, ListID: l.ID})
	e.post(fmt.Sprintf("/filters/%d/delete", listRules[0].ID), url.Values{"csrf": {csrf}})
	if n, _ := e.app.St.Rules(store.RuleFilter{KidID: -1, ListID: l.ID}); len(n) != 3 {
		t.Fatal("list rule deleted through the parent rule UI")
	}

	// Unchanged list: 304, rules kept. Failing server: previous copy kept.
	if err := e.app.RefreshList(context.Background(), l.ID, false); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if err := e.app.RefreshList(context.Background(), l.ID, false); err == nil {
		t.Fatal("want error")
	}
	l, _ = e.app.St.List(l.ID)
	if l.Error == "" || l.RuleCount != 3 {
		t.Fatalf("after failure: %+v", l)
	}
	if got := decide(other, rules.Meta{VideoID: "v2", Title: "x", ChannelID: "UCaaaaaaaaaaaaaaaaaaaaaa", Full: true}); got != rules.Play {
		t.Fatal("other kid affected")
	}
	// Changed list replaces the rules.
	fail.Store(false)
	body.Store("! Title: Scary stuff v2\n[hide deny]\nkeyword: jumpscare\n")
	if err := e.app.RefreshList(context.Background(), l.ID, false); err != nil {
		t.Fatal(err)
	}
	l, _ = e.app.St.List(l.ID)
	if l.RuleCount != 1 || l.Title != "Scary stuff v2" || l.Error != "" {
		t.Fatalf("after update: %+v", l)
	}

	// Pages render.
	for _, p := range []string{"/filters/lists", fmt.Sprintf("/filters/lists/%d", l.ID), "/filters", "/tester", "/filters/lists/export.txt"} {
		code, html := e.get(p)
		if code != 200 || strings.Contains(html, "<no value>") {
			t.Errorf("%s: %d", p, code)
		}
		if p == "/filters/lists" && (!strings.Contains(html, "Other") || !strings.Contains(html, "subscribed")) {
			t.Errorf("catalog not shown")
		}
		if p == "/filters/lists/export.txt" && !strings.Contains(html, "keyword: creepypasta") {
			t.Errorf("export: %s", html)
		}
	}

	// Switching the list off stops it applying; deleting a kid drops it from lists.
	e.post(fmt.Sprintf("/filters/lists/%d/options", l.ID), url.Values{"csrf": {csrf}, "kid": {fmt.Sprint(e.kid.ID)}})
	if got := decide(e.kid, rules.Meta{VideoID: "v3", Title: "Jumpscare", Full: true}); got != rules.Play {
		t.Fatalf("disabled list still applies: %s", got)
	}
	// Unsubscribe removes its rules.
	e.post(fmt.Sprintf("/filters/lists/%d/delete", l.ID), url.Values{"csrf": {csrf}})
	var n int
	e.app.St.DB.QueryRow(`SELECT COUNT(*) FROM rules WHERE list_id>0`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d list rules left", n)
	}
}

func TestCheckURL(t *testing.T) {
	for _, bad := range []string{"http://example.com/l.txt", "ftp://x/y", "https://user:pw@example.com/l", "nonsense"} {
		if filterlist.CheckURL(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if filterlist.CheckURL("https://raw.githubusercontent.com/o/ytguard-lists/main/x.txt") != nil {
		t.Error("https rejected")
	}
}
