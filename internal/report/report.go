// Package report builds and sends the daily per-kid watch report.
package report

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"os/user"
	"strconv"
	"strings"
	"time"

	"ytguard/internal/notify"
	"ytguard/internal/store"
	"ytguard/internal/timekeeper"
)

// Report is one kid's day.
type Report struct {
	PC        string        `json:"pc"`
	Kid       string        `json:"kid"`
	Date      string        `json:"date"`
	TotalMin  int           `json:"total_minutes"`
	LimitMin  int           `json:"limit_minutes"` // -1 unlimited
	Videos    []Video       `json:"videos"`
	Blocked   []Attempt     `json:"blocked"`
	Hidden    []Attempt     `json:"hidden"`
	Requests  []RequestLine `json:"requests"`
	OtherApps []string      `json:"other_apps"` // other browsers / video apps seen that day
	PublicURL string        `json:"review_url,omitempty"`
}

// Video is one watched video.
type Video struct {
	Time    string `json:"time"`
	Title   string `json:"title"`
	Channel string `json:"channel"`
	URL     string `json:"url"`
	Minutes int    `json:"minutes"`
	Seconds int    `json:"seconds"`
	Thumb   string `json:"thumbnail"`
}

// Attempt is a blocked or hidden video the kid tried to open.
type Attempt struct {
	Time    string `json:"time"`
	Title   string `json:"title"`
	Channel string `json:"channel"`
	URL     string `json:"url"`
	Reason  string `json:"reason"`
}

