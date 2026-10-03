// Package core holds the application logic shared by the extension API,
// the admin UI and the REST API.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"ytguard"
	"ytguard/internal/notify"
	"ytguard/internal/rules"
	"ytguard/internal/store"
	"ytguard/internal/timekeeper"
	"ytguard/internal/update"
	"ytguard/internal/ytmeta"
)

// App is the running application.
type App struct {
	St      *store.Store
	YT      *ytmeta.Client
	Updates *update.Checker
	Now     func() time.Time
	// MQTT publishes an event to Home Assistant over MQTT (nil = off).
	MQTT func(kind string, payload map[string]any) error

	mu         sync.Mutex // serializes time accounting
	timeUpSent map[int64]string
}

// New creates an App.
func New(st *store.Store) *App {
	a := &App{St: st, Now: time.Now, timeUpSent: map[int64]string{}}
	a.YT = ytmeta.New(func() string {
		s, _ := st.Settings()
		return s.YouTubeAPIKey
	})
	a.Updates = &update.Checker{
		Repo: ytguard.Repo,
		Enabled: func() bool {
			s, _ := st.Settings()
			return s.UpdateCheck
		},
		OnNew: func(u update.Status) {
			a.Event("update_available", map[string]any{"current": u.Current, "latest": u.Latest, "url": u.URL, "notes": u.Notes,
				"how": "On this PC run: sudo ytguard upgrade"})
		},
		Load: func() (update.Status, string) {
			var saved updateState
			_, _ = st.GetJSON("update_status", &saved)
			return saved.Status, saved.Notified
		},
		Save: func(u update.Status, notified string) {
			_ = st.SetJSON("update_status", updateState{u, notified})
		},
	}
	return a
}

type updateState struct {
	Status   update.Status `json:"status"`
	Notified string        `json:"notified"`
}

// Policy loads the rule policy for a kid.
func (a *App) Policy(k store.Kid) (rules.Policy, error) {
	rs, err := a.St.RulesForKid(k.ID)
	return rules.Policy{KidID: k.ID, HideDefault: k.HideDefault, BlockDefault: k.BlockDefault, Rules: rs}, err
}

// Decide evaluates a video for a kid. When full is true (the kid opened
// the video) missing metadata is fetched so every rule can be checked.
func (a *App) Decide(ctx context.Context, k store.Kid, m rules.Meta, full bool) (rules.Decision, rules.Meta, error) {
	// The cache only holds metadata the daemon fetched itself (plus "is a
	// Short" flags): what the extension sends is used for this decision but
	// never stored, so a kid can't poison it by calling the API directly.
	if cached, ok := a.St.Meta(m.VideoID); ok {
		m.Merge(cached) // keeps m's own values
	}
	if m.IsShort {
		_ = a.St.PutMeta(rules.Meta{VideoID: m.VideoID, IsShort: true})
	}
	if full && !m.Full && m.VideoID != "" {
		ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		fetched, err := a.YT.Video(ctx, m.VideoID)
		cancel()
		if err == nil {
			fetched.IsShort = fetched.IsShort || m.IsShort
			_ = a.St.PutMeta(fetched)
			m.Merge(fetched)
		} else {
			slog.Warn("fetch video metadata", "video", m.VideoID, "err", err)
		}
	}
	p, err := a.Policy(k)
	if err != nil {
		return rules.Decision{}, m, err
	}
	d := rules.Evaluate(p, m)
	if m.IsShort {
		d = applyShorts(k.Options.ShortsMode(), d)
	}
	return d, m, nil
}

// applyShorts enforces the kid's Shorts setting, which overrides all rules.
func applyShorts(mode string, d rules.Decision) rules.Decision {
	switch mode {
	case store.ShortsHide:
		const why = "Shorts are hidden for this kid (kid setting)"
		return rules.Decision{Outcome: rules.Hide, Final: true, Reason: why,
			Hide: rules.TierResult{Tier: rules.TierHide, Verdict: rules.ListDeny, Reason: why}}
	case store.ShortsBlock:
		if d.Outcome == rules.Hide {
			return d // hidden by a rule stays hidden
		}
		const why = "Shorts are blocked for this kid (kid setting)"
		return rules.Decision{Outcome: rules.Block, Final: true, Reason: why, Hide: d.Hide,
			Block: &rules.TierResult{Tier: rules.TierBlock, Verdict: rules.ListDeny, Reason: why}}
	}
	return d
}

// Enrich fills in metadata for feed tiles from the cache, and from the
// Data API when a key is configured.
func (a *App) Enrich(ctx context.Context, items []rules.Meta) {
	var missing []string
	for i := range items {
		if cached, ok := a.St.Meta(items[i].VideoID); ok {
			items[i].Merge(cached)
		}
		if !items[i].Full && items[i].VideoID != "" {
			missing = append(missing, items[i].VideoID)
		}
	}
	if len(missing) == 0 || !a.YT.HasAPIKey() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	got, err := a.YT.Videos(ctx, missing)
	if err != nil {
		slog.Warn("youtube api", "err", err)
	}
	for i := range items {
		if m, ok := got[items[i].VideoID]; ok {
			items[i].Merge(m)
			_ = a.St.PutMeta(m)
		}
	}
}

