package extapi

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/user"
	"testing"
	"time"

	"ytguard/internal/core"
	"ytguard/internal/rules"
	"ytguard/internal/store"
	"ytguard/internal/timekeeper"
)

type env struct {
	t   *testing.T
	url string
	app *core.App
	kid store.Kid
	now time.Time
}

func setup(t *testing.T) *env {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	// The test process is "the kid": it owns the client socket.
	k := store.Kid{LinuxUser: me.Username, Name: "Kid", BlockDefault: rules.ListDeny}
	if err := st.SaveKid(&k); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSchedule(k.ID, int(time.Now().Weekday()), timekeeper.Schedule{DailyMinutes: 30}); err != nil {
		t.Fatal(err)
	}
	app := core.New(st)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	s := &Server{App: app, Key: key, Addr: "127.0.0.1:7878"}
	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, url: srv.URL, app: app, kid: k}
}

func (e *env) call(path string, body any, out any) int {
	e.t.Helper()
	var req *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		req, _ = http.NewRequest("POST", e.url+path, bytes.NewReader(b))
	} else {
		req, _ = http.NewRequest("GET", e.url+path, nil)
	}
	req.Header.Set("X-YTGuard", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == 200 {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

var meta = rules.Meta{VideoID: "abcdefghijk", Title: "Minecraft build", Description: "d", ChannelID: "UCxxxxxxxxxxxxxxxxxxxxxx",
	ChannelName: "Builder", Category: "Gaming", LengthSeconds: 300, Full: true}

func TestFlow(t *testing.T) {
	e := setup(t)

	var cfg map[string]any
	if c := e.call("/api/config", nil, &cfg); c != 200 || cfg["managed"] != true || cfg["kid"] != "Kid" {
		t.Fatalf("config: %d %v", c, cfg)
	}

	// Block default deny: blocked, title shown, kid can ask.
	var chk checkResp
	e.call("/api/check", meta, &chk)
	if chk.Outcome != rules.Block || chk.Title != meta.Title || chk.Reason == "" {
		t.Fatalf("check: %+v", chk)
	}
	var rq map[string]string
	if c := e.call("/api/request", map[string]string{"videoId": meta.VideoID, "message": "please"}, &rq); c != 200 || rq["status"] != "pending" {
		t.Fatalf("request: %d %v", c, rq)
	}
	pend, _ := e.app.St.Requests(store.RequestPending, 10)
	if len(pend) != 1 || pend[0].Message != "please" {
		t.Fatalf("pending: %+v", pend)
	}
	if _, err := e.app.Approve(pend[0].ID, core.ApproveVideo); err != nil {
		t.Fatal(err)
	}
	e.call("/api/request?videoId="+meta.VideoID, nil, &rq)
	if rq["status"] != "approved" {
		t.Fatalf("status after approve: %v", rq)
	}
	e.call("/api/check", rules.Meta{VideoID: meta.VideoID}, &chk)
	if chk.Outcome != rules.Play {
		t.Fatalf("after approve: %+v", chk)
	}

	// Feed tiles.
	var filt struct{ Results map[string]string }
	e.call("/api/filter", map[string]any{"items": []rules.Meta{{VideoID: meta.VideoID}, {VideoID: "zzzzzzzzzzz", Title: "other"}}}, &filt)
	if filt.Results[meta.VideoID] != "show" || filt.Results["zzzzzzzzzzz"] != "locked" {
		t.Fatalf("filter: %v", filt.Results)
	}

	// Heartbeats record watch time.
	var hb struct {
		Status  timekeeper.Status
		Outcome string
	}
	e.call("/api/heartbeat", map[string]any{"videoId": meta.VideoID, "playing": true, "start": true}, &hb)
	if !hb.Status.Allowed || hb.Outcome != rules.Play {
		t.Fatalf("heartbeat: %+v", hb)
	}
	ws, _ := e.app.St.Watches(e.kid.ID, timekeeper.Day(time.Now()))
	if len(ws) != 1 || ws[0].Title != meta.Title {
		t.Fatalf("watch log: %+v", ws)
	}

	// Hide tier: no metadata leaks to the kid, and the parent sees the attempt.
	r := rules.Rule{Tier: rules.TierHide, List: rules.ListDeny, Type: rules.TypeKeyword, Value: "minecraft", Match: rules.MatchWord, Fields: []string{"title"}}
	e.app.St.AddRule(&r)
	chk = checkResp{}
	e.call("/api/check", meta, &chk)
	if chk.Outcome != rules.Hide || chk.Title != "" || chk.Reason != "" || chk.Channel != "" {
		t.Fatalf("hidden check leaked: %+v", chk)
	}
	if c := e.call("/api/request", map[string]string{"videoId": meta.VideoID}, nil); c != 400 {
		t.Fatalf("request for hidden video: %d", c)
	}
	evs, _ := e.app.St.Events(e.kid.ID, timekeeper.Day(time.Now()))
	kinds := map[string]bool{}
	for _, ev := range evs {
		kinds[ev.Kind] = true
	}
	if !kinds[store.EventHidden] || !kinds[store.EventBlocked] {
		t.Fatalf("events: %+v", evs)
	}
	// Heartbeat for a now-hidden video doesn't count and tells the page.
	e.call("/api/heartbeat", map[string]any{"videoId": meta.VideoID, "playing": true}, &hb)
	if hb.Outcome != rules.Hide {
		t.Fatalf("heartbeat outcome: %+v", hb)
	}
}

func TestOriginAndHeader(t *testing.T) {
	e := setup(t)
	req, _ := http.NewRequest("GET", e.url+"/api/config", nil)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("missing header: %d", resp.StatusCode)
	}
	req.Header.Set("X-YTGuard", "1")
	req.Header.Set("Origin", "https://evil.example")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("foreign origin: %d", resp.StatusCode)
	}
	resp, _ = http.Get(e.url + "/ext/updates.xml")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("updates.xml: %d", resp.StatusCode)
	}
}

