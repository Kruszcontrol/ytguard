package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ytguard/internal/auth"
	"ytguard/internal/core"
	"ytguard/internal/rules"
	"ytguard/internal/store"
)

// REST API for Home Assistant and scripts. Bearer token auth with scopes:
//
//	read    GET  /api/v1/state, /api/v1/requests
//	control POST /api/v1/kids/{kid}/bonus|lock|unlock|end-break, /api/v1/requests/{id}/approve|deny
//	admin   GET/POST /api/v1/rules, DELETE /api/v1/rules/{id}
//
// {kid} is a kid ID, name or Linux user.

type apiHandler func(w http.ResponseWriter, r *http.Request, t store.APIToken)

func (s *Server) apiRoutes(mux *http.ServeMux) {
	h := func(pattern, scope string, fn apiHandler) { mux.Handle(pattern, s.bearer(scope, fn)) }
	h("GET /api/v1/state", auth.ScopeRead, s.apiState)
	h("GET /api/v1/requests", auth.ScopeRead, s.apiRequests)
	h("POST /api/v1/kids/{kid}/{action}", auth.ScopeControl, s.apiKidAction)
	h("POST /api/v1/requests/{id}/{action}", auth.ScopeControl, s.apiRequestAction)
	h("GET /api/v1/rules", auth.ScopeAdmin, s.apiRules)
	h("POST /api/v1/rules", auth.ScopeAdmin, s.apiRuleAdd)
	h("DELETE /api/v1/rules/{id}", auth.ScopeAdmin, s.apiRuleDelete)
}

func (s *Server) bearer(scope string, fn apiHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			apiError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		ip := s.clientIP(r)
		t, err := s.Auth.Token(strings.TrimSpace(tok), ip)
		if err != nil {
			var le auth.LockedError
			if errors.As(err, &le) {
				apiError(w, http.StatusTooManyRequests, le.Error())
				return
			}
			s.App.St.Audit("api", ip, "bad API token", r.URL.Path)
			apiError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		if !t.HasScope(scope) {
			apiError(w, http.StatusForbidden, "token lacks scope "+scope)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		fn(w, r, t)
	})
}

func apiError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func apiJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// body decodes an optional JSON or form body into a string map.
func body(r *http.Request) map[string]string {
	out := map[string]string{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var raw map[string]any
		if json.NewDecoder(r.Body).Decode(&raw) == nil {
			for k, v := range raw {
				switch t := v.(type) {
				case string:
					out[k] = t
				case float64:
					out[k] = strconv.FormatFloat(t, 'f', -1, 64)
				case bool:
					out[k] = strconv.FormatBool(t)
				}
			}
		}
		return out
	}
	_ = r.ParseForm()
	for k := range r.Form {
		out[k] = r.Form.Get(k)
	}
	return out
}

type apiKid struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	State            string `json:"state"` // watching | idle | break | time_up | locked | outside_window | none_today
	Allowed          bool   `json:"allowed"`
	Message          string `json:"message,omitempty"`
	UsedMinutes      int    `json:"used_minutes"`
	LimitMinutes     int    `json:"limit_minutes"`     // -1 unlimited
	RemainingMinutes int    `json:"remaining_minutes"` // -1 unlimited
	BreakInMinutes   int    `json:"break_in_minutes"`  // -1 no breaks
	BreakUntil       string `json:"break_until,omitempty"`
	LockedUntil      string `json:"locked_until,omitempty"`
	TodayVideos      int    `json:"today_videos"`
	PendingRequests  int    `json:"pending_requests"`
	VideoTitle       string `json:"video_title,omitempty"`
	VideoChannel     string `json:"video_channel,omitempty"`
	VideoURL         string `json:"video_url,omitempty"`
}

func toAPIKid(ks core.KidState) apiKid {
	st := ks.Status
	toMin := func(sec int) int {
		if sec < 0 {
			return -1
		}
		return (sec + 59) / 60
	}
	k := apiKid{ID: ks.Kid.ID, Name: ks.Kid.Name, Allowed: st.Allowed, Message: st.Message,
		UsedMinutes: st.UsedSec / 60, LimitMinutes: toMin(st.LimitSec), RemainingMinutes: toMin(st.RemainingSec),
		BreakInMinutes: toMin(st.BreakInSec), TodayVideos: ks.TodayVideos, PendingRequests: ks.Pending}
	if st.LimitSec >= 0 {
		k.LimitMinutes = st.LimitSec / 60
	}
	if st.BreakUntil > 0 {
		k.BreakUntil = time.Unix(st.BreakUntil, 0).Format(time.RFC3339)
	}
	if st.LockedUntil > 0 {
		k.LockedUntil = time.Unix(st.LockedUntil, 0).Format(time.RFC3339)
	}
	switch {
	case st.Reason != "":
		k.State = st.Reason
	case ks.WatchingNow:
		k.State = "watching"
	default:
		k.State = "idle"
	}
	if ks.WatchingNow && ks.LastVideo != nil {
		k.VideoTitle, k.VideoChannel = ks.LastVideo.Title, ks.LastVideo.ChannelName
		k.VideoURL = "https://www.youtube.com/watch?v=" + ks.LastVideo.VideoID
	}
	return k
}