// schedule returns a kid's schedule for t's weekday.
func (a *App) schedule(kidID int64, t time.Time) timekeeper.Schedule {
	sch, err := a.St.Schedule(kidID, int(t.Weekday()))
	if err != nil {
		slog.Error("load schedule", "err", err)
		return timekeeper.Schedule{DailyMinutes: 0}
	}
	return sch
}

// Status returns a kid's current time status.
func (a *App) Status(kidID int64) (timekeeper.Status, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.Now()
	st, err := a.St.State(kidID)
	if err != nil {
		return timekeeper.Status{}, err
	}
	sch := a.schedule(kidID, now)
	timekeeper.Roll(&st, sch, now)
	bonus, _ := a.St.BonusMinutes(kidID, timekeeper.Day(now))
	return timekeeper.Evaluate(&st, sch, bonus, now), nil
}

// Heartbeat records play from the extension and returns the time status.
func (a *App) Heartbeat(k store.Kid, m rules.Meta, playing, start bool) (timekeeper.Status, error) {
	a.mu.Lock()
	now := a.Now()
	st, err := a.St.State(k.ID)
	if err != nil {
		a.mu.Unlock()
		return timekeeper.Status{}, err
	}
	sch := a.schedule(k.ID, now)
	day := timekeeper.Day(now)
	bonus, _ := a.St.BonusMinutes(k.ID, day)
	status, counted := timekeeper.Heartbeat(&st, sch, bonus, now, playing, start)
	err = a.St.SaveState(k.ID, st)
	if err == nil && m.VideoID != "" && (counted > 0 || (playing && status.Allowed)) {
		if cached, ok := a.St.Meta(m.VideoID); ok {
			m.Merge(cached)
		}
		err = a.St.AddWatch(k.ID, st.Day, m, counted, now.Unix())
	}
	notifyTimeUp := !status.Allowed && (status.Reason == timekeeper.ReasonTimeUp) && a.timeUpSent[k.ID] != day
	if notifyTimeUp {
		a.timeUpSent[k.ID] = day
	}
	a.mu.Unlock()
	if notifyTimeUp {
		a.Event("time_up", map[string]any{"kid": k.Name, "used_minutes": status.UsedSec / 60})
	}
	return status, err
}

// AddBonus grants extra minutes today.
func (a *App) AddBonus(kidID int64, minutes int, note string) error {
	return a.St.AddGrant(kidID, timekeeper.Day(a.Now()), minutes, note)
}

// Lock pauses YouTube for a kid for d (0 = until end of day).
func (a *App) Lock(kidID int64, d time.Duration) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.St.State(kidID)
	if err != nil {
		return err
	}
	now := a.Now()
	until := timekeeper.EndOfDay(now)
	if d > 0 {
		until = now.Add(d)
	}
	timekeeper.Roll(&st, a.schedule(kidID, now), now)
	st.LockedUntil = until.Unix()
	return a.St.SaveState(kidID, st)
}

// Unlock removes a lock and ends any break.
func (a *App) Unlock(kidID int64, endBreak bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.St.State(kidID)
	if err != nil {
		return err
	}
	st.LockedUntil = 0
	if endBreak {
		st.BreakUntil, st.ContinuousSec = 0, 0
	}
	return a.St.SaveState(kidID, st)
}

// ResetToday clears today's used time.
func (a *App) ResetToday(kidID int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.St.State(kidID)
	if err != nil {
		return err
	}
	st.UsedSec, st.ContinuousSec, st.BreakUntil = 0, 0, 0
	return a.St.SaveState(kidID, st)
}

// RequestAccess records a kid asking to watch a blocked video.
func (a *App) RequestAccess(ctx context.Context, k store.Kid, videoID, message string) (store.Request, error) {
	m := rules.Meta{VideoID: videoID}
	d, m, err := a.Decide(ctx, k, m, true)
	if err != nil {
		return store.Request{}, err
	}
	if d.Outcome == rules.Hide {
		// Kids can't ask for hidden videos (they shouldn't know they exist).
		return store.Request{}, errors.New("not available")
	}
	if d.Final {
		return store.Request{}, errors.New("this is turned off by a parent setting")
	}
	// Only videos the kid was actually shown as blocked can be requested.
	// (Also covers the case where YouTube couldn't be reached to check the
	// video's details here.)
	ev, err := a.St.LatestEvent(k.ID, videoID, a.Now().Add(-time.Hour))
	if err != nil || ev.Kind != store.EventBlocked {
		return store.Request{}, errors.New("not available")
	}
	if m.Title == "" {
		m.Title, m.ChannelID, m.ChannelName = ev.Title, ev.ChannelID, ev.ChannelName
	}
	if n, err := a.St.CountRequestsSince(k.ID, a.Now().Add(-time.Hour)); err == nil && n >= MaxRequestsPerHour {
		return store.Request{}, ErrTooManyRequests
	}
	r := store.Request{KidID: k.ID, KidName: k.Name, VideoID: videoID, Title: m.Title, ChannelID: m.ChannelID,
		ChannelName: m.ChannelName, Reason: d.Reason, Message: truncate(strings.TrimSpace(message), 300)}
	created, err := a.St.CreateRequest(&r)
	if err != nil {
		return r, err
	}
	if created {
		s, _ := a.St.Settings()
		a.Event("approval_request", map[string]any{
			"kid": k.Name, "request_id": r.ID, "video_id": r.VideoID, "title": r.Title, "channel": r.ChannelName,
			"message": r.Message, "reason": r.Reason, "url": "https://www.youtube.com/watch?v=" + r.VideoID,
			"review_url": strings.TrimRight(s.PublicURL, "/") + "/",
		})
	}
	return r, nil
}

