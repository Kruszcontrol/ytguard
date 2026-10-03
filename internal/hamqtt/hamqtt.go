// Package hamqtt connects YTGuard to Home Assistant through an MQTT broker
// using MQTT discovery: each PC appears as a device, each kid as a device
// under it, with sensors, a pause switch, bonus-time buttons, an update
// entity and an event entity. Commands (buttons, approvals) come back over
// MQTT, so one Home Assistant automation works for every PC.
//
// Topics (base = "<prefix>/<pc id>", prefix "ytguard" by default):
//
//	base/status                 online | offline (retained, last will)
//	base/state                  PC state JSON (retained)
//	base/update                 version info for the HA update entity (retained)
//	base/event                  events: {"event_type": ..., ...}
//	base/kid/<id>/state         kid state JSON (retained)
//	base/kid/<id>/cmd           <- bonus:<min> | end_break | lock:<min> | unlock
//	base/kid/<id>/pause/set     <- ON | OFF
//	base/request/<id>/set       <- approve_video | approve_channel | deny
//
// The "<prefix>/<pc id>/..." layout leaves room for future topics shared
// between PCs (e.g. syncing a kid's time when they use several PCs).
package hamqtt

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"ytguard"
	"ytguard/internal/core"
	"ytguard/internal/mqtt"
	"ytguard/internal/store"
)

// EventTypes are the kinds sent to the HA event entity.
var EventTypes = []string{"approval_request", "time_up", "unapproved_app", "update_available", "daily_report", "admin_login", "test"}

// Bridge keeps the MQTT connection and publishes state.
type Bridge struct {
	App  *core.App
	PCID string // stable per install, e.g. "dcchlejacgbd"

	mu          sync.Mutex
	c           *mqtt.Client
	base        string
	status      string
	connectedAt time.Time
	kick        chan struct{}
	rediscover  chan struct{}

	kidKeyPublished string
	lastPayload     map[string]string // topic -> last retained state sent
}

// New creates a bridge.
func New(app *core.App, pcID string) *Bridge {
	return &Bridge{App: app, PCID: pcID, status: "off", kick: make(chan struct{}, 1), rediscover: make(chan struct{}, 1)}
}

// Status describes the connection for the Settings page.
func (b *Bridge) Status() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.c != nil {
		return "connected since " + b.connectedAt.Format("Jan 2 15:04")
	}
	return b.status
}

// Kick publishes fresh state soon (after a change made elsewhere).
func (b *Bridge) Kick() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// Publish sends an event to Home Assistant (used as core.App.MQTT).
func (b *Bridge) Publish(kind string, payload map[string]any) error {
	b.mu.Lock()
	c, base := b.c, b.base
	b.mu.Unlock()
	if c == nil {
		return core.ErrMQTTOff
	}
	out := map[string]any{"event_type": kind, "pc_id": b.PCID}
	for k, v := range payload {
		out[k] = v
	}
	if id, ok := payload["request_id"]; ok {
		out["approve_topic"] = fmt.Sprintf("%s/request/%v/set", base, id)
	}
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return c.Publish(base+"/event", data, false)
}

type connKey struct {
	addr, user, pass, base, disc string
	tls, insecure                bool
}

func keyOf(s store.Settings) connKey {
	return connKey{net.JoinHostPort(s.MQTTHost, strconv.Itoa(s.MQTTPort)), s.MQTTUser, s.MQTTPass, s.MQTTBase, s.MQTTDiscovery, s.MQTTTLS, s.MQTTInsecure}
}

func (b *Bridge) setStatus(st string) {
	b.mu.Lock()
	b.status = st
	b.mu.Unlock()
}

// Run connects while MQTT is enabled, reconnecting as needed.
func (b *Bridge) Run(ctx context.Context) {
	backoff := 5 * time.Second
	lastErr := ""
	for ctx.Err() == nil {
		s, _ := b.App.St.Settings()
		if !s.MQTTEnabled || s.MQTTHost == "" {
			if s.MQTTEnabled {
				b.setStatus("not configured: enter the broker address")
			} else {
				b.setStatus("off")
			}
			b.wait(ctx, 15*time.Second)
			continue
		}
		err := b.session(ctx, s)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			b.setStatus("not connected: " + err.Error())
			if err.Error() != lastErr { // don't fill the log while the broker is down
				slog.Warn("mqtt (retrying quietly)", "err", err)
				lastErr = err.Error()
			}
			b.wait(ctx, backoff)
			backoff = min(backoff*2, 2*time.Minute)
		} else {
			backoff, lastErr = 5*time.Second, ""
		}
	}
}

func (b *Bridge) wait(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	case <-b.kick:
	}
}