func TestUnmappedUser(t *testing.T) {
	e := setup(t)
	if err := e.app.St.DeleteKid(e.kid.ID); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	e.call("/api/config", nil, &cfg)
	if cfg["managed"] != false {
		t.Fatalf("unmapped allow: %v", cfg)
	}
	s, _ := e.app.St.Settings()
	s.UnmappedPolicy = "block"
	e.app.St.SaveSettings(s)
	cfg = nil
	e.call("/api/config", nil, &cfg)
	if cfg["blockAll"] != true {
		t.Fatalf("unmapped block: %v", cfg)
	}
}

func TestShortsSetting(t *testing.T) {
	e := setup(t)
	// Allow everything by rule, so only the Shorts setting decides.
	k, _ := e.app.St.Kid(e.kid.ID)
	k.BlockDefault = rules.ListAllow
	allow := rules.Rule{Tier: rules.TierBlock, List: rules.ListAllow, Type: rules.TypeVideo, Value: "shortshort1", KidID: k.ID}
	e.app.St.AddRule(&allow)
	short := rules.Meta{VideoID: "shortshort1", Title: "a short", IsShort: true}
	long := rules.Meta{VideoID: "longlonglon", Title: "a video", Full: true}

	check := func(m rules.Meta) checkResp {
		var c checkResp
		e.call("/api/check", m, &c)
		return c
	}
	for _, tc := range []struct{ mode, short, long string }{
		{store.ShortsFilter, rules.Play, rules.Play},
		{store.ShortsBlock, rules.Block, rules.Play},
		{store.ShortsHide, rules.Hide, rules.Play},
	} {
		k.Options.Shorts = tc.mode
		if err := e.app.St.SaveKid(&k); err != nil {
			t.Fatal(err)
		}
		if c := check(short); c.Outcome != tc.short {
			t.Errorf("%s: short = %+v", tc.mode, c)
		} else if tc.mode == store.ShortsBlock && !c.NoAsk {
			t.Errorf("block mode should not offer asking: %+v", c)
		}
		if c := check(long); c.Outcome != tc.long {
			t.Errorf("%s: long video = %+v", tc.mode, c)
		}
		var f struct{ Results map[string]string }
		e.call("/api/filter", map[string]any{"items": []rules.Meta{{VideoID: "shortshort1", IsShort: true}}}, &f)
		want := map[string]string{rules.Play: "show", rules.Block: "locked", rules.Hide: "hide"}[tc.short]
		if f.Results["shortshort1"] != want {
			t.Errorf("%s: tile = %q want %q", tc.mode, f.Results["shortshort1"], want)
		}
	}
	// Opened later as /watch?v= (no Short flag from the page): still a Short.
	if c := check(rules.Meta{VideoID: "shortshort1", Title: "a short", Full: true}); c.Outcome != rules.Hide {
		t.Errorf("short via /watch: %+v", c)
	}
	// Kids can't ask for a Short blocked by the setting.
	k.Options.Shorts = store.ShortsBlock
	e.app.St.SaveKid(&k)
	if c := e.call("/api/request", map[string]string{"videoId": "shortshort1"}, nil); c != 400 {
		t.Errorf("request allowed for blocked short: %d", c)
	}
	// Legacy option still means hide.
	if (store.KidOptions{HideShorts: true}).ShortsMode() != store.ShortsHide {
		t.Error("legacy hideShorts not honored")
	}
}