// MaxRequestsPerHour limits new approval requests per kid (notification spam).
const MaxRequestsPerHour = 10

// ErrTooManyRequests is returned when a kid hits MaxRequestsPerHour.
var ErrTooManyRequests = errors.New("you've asked a lot already; wait a bit and try again")

// Approve scopes.
const (
	ApproveVideo   = "video"
	ApproveChannel = "channel"
)

// Approve allows a requested video (or its whole channel) for that kid in
// the Block tier.
func (a *App) Approve(id int64, scope string) (store.Request, error) {
	r, err := a.St.Request(id)
	if err != nil {
		return r, err
	}
	rule := rules.Rule{Tier: rules.TierBlock, List: rules.ListAllow, KidID: r.KidID, Note: fmt.Sprintf("Approved request #%d", r.ID)}
	switch scope {
	case ApproveChannel:
		if r.ChannelID == "" {
			return r, errors.New("channel unknown for this video; approve the video instead")
		}
		rule.Type, rule.Value, rule.Label = rules.TypeChannel, r.ChannelID, r.ChannelName
		if cached, ok := a.St.Meta(r.VideoID); ok {
			rule.Extra = cached.ChannelHandle
		}
	default:
		scope = ApproveVideo
		rule.Type, rule.Value, rule.Label = rules.TypeVideo, r.VideoID, r.Title
	}
	// A kid-scoped deny at the same level would beat the new allow.
	if err := a.St.DeleteRulesWhere(rules.TierBlock, rules.ListDeny, rule.Type, rule.Value, r.KidID); err != nil {
		return r, err
	}
	if _, err := a.St.AddRule(&rule); err != nil {
		return r, err
	}
	return r, a.St.DecideRequest(id, store.RequestApproved, scope)
}

// Deny rejects a request.
func (a *App) Deny(id int64) (store.Request, error) {
	r, err := a.St.Request(id)
	if err != nil {
		return r, err
	}
	return r, a.St.DecideRequest(id, store.RequestDenied, "")
}

// Event sends an instant Home Assistant event if enabled. Runs async.
// The event goes to the Home Assistant webhook and/or MQTT, whichever are
// configured.
func (a *App) Event(kind string, data map[string]any) {
	s, err := a.St.Settings()
	if err != nil || !s.HAEvents {
		return
	}
	payload := map[string]any{"type": kind, "pc": s.PCName, "time": a.Now().Format(time.RFC3339)}
	for k, v := range data {
		payload[k] = v
	}
	if s.HAWebhookURL != "" {
		go func() {
			if err := notify.HA(context.Background(), s.HAWebhookURL, s.HAInsecureTLS, payload); err != nil {
				slog.Warn("home assistant event", "type", kind, "err", err)
			}
		}()
	}
	if a.MQTT != nil {
		if err := a.MQTT(kind, payload); err != nil && !errors.Is(err, ErrMQTTOff) {
			slog.Warn("mqtt event", "type", kind, "err", err)
		}
	}
}

// ErrMQTTOff is returned by the MQTT publisher when MQTT isn't connected.
var ErrMQTTOff = errors.New("MQTT not connected")

// KidState summarizes a kid for the dashboard and REST API.
type KidState struct {
	Kid         store.Kid         `json:"kid"`
	Status      timekeeper.Status `json:"status"`
	WatchingNow bool              `json:"watchingNow"`
	LastVideo   *store.Watch      `json:"lastVideo,omitempty"`
	TodayVideos int               `json:"todayVideos"`
	Pending     int               `json:"pendingRequests"`
}

// KidStates returns the state of every kid.
func (a *App) KidStates() ([]KidState, error) {
	kids, err := a.St.Kids()
	if err != nil {
		return nil, err
	}
	pending, _ := a.St.Requests(store.RequestPending, 500)
	now := a.Now()
	var out []KidState
	for _, k := range kids {
		s, err := a.Status(k.ID)
		if err != nil {
			return nil, err
		}
		ks := KidState{Kid: k, Status: s}
		if w, ok := a.St.LastWatch(k.ID); ok {
			ks.LastVideo = &w
			ks.WatchingNow = now.Unix()-w.LastSeen < 45
		}
		ws, _ := a.St.Watches(k.ID, timekeeper.Day(now))
		ks.TodayVideos = len(ws)
		for _, r := range pending {
			if r.KidID == k.ID {
				ks.Pending++
			}
		}
		out = append(out, ks)
	}
	return out, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
