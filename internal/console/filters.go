package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"ytguard/internal/core"
	"ytguard/internal/filterlist"
	"ytguard/internal/rules"
	"ytguard/internal/store"
	"ytguard/internal/ytmeta"
)

// The parent's own filter rules and list subscriptions on every PC, shown
// side by side, so they can be kept the same everywhere. Kid-specific rules
// are matched across PCs by the kid's name.

// pcFilters is what one PC has.
type pcFilters struct {
	PC     PC
	Err    string
	Kids   map[int64]string // ID → name
	ByName map[string]int64 // lower-case name → ID
	Rules  []rules.Rule
	Lists  []store.FilterList
}

func (f pcFilters) online() bool { return f.Err == "" }

// loadFilters fetches kids plus rules and/or lists from every PC.
func (s *Server) loadFilters(ctx context.Context, wantRules, wantLists bool) []pcFilters {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return forEach(s.pcs(), func(p PC) pcFilters {
		f := pcFilters{PC: p, Kids: map[int64]string{}, ByName: map[string]int64{}}
		var st pcState
		err := s.call(ctx, p, "GET", "/api/v1/state", nil, &st)
		if err == nil && wantRules {
			var out struct{ Rules []rules.Rule }
			err = s.call(ctx, p, "GET", "/api/v1/rules", nil, &out)
			f.Rules = out.Rules
		}
		if err == nil && wantLists {
			var out struct{ Lists []store.FilterList }
			err = s.call(ctx, p, "GET", "/api/v1/lists", nil, &out)
			f.Lists = out.Lists
		}
		if err != nil {
			f.Err = err.Error()
		}
		for _, k := range st.Kids {
			f.Kids[k.ID] = k.Name
			f.ByName[strings.ToLower(k.Name)] = k.ID
		}
		return f
	})
}

// ---- rules ----

// ruleKey identifies "the same rule" on different PCs.
func ruleKey(r rules.Rule, kidName string) string {
	v, match, fields := r.Value, "", ""
	switch r.Type {
	case rules.TypeKeyword:
		v = strings.ToLower(v)
		match = r.Match
		if match == "" {
			match = rules.MatchWord
		}
		fs := append([]string(nil), r.Fields...)
		if len(fs) == 0 {
			fs = []string{rules.FieldTitle}
		}
		sort.Strings(fs)
		fields = strings.Join(fs, ",")
	case rules.TypeCategory:
		v = strings.ToLower(v)
	}
	return strings.Join([]string{r.Tier, r.List, r.Type, v, match, fields, strings.ToLower(kidName)}, "\x1f")
}

type ruleCell struct {
	State string // yes | no | offline | nokid
	ID    int64
}

type ruleRow struct {
	Key     string
	Rule    rules.Rule
	Kid     string // "" = all kids
	Cells   []ruleCell
	Have    int
	Missing int // online PCs that could get it
}

// What is shown in the table, with the kid scope in words.
func (r ruleRow) What() string {
	d := r.Rule.Describe()
	if i := strings.LastIndex(d, " ("); i > 0 {
		d = d[:i]
	}
	if i := strings.Index(d, ": "); i > 0 {
		d = d[i+2:]
	}
	return d
}

func ruleRows(pfs []pcFilters) []ruleRow {
	byKey := map[string]*ruleRow{}
	var order []string
	for i, f := range pfs {
		for _, r := range f.Rules {
			kid := ""
			if r.KidID != 0 {
				kid = f.Kids[r.KidID]
				if kid == "" {
					continue
				}
			}
			k := ruleKey(r, kid)
			row := byKey[k]
			if row == nil {
				rep := r
				rep.ID, rep.KidID, rep.Source = 0, 0, 0
				row = &ruleRow{Key: k, Rule: rep, Kid: kid, Cells: make([]ruleCell, len(pfs))}
				byKey[k] = row
				order = append(order, k)
			}
			if row.Rule.Label == "" {
				row.Rule.Label = r.Label
			}
			row.Cells[i] = ruleCell{State: "yes", ID: r.ID}
		}
	}
	out := make([]ruleRow, 0, len(order))
	for _, k := range order {
		row := *byKey[k]
		for i, f := range pfs {
			c := &row.Cells[i]
			switch {
			case c.State == "yes":
				row.Have++
			case !f.online():
				c.State = "offline"
			case row.Kid != "" && f.ByName[strings.ToLower(row.Kid)] == 0:
				c.State = "nokid"
			default:
				c.State = "no"
				row.Missing++
			}
		}
		out = append(out, row)
	}
	tierOrder := map[string]int{rules.TierHide: 0, rules.TierBlock: 1}
	typeOrder := map[string]int{}
	for i, t := range rules.Levels {
		typeOrder[t] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Rule, out[j].Rule
		if a.Tier != b.Tier {
			return tierOrder[a.Tier] < tierOrder[b.Tier]
		}
		if a.Type != b.Type {
			return typeOrder[a.Type] < typeOrder[b.Type]
		}
		if a.List != b.List {
			return a.List < b.List
		}
		return strings.ToLower(firstNonEmpty(a.Label, a.Value)) < strings.ToLower(firstNonEmpty(b.Label, b.Value))
	})
	return out
}

