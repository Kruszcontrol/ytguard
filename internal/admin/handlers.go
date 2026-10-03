package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ytguard/internal/auth"
	"ytguard/internal/core"
	"ytguard/internal/filterlist"
	"ytguard/internal/notify"
	"ytguard/internal/report"
	"ytguard/internal/rules"
	"ytguard/internal/store"
	"ytguard/internal/timekeeper"
	"ytguard/internal/ytmeta"
)

// ---- dashboard ----

func (s *Server) dashboard(w http.ResponseWriter, r *req) {
	states, err := s.App.KidStates()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	pending, _ := s.App.St.Requests(store.RequestPending, 100)
	st, _ := s.App.St.Settings()
	var warnings []string
	if len(states) == 0 {
		warnings = append(warnings, "No kids set up yet. Add one under Kids.")
	}
	if !st.ReportEmail && !st.ReportHA {
		warnings = append(warnings, "Daily reports are off. Turn on email or Home Assistant under Settings.")
	}
	if st.PublicURL == "" {
		warnings = append(warnings, "Set this PC's address (Public URL) in Settings so reports and notifications can link here.")
	}
	s.page(w, r, "dashboard", map[string]any{"States": states, "Pending": pending, "Warnings": warnings, "Now": time.Now().Unix(),
		"Findings": s.App.ActiveFindings()})
}

func (s *Server) findingDismiss(w http.ResponseWriter, r *req) {
	key := r.FormValue("key")
	err := s.App.DismissFinding(key)
	if err == nil {
		s.App.St.Audit("admin", r.IP, "app finding dismissed", key)
	}
	back(w, r, "/", "Dismissed. It won't be shown again.", err)
}

func (s *Server) kidAction(w http.ResponseWriter, r *req) {
	id, err := pathID(r)
	if err != nil {
		back(w, r, "/", "", errBadID)
		return
	}
	k, err := s.App.St.Kid(id)
	if err != nil {
		back(w, r, "/", "", err)
		return
	}
	action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	var msg string
	switch action {
	case "bonus":
		m := formInt(r, "minutes", 15)
		err = s.App.AddBonus(id, m, "dashboard")
		msg = fmt.Sprintf("Gave %s %+d minutes today.", k.Name, m)
	case "lock":
		m := formInt(r, "minutes", 0)
		err = s.App.Lock(id, time.Duration(m)*time.Minute)
		msg = "Paused YouTube for " + k.Name + "."
	case "unlock":
		err = s.App.Unlock(id, false)
		msg = "Unpaused " + k.Name + "."
	case "endbreak":
		err = s.App.Unlock(id, true)
		msg = "Ended " + k.Name + "'s break."
	case "reset":
		err = s.App.ResetToday(id)
		msg = "Reset " + k.Name + "'s time for today."
	}
	if err == nil {
		s.App.St.Audit("admin", r.IP, "kid "+action, k.Name+" "+r.FormValue("minutes"))
		s.changed()
	}
	back(w, r, "/", msg, err)
}

func (s *Server) requestDecide(w http.ResponseWriter, r *req) {
	id, err := pathID(r)
	if err != nil {
		back(w, r, "/", "", errBadID)
		return
	}
	var rq store.Request
	var msg string
	if strings.HasSuffix(r.URL.Path, "/approve") {
		rq, err = s.App.Approve(id, r.FormValue("scope"))
		msg = "Approved “" + rq.Title + "”."
		if r.FormValue("scope") == core.ApproveChannel {
			msg = "Approved channel " + rq.ChannelName + "."
		}
	} else {
		rq, err = s.App.Deny(id)
		msg = "Denied “" + rq.Title + "”."
	}
	if err == nil {
		s.App.St.Audit("admin", r.IP, "request", msg)
		s.changed()
	}
	back(w, r, "/", msg, err)
}

// ---- kids ----

func (s *Server) kidsPage(w http.ResponseWriter, r *req) {
	kids, _ := s.App.St.Kids()
	used := map[string]bool{}
	for _, k := range kids {
		used[k.LinuxUser] = true
	}
	var free []string
	for _, u := range LinuxUsers() {
		if !used[u] {
			free = append(free, u)
		}
	}
	s.page(w, r, "kids", map[string]any{"FreeUsers": free})
}

