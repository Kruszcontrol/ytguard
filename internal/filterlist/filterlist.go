// Package filterlist parses and writes shareable YTGuard filter lists:
// plain-text files anyone can publish (like Pi-hole block lists) that
// parents subscribe to.
//
// Format:
//
//	! Title: Scary and horror content
//	! Description: Hides horror, creepypasta and jumpscare videos.
//	! Ages: 0-12
//	! Homepage: https://github.com/owner/ytguard-lists
//	! License: CC0-1.0
//	! Version: 2026-10-02
//
//	# comment
//	[hide deny]
//	keyword: creepypasta
//	keyword(title,description): jumpscare
//	keyword(regex): \bfnaf\b
//	channel: UCxxxxxxxxxxxxxxxxxxxxxx @handle Channel name
//	channel: @handle Channel name
//	video: dQw4w9WgXcQ Video title
//	category: News & Politics
//	attribute: not_family_safe
//	attribute: longer_than:30
//	[block deny]
//	...
//
// Sections are [hide deny], [hide allow], [block deny] or [block allow].
// keyword options are any of the fields title, description, tags, channel
// and one match mode word (default), substring or regex. With no fields,
// keywords match the title.
package filterlist

import (
	"bufio"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"ytguard/internal/rules"
)

// Limits for downloaded lists.
const (
	MaxBytes   = 2 << 20
	MaxRules   = 20000
	maxLineLen = 2000
	maxWarn    = 50
)

// Meta is a list's header.
type Meta struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Ages        string `json:"ages"`
	Homepage    string `json:"homepage"`
	License     string `json:"license"`
	Version     string `json:"version"`
}

// List is a parsed filter list.
type List struct {
	Meta     Meta
	Rules    []rules.Rule
	Warnings []string // lines that were skipped, with reasons
}

var (
	reChannelID = regexp.MustCompile(`^UC[A-Za-z0-9_-]{22}$`)
	reVideoID   = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	reHandle    = regexp.MustCompile(`^@[A-Za-z0-9._·-]{3,100}$`)
	reSection   = regexp.MustCompile(`^\[\s*(hide|block)\s+(allow|deny)\s*\]$`)
	reEntry     = regexp.MustCompile(`^([a-z]+)\s*(?:\(([^)]*)\))?\s*:\s*(.*)$`)
)

// Parse reads a list. Invalid lines are skipped and reported in Warnings;
// an error is returned only if the text isn't a usable list at all.
func Parse(text string) (List, error) {
	var l List
	if len(text) > MaxBytes {
		return l, fmt.Errorf("list is larger than %d MB", MaxBytes>>20)
	}
	tier, list := "", ""
	warn := func(n int, format string, args ...any) {
		if len(l.Warnings) < maxWarn {
			l.Warnings = append(l.Warnings, fmt.Sprintf("line %d: ", n)+fmt.Sprintf(format, args...))
		} else if len(l.Warnings) == maxWarn {
			l.Warnings = append(l.Warnings, "more problems not shown")
		}
	}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), maxLineLen*4)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if n == 1 {
			line = strings.TrimPrefix(line, "\uFEFF")
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) > maxLineLen {
			warn(n, "line too long")
			continue
		}
		if strings.HasPrefix(line, "!") {
			k, v, ok := strings.Cut(strings.TrimSpace(line[1:]), ":")
			if ok {
				setMeta(&l.Meta, strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v))
			}
			continue
		}
		if m := reSection.FindStringSubmatch(strings.ToLower(line)); m != nil {
			tier, list = m[1], m[2]
			continue
		}
		if tier == "" {
			warn(n, "entry before any [hide deny] / [block deny] style section")
			continue
		}
		r, err := parseEntry(line)
		if err != nil {
			warn(n, "%v", err)
			continue
		}
		if len(l.Rules) >= MaxRules {
			warn(n, "more than %d entries; the rest are ignored", MaxRules)
			break
		}
		r.Tier, r.List = tier, list
		l.Rules = append(l.Rules, r)
	}
	if err := sc.Err(); err != nil {
		return l, err
	}
	if l.Meta.Title == "" {
		return l, errors.New(`not a YTGuard filter list (missing "! Title:" line)`)
	}
	return l, nil
}

func setMeta(m *Meta, key, val string) {
	val = truncate(val, 500)
	switch key {
	case "title":
		m.Title = truncate(val, 120)
	case "description":
		m.Description = val
	case "ages", "age":
		m.Ages = truncate(val, 40)
	case "homepage":
		if strings.HasPrefix(val, "https://") || strings.HasPrefix(val, "http://") {
			m.Homepage = val
		}
	case "license":
		m.License = truncate(val, 80)
	case "version":
		m.Version = truncate(val, 80)
	}
}