func (s *Server) apiState(w http.ResponseWriter, r *http.Request, _ store.APIToken) {
	states, err := s.App.KidStates()
	if err != nil {
		apiError(w, 500, err.Error())
		return
	}
	st, _ := s.App.St.Settings()
	out := map[string]any{"pc": st.PCName, "kids": []apiKid{}}
	var kids []apiKid
	pending := 0
	for _, ks := range states {
		kids = append(kids, toAPIKid(ks))
		pending += ks.Pending
	}
	if kids != nil {
		out["kids"] = kids
	}
	out["pending_requests"] = pending
	u := s.App.Updates.Status()
	yt := map[string]any{"version": u.Current, "latest_version": u.Latest, "update_available": u.Available,
		"release_url": u.URL, "update_check_enabled": u.Enabled}
	if !u.Checked.IsZero() {
		yt["update_checked"] = u.Checked.Format(time.RFC3339)
	}
	out["ytguard"] = yt
	findings := s.App.ActiveFindings()
	if findings == nil {
		findings = []core.FindingView{}
	}
	out["other_apps"] = len(findings)
	out["other_apps_list"] = findings
	apiJSON(w, out)
}

func (s *Server) apiRequests(w http.ResponseWriter, r *http.Request, _ store.APIToken) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = store.RequestPending
	} else if status == "all" {
		status = ""
	}
	rs, err := s.App.St.Requests(status, 100)
	if err != nil {
		apiError(w, 500, err.Error())
		return
	}
	if rs == nil {
		rs = []store.Request{}
	}
	apiJSON(w, map[string]any{"requests": rs})
}

func (s *Server) kidFromPath(r *http.Request) (store.Kid, error) {
	v := r.PathValue("kid")
	if id, err := strconv.ParseInt(v, 10, 64); err == nil {
		return s.App.St.Kid(id)
	}
	return s.App.St.KidByName(v)
}

func (s *Server) apiKidAction(w http.ResponseWriter, r *http.Request, t store.APIToken) {
	k, err := s.kidFromPath(r)
	if err != nil {
		apiError(w, 404, "kid not found")
		return
	}
	b := body(r)
	minutes, _ := strconv.Atoi(b["minutes"])
	action := r.PathValue("action")
	switch action {
	case "bonus":
		if minutes == 0 {
			minutes = 15
		}
		err = s.App.AddBonus(k.ID, minutes, "api: "+t.Name)
	case "lock":
		err = s.App.Lock(k.ID, time.Duration(minutes)*time.Minute)
	case "unlock":
		err = s.App.Unlock(k.ID, false)
	case "end-break":
		err = s.App.Unlock(k.ID, true)
	default:
		apiError(w, 404, "unknown action")
		return
	}
	if err != nil {
		apiError(w, 500, err.Error())
		return
	}
	s.App.St.Audit("api:"+t.Name, s.clientIP(r), "kid "+action, k.Name+" "+b["minutes"])
	st, _ := s.App.Status(k.ID)
	apiJSON(w, map[string]any{"ok": true, "kid": k.Name, "status": st})
}

func (s *Server) apiRequestAction(w http.ResponseWriter, r *http.Request, t store.APIToken) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		apiError(w, 400, "bad id")
		return
	}
	var rq store.Request
	switch r.PathValue("action") {
	case "approve":
		rq, err = s.App.Approve(id, body(r)["scope"])
	case "deny":
		rq, err = s.App.Deny(id)
	default:
		apiError(w, 404, "unknown action")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, 404, "request not found")
		return
	}
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	s.App.St.Audit("api:"+t.Name, s.clientIP(r), "request "+r.PathValue("action"), rq.Title)
	rq, _ = s.App.St.Request(id)
	apiJSON(w, map[string]any{"ok": true, "request": rq})
}

func (s *Server) apiRules(w http.ResponseWriter, r *http.Request, _ store.APIToken) {
	q := r.URL.Query()
	f := store.RuleFilter{Tier: q.Get("tier"), List: q.Get("list"), Type: q.Get("type"), KidID: scopeFilter(q.Get("scope")), Search: q.Get("q")}
	rs, err := s.App.St.Rules(f)
	if err != nil {
		apiError(w, 500, err.Error())
		return
	}
	if rs == nil {
		rs = []rules.Rule{}
	}
	apiJSON(w, map[string]any{"rules": rs})
}

func (s *Server) apiRuleAdd(w http.ResponseWriter, r *http.Request, t store.APIToken) {
	var ru rules.Rule
	if err := json.NewDecoder(r.Body).Decode(&ru); err != nil {
		apiError(w, 400, "body must be a JSON rule")
		return
	}
	ru.ID = 0
	ru.List = pickList(ru.List)
	if ru.Tier != rules.TierHide && ru.Tier != rules.TierBlock {
		apiError(w, 400, "tier must be hide or block")
		return
	}
	valid := false
	for _, l := range rules.Levels {
		valid = valid || l == ru.Type
	}
	if !valid || strings.TrimSpace(ru.Value) == "" {
		apiError(w, 400, "type must be video|channel|keyword|category|attribute and value non-empty")
		return
	}
	if ru.Type == rules.TypeKeyword {
		if err := rules.ValidateKeyword(ru.Value, ru.Match); err != nil {
			apiError(w, 400, err.Error())
			return
		}
	}
	created, err := s.App.St.AddRule(&ru)
	if err != nil {
		apiError(w, 500, err.Error())
		return
	}
	s.App.St.Audit("api:"+t.Name, s.clientIP(r), "rule added", ru.Describe())
	apiJSON(w, map[string]any{"ok": true, "created": created, "rule": ru})
}

func (s *Server) apiRuleDelete(w http.ResponseWriter, r *http.Request, t store.APIToken) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		apiError(w, 400, "bad id")
		return
	}
	ru, err := s.App.St.Rule(id)
	if err != nil {
		apiError(w, 404, "rule not found")
		return
	}
	if err := s.App.St.DeleteRule(id); err != nil {
		apiError(w, 500, err.Error())
		return
	}
	s.App.St.Audit("api:"+t.Name, s.clientIP(r), "rule deleted", ru.Describe())
	apiJSON(w, map[string]any{"ok": true})
}
