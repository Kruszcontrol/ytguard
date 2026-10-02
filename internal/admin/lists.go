package admin

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ytguard/internal/core"
	"ytguard/internal/filterlist"
	"ytguard/internal/rules"
	"ytguard/internal/store"
)

type listView struct {
	store.FilterList
	AppliesTo string
}

type catalogView struct {
	filterlist.CatalogEntry
	Subscribed bool
}

func (s *Server) listsPage(w http.ResponseWriter, r *req) {
	ls, err := s.App.St.Lists()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	kids, _ := s.App.St.Kids()
	subscribed := map[string]bool{}
	var views []listView
	for _, l := range ls {
		subscribed[l.URL] = true
		views = append(views, listView{l, core.ListSummary(l, kids)})
	}
	data := map[string]any{"Lists": views, "CatalogURL": s.App.CatalogURL(), "CatalogError": ""}
	cat, err := s.App.Catalog(r.Context())
	if err != nil {
		data["CatalogError"] = err.Error()
	}
	var entries []catalogView
	for _, e := range cat.Lists {
		entries = append(entries, catalogView{e, subscribed[e.URL]})
	}
	data["Catalog"] = entries
	own, _ := s.App.St.Rules(store.RuleFilter{KidID: 0})
	data["OwnCount"] = len(own)
	s.page(w, r, "lists", data)
}

// formKids reads kid checkboxes; "all" (or none ticked) means every kid.
func formKids(r *req) []int64 {
	r.ParseForm()
	var ids []int64
	for _, v := range r.Form["kid"] {
		if v == "all" {
			return nil
		}
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Server) listSubscribe(w http.ResponseWriter, r *req) {
	l, err := s.App.Subscribe(r.Context(), r.FormValue("url"), formKids(r))
	if err != nil {
		back(w, r, "/filters/lists", "", fmt.Errorf("couldn't subscribe: %w", err))
		return
	}
	s.App.St.Audit("admin", r.IP, "list subscribed", l.Title+" "+l.URL)
	msg := fmt.Sprintf("Subscribed to “%s” (%d entries).", l.Title, l.RuleCount)
	if len(l.Warnings) > 0 {
		msg += fmt.Sprintf(" %d lines were skipped; see the list for details.", len(l.Warnings))
	}
	back(w, r, "/filters/lists", msg, nil)
}

func (s *Server) listID(w http.ResponseWriter, r *req) (store.FilterList, bool) {
	id, err := pathID(r)
	if err == nil {
		if l, err := s.App.St.List(id); err == nil {
			return l, true
		}
	}
	http.NotFound(w, r.Request)
	return store.FilterList{}, false
}

func (s *Server) listOptions(w http.ResponseWriter, r *req) {
	l, ok := s.listID(w, r)
	if !ok {
		return
	}
	kids := formKids(r)
	enabled := r.FormValue("enabled") == "on"
	if err := s.App.St.SetListOptions(l.ID, kids, enabled); err != nil {
		back(w, r, "/filters/lists", "", err)
		return
	}
	s.App.St.Audit("admin", r.IP, "list options", fmt.Sprintf("%s enabled=%v kids=%v", l.Title, enabled, kids))
	back(w, r, "/filters/lists", "Saved “"+l.Title+"”.", nil)
}

func (s *Server) listRefresh(w http.ResponseWriter, r *req) {
	l, ok := s.listID(w, r)
	if !ok {
		return
	}
	err := s.App.RefreshList(r.Context(), l.ID, true)
	if err == nil {
		l, _ = s.App.St.List(l.ID)
	}
	back(w, r, "/filters/lists", fmt.Sprintf("Updated “%s” (%d entries).", l.Title, l.RuleCount), err)
}

func (s *Server) listDelete(w http.ResponseWriter, r *req) {
	l, ok := s.listID(w, r)
	if !ok {
		return
	}
	if err := s.App.St.DeleteList(l.ID); err != nil {
		back(w, r, "/filters/lists", "", err)
		return
	}
	s.App.St.Audit("admin", r.IP, "list removed", l.Title+" "+l.URL)
	back(w, r, "/filters/lists", "Unsubscribed from “"+l.Title+"”.", nil)
}

func (s *Server) listEntries(w http.ResponseWriter, r *req) {
	l, ok := s.listID(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	rs, err := s.App.St.Rules(store.RuleFilter{KidID: -1, ListID: l.ID, Search: q})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	groups := map[string][]rules.Rule{}
	for _, ru := range rs {
		k := ru.Tier + " " + ru.List
		groups[k] = append(groups[k], ru)
	}
	kids, _ := s.App.St.Kids()
	s.page(w, r, "list", map[string]any{"L": listView{l, core.ListSummary(l, kids)}, "Groups": groups, "Q": q,
		"Order": []string{"hide deny", "hide allow", "block deny", "block allow"}, "Count": len(rs)})
}

// listExport downloads the parent's all-kids rules as a list file.
func (s *Server) listExport(w http.ResponseWriter, r *req) {
	rs, err := s.App.St.Rules(store.RuleFilter{KidID: 0})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	st, _ := s.App.St.Settings()
	title := strings.TrimSpace(r.URL.Query().Get("title"))
	if title == "" {
		title = "Filters from " + st.PCName
	}
	meta := filterlist.Meta{Title: title, Description: "Exported from YTGuard.", Version: time.Now().Format("2006-01-02")}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="ytguard-list-%s.txt"`, safeFile(st.PCName)))
	fmt.Fprint(w, filterlist.Format(meta, rs))
}

func (s *Server) apiLists(w http.ResponseWriter, r *http.Request, _ store.APIToken) {
	ls, err := s.App.St.Lists()
	if err != nil {
		apiError(w, 500, err.Error())
		return
	}
	if ls == nil {
		ls = []store.FilterList{}
	}
	apiJSON(w, map[string]any{"lists": ls})
}
