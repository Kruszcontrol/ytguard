package hamqtt

import (
	"strings"
	"testing"

	"ytguard/internal/core"
	"ytguard/internal/mqtt"
	"ytguard/internal/store"
)

func newBridge(t *testing.T, pcID string) (*Bridge, store.Kid) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	k := store.Kid{LinuxUser: "kid", Name: "Alice"}
	st.SaveKid(&k)
	return New(core.New(st), pcID), k
}

func TestEntitiesUniquePerPC(t *testing.T) {
	a, _ := newBridge(t, "pcaaaaaaaaaa")
	b, _ := newBridge(t, "pcbbbbbbbbbb")
	ea, _ := a.entities("ytguard/pcaaaaaaaaaa")
	eb, _ := b.entities("ytguard/pcbbbbbbbbbb")
	if len(ea) != 4+13 {
		t.Fatalf("entities: %d", len(ea))
	}
	ids := map[string]bool{}
	for _, e := range append(ea, eb...) {
		id := e.config["unique_id"].(string)
		if ids[id] {
			t.Fatalf("duplicate unique_id %s", id)
		}
		ids[id] = true
		for _, k := range []string{"state_topic", "command_topic"} {
			if v, ok := e.config[k].(string); ok && !strings.HasPrefix(v, "ytguard/pcaaaaaaaaaa/") && !strings.HasPrefix(v, "ytguard/pcbbbbbbbbbb/") {
				t.Errorf("%s %s outside the PC's topics: %s", id, k, v)
			}
		}
	}
}

func TestKidsChangedAndCommands(t *testing.T) {
	b, k := newBridge(t, "pcaaaaaaaaaa")
	_, key := b.entities("ytguard/pcaaaaaaaaaa")
	b.kidKeyPublished = key
	if b.kidsChanged(nil) {
		t.Fatal("no change expected")
	}
	k2 := store.Kid{LinuxUser: "kid2", Name: "Bob"}
	b.App.St.SaveKid(&k2)
	if !b.kidsChanged(nil) {
		t.Fatal("new kid not noticed")
	}

	base := "ytguard/pcaaaaaaaaaa"
	send := func(topic, payload string, retain bool) {
		b.handle(base, "homeassistant", mqtt.Message{Topic: base + topic, Payload: []byte(payload), Retain: retain})
	}
	st0, _ := b.App.Status(k.ID)
	send("/kid/1/pause/set", "ON", false)
	if st, _ := b.App.Status(k.ID); st.Reason != "locked" {
		t.Fatalf("pause: %+v", st)
	}
	send("/kid/1/pause/set", "OFF", false)
	send("/kid/1/cmd", "bonus:15", false)
	send("/kid/1/cmd", "bonus:15", true)    // retained: ignored
	send("/kid/1/cmd", "bonus:9999", false) // out of range: ignored
	send("/kid/abc/cmd", "bonus:15", false) // bad id: ignored
	st, _ := b.App.Status(k.ID)
	if st.Reason == "locked" || st.LimitSec != st0.LimitSec+15*60 {
		t.Fatalf("after commands: %+v (before %+v)", st, st0)
	}
	// Enforcement switches.
	send("/kid/1/limits/set", "OFF", false)
	send("/kid/1/breaks/set", "OFF", false)
	if kk, _ := b.App.St.Kid(k.ID); !kk.Options.TimeLimitsOff || !kk.Options.BreaksOff {
		t.Fatalf("switches: %+v", kk.Options)
	}
	if st, _ := b.App.Status(k.ID); st.LimitSec != -1 || st.BreakInSec != -1 {
		t.Fatalf("limits still enforced: %+v", st)
	}
	send("/kid/1/limits/set", "ON", false)
	if kk, _ := b.App.St.Kid(k.ID); kk.Options.TimeLimitsOff {
		t.Fatal("limits not back on")
	}
	// Approval by topic.
	r := store.Request{KidID: k.ID, VideoID: "abcdefghijk", Title: "T"}
	b.App.St.CreateRequest(&r)
	send("/request/1/set", "approve_video", false)
	if got, _ := b.App.St.Request(r.ID); got.Status != store.RequestApproved {
		t.Fatalf("request: %+v", got)
	}
	// Another PC's topics are ignored.
	b.handle(base, "homeassistant", mqtt.Message{Topic: "ytguard/otherpc/kid/1/pause/set", Payload: []byte("ON")})
	if st, _ := b.App.Status(k.ID); st.Reason == "locked" {
		t.Fatal("acted on another PC's command")
	}
}

func TestPublishNeedsConnection(t *testing.T) {
	b, _ := newBridge(t, "pcaaaaaaaaaa")
	if err := b.Publish("test", map[string]any{}); err != core.ErrMQTTOff {
		t.Fatalf("err = %v", err)
	}
}
