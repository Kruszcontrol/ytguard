package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ytguard/internal/auth"
	"ytguard/internal/timekeeper"
)

// ---- dashboard ----

func (s *Server) dashboard(w http.ResponseWriter, r *req) {
	snap := s.fetch(r.Context())
	var updates, problems []PCView
	findings := 0
	for _, v := range snap.PCs {
		if !v.Online {
			problems = append(problems, v)
		} else if v.State.YTGuard.UpdateAvailable {
			updates = append(updates, v)
		}
		findings += len(v.State.OtherApps)
	}
	s.page(w, r, "dashboard", map[string]any{"Snap": snap, "Updates": updates, "Problems": problems, "Findings": findings})
}

// pcFrom finds the PC named in the URL.
func (s *Server) pcFrom(r *req) (PC, error) {
	if p, ok := s.pc(r.PathValue("pc")); ok {
		return p, nil
	}
	return PC{}, errors.New("unknown PC")
}

func (s *Server) actx(r *req) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 20*time.Second)
}

var kidActions = map[string]string{"bonus": "bonus", "lock": "lock", "unlock": "unlock", "endbreak": "end-break"}

func (s *Server) kidAction(w http.ResponseWriter, r *req) {
	p, err := s.pcFrom(r)
	if err != nil {
		back(w, r, "/", "", err)
		return
	}
	action, ok := kidActions[r.PathValue("action")]
	kid, kerr := strconv.ParseInt(r.PathValue("kid"), 10, 64)
	if !ok || kerr != nil {
		back(w, r, "/", "", errors.New("unknown action"))
		return
	}
	minutes, _ := strconv.Atoi(r.FormValue("minutes"))
	ctx, cancel := s.actx(r)
	defer cancel()
	var out struct{ Kid string }
	err = s.call(ctx, p, "POST", fmt.Sprintf("/api/v1/kids/%d/%s", kid, action), map[string]any{"minutes": minutes}, &out)
	who := out.Kid + " (" + p.Name + ")"
	msg := map[string]string{
		"bonus":     fmt.Sprintf("Gave %s %+d minutes today.", who, minutes),
		"lock":      "Paused YouTube for " + who + ".",
		"unlock":    "Unpaused " + who + ".",
		"end-break": "Ended " + who + "'s break.",
	}[action]
	if err == nil {
		s.St.Audit("console", r.IP, "kid "+action, who+" "+r.FormValue("minutes"))
	}
	back(w, r, "/", msg, err)
}