func (s *Server) kidCreate(w http.ResponseWriter, r *req) {
	k := store.Kid{LinuxUser: strings.TrimSpace(r.FormValue("linux_user")), Name: strings.TrimSpace(r.FormValue("name"))}
	if k.LinuxUser == "" || k.Name == "" {
		back(w, r, "/kids", "", errors.New("pick a Linux user and enter a name"))
		return
	}
	if err := s.App.St.SaveKid(&k); err != nil {
		back(w, r, "/kids", "", err)
		return
	}
	s.App.St.Audit("admin", r.IP, "kid added", k.Name+" ("+k.LinuxUser+")")
	http.Redirect(w, r.Request, fmt.Sprintf("/kids/%d?msg=%s", k.ID, "Added+"+k.Name+".+Review+the+schedule+below."), http.StatusSeeOther)
}

type schedRow struct {
	Weekday int
	Sched   timekeeper.Schedule
	Windows string
}

func (s *Server) kidPage(w http.ResponseWriter, r *req) {
	id, _ := pathID(r)
	k, err := s.App.St.Kid(id)
	if err != nil {
		http.NotFound(w, r.Request)
		return
	}
	scheds, _ := s.App.St.Schedules(id)
	var rows []schedRow
	for _, wd := range []int{1, 2, 3, 4, 5, 6, 0} {
		rows = append(rows, schedRow{wd, scheds[wd], timekeeper.FormatWindows(scheds[wd].Windows)})
	}
	s.page(w, r, "kid", map[string]any{"Kid": k, "Rows": rows})
}

func (s *Server) kidSave(w http.ResponseWriter, r *req) {
	id, _ := pathID(r)
	k, err := s.App.St.Kid(id)
	if err != nil {
		http.NotFound(w, r.Request)
		return
	}
	if n := strings.TrimSpace(r.FormValue("name")); n != "" {
		k.Name = n
	}
	k.HideDefault = pickList(r.FormValue("hide_default"))
	k.BlockDefault = pickList(r.FormValue("block_default"))
	k.Options = store.KidOptions{
		HideComments:    r.FormValue("hide_comments") == "on",
		Shorts:          r.FormValue("shorts"),
		DisableAutoplay: r.FormValue("disable_autoplay") == "on",
	}
	for wd := 0; wd < 7; wd++ {
		p := strconv.Itoa(wd) + "_"
		sch := timekeeper.Schedule{DailyMinutes: formInt(r, p+"daily", -1), BreakAfter: formInt(r, p+"break_after", 0), BreakLen: formInt(r, p+"break_len", 0)}
		if sch.Windows, err = timekeeper.ParseWindows(r.FormValue(p + "windows")); err != nil {
			back(w, r, "/kids", "", fmt.Errorf("%s: %w", time.Weekday(wd), err))
			return
		}
		if (sch.BreakAfter > 0) != (sch.BreakLen > 0) {
			back(w, r, "/kids", "", fmt.Errorf("%s: set both 'break every' and 'break length', or neither", time.Weekday(wd)))
			return
		}
		if err := s.App.St.SaveSchedule(id, wd, sch); err != nil {
			back(w, r, "/kids", "", err)
			return
		}
	}
	if err := s.App.St.SaveKid(&k); err != nil {
		back(w, r, "/kids", "", err)
		return
	}
	s.App.St.Audit("admin", r.IP, "kid settings", k.Name)
	back(w, r, "/kids", "Saved.", nil)
}

func (s *Server) kidDelete(w http.ResponseWriter, r *req) {
	id, _ := pathID(r)
	k, err := s.App.St.Kid(id)
	if err == nil {
		err = s.App.St.DeleteKid(id)
	}
	if err != nil {
		back(w, r, "/kids", "", err)
		return
	}
	s.App.St.Audit("admin", r.IP, "kid deleted", k.Name)
	http.Redirect(w, r.Request, "/kids?msg=Removed+"+k.Name, http.StatusSeeOther)
}

func pickList(v string) string {
	if v == rules.ListDeny {
		return rules.ListDeny
	}
	return rules.ListAllow
}

// ---- filters ----

type filterSection struct {
	Type  string
	Title string
	Help  string
	Lists map[string][]rules.Rule // "allow", "deny"
}

var sectionInfo = []struct{ Type, Title, Help string }{
	{rules.TypeVideo, "Videos", "Paste a video link. Most specific: beats channel and keyword rules."},
	{rules.TypeChannel, "Channels", "Paste a channel link or @handle. Beats keyword, category and attribute rules."},
	{rules.TypeKeyword, "Keywords", "Matches title, description, tags or channel name. Description/tags are checked when a video is opened (or in feeds too with a YouTube API key)."},
	{rules.TypeCategory, "Categories", "YouTube's category for the video (checked when the video is opened, or in feeds with an API key)."},
	{rules.TypeAttribute, "Attributes", "Shorts, live streams, long videos and YouTube's own kid-safety flags."},
}