func parseEntry(line string) (rules.Rule, error) {
	m := reEntry.FindStringSubmatch(line)
	if m == nil {
		return rules.Rule{}, fmt.Errorf("can't read %q (want e.g. 'keyword: word')", truncate(line, 60))
	}
	typ, opts, rest := m[1], strings.TrimSpace(m[2]), strings.TrimSpace(m[3])
	if rest == "" {
		return rules.Rule{}, fmt.Errorf("%s has no value", typ)
	}
	r := rules.Rule{Type: typ}
	switch typ {
	case rules.TypeKeyword:
		r.Value, r.Match = rest, rules.MatchWord
		for _, o := range strings.Split(opts, ",") {
			switch o = strings.ToLower(strings.TrimSpace(o)); o {
			case "":
			case rules.FieldTitle, rules.FieldDescription, rules.FieldTags, rules.FieldChannel:
				r.Fields = append(r.Fields, o)
			case rules.MatchWord, rules.MatchSubstring, rules.MatchRegex:
				r.Match = o
			default:
				return r, fmt.Errorf("unknown keyword option %q", o)
			}
		}
		if len(r.Fields) == 0 {
			r.Fields = []string{rules.FieldTitle}
		}
		if err := rules.ValidateKeyword(r.Value, r.Match); err != nil {
			return r, fmt.Errorf("keyword %q: %v", truncate(r.Value, 40), err)
		}
	case rules.TypeChannel:
		f := strings.Fields(rest)
		switch {
		case reChannelID.MatchString(f[0]):
			r.Value = f[0]
			f = f[1:]
			if len(f) > 0 && reHandle.MatchString(f[0]) {
				r.Extra, f = f[0], f[1:]
			}
		case reHandle.MatchString(f[0]):
			r.Value, r.Extra, f = f[0], f[0], f[1:]
		default:
			return r, fmt.Errorf("channel %q: want a UC… channel ID or @handle", truncate(f[0], 40))
		}
		r.Label = truncate(strings.Join(f, " "), 120)
	case rules.TypeVideo:
		f := strings.Fields(rest)
		if !reVideoID.MatchString(f[0]) {
			return r, fmt.Errorf("video %q: want an 11-character video ID", truncate(f[0], 40))
		}
		r.Value, r.Label = f[0], truncate(strings.Join(f[1:], " "), 120)
	case rules.TypeCategory:
		r.Value = truncate(rest, 60)
	case rules.TypeAttribute:
		r.Value = strings.ToLower(rest)
		if !validAttribute(r.Value) {
			return r, fmt.Errorf("unknown attribute %q", truncate(rest, 40))
		}
	default:
		return r, fmt.Errorf("unknown entry type %q", typ)
	}
	return r, nil
}

func validAttribute(v string) bool {
	switch v {
	case rules.AttrShorts, rules.AttrLive, rules.AttrNotFamilySafe, rules.AttrNotMadeForKids:
		return true
	}
	if n, ok := strings.CutPrefix(v, rules.AttrLongerThan+":"); ok {
		m, err := strconv.Atoi(n)
		return err == nil && m > 0
	}
	return false
}

// Format writes rules as a list file (used to export a parent's own rules
// for sharing). Kid-specific scoping is dropped: lists apply to whichever
// kids subscribe.
func Format(meta Meta, rs []rules.Rule) string {
	var b strings.Builder
	for _, kv := range [][2]string{{"Title", meta.Title}, {"Description", meta.Description}, {"Ages", meta.Ages},
		{"Homepage", meta.Homepage}, {"License", meta.License}, {"Version", meta.Version}} {
		if kv[1] != "" {
			fmt.Fprintf(&b, "! %s: %s\n", kv[0], oneLine(kv[1]))
		}
	}
	for _, tier := range rules.Tiers {
		for _, list := range []string{rules.ListDeny, rules.ListAllow} {
			var lines []string
			for _, r := range rs {
				if r.Tier == tier && r.List == list {
					if l := entryLine(r); l != "" {
						lines = append(lines, l)
					}
				}
			}
			if len(lines) == 0 {
				continue
			}
			fmt.Fprintf(&b, "\n[%s %s]\n%s\n", tier, list, strings.Join(lines, "\n"))
		}
	}
	return b.String()
}

func entryLine(r rules.Rule) string {
	switch r.Type {
	case rules.TypeKeyword:
		var opts []string
		if !(len(r.Fields) == 1 && r.Fields[0] == rules.FieldTitle) {
			opts = append(opts, r.Fields...)
		}
		if r.Match != "" && r.Match != rules.MatchWord {
			opts = append(opts, r.Match)
		}
		if len(opts) > 0 {
			return fmt.Sprintf("keyword(%s): %s", strings.Join(opts, ","), oneLine(r.Value))
		}
		return "keyword: " + oneLine(r.Value)
	case rules.TypeChannel:
		parts := []string{r.Value}
		if r.Extra != "" && r.Extra != r.Value {
			parts = append(parts, r.Extra)
		}
		if r.Label != "" {
			parts = append(parts, oneLine(r.Label))
		}
		return "channel: " + strings.Join(parts, " ")
	case rules.TypeVideo:
		return strings.TrimSpace("video: " + r.Value + " " + oneLine(r.Label))
	case rules.TypeCategory, rules.TypeAttribute:
		return r.Type + ": " + oneLine(r.Value)
	}
	return ""
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