// session runs one connection until it drops or the settings change.
// It returns nil when it should reconnect immediately.
func (b *Bridge) session(ctx context.Context, s store.Settings) error {
	base := strings.Trim(s.MQTTBase, "/") + "/" + b.PCID
	opts := mqtt.Options{
		Addr:     net.JoinHostPort(s.MQTTHost, strconv.Itoa(s.MQTTPort)),
		ClientID: "ytguard-" + b.PCID,
		Username: s.MQTTUser, Password: s.MQTTPass,
		Will: &mqtt.Message{Topic: base + "/status", Payload: []byte("offline"), Retain: true},
	}
	if s.MQTTTLS {
		opts.TLS = &tls.Config{ServerName: s.MQTTHost, MinVersion: tls.VersionTLS12, InsecureSkipVerify: s.MQTTInsecure}
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	c, err := mqtt.Dial(dctx, opts, func(m mqtt.Message) { b.handle(base, s.MQTTDiscovery, m) })
	cancel()
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.c, b.base, b.connectedAt = c, base, time.Now()
	b.lastPayload = nil // new connection: send everything again
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.c = nil
		b.mu.Unlock()
	}()
	slog.Info("mqtt connected", "broker", opts.Addr, "base", base)

	if err := c.Subscribe(base+"/kid/+/cmd", base+"/kid/+/pause/set", base+"/kid/+/limits/set", base+"/kid/+/breaks/set",
		base+"/request/+/set", s.MQTTDiscovery+"/status"); err != nil {
		c.Close()
		return err
	}
	if err := b.publishDiscovery(c, base, s.MQTTDiscovery); err != nil {
		c.Close()
		return err
	}
	_ = c.Publish(base+"/status", []byte("online"), true)
	key := keyOf(s)
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		if err := b.publishState(c, base); err != nil {
			c.Close()
			return err
		}
		select {
		case <-ctx.Done():
			_ = c.Publish(base+"/status", []byte("offline"), true)
			c.Close()
			return nil
		case <-c.Done():
			return c.Err()
		case <-b.rediscover:
			if err := b.publishDiscovery(c, base, s.MQTTDiscovery); err != nil {
				c.Close()
				return err
			}
			b.mu.Lock()
			b.lastPayload = nil // Home Assistant restarted: send all state again
			b.mu.Unlock()
		case <-b.kick:
		case <-t.C:
		}
		cur, _ := b.App.St.Settings()
		if !cur.MQTTEnabled || keyOf(cur) != key {
			_ = c.Publish(base+"/status", []byte("offline"), true)
			c.Close()
			return nil // settings changed: reconnect (or stop)
		}
		if cur.PCName != s.PCName || cur.PublicURL != s.PublicURL || b.kidsChanged(c) {
			s = cur
			if err := b.publishDiscovery(c, base, s.MQTTDiscovery); err != nil {
				c.Close()
				return err
			}
		}
	}
}

// ---- state ----

type kidState struct {
	UsedMinutes      int    `json:"used_minutes"`
	RemainingMinutes *int   `json:"remaining_minutes"` // null = unlimited
	LimitMinutes     *int   `json:"limit_minutes"`
	BreakInMinutes   *int   `json:"break_in_minutes"` // null = no breaks
	State            string `json:"state"`
	Allowed          bool   `json:"allowed"`
	Watching         bool   `json:"watching"`
	Locked           bool   `json:"locked"`
	Message          string `json:"message"`
	VideoTitle       string `json:"video_title"`
	VideoChannel     string `json:"video_channel"`
	VideoURL         string `json:"video_url"`
	TodayVideos      int    `json:"today_videos"`
	PendingRequests  int    `json:"pending_requests"`
	TimeLimits       bool   `json:"time_limits"` // enforcement switches
	Breaks           bool   `json:"breaks"`
}

func minutes(sec int) *int {
	if sec < 0 {
		return nil
	}
	m := (sec + 59) / 60
	return &m
}

func (b *Bridge) publishState(c *mqtt.Client, base string) error {
	states, err := b.App.KidStates()
	if err != nil {
		return nil // database hiccup: try again next tick
	}
	pending := 0
	for _, ks := range states {
		st := ks.Status
		k := kidState{UsedMinutes: st.UsedSec / 60, RemainingMinutes: minutes(st.RemainingSec), LimitMinutes: minutes(st.LimitSec),
			BreakInMinutes: minutes(st.BreakInSec), Allowed: st.Allowed, Locked: st.Reason == "locked", Message: st.Message,
			TodayVideos: ks.TodayVideos, PendingRequests: ks.Pending, Watching: ks.WatchingNow && st.Allowed,
			TimeLimits: !ks.Kid.Options.TimeLimitsOff, Breaks: !ks.Kid.Options.BreaksOff}
		switch {
		case st.Reason != "":
			k.State = st.Reason
		case k.Watching:
			k.State = "watching"
		default:
			k.State = "idle"
		}
		if k.Watching && ks.LastVideo != nil {
			k.VideoTitle, k.VideoChannel = ks.LastVideo.Title, ks.LastVideo.ChannelName
			k.VideoURL = "https://www.youtube.com/watch?v=" + ks.LastVideo.VideoID
		}
		pending += ks.Pending
		if err := b.publishChanged(c, fmt.Sprintf("%s/kid/%d/state", base, ks.Kid.ID), k); err != nil {
			return err
		}
	}
	u := b.App.Updates.Status()
	latest := u.Latest
	if latest == "" || !u.Available {
		latest = ytguard.Version
	}
	if err := b.publishChanged(c, base+"/update", map[string]any{"installed_version": ytguard.Version, "latest_version": latest,
		"title": "YTGuard", "release_url": u.URL, "release_summary": truncate(u.Notes, 250)}); err != nil {
		return err
	}
	s, _ := b.App.St.Settings()
	return b.publishChanged(c, base+"/state", map[string]any{"pc": s.PCName, "pc_id": b.PCID, "version": ytguard.Version,
		"pending_requests": pending, "other_apps": len(b.App.ActiveFindings()), "kids": len(states)})
}