// RequestLine is an approval request made that day.
type RequestLine struct {
	Title   string `json:"title"`
	Channel string `json:"channel"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// Build assembles a report for kid on day (YYYY-MM-DD, local).
func Build(st *store.Store, k store.Kid, day string) (Report, error) {
	t, err := time.ParseInLocation("2006-01-02", day, time.Local)
	if err != nil {
		return Report{}, err
	}
	s, _ := st.Settings()
	r := Report{PC: s.PCName, Kid: k.Name, Date: day, PublicURL: s.PublicURL, LimitMin: -1}
	if sch, err := st.Schedule(k.ID, int(t.Weekday())); err == nil && sch.DailyMinutes >= 0 {
		bonus, _ := st.BonusMinutes(k.ID, day)
		r.LimitMin = sch.DailyMinutes + bonus
	}
	ws, err := st.Watches(k.ID, day)
	if err != nil {
		return r, err
	}
	total := 0
	for _, w := range ws {
		total += w.Seconds
		r.Videos = append(r.Videos, Video{
			Time: clock(w.FirstSeen), Title: orUnknown(w.Title), Channel: w.ChannelName,
			URL: "https://www.youtube.com/watch?v=" + w.VideoID, Minutes: (w.Seconds + 30) / 60, Seconds: w.Seconds,
			Thumb: "https://i.ytimg.com/vi/" + w.VideoID + "/mqdefault.jpg",
		})
	}
	r.TotalMin = (total + 30) / 60
	evs, err := st.Events(k.ID, day)
	if err != nil {
		return r, err
	}
	for _, e := range evs {
		a := Attempt{Time: clock(e.TS), Title: orUnknown(e.Title), Channel: e.ChannelName, URL: "https://www.youtube.com/watch?v=" + e.VideoID, Reason: e.Reason}
		if e.Kind == store.EventHidden {
			r.Hidden = append(r.Hidden, a)
		} else {
			r.Blocked = append(r.Blocked, a)
		}
	}
	reqs, err := st.RequestsForDay(k.ID, t, timekeeper.EndOfDay(t))
	if err != nil {
		return r, err
	}
	for _, q := range reqs {
		r.Requests = append(r.Requests, RequestLine{Title: orUnknown(q.Title), Channel: q.ChannelName, Status: q.Status, Message: q.Message})
	}
	uid := -2
	if u, err := user.Lookup(k.LinuxUser); err == nil {
		uid, _ = strconv.Atoi(u.Uid)
	}
	fs, _ := st.Findings(t.Unix(), false)
	for _, f := range fs {
		if f.FirstSeen >= timekeeper.EndOfDay(t).Unix() || (f.UID != uid && f.UID != -1) {
			continue
		}
		line := f.App + " — " + f.How + " (" + f.Location + ")"
		if f.UID == -1 {
			line += " [everyone on this PC]"
		}
		if f.Kind == "running" {
			line = "RUNNING: " + line
		}
		r.OtherApps = append(r.OtherApps, line)
	}
	return r, nil
}

func clock(ts int64) string { return time.Unix(ts, 0).Format("15:04") }

func orUnknown(s string) string {
	if s == "" {
		return "(unknown title)"
	}
	return s
}

// Subject is the email subject line.
func (r Report) Subject() string {
	return fmt.Sprintf("YouTube report: %s, %s — %d min, %d videos", r.Kid, r.Date, r.TotalMin, len(r.Videos))
}

// Text renders a plain-text version.
func (r Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "YouTube report for %s on %s (%s)\n", r.Kid, r.Date, r.PC)
	if r.LimitMin >= 0 {
		fmt.Fprintf(&b, "Watched %d of %d minutes, %d videos.\n\n", r.TotalMin, r.LimitMin, len(r.Videos))
	} else {
		fmt.Fprintf(&b, "Watched %d minutes, %d videos.\n\n", r.TotalMin, len(r.Videos))
	}
	for _, v := range r.Videos {
		fmt.Fprintf(&b, "%s  %3d min  %s — %s\n         %s\n", v.Time, v.Minutes, v.Title, v.Channel, v.URL)
	}
	section := func(name string, as []Attempt) {
		if len(as) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s:\n", name)
		for _, a := range as {
			fmt.Fprintf(&b, "%s  %s — %s (%s)\n", a.Time, a.Title, a.Channel, a.Reason)
		}
	}
	section("Blocked", r.Blocked)
	section("Hidden (tried to open directly)", r.Hidden)
	if len(r.Requests) > 0 {
		b.WriteString("\nRequests:\n")
		for _, q := range r.Requests {
			fmt.Fprintf(&b, "%s — %s: %s\n", q.Title, q.Channel, q.Status)
		}
	}
	if len(r.OtherApps) > 0 {
		b.WriteString("\nOther browsers / video apps found (not controlled by YTGuard):\n")
		for _, a := range r.OtherApps {
			fmt.Fprintf(&b, "  %s\n", a)
		}
	}
	if r.PublicURL != "" {
		fmt.Fprintf(&b, "\nManage: %s\n", r.PublicURL)
	}
	return b.String()
}

var htmlTmpl = template.Must(template.New("report").Parse(`<!doctype html>
<html><body style="margin:0;padding:16px;background:#f4f4f5;font-family:-apple-system,Segoe UI,Roboto,sans-serif;color:#18181b">
<div style="max-width:640px;margin:0 auto;background:#fff;border-radius:12px;padding:20px">
<h2 style="margin:0 0 4px">{{.Kid}} — {{.Date}}</h2>
<p style="margin:0 0 16px;color:#52525b">{{.TotalMin}} min watched{{if ge .LimitMin 0}} of {{.LimitMin}}{{end}} · {{len .Videos}} videos · {{.PC}}</p>
{{if .Videos}}<table style="width:100%;border-collapse:collapse">
{{range .Videos}}<tr style="border-top:1px solid #e4e4e7">
<td style="padding:8px 8px 8px 0;width:120px"><a href="{{.URL}}"><img src="{{.Thumb}}" width="120" height="68" style="border-radius:6px;display:block" alt=""></a></td>
<td style="padding:8px 0;vertical-align:top"><a href="{{.URL}}" style="color:#18181b;font-weight:600;text-decoration:none">{{.Title}}</a><br>
<span style="color:#52525b;font-size:13px">{{.Channel}} · {{.Time}} · {{.Minutes}} min</span></td></tr>
{{end}}</table>{{else}}<p>No videos watched.</p>{{end}}
{{if .Blocked}}<h3 style="margin:20px 0 6px">Blocked</h3><ul style="padding-left:18px;margin:0">
{{range .Blocked}}<li style="margin:4px 0"><a href="{{.URL}}">{{.Title}}</a> — {{.Channel}} <span style="color:#71717a;font-size:13px">{{.Time}} · {{.Reason}}</span></li>{{end}}</ul>{{end}}
{{if .Hidden}}<h3 style="margin:20px 0 6px">Hidden (opened from a direct link)</h3><ul style="padding-left:18px;margin:0">
{{range .Hidden}}<li style="margin:4px 0"><a href="{{.URL}}">{{.Title}}</a> — {{.Channel}} <span style="color:#71717a;font-size:13px">{{.Time}} · {{.Reason}}</span></li>{{end}}</ul>{{end}}
{{if .Requests}}<h3 style="margin:20px 0 6px">Requests</h3><ul style="padding-left:18px;margin:0">
{{range .Requests}}<li style="margin:4px 0">{{.Title}} — {{.Channel}}: <b>{{.Status}}</b>{{if .Message}} “{{.Message}}”{{end}}</li>{{end}}</ul>{{end}}
{{if .OtherApps}}<h3 style="margin:20px 0 6px;color:#b91c1c">Other browsers / video apps found</h3>
<p style="margin:0 0 6px;color:#52525b;font-size:13px">YTGuard doesn't control these; they could be used to watch YouTube without limits.</p>
<ul style="padding-left:18px;margin:0">{{range .OtherApps}}<li style="margin:4px 0">{{.}}</li>{{end}}</ul>{{end}}
{{if .PublicURL}}<p style="margin-top:20px"><a href="{{.PublicURL}}">Open YTGuard</a></p>{{end}}
</div></body></html>`))

// HTML renders the email body.
func (r Report) HTML() (string, error) {
	var b bytes.Buffer
	err := htmlTmpl.Execute(&b, r)
	return b.String(), err
}

// Send delivers a report through every enabled channel. It returns nil if
// at least one channel succeeded (or none are enabled).
// mqtt, if not nil, also delivers the report to Home Assistant over MQTT.
func Send(ctx context.Context, s store.Settings, r Report, mqtt func(kind string, payload map[string]any) error) error {
	var errs []string
	sent := 0
	if s.ReportEmail {
		html, err := r.HTML()
		if err == nil {
			err = notify.Email(s, r.Subject(), html, r.Text())
		}
		if err != nil {
			errs = append(errs, "email: "+err.Error())
		} else {
			sent++
		}
	}
	payload := map[string]any{"type": "daily_report", "pc": s.PCName, "kid": r.Kid, "date": r.Date,
		"summary": fmt.Sprintf("%s watched %d min (%d videos) on %s", r.Kid, r.TotalMin, len(r.Videos), r.Date),
		"text":    r.Text(), "report": r}
	if s.ReportHA && s.HAWebhookURL != "" {
		if err := notify.HA(ctx, s.HAWebhookURL, s.HAInsecureTLS, payload); err != nil {
			errs = append(errs, "home assistant webhook: "+err.Error())
		} else {
			sent++
		}
	}
	if s.ReportHA && s.MQTTEnabled && mqtt != nil {
		if err := mqtt("daily_report", payload); err != nil {
			errs = append(errs, "MQTT: "+err.Error())
		} else {
			sent++
		}
	}
	if len(errs) > 0 && sent == 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	for _, e := range errs {
		slog.Warn("report delivery", "kid", r.Kid, "err", e)
	}
	return nil
}

// Scheduler sends each kid's report once a day at the configured time,
// catching up on yesterday's report if the PC was off at report time.
type Scheduler struct {
	St   *store.Store
	Now  func() time.Time
	MQTT func(kind string, payload map[string]any) error

	failed map[string]time.Time // kid/day -> last failed attempt
}

// Run loops until ctx is cancelled.
func (sc *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		sc.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (sc *Scheduler) tick(ctx context.Context) {
	s, err := sc.St.Settings()
	if err != nil || (!s.ReportEmail && !s.ReportHA) {
		return
	}
	now := sc.Now()
	hh, mm := 20, 30
	fmt.Sscanf(s.ReportTime, "%d:%d", &hh, &mm)
	dueToday := now.Hour()*60+now.Minute() >= hh*60+mm
	kids, err := sc.St.Kids()
	if err != nil {
		return
	}
	days := []string{timekeeper.Day(now.AddDate(0, 0, -1))}
	if dueToday {
		days = append(days, timekeeper.Day(now))
	}
	for _, k := range kids {
		for _, day := range days {
			if sc.St.ReportSent(k.ID, day) {
				continue
			}
			key := fmt.Sprint(k.ID, "/", day)
			if last, ok := sc.failed[key]; ok && now.Sub(last) < 30*time.Minute {
				continue
			}
			r, err := Build(sc.St, k, day)
			if err == nil {
				err = Send(ctx, s, r, sc.MQTT)
			}
			if err != nil {
				slog.Error("send report (retrying in 30 min)", "kid", k.Name, "day", day, "err", err)
				if sc.failed == nil {
					sc.failed = map[string]time.Time{}
				}
				sc.failed[key] = now
				continue
			}
			delete(sc.failed, key)
			slog.Info("report sent", "kid", k.Name, "day", day)
			_ = sc.St.MarkReportSent(k.ID, day)
		}
	}
}