func (s *Server) filtersPage(w http.ResponseWriter, r *req) {
	pfs := s.loadFilters(r.Context(), true, false)
	q := r.URL.Query()
	tier, typ, search, diff := q.Get("tier"), q.Get("type"), strings.ToLower(strings.TrimSpace(q.Get("q"))), q.Get("diff") == "1"
	all := ruleRows(pfs)
	var rows []ruleRow
	differ := 0
	for _, row := range all {
		if row.Missing > 0 {
			differ++
		}
		if (tier != "" && row.Rule.Tier != tier) || (typ != "" && row.Rule.Type != typ) || (diff && row.Missing == 0) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(row.Rule.Value+" "+row.Rule.Label+" "+row.Rule.Extra+" "+row.Kid), search) {
			continue
		}
		rows = append(rows, row)
	}
	s.page(w, r, "filters", map[string]any{"PCs": pfs, "Rows": rows, "Total": len(all), "Differ": differ,
		"Tier": tier, "Type": typ, "Q": q.Get("q"), "Diff": diff, "KidNames": kidNames(pfs),
		"Categories": ytmeta.CategoryNames(), "Fields": rules.Fields, "Levels": rules.Levels})
}

func kidNames(pfs []pcFilters) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range pfs {
		for _, n := range f.Kids {
			if !seen[strings.ToLower(n)] {
				seen[strings.ToLower(n)] = true
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// result collects what happened on each PC for one message.
type result struct {
	mu   sync.Mutex
	done []string
	same []string
	off  []string
	skip []string
	errs []string
}

func (res *result) add(list *[]string, s string) {
	res.mu.Lock()
	defer res.mu.Unlock()
	*list = append(*list, s)
}

func (res *result) message(verb string) (string, error) {
	var parts []string
	if len(res.done) > 0 {
		parts = append(parts, verb+" on "+strings.Join(res.done, ", ")+".")
	}
	if len(res.same) > 0 {
		parts = append(parts, "Already there on "+strings.Join(res.same, ", ")+".")
	}
	if len(res.off) > 0 {
		parts = append(parts, "Turned off on "+strings.Join(res.off, "; ")+".")
	}
	if len(res.skip) > 0 {
		parts = append(parts, "Skipped "+strings.Join(res.skip, "; ")+".")
	}
	msg := strings.Join(parts, " ")
	if len(res.errs) > 0 {
		return "", errors.New(strings.TrimSpace(msg + " Failed: " + strings.Join(res.errs, "; ")))
	}
	if msg == "" {
		msg = "Nothing to do."
	}
	return msg, nil
}

// addRule adds r (with the kid named kid, or all kids) to the given PCs.
func (s *Server) addRule(ctx context.Context, pfs []pcFilters, r rules.Rule, kid string, onlyPCs map[string]bool) *result {
	res := &result{}
	forEach(pfs, func(f pcFilters) struct{} {
		if onlyPCs != nil && !onlyPCs[f.PC.ID] {
			return struct{}{}
		}
		if !f.online() {
			res.add(&res.skip, f.PC.Name+" (offline)")
			return struct{}{}
		}
		ru := r
		ru.ID, ru.KidID, ru.Source = 0, 0, 0
		if kid != "" {
			id := f.ByName[strings.ToLower(kid)]
			if id == 0 {
				res.add(&res.skip, f.PC.Name+" (no kid named "+kid+")")
				return struct{}{}
			}
			ru.KidID = id
		}
		var out struct{ Created bool }
		if err := s.call(ctx, f.PC, "POST", "/api/v1/rules", ru, &out); err != nil {
			res.add(&res.errs, f.PC.Name+": "+err.Error())
		} else if out.Created {
			res.add(&res.done, f.PC.Name)
		} else {
			res.add(&res.same, f.PC.Name)
		}
		return struct{}{}
	})
	return res
}

func (s *Server) filterAdd(w http.ResponseWriter, r *req) {
	_ = r.ParseForm()
	ru := rules.Rule{Tier: r.FormValue("tier"), List: r.FormValue("list"), Type: r.FormValue("type"),
		Value: strings.TrimSpace(r.FormValue("value")), Label: truncate(r.FormValue("label"), 200),
		Note: truncate(strings.TrimSpace(firstNonEmpty(r.FormValue("note"), "added from the console")), 200)}
	if ru.Tier != rules.TierBlock {
		ru.Tier = rules.TierHide
	}
	if ru.List != rules.ListAllow {
		ru.List = rules.ListDeny
	}
	var err error
	switch ru.Type {
	case rules.TypeVideo, rules.TypeChannel, rules.TypeCategory:
	case rules.TypeKeyword:
		ru.Match = r.FormValue("match")
		for _, f := range rules.Fields {
			if r.FormValue("field_"+f) == "on" {
				ru.Fields = append(ru.Fields, f)
			}
		}
		err = rules.ValidateKeyword(ru.Value, firstNonEmpty(ru.Match, rules.MatchWord))
	case rules.TypeAttribute:
		if ru.Value == rules.AttrLongerThan {
			var n int
			if _, e := fmt.Sscan(r.FormValue("minutes"), &n); e != nil || n <= 0 {
				err = errors.New("enter a number of minutes")
			}
			ru.Value = fmt.Sprintf("%s:%d", rules.AttrLongerThan, n)
		}
	default:
		err = errors.New("unknown filter type")
	}
	if err == nil && ru.Value == "" {
		err = errors.New("enter what to filter")
	}
	if err != nil {
		back(w, r, "/filters", "", err)
		return
	}
	kid := r.FormValue("kid")
	if kid == "all" {
		kid = ""
	}
	var only map[string]bool
	if r.FormValue("pcs") != "all" {
		only = map[string]bool{}
		for _, id := range r.Form["pc"] {
			only[id] = true
		}
		if len(only) == 0 {
			back(w, r, "/filters", "", errors.New("pick at least one PC"))
			return
		}
	}
	ctx, cancel := s.actx(r)
	defer cancel()
	pfs := s.loadFilters(ctx, false, false)
	res := s.addRule(ctx, pfs, ru, kid, only)
	msg, err := res.message("Added")
	if len(res.done) > 0 {
		s.St.Audit("console", r.IP, "rule added", ru.Describe()+" on "+strings.Join(res.done, ", "))
	}
	back(w, r, "/filters", msg, err)
}

// findRow re-reads every PC and finds the row with the given key.
func (s *Server) findRow(ctx context.Context, key string) ([]pcFilters, ruleRow, bool) {
	pfs := s.loadFilters(ctx, true, false)
	for _, row := range ruleRows(pfs) {
		if row.Key == key {
			return pfs, row, true
		}
	}
	return pfs, ruleRow{}, false
}

// filterSync copies a rule to the PCs that don't have it yet.
func (s *Server) filterSync(w http.ResponseWriter, r *req) {
	ctx, cancel := s.actx(r)
	defer cancel()
	pfs, row, ok := s.findRow(ctx, r.FormValue("key"))
	if !ok {
		back(w, r, "/filters", "", errors.New("that rule is gone; the page was out of date"))
		return
	}
	missing := map[string]bool{}
	for i, c := range row.Cells {
		if c.State == "no" {
			missing[pfs[i].PC.ID] = true
		}
	}
	res := s.addRule(ctx, pfs, row.Rule, row.Kid, missing)
	msg, err := res.message("Copied")
	if len(res.done) > 0 {
		s.St.Audit("console", r.IP, "rule copied", row.Rule.Describe()+" to "+strings.Join(res.done, ", "))
	}
	back(w, r, "/filters", msg, err)
}

// filterRemove deletes a rule from every PC that has it (or one PC).
func (s *Server) filterRemove(w http.ResponseWriter, r *req) {
	ctx, cancel := s.actx(r)
	defer cancel()
	pfs, row, ok := s.findRow(ctx, r.FormValue("key"))
	if !ok {
		back(w, r, "/filters", "", errors.New("that rule is gone; the page was out of date"))
		return
	}
	only := r.FormValue("pc")
	res := &result{}
	var targets []pcFilters
	ids := map[string]int64{}
	for i, c := range row.Cells {
		if c.State == "yes" && (only == "" || only == pfs[i].PC.ID) {
			targets = append(targets, pfs[i])
			ids[pfs[i].PC.ID] = c.ID
		}
	}
	forEach(targets, func(f pcFilters) struct{} {
		if err := s.call(ctx, f.PC, "DELETE", fmt.Sprintf("/api/v1/rules/%d", ids[f.PC.ID]), nil, nil); err != nil {
			res.add(&res.errs, f.PC.Name+": "+err.Error())
		} else {
			res.add(&res.done, f.PC.Name)
		}
		return struct{}{}
	})
	msg, err := res.message("Removed")
	if len(res.done) > 0 {
		s.St.Audit("console", r.IP, "rule removed", row.Rule.Describe()+" from "+strings.Join(res.done, ", "))
	}
	back(w, r, "/filters", msg, err)
}

// ---- shared lists ----

type listCell struct {
	PC    string // PC name
	State string // on | off | no | offline
	Mode  string
	Kids  []string // kid names; empty = all kids
	Error string
	ID    int64
}

type listRow struct {
	URL, Title, Description, Ages string
	Catalog                       bool
	Cells                         []listCell
	Have, Missing                 int
	// Defaults for the form: the current mode and kids where subscribed.
	Mode    string
	AllKids bool
	Kids    []string
}

// Checked reports whether a kid's box starts ticked in the form.
func (r listRow) Checked(name string) bool {
	for _, k := range r.Kids {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

var (
	catMu    sync.Mutex
	catCache filterlist.Catalog
	catAt    time.Time
	catErr   error
)

func catalog(ctx context.Context) (filterlist.Catalog, error) {
	catMu.Lock()
	defer catMu.Unlock()
	if !catAt.IsZero() && time.Since(catAt) < time.Hour {
		return catCache, catErr
	}
	u := core.DefaultCatalogURL()
	if u == "" {
		return filterlist.Catalog{}, errors.New("this build has no list catalog")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	catCache, catErr = filterlist.FetchCatalog(ctx, u)
	catAt = time.Now()
	return catCache, catErr
}

func listRows(pfs []pcFilters, cat filterlist.Catalog) []listRow {
	byURL := map[string]*listRow{}
	var order []string
	get := func(u string) *listRow {
		if row := byURL[u]; row != nil {
			return row
		}
		row := &listRow{URL: u, Cells: make([]listCell, len(pfs))}
		byURL[u] = row
		order = append(order, u)
		return row
	}
	for _, e := range cat.Lists {
		row := get(e.URL)
		row.Title, row.Description, row.Ages, row.Catalog = e.Title, e.Description, e.Ages, true
	}
	for i, f := range pfs {
		for _, l := range f.Lists {
			row := get(l.URL)
			if row.Title == "" {
				row.Title, row.Description, row.Ages = l.Title, l.Description, l.Ages
			}
			c := listCell{State: "on", Mode: l.Mode, Error: l.Error, ID: l.ID}
			for _, id := range l.Kids {
				if n := f.Kids[id]; n != "" {
					c.Kids = append(c.Kids, n)
				}
			}
			if !l.Enabled {
				c.State = "off"
			}
			row.Cells[i] = c
		}
	}
	out := make([]listRow, 0, len(order))
	for _, u := range order {
		row := *byURL[u]
		if row.Title == "" {
			row.Title = u
		}
		seen := map[string]bool{}
		row.AllKids = true
		for i, f := range pfs {
			c := &row.Cells[i]
			c.PC = f.PC.Name
			switch {
			case c.State == "on":
				if row.Have == 0 {
					row.Mode, row.AllKids = c.Mode, false
				}
				row.Have++
				if len(c.Kids) == 0 {
					row.AllKids = true
				}
				for _, k := range c.Kids {
					if !seen[strings.ToLower(k)] {
						seen[strings.ToLower(k)] = true
						row.Kids = append(row.Kids, k)
					}
				}
			case c.State == "off":
				row.Have++
			case !f.online():
				c.State = "offline"
			default:
				c.State = "no"
				row.Missing++
			}
		}
		if row.Mode == "" {
			row.Mode = store.ListModeMixed
		}
		out = append(out, row)
	}
	return out
}

func (s *Server) listsPage(w http.ResponseWriter, r *req) {
	pfs := s.loadFilters(r.Context(), false, true)
	cat, catErr := catalog(r.Context())
	data := map[string]any{"PCs": pfs, "Rows": listRows(pfs, cat), "KidNames": kidNames(pfs)}
	if catErr != nil {
		data["CatalogError"] = catErr.Error()
	}
	s.page(w, r, "lists", data)
}

// eachList runs fn on every PC subscribed to listURL (or, with missing, every
// online PC that isn't).
func (s *Server) eachList(ctx context.Context, listURL string, missing bool, fn func(PC, int64) error) *result {
	pfs := s.loadFilters(ctx, false, true)
	res := &result{}
	forEach(pfs, func(f pcFilters) struct{} {
		var id int64
		for _, l := range f.Lists {
			if l.URL == listURL {
				id = l.ID
			}
		}
		switch {
		case !f.online():
			res.add(&res.skip, f.PC.Name+" (offline)")
		case missing && id != 0:
			res.add(&res.same, f.PC.Name)
		case missing || id != 0:
			if err := fn(f.PC, id); err != nil {
				res.add(&res.errs, f.PC.Name+": "+err.Error())
			} else {
				res.add(&res.done, f.PC.Name)
			}
		}
		return struct{}{}
	})
	return res
}

// listApply makes every reachable PC use a list for the chosen kids with
// the chosen mode: PCs without it subscribe, PCs with it are updated.
// Kids are matched by name; a PC with none of them doesn't use the list
// (it's turned off there if it was subscribed).
func (s *Server) listApply(w http.ResponseWriter, r *req) {
	_ = r.ParseForm()
	u := strings.TrimSpace(r.FormValue("url"))
	if err := filterlist.CheckURL(u); err != nil {
		back(w, r, "/lists", "", err)
		return
	}
	mode := store.ValidListMode(r.FormValue("mode"))
	var names []string
	all := len(r.Form["kid"]) == 0
	for _, k := range r.Form["kid"] {
		if k == "all" {
			all = true
		}
		names = append(names, k)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	pfs := s.loadFilters(ctx, false, true)
	res := &result{}
	forEach(pfs, func(f pcFilters) struct{} {
		if !f.online() {
			res.add(&res.skip, f.PC.Name+" (offline)")
			return struct{}{}
		}
		var cur *store.FilterList
		for i := range f.Lists {
			if f.Lists[i].URL == u {
				cur = &f.Lists[i]
			}
		}
		kids := []int64{} // empty = all kids
		if !all {
			for _, n := range names {
				if id := f.ByName[strings.ToLower(n)]; id != 0 {
					kids = append(kids, id)
				}
			}
			if len(kids) == 0 {
				why := f.PC.Name + " (no kid named " + strings.Join(names, " or ") + ")"
				if cur != nil && cur.Enabled {
					err := s.call(ctx, f.PC, "POST", fmt.Sprintf("/api/v1/lists/%d", cur.ID), map[string]any{"enabled": false}, nil)
					if err != nil {
						res.add(&res.errs, f.PC.Name+": "+err.Error())
					} else {
						res.add(&res.off, why)
					}
				} else {
					res.add(&res.skip, why)
				}
				return struct{}{}
			}
		}
		var err error
		if cur == nil {
			err = s.call(ctx, f.PC, "POST", "/api/v1/lists", map[string]any{"url": u, "mode": mode, "kids": kids}, nil)
		} else {
			err = s.call(ctx, f.PC, "POST", fmt.Sprintf("/api/v1/lists/%d", cur.ID), map[string]any{"mode": mode, "kids": kids, "enabled": true}, nil)
		}
		if err != nil {
			res.add(&res.errs, f.PC.Name+": "+err.Error())
		} else {
			res.add(&res.done, f.PC.Name)
		}
		return struct{}{}
	})
	who := "all kids"
	if !all {
		who = strings.Join(names, ", ")
	}
	msg, err := res.message("Set to " + mode + " for " + who)
	if len(res.done) > 0 {
		s.St.Audit("console", r.IP, "list applied", u+" ("+mode+", "+who+") on "+strings.Join(res.done, ", "))
	}
	back(w, r, "/lists", msg, err)
}

func (s *Server) listRefresh(w http.ResponseWriter, r *req) {
	u := r.FormValue("url")
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	res := s.eachList(ctx, u, false, func(p PC, id int64) error {
		return s.call(ctx, p, "POST", fmt.Sprintf("/api/v1/lists/%d/refresh", id), nil, nil)
	})
	msg, err := res.message("Updated")
	back(w, r, "/lists", msg, err)
}

func (s *Server) listRemove(w http.ResponseWriter, r *req) {
	u := r.FormValue("url")
	ctx, cancel := s.actx(r)
	defer cancel()
	res := s.eachList(ctx, u, false, func(p PC, id int64) error {
		return s.call(ctx, p, "DELETE", fmt.Sprintf("/api/v1/lists/%d", id), nil, nil)
	})
	msg, err := res.message("Unsubscribed")
	if len(res.done) > 0 {
		s.St.Audit("console", r.IP, "list removed", u+" from "+strings.Join(res.done, ", "))
	}
	back(w, r, "/lists", msg, err)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