// publishChanged sends a retained state only when it changed, so Home
// Assistant isn't asked to record identical values every 20 seconds.
func (b *Bridge) publishChanged(c *mqtt.Client, topic string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b.mu.Lock()
	same := b.lastPayload[topic] == string(data)
	b.mu.Unlock()
	if same {
		return nil
	}
	if err := c.Publish(topic, data, true); err != nil {
		return err
	}
	b.mu.Lock()
	if b.lastPayload == nil {
		b.lastPayload = map[string]string{}
	}
	b.lastPayload[topic] = string(data)
	b.mu.Unlock()
	return nil
}

func publishJSON(c *mqtt.Client, topic string, v any, retain bool) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Publish(topic, data, retain)
}

// ---- commands ----

func (b *Bridge) handle(base, disc string, m mqtt.Message) {
	if m.Topic == disc+"/status" {
		if string(m.Payload) == "online" { // Home Assistant restarted
			select {
			case b.rediscover <- struct{}{}:
			default:
			}
		}
		return
	}
	rel, ok := strings.CutPrefix(m.Topic, base+"/")
	if !ok || m.Retain { // ignore stale retained commands
		return
	}
	payload := strings.TrimSpace(string(m.Payload))
	parts := strings.Split(rel, "/")
	var err error
	var what string
	switch {
	case len(parts) == 3 && parts[0] == "kid" && parts[2] == "cmd":
		what, err = b.kidCommand(parts[1], payload)
	case len(parts) == 4 && parts[0] == "kid" && parts[2] == "pause" && parts[3] == "set":
		cmd := "unlock"
		if strings.EqualFold(payload, "ON") {
			cmd = "lock:0"
		}
		what, err = b.kidCommand(parts[1], cmd)
	case len(parts) == 4 && parts[0] == "kid" && (parts[2] == "limits" || parts[2] == "breaks") && parts[3] == "set":
		what, err = b.kidSwitch(parts[1], parts[2], strings.EqualFold(payload, "ON"))
	case len(parts) == 3 && parts[0] == "request" && parts[2] == "set":
		what, err = b.requestCommand(parts[1], payload)
	default:
		return
	}
	if err != nil {
		slog.Warn("mqtt command", "topic", m.Topic, "payload", payload, "err", err)
		return
	}
	b.App.St.Audit("mqtt", "broker", what, payload)
	b.Kick()
}

func (b *Bridge) kidCommand(idStr, cmd string) (string, error) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return "", errors.New("bad kid id")
	}
	k, err := b.App.St.Kid(id)
	if err != nil {
		return "", err
	}
	verb, arg, _ := strings.Cut(cmd, ":")
	n, _ := strconv.Atoi(arg)
	switch verb {
	case "bonus":
		if n <= 0 || n > 600 {
			return "", errors.New("bonus minutes must be 1-600")
		}
		return "kid bonus " + k.Name, b.App.AddBonus(id, n, "home assistant")
	case "lock":
		if n < 0 || n > 1440 {
			return "", errors.New("lock minutes must be 0-1440")
		}
		return "kid lock " + k.Name, b.App.Lock(id, time.Duration(n)*time.Minute)
	case "unlock":
		return "kid unlock " + k.Name, b.App.Unlock(id, false)
	case "end_break":
		return "kid end break " + k.Name, b.App.Unlock(id, true)
	}
	return "", fmt.Errorf("unknown command %q", cmd)
}

// kidSwitch turns a kid's time limits or breaks enforcement on or off.
func (b *Bridge) kidSwitch(idStr, which string, on bool) (string, error) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return "", errors.New("bad kid id")
	}
	k, err := b.App.St.Kid(id)
	if err != nil {
		return "", err
	}
	if which == "limits" {
		k.Options.TimeLimitsOff = !on
	} else {
		k.Options.BreaksOff = !on
	}
	return fmt.Sprintf("kid %s %s on=%v", which, k.Name, on), b.App.St.SaveKid(&k)
}

func (b *Bridge) requestCommand(idStr, cmd string) (string, error) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return "", errors.New("bad request id")
	}
	var r store.Request
	switch cmd {
	case "approve_video":
		r, err = b.App.Approve(id, core.ApproveVideo)
	case "approve_channel":
		r, err = b.App.Approve(id, core.ApproveChannel)
	case "deny":
		r, err = b.App.Deny(id)
	default:
		return "", fmt.Errorf("unknown request command %q", cmd)
	}
	return "request " + cmd + ": " + r.Title, err
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