func scopeFilter(v string) int64 {
	switch v {
	case "", "any":
		return -1
	case "all":
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func (s *Server) filtersPage(w http.ResponseWriter, r *req) {
	tier := r.URL.Query().Get("tier")
	if tier != rules.TierBlock {
		tier = rules.TierHide
	}
	scope := r.URL.Query().Get("scope")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	all, err := s.App.St.Rules(store.RuleFilter{Tier: tier, KidID: scopeFilter(scope), Search: q})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	var secs []filterSection
	for _, si := range sectionInfo {
		sec := filterSection{Type: si.Type, Title: si.Title, Help: si.Help, Lists: map[string][]rules.Rule{}}
		for _, ru := range all {
			if ru.Type == si.Type {
				sec.Lists[ru.List] = append(sec.Lists[ru.List], ru)
			}
		}
		secs = append(secs, sec)
	}
	st, _ := s.App.St.Settings()
	ls, _ := s.App.St.Lists()
	listRules := 0
	for _, l := range ls {
		if l.Enabled {
			listRules += l.RuleCount
		}
	}
	s.page(w, r, "filters", map[string]any{"ListCount": len(ls), "ListRules": listRules,
		"Tier": tier, "Other": map[string]string{rules.TierHide: rules.TierBlock, rules.TierBlock: rules.TierHide}[tier],
		"Scope": scope, "Q": q, "Sections": secs, "Categories": ytmeta.CategoryNames(), "Fields": rules.Fields,
		"HasAPIKey": st.YouTubeAPIKey != "",
	})
}

func (s *Server) filterAdd(w http.ResponseWriter, r *req) {
	ru := rules.Rule{
		Tier: r.FormValue("tier"), List: r.FormValue("list"), Type: r.FormValue("type"),
		Note: truncate(strings.TrimSpace(r.FormValue("note")), 200),
	}
	if ru.Tier != rules.TierBlock {
		ru.Tier = rules.TierHide
	}
	ru.List = pickList(ru.List)
	if sc := r.FormValue("kid"); sc != "" && sc != "all" {
		ru.KidID, _ = strconv.ParseInt(sc, 10, 64)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	value := strings.TrimSpace(r.FormValue("value"))
	var err error
	switch ru.Type {
	case rules.TypeVideo, rules.TypeChannel:
		err = s.resolveRef(ctx, &ru, value)
	case rules.TypeKeyword:
		ru.Value, ru.Match = value, r.FormValue("match")
		if ru.Match != rules.MatchRegex && ru.Match != rules.MatchSubstring {
			ru.Match = rules.MatchWord
		}
		for _, f := range rules.Fields {
			if r.FormValue("field_"+f) == "on" {
				ru.Fields = append(ru.Fields, f)
			}
		}
		if len(ru.Fields) == 0 {
			ru.Fields = []string{rules.FieldTitle}
		}
		err = rules.ValidateKeyword(ru.Value, ru.Match)
	case rules.TypeCategory:
		ru.Value = value
		if value == "" {
			err = errors.New("pick a category")
		}
	case rules.TypeAttribute:
		ru.Value = value
		if value == rules.AttrLongerThan {
			n := formInt(r, "minutes", 0)
			if n <= 0 {
				err = errors.New("enter a number of minutes")
			}
			ru.Value = fmt.Sprintf("%s:%d", rules.AttrLongerThan, n)
		} else if value == "" {
			err = errors.New("pick an attribute")
		}
	default:
		err = errors.New("unknown filter type")
	}
	if err == nil {
		var created bool
		created, err = s.App.St.AddRule(&ru)
		if err == nil && !created {
			back(w, r, "/filters", "That rule already exists.", nil)
			return
		}
	}
	if err != nil {
		back(w, r, "/filters", "", err)
		return
	}
	s.App.St.Audit("admin", r.IP, "rule added", ru.Describe())
	back(w, r, "/filters", "Added: "+ru.Describe(), nil)
}

// resolveRef fills a video/channel rule from a pasted link.
func (s *Server) resolveRef(ctx context.Context, ru *rules.Rule, value string) error {
	ref, err := ytmeta.ParseRef(value)
	if err != nil {
		return err
	}
	if ru.Type == rules.TypeVideo {
		if ref.Kind != "video" {
			return errors.New("that's a channel link; add it under Channels")
		}
		ru.Value = ref.ID
		if m, ok := s.App.St.Meta(ref.ID); ok && m.Title != "" {
			ru.Label = m.Title
		} else if t, a, err := s.App.YT.OEmbed(ctx, ref.ID); err == nil {
			ru.Label = t
			if a != "" {
				ru.Note = strings.TrimSpace(ru.Note + " (" + a + ")")
			}
		}
		return nil
	}
	if ref.Kind == "video" {
		// Allow "add this video's channel".
		m, err := s.App.YT.Video(ctx, ref.ID)
		if err != nil {
			return fmt.Errorf("couldn't look up that video's channel: %w", err)
		}
		ref = ytmeta.Ref{Kind: "channel", ID: m.ChannelID}
	}
	ch, err := s.App.YT.ResolveChannel(ctx, ref)
	if err != nil {
		if ref.Kind == "channel" {
			ru.Value = ref.ID // keep the ID even if we couldn't get a name
			return nil
		}
		return fmt.Errorf("couldn't find channel %s: %w", ref.ID, err)
	}
	ru.Value, ru.Label, ru.Extra = ch.ID, ch.Name, ch.Handle
	return nil
}

// filterQuick adds a rule from the history/tester pages.
func (s *Server) filterQuick(w http.ResponseWriter, r *req) {
	ru := rules.Rule{Tier: r.FormValue("tier"), List: pickList(r.FormValue("list")), Type: r.FormValue("type"),
		Value: r.FormValue("value"), Label: r.FormValue("label"), Extra: r.FormValue("extra"), Note: "added from " + r.FormValue("from")}
	if ru.Tier != rules.TierBlock {
		ru.Tier = rules.TierHide
	}
	if ru.Type != rules.TypeVideo && ru.Type != rules.TypeChannel || ru.Value == "" {
		back(w, r, "/history", "", errors.New("bad quick action"))
		return
	}
	if sc := r.FormValue("kid"); sc != "" && sc != "all" {
		ru.KidID, _ = strconv.ParseInt(sc, 10, 64)
	}
	if ru.Type == rules.TypeChannel && ru.Extra == "" {
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		if ch, err := s.App.YT.ResolveChannel(ctx, ytmeta.Ref{Kind: "channel", ID: ru.Value}); err == nil {
			ru.Extra = ch.Handle
			if ru.Label == "" {
				ru.Label = ch.Name
			}
		}
		cancel()
	}
	// A kid-scoped allow from an approval/quick action replaces a same-scope deny and vice versa.
	other := rules.ListDeny
	if ru.List == rules.ListDeny {
		other = rules.ListAllow
	}
	_ = s.App.St.DeleteRulesWhere(ru.Tier, other, ru.Type, ru.Value, ru.KidID)
	if _, err := s.App.St.AddRule(&ru); err != nil {
		back(w, r, "/history", "", err)
		return
	}
	s.App.St.Audit("admin", r.IP, "rule added", ru.Describe())
	back(w, r, "/history", "Added: "+ru.Describe(), nil)
}

func (s *Server) filterMove(w http.ResponseWriter, r *req) {
	id, _ := pathID(r)
	ru, err := s.App.St.Rule(id)
	if err != nil {
		back(w, r, "/filters", "", err)
		return
	}
	tier, list, ok := strings.Cut(r.FormValue("to"), ":")
	if !ok || (tier != rules.TierHide && tier != rules.TierBlock) {
		back(w, r, "/filters", "", errors.New("bad destination"))
		return
	}
	list = pickList(list)
	if err := s.App.St.UpdateRuleTierList(id, tier, list); err != nil {
		back(w, r, "/filters", "", err)
		return
	}
	ru.Tier, ru.List = tier, list
	s.App.St.Audit("admin", r.IP, "rule moved", ru.Describe())
	back(w, r, "/filters", "Moved: "+ru.Describe(), nil)
}

func (s *Server) filterDelete(w http.ResponseWriter, r *req) {
	id, _ := pathID(r)
	ru, err := s.App.St.Rule(id)
	if err == nil {
		err = s.App.St.DeleteRule(id)
	}
	if err != nil {
		back(w, r, "/filters", "", err)
		return
	}
	s.App.St.Audit("admin", r.IP, "rule deleted", ru.Describe())
	back(w, r, "/filters", "Removed: "+ru.Describe(), nil)
}

// overviewRow is one filter target with its status in both tiers.
type overviewRow struct {
	Type, Value, Label, Extra string
	KidID                     int64
	Hide, Block               *rules.Rule
	Effect                    string
}

func (s *Server) overviewPage(w http.ResponseWriter, r *req) {
	all, err := s.App.St.Rules(store.RuleFilter{KidID: scopeFilter(r.URL.Query().Get("scope")), Search: strings.TrimSpace(r.URL.Query().Get("q"))})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	idx := map[string]*overviewRow{}
	var rows []*overviewRow
	for i := range all {
		ru := all[i]
		key := fmt.Sprint(ru.Type, "\x00", ru.Value, "\x00", ru.Match, "\x00", strings.Join(ru.Fields, ","), "\x00", ru.KidID)
		row := idx[key]
		if row == nil {
			row = &overviewRow{Type: ru.Type, Value: ru.Value, Label: ru.Label, Extra: ru.Extra, KidID: ru.KidID}
			idx[key] = row
			rows = append(rows, row)
		}
		if ru.Tier == rules.TierHide {
			row.Hide = &all[i]
		} else {
			row.Block = &all[i]
		}
	}
	for _, row := range rows {
		switch {
		case row.Hide != nil && row.Hide.List == rules.ListDeny:
			row.Effect = "Hidden"
		case row.Block != nil && row.Block.List == rules.ListDeny:
			row.Effect = "Visible, blocked"
		case row.Block != nil && row.Block.List == rules.ListAllow:
			row.Effect = "Visible, plays"
		default:
			row.Effect = "Visible; Block tier decides"
		}
	}
	s.page(w, r, "overview", map[string]any{"Rows": rows, "Scope": r.URL.Query().Get("scope"), "Q": r.URL.Query().Get("q")})
}

// ---- tester ----

type testResult struct {
	Kid      store.Kid
	Decision rules.Decision
}

func (s *Server) testerPage(w http.ResponseWriter, r *req) {
	in := strings.TrimSpace(r.URL.Query().Get("url"))
	data := map[string]any{"URL": in}
	if in != "" {
		ref, err := ytmeta.ParseRef(in)
		if err == nil && ref.Kind != "video" {
			err = errors.New("paste a video link (the tester checks one video against every rule)")
		}
		if err != nil {
			data["Error"] = err.Error()
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			kids, _ := s.App.St.Kids()
			var results []testResult
			var meta rules.Meta
			for _, k := range kids {
				d, m, err := s.App.Decide(ctx, k, rules.Meta{VideoID: ref.ID}, true)
				if err != nil {
					data["Error"] = err.Error()
					break
				}
				meta = m
				results = append(results, testResult{k, d})
			}
			if len(kids) == 0 {
				data["Error"] = "add a kid first"
			}
			if !meta.Full && meta.VideoID != "" {
				data["Partial"] = true
			}
			data["Meta"], data["Results"] = meta, results
		}
	}
	s.page(w, r, "tester", data)
}

// ---- history & reports ----

func (s *Server) historyKidDay(r *req) (store.Kid, string, error) {
	kids, _ := s.App.St.Kids()
	if len(kids) == 0 {
		return store.Kid{}, "", errors.New("no kids yet")
	}
	k := kids[0]
	if id, err := strconv.ParseInt(r.FormValue("kid"), 10, 64); err == nil {
		for _, kk := range kids {
			if kk.ID == id {
				k = kk
			}
		}
	}
	day := r.FormValue("day")
	if _, err := time.Parse("2006-01-02", day); err != nil {
		day = timekeeper.Day(time.Now())
	}
	return k, day, nil
}

func (s *Server) historyPage(w http.ResponseWriter, r *req) {
	k, day, err := s.historyKidDay(r)
	if err != nil {
		s.page(w, r, "history", map[string]any{"Error": err.Error()})
		return
	}
	rep, err := report.Build(s.App.St, k, day)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	ws, _ := s.App.St.Watches(k.ID, day)
	evs, _ := s.App.St.Events(k.ID, day)
	t, _ := time.ParseInLocation("2006-01-02", day, time.Local)
	s.page(w, r, "history", map[string]any{"Kid": k, "Day": day, "Report": rep, "Watches": ws, "Events": evs,
		"Prev": timekeeper.Day(t.AddDate(0, 0, -1)), "Next": timekeeper.Day(t.AddDate(0, 0, 1)), "Sent": s.App.St.ReportSent(k.ID, day)})
}

func (s *Server) reportPreview(w http.ResponseWriter, r *req) {
	k, day, err := s.historyKidDay(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	rep, err := report.Build(s.App.St, k, day)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	html, err := rep.HTML()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, html)
}

func (s *Server) reportSend(w http.ResponseWriter, r *req) {
	k, day, err := s.historyKidDay(r)
	if err == nil {
		var rep report.Report
		rep, err = report.Build(s.App.St, k, day)
		if err == nil {
			st, _ := s.App.St.Settings()
			if !st.ReportEmail && !(st.ReportHA && (st.HAWebhookURL != "" || st.MQTTEnabled)) {
				err = errors.New("no report delivery turned on in Settings")
			} else {
				err = report.Send(r.Context(), st, rep, s.App.MQTT)
			}
		}
	}
	back(w, r, "/history", "Report sent.", err)
}

// ---- settings ----

func (s *Server) settingsPage(w http.ResponseWriter, r *req) {
	st, _ := s.App.St.Settings()
	mqttStatus := "off"
	if s.MQTTStatus != nil {
		mqttStatus = s.MQTTStatus()
	}
	s.page(w, r, "settings", map[string]any{"S": st, "Host": r.Host, "DefaultCatalog": core.DefaultCatalogURL(),
		"MQTTStatus": mqttStatus, "PCID": s.PCID})
}

func (s *Server) settingsSave(w http.ResponseWriter, r *req) {
	st, _ := s.App.St.Settings()
	st.PCName = strings.TrimSpace(r.FormValue("pc_name"))
	st.PublicURL = strings.TrimRight(strings.TrimSpace(r.FormValue("public_url")), "/")
	st.UnmappedPolicy = r.FormValue("unmapped_policy")
	if st.UnmappedPolicy != "block" {
		st.UnmappedPolicy = "allow"
	}
	if t := strings.TrimSpace(r.FormValue("report_time")); t != "" {
		if _, err := timekeeper.ParseWindows(t + "-24:00"); err != nil {
			back(w, r, "/settings", "", fmt.Errorf("report time: %w", err))
			return
		}
		st.ReportTime = t
	}
	st.ReportEmail = r.FormValue("report_email") == "on"
	st.SMTPHost = strings.TrimSpace(r.FormValue("smtp_host"))
	st.SMTPPort = formInt(r, "smtp_port", 587)
	st.SMTPUser = strings.TrimSpace(r.FormValue("smtp_user"))
	if p := r.FormValue("smtp_pass"); p != "" {
		st.SMTPPass = p
	}
	if r.FormValue("smtp_pass_clear") == "on" {
		st.SMTPPass = ""
	}
	st.SMTPFrom = strings.TrimSpace(r.FormValue("smtp_from"))
	st.SMTPTo = strings.TrimSpace(r.FormValue("smtp_to"))
	st.SMTPTLS = r.FormValue("smtp_tls")
	st.ReportHA = r.FormValue("report_ha") == "on"
	st.HAEvents = r.FormValue("ha_events") == "on"
	st.HAInsecureTLS = r.FormValue("ha_insecure") == "on"
	if v := strings.TrimSpace(r.FormValue("ha_webhook")); v != "" || r.FormValue("ha_webhook_clear") == "on" {
		st.HAWebhookURL = v
	}
	if v := strings.TrimSpace(r.FormValue("yt_key")); v != "" {
		st.YouTubeAPIKey = v
	}
	if r.FormValue("yt_key_clear") == "on" {
		st.YouTubeAPIKey = ""
	}
	var origins []string
	for _, o := range strings.Fields(r.FormValue("embed_origins")) {
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.Trim(u.Path, "/") != "" ||
			u.RawQuery != "" || u.User != nil || strings.ContainsAny(u.Host, ";,'\" ") {
			back(w, r, "/settings", "", fmt.Errorf("embed origin %q must look like https://homeassistant.local:8123", o))
			return
		}
		origins = append(origins, u.Scheme+"://"+u.Host)
	}
	st.EmbedOrigins = strings.Join(origins, " ")
	st.SessionDays = min(max(formInt(r, "session_days", 90), 1), 400)
	st.UpdateCheck = r.FormValue("update_check") == "on"
	st.MQTTEnabled = r.FormValue("mqtt_enabled") == "on"
	st.MQTTHost = strings.TrimSpace(r.FormValue("mqtt_host"))
	st.MQTTPort = formInt(r, "mqtt_port", 1883)
	st.MQTTTLS = r.FormValue("mqtt_tls") == "on"
	st.MQTTInsecure = r.FormValue("mqtt_insecure") == "on"
	st.MQTTUser = strings.TrimSpace(r.FormValue("mqtt_user"))
	if p := r.FormValue("mqtt_pass"); p != "" {
		st.MQTTPass = p
	}
	if r.FormValue("mqtt_pass_clear") == "on" {
		st.MQTTPass = ""
	}
	for name, v := range map[string]*string{"mqtt_base": &st.MQTTBase, "mqtt_discovery": &st.MQTTDiscovery} {
		t := strings.Trim(strings.TrimSpace(r.FormValue(name)), "/")
		if t != "" && (strings.ContainsAny(t, "+#") || strings.Contains(t, "//")) {
			back(w, r, "/settings", "", fmt.Errorf("MQTT topic %q can't contain + or #", t))
			return
		}
		if t != "" {
			*v = t
		}
	}
	if st.MQTTEnabled && st.MQTTHost == "" {
		back(w, r, "/settings", "", errors.New("enter the MQTT broker's address (e.g. homeassistant.local)"))
		return
	}
	st.AppScan = r.FormValue("app_scan") == "on"
	if c := strings.TrimSpace(r.FormValue("catalog_url")); c == "" || c == core.DefaultCatalogURL() {
		st.ListCatalogURL = ""
	} else if err := filterlist.CheckURL(c); err != nil {
		back(w, r, "/settings", "", fmt.Errorf("catalog address: %w", err))
		return
	} else {
		st.ListCatalogURL = c
	}
	if err := s.App.St.SaveSettings(st); err != nil {
		back(w, r, "/settings", "", err)
		return
	}
	s.App.St.Audit("admin", r.IP, "settings saved", "")
	s.changed() // e.g. connect to MQTT right away
	back(w, r, "/settings", "Settings saved.", nil)
}

func (s *Server) testEmail(w http.ResponseWriter, r *req) {
	st, _ := s.App.St.Settings()
	err := notify.Email(st, "YTGuard test email", "<p>Email from YTGuard on <b>"+template.HTMLEscapeString(st.PCName)+"</b> works.</p>", "Email from YTGuard on "+st.PCName+" works.")
	back(w, r, "/settings", "Test email sent to "+st.SMTPTo+".", err)
}

func (s *Server) testHA(w http.ResponseWriter, r *req) {
	st, _ := s.App.St.Settings()
	payload := map[string]any{"type": "test", "pc": st.PCName, "summary": "YTGuard test from " + st.PCName}
	var sent []string
	var errs []string
	if st.HAWebhookURL != "" {
		if err := notify.HA(r.Context(), st.HAWebhookURL, st.HAInsecureTLS, payload); err != nil {
			errs = append(errs, "webhook: "+err.Error())
		} else {
			sent = append(sent, "webhook")
		}
	}
	if st.MQTTEnabled && s.App.MQTT != nil {
		if err := s.App.MQTT("test", payload); err != nil {
			errs = append(errs, "MQTT: "+err.Error())
		} else {
			sent = append(sent, "MQTT")
		}
	}
	var err error
	if len(errs) > 0 {
		err = errors.New(strings.Join(errs, "; "))
	} else if len(sent) == 0 {
		err = errors.New("set up MQTT or a webhook URL first (and save)")
	}
	back(w, r, "/settings", "Test event sent to Home Assistant ("+strings.Join(sent, " and ")+").", err)
}

func (s *Server) changed() {
	if s.OnChange != nil {
		s.OnChange()
	}
}

func (s *Server) checkUpdate(w http.ResponseWriter, r *req) {
	u := s.App.Updates.Check(r.Context())
	switch {
	case u.Error != "":
		back(w, r, "/settings", "", errors.New("update check failed: "+u.Error))
	case u.Available:
		back(w, r, "/settings", "YTGuard "+u.Latest+" is available. To install it, run on this PC: sudo ytguard upgrade", nil)
	default:
		back(w, r, "/settings", "YTGuard is up to date ("+u.Current+").", nil)
	}
}

// ---- export / import ----

type exportRule struct {
	rules.Rule
	Kid string `json:"kid,omitempty"` // kid name for kid-scoped rules
}

type exportFile struct {
	App     string       `json:"app"`
	Version int          `json:"version"`
	From    string       `json:"from"`
	Rules   []exportRule `json:"rules"`
}

func (s *Server) export(w http.ResponseWriter, r *req) {
	all, err := s.App.St.Rules(store.RuleFilter{KidID: -1})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	kids, _ := s.App.St.Kids()
	names := map[int64]string{}
	for _, k := range kids {
		names[k.ID] = k.Name
	}
	st, _ := s.App.St.Settings()
	f := exportFile{App: "ytguard", Version: 1, From: st.PCName}
	for _, ru := range all {
		er := exportRule{Rule: ru, Kid: names[ru.KidID]}
		er.ID, er.KidID = 0, 0
		f.Rules = append(f.Rules, er)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="ytguard-filters-%s-%s.json"`, safeFile(st.PCName), time.Now().Format("2006-01-02")))
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(f)
}

func safeFile(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

func (s *Server) importRules(w http.ResponseWriter, r *req) {
	file, _, err := r.FormFile("file")
	if err != nil {
		back(w, r, "/settings", "", errors.New("choose a file"))
		return
	}
	defer file.Close()
	var f exportFile
	if err := json.NewDecoder(io.LimitReader(file, 4<<20)).Decode(&f); err != nil || f.App != "ytguard" {
		back(w, r, "/settings", "", errors.New("not a YTGuard export file"))
		return
	}
	added, skipped, dup := 0, 0, 0
	for _, er := range f.Rules {
		ru := er.Rule
		if er.Kid != "" {
			k, err := s.App.St.KidByName(er.Kid)
			if err != nil {
				skipped++
				continue
			}
			ru.KidID = k.ID
		}
		if (ru.Tier != rules.TierHide && ru.Tier != rules.TierBlock) || ru.Value == "" {
			skipped++
			continue
		}
		ru.List = pickList(ru.List)
		created, err := s.App.St.AddRule(&ru)
		switch {
		case err != nil:
			skipped++
		case created:
			added++
		default:
			dup++
		}
	}
	msg := fmt.Sprintf("Imported %d rules (%d already present, %d skipped — kid-specific rules need a kid with the same name here).", added, dup, skipped)
	s.App.St.Audit("admin", r.IP, "rules imported", msg)
	back(w, r, "/settings", msg, nil)
}

// ---- security ----

func (s *Server) securityPage(w http.ResponseWriter, r *req) {
	sessions, _ := s.App.St.Sessions(time.Now().Unix())
	tokens, _ := s.App.St.Tokens()
	audit, _ := s.App.St.AuditLog(100)
	user, _, _ := s.App.St.Admin()
	s.page(w, r, "security", map[string]any{"Sessions": sessions, "Tokens": tokens, "Audit": audit, "Current": r.Session.ID,
		"User": user, "Scopes": auth.Scopes})
}

func (s *Server) passwordChange(w http.ResponseWriter, r *req) {
	user, _, _ := s.App.St.Admin()
	if err := s.Auth.Login(user, r.FormValue("current"), r.IP); err != nil {
		back(w, r, "/security", "", errors.New("current password is wrong"))
		return
	}
	if r.FormValue("new") != r.FormValue("confirm") {
		back(w, r, "/security", "", errors.New("new passwords don't match"))
		return
	}
	newUser := strings.TrimSpace(r.FormValue("username"))
	if newUser == "" {
		newUser = user
	}
	if err := s.Auth.SetPassword(newUser, r.FormValue("new")); err != nil {
		back(w, r, "/security", "", err)
		return
	}
	s.App.St.Audit("admin", r.IP, "password changed", "all devices signed out")
	s.setCookie(w, "", time.Time{}, false)
	http.Redirect(w, r.Request, "/login", http.StatusSeeOther)
}

func (s *Server) sessionRename(w http.ResponseWriter, r *req) {
	id, _ := pathID(r)
	err := s.App.St.RenameSession(id, truncate(strings.TrimSpace(r.FormValue("name")), 60))
	back(w, r, "/security", "Renamed.", err)
}

func (s *Server) sessionRevoke(w http.ResponseWriter, r *req) {
	id, _ := pathID(r)
	err := s.App.St.DeleteSession(id)
	if err == nil {
		s.App.St.Audit("admin", r.IP, "device signed out", strconv.FormatInt(id, 10))
	}
	if id == r.Session.ID {
		s.setCookie(w, "", time.Time{}, false)
		http.Redirect(w, r.Request, "/login", http.StatusSeeOther)
		return
	}
	back(w, r, "/security", "Device signed out.", err)
}

func (s *Server) sessionRevokeAll(w http.ResponseWriter, r *req) {
	err := s.App.St.DeleteAllSessions()
	if err != nil {
		back(w, r, "/security", "", err)
		return
	}
	s.App.St.Audit("admin", r.IP, "all devices signed out", "")
	s.setCookie(w, "", time.Time{}, false)
	http.Redirect(w, r.Request, "/login", http.StatusSeeOther)
}

func (s *Server) tokenCreate(w http.ResponseWriter, r *req) {
	r.ParseForm()
	tok, err := s.Auth.NewToken(r.FormValue("name"), r.Form["scope"])
	if err != nil {
		back(w, r, "/security", "", err)
		return
	}
	s.App.St.Audit("admin", r.IP, "API token created", r.FormValue("name")+" ["+strings.Join(r.Form["scope"], ",")+"]")
	st, _ := s.App.St.Settings()
	base := st.PublicURL
	if base == "" {
		base = "https://" + r.Host
	}
	s.page(w, r, "token", map[string]any{"Token": tok, "Name": r.FormValue("name"), "Base": base})
}

func (s *Server) tokenRevoke(w http.ResponseWriter, r *req) {
	id, _ := pathID(r)
	err := s.App.St.DeleteToken(id)
	if err == nil {
		s.App.St.Audit("admin", r.IP, "API token revoked", strconv.FormatInt(id, 10))
	}
	back(w, r, "/security", "Token revoked.", err)
}