func (s *Server) requestAction(w http.ResponseWriter, r *req) {
	p, err := s.pcFrom(r)
	if err != nil {
		back(w, r, "/", "", err)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	action := r.PathValue("action")
	if err != nil || (action != "approve" && action != "deny") {
		back(w, r, "/", "", errors.New("bad request"))
		return
	}
	scope := r.FormValue("scope")
	ctx, cancel := s.actx(r)
	defer cancel()
	var out struct{ Request Request }
	err = s.call(ctx, p, "POST", fmt.Sprintf("/api/v1/requests/%d/%s", id, action), map[string]string{"scope": scope}, &out)
	rq := out.Request
	msg := "Denied “" + rq.Title + "” for " + rq.KidName + "."
	if action == "approve" {
		msg = "Allowed “" + rq.Title + "” for " + rq.KidName + "."
		if scope == "channel" {
			msg = "Allowed channel " + rq.ChannelName + " for " + rq.KidName + "."
		}
	}
	if err == nil {
		s.St.Audit("console", r.IP, "request "+action, rq.KidName+" on "+p.Name+": "+rq.Title)
	}
	back(w, r, "/", msg, err)
}

func (s *Server) findingDismiss(w http.ResponseWriter, r *req) {
	p, err := s.pcFrom(r)
	if err == nil {
		ctx, cancel := s.actx(r)
		defer cancel()
		err = s.call(ctx, p, "POST", "/api/v1/findings/dismiss", map[string]string{"key": r.FormValue("key")}, nil)
	}
	back(w, r, "/", "Dismissed. It won't be shown again.", err)
}

// ---- history ----

type watch struct {
	VideoID     string `json:"videoId"`
	Title       string `json:"title"`
	ChannelID   string `json:"channelId"`
	ChannelName string `json:"channelName"`
	FirstSeen   int64  `json:"firstSeen"`
	Seconds     int    `json:"seconds"`
}

type attempt struct {
	TS          int64  `json:"ts"`
	Kind        string `json:"kind"`
	VideoID     string `json:"videoId"`
	Title       string `json:"title"`
	ChannelID   string `json:"channelId"`
	ChannelName string `json:"channelName"`
	Reason      string `json:"reason"`
}

type history struct {
	Day          string    `json:"day"`
	UsedMinutes  int       `json:"used_minutes"`
	LimitMinutes int       `json:"limit_minutes"`
	Watches      []watch   `json:"watches"`
	Events       []attempt `json:"events"`
}

func (s *Server) snapshot(ctx context.Context) Snapshot {
	if snap, ok := s.cached(); ok {
		return snap
	}
	return s.fetch(ctx)
}

func (s *Server) historyPage(w http.ResponseWriter, r *req) {
	snap := s.snapshot(r.Context())
	data := map[string]any{"Kids": snap.Kids}
	if len(snap.Kids) == 0 {
		s.page(w, r, "history", data)
		return
	}
	// "pc:kid" picks the kid; default is the first one.
	cur := snap.Kids[0]
	sel := r.FormValue("who")
	for _, k := range snap.Kids {
		if sel == fmt.Sprintf("%s:%d", k.PC.ID, k.ID) {
			cur = k
		}
	}
	day := r.FormValue("day")
	if _, err := time.Parse("2006-01-02", day); err != nil {
		day = timekeeper.Day(time.Now())
	}
	t, _ := time.ParseInLocation("2006-01-02", day, time.Local)
	ctx, cancel := s.actx(r)
	defer cancel()
	var h history
	err := s.call(ctx, cur.PC, "GET", "/api/v1/history?kid="+strconv.FormatInt(cur.ID, 10)+"&day="+url.QueryEscape(day), nil, &h)
	if err != nil {
		data["Error"] = err.Error()
	}
	var names []string
	seen := map[string]bool{}
	for _, k := range snap.Kids {
		if !seen[k.Name] {
			seen[k.Name] = true
			names = append(names, k.Name)
		}
	}
	data["Cur"], data["Who"], data["Day"], data["H"], data["KidNames"] = cur, fmt.Sprintf("%s:%d", cur.PC.ID, cur.ID), day, h, names
	data["Prev"], data["Next"] = timekeeper.Day(t.AddDate(0, 0, -1)), timekeeper.Day(t.AddDate(0, 0, 1))
	data["Today"] = day == timekeeper.Day(time.Now())
	s.page(w, r, "history", data)
}

// ---- settings ----

func (s *Server) settingsPage(w http.ResponseWriter, r *req) {
	sessions, _ := s.St.Sessions(time.Now().Unix())
	audit, _ := s.St.AuditLog(100)
	user, _, _ := s.St.Admin()
	s.page(w, r, "settings", map[string]any{"Sessions": sessions, "Audit": audit, "User": user, "Current": r.Session.ID,
		"MinPw": auth.MinPasswordLen, "Phone": s.phoneStatus()})
}

func (s *Server) settingsName(w http.ResponseWriter, r *req) {
	name := truncate(strings.TrimSpace(r.FormValue("name")), 40)
	if name == "" {
		back(w, r, "/settings", "", errors.New("enter a name"))
		return
	}
	st, _ := s.St.Settings()
	st.PCName = name
	back(w, r, "/settings", "Saved.", s.St.SaveSettings(st))
}

func (s *Server) settingsPassword(w http.ResponseWriter, r *req) {
	user, _, _ := s.St.Admin()
	if err := s.Auth.Login(user, r.FormValue("current"), r.IP); err != nil {
		back(w, r, "/settings", "", errors.New("current password is wrong"))
		return
	}
	if r.FormValue("new") != r.FormValue("confirm") {
		back(w, r, "/settings", "", errors.New("new passwords don't match"))
		return
	}
	newUser := strings.TrimSpace(r.FormValue("username"))
	if newUser == "" {
		newUser = user
	}
	if err := s.Auth.SetPassword(newUser, r.FormValue("new")); err != nil {
		back(w, r, "/settings", "", err)
		return
	}
	s.St.Audit("console", r.IP, "password changed", "all devices signed out")
	s.setCookie(w, r.Request, "", time.Time{}, false)
	http.Redirect(w, r.Request, "/login", http.StatusSeeOther)
}

func (s *Server) sessionRevoke(w http.ResponseWriter, r *req) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	err := s.St.DeleteSession(id)
	if id == r.Session.ID {
		s.setCookie(w, r.Request, "", time.Time{}, false)
		http.Redirect(w, r.Request, "/login", http.StatusSeeOther)
		return
	}
	back(w, r, "/settings", "Device signed out.", err)
}
