// Package rules implements the two-tier (Hide, Block) filtering engine.
//
// Each tier has Allow and Deny lists over five filter types. A tier is
// evaluated level by level, most specific first:
//
//	video → channel → keyword → category → attribute → tier default
//
// Within a level, rules scoped to the kid are checked before rules for all
// kids, and within a scope Deny beats Allow. The first level that produces a
// verdict decides the tier.
//
// The Hide tier is evaluated first. Hidden videos never appear to the kid.
// Otherwise the Block tier decides whether the video plays or is blocked
// (shown, but playback needs a parent's approval).
package rules

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const (
	TierHide  = "hide"
	TierBlock = "block"

	ListAllow = "allow"
	ListDeny  = "deny"

	TypeVideo     = "video"
	TypeChannel   = "channel"
	TypeKeyword   = "keyword"
	TypeCategory  = "category"
	TypeAttribute = "attribute"

	FieldTitle       = "title"
	FieldDescription = "description"
	FieldTags        = "tags"
	FieldChannel     = "channel"

	MatchWord      = "word"
	MatchSubstring = "substring"
	MatchRegex     = "regex"

	AttrShorts         = "shorts"
	AttrLive           = "live"
	AttrLongerThan     = "longer_than" // value "longer_than:<minutes>"
	AttrNotFamilySafe  = "not_family_safe"
	AttrNotMadeForKids = "not_made_for_kids"

	Play  = "play"
	Block = "block"
	Hide  = "hide"
)

// Tiers and Levels in evaluation order.
var (
	Tiers  = []string{TierHide, TierBlock}
	Levels = []string{TypeVideo, TypeChannel, TypeKeyword, TypeCategory, TypeAttribute}
	Fields = []string{FieldTitle, FieldDescription, FieldTags, FieldChannel}
)

// Rule is one entry in a tier's Allow or Deny list.
type Rule struct {
	ID     int64    `json:"id"`
	Tier   string   `json:"tier"`
	List   string   `json:"list"`
	Type   string   `json:"type"`
	Value  string   `json:"value"`            // video ID, channel ID, keyword, category name, attribute
	Extra  string   `json:"extra,omitempty"`  // channel @handle for channel rules
	Fields []string `json:"fields,omitempty"` // keyword rules: which fields to search
	Match  string   `json:"match,omitempty"`  // keyword rules: word | substring | regex
	KidID  int64    `json:"kid_id"`           // 0 = all kids
	Label  string   `json:"label,omitempty"`  // human name (video title, channel name)
	Note   string   `json:"note,omitempty"`
}

// Describe returns a short parent-facing description of the rule.
func (r Rule) Describe() string {
	var what string
	switch r.Type {
	case TypeVideo:
		what = "video " + quoteLabel(r.Label, r.Value)
	case TypeChannel:
		what = "channel " + quoteLabel(r.Label, firstNonEmpty(r.Extra, r.Value))
	case TypeKeyword:
		what = fmt.Sprintf("keyword %q (%s in %s)", r.Value, firstNonEmpty(r.Match, MatchWord), strings.Join(r.fields(), ", "))
	case TypeCategory:
		what = "category " + strconv.Quote(r.Value)
	case TypeAttribute:
		what = DescribeAttribute(r.Value)
	default:
		what = r.Type + " " + r.Value
	}
	scope := "all kids"
	if r.KidID != 0 {
		scope = "this kid"
	}
	return fmt.Sprintf("%s/%s: %s (%s)", title(r.Tier), r.List, what, scope)
}

// DescribeAttribute turns an attribute rule value into words.
func DescribeAttribute(v string) string {
	switch {
	case v == AttrShorts:
		return "Shorts"
	case v == AttrLive:
		return "live streams"
	case v == AttrNotFamilySafe:
		return "videos YouTube marks as not family safe"
	case v == AttrNotMadeForKids:
		return "videos not marked made-for-kids (needs API key)"
	case strings.HasPrefix(v, AttrLongerThan+":"):
		return "videos longer than " + strings.TrimPrefix(v, AttrLongerThan+":") + " min"
	}
	return v
}

func (r Rule) fields() []string {
	if len(r.Fields) == 0 {
		return []string{FieldTitle}
	}
	return r.Fields
}

// Meta is what is known about a video. Feed tiles only give partial
// metadata (Full=false); the watch page or API give everything.
type Meta struct {
	VideoID       string   `json:"videoId"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	Tags          []string `json:"tags"`
	ChannelID     string   `json:"channelId"`
	ChannelHandle string   `json:"channelHandle"`
	ChannelName   string   `json:"channelName"`
	Category      string   `json:"category"`
	LengthSeconds int      `json:"lengthSeconds"`
	IsShort       bool     `json:"isShort"`
	IsLive        bool     `json:"isLive"`
	FamilySafe    *bool    `json:"familySafe"`
	MadeForKids   *bool    `json:"madeForKids"`
	Full          bool     `json:"full"`
}

// Merge fills empty fields of m from o. If o is full, the result is full.
func (m *Meta) Merge(o Meta) {
	if m.VideoID == "" {
		m.VideoID = o.VideoID
	}
	if m.Title == "" {
		m.Title = o.Title
	}
	if m.Description == "" {
		m.Description = o.Description
	}
	if len(m.Tags) == 0 {
		m.Tags = o.Tags
	}
	if m.ChannelID == "" {
		m.ChannelID = o.ChannelID
	}
	if m.ChannelHandle == "" {
		m.ChannelHandle = o.ChannelHandle
	}
	if m.ChannelName == "" {
		m.ChannelName = o.ChannelName
	}
	if m.Category == "" {
		m.Category = o.Category
	}
	if m.LengthSeconds == 0 {
		m.LengthSeconds = o.LengthSeconds
	}
	m.IsShort = m.IsShort || o.IsShort
	m.IsLive = m.IsLive || o.IsLive
	if m.FamilySafe == nil {
		m.FamilySafe = o.FamilySafe
	}
	if m.MadeForKids == nil {
		m.MadeForKids = o.MadeForKids
	}
	m.Full = m.Full || o.Full
}

// Policy is everything needed to evaluate videos for one kid: the kid's
// per-tier defaults and all rules that apply to them (their own + global).
type Policy struct {
	KidID        int64
	HideDefault  string // allow | deny
	BlockDefault string // allow | deny
	Rules        []Rule
}

// TierResult is the verdict of one tier.
type TierResult struct {
	Tier      string   `json:"tier"`
	Verdict   string   `json:"verdict"` // allow | deny
	Rule      *Rule    `json:"rule,omitempty"`
	Default   bool     `json:"default"`
	Reason    string   `json:"reason"`
	Unchecked []string `json:"unchecked,omitempty"` // rules that couldn't be checked with the metadata available
}

// Decision is the combined result for a video.
type Decision struct {
	Outcome string      `json:"outcome"` // play | block | hide
	Hide    TierResult  `json:"hide"`
	Block   *TierResult `json:"block,omitempty"` // nil when hidden (block tier not evaluated)
	Reason  string      `json:"reason"`
	// Final is set when a parent setting (not a filter rule) decided, so
	// approving a request wouldn't change anything.
	Final bool `json:"final,omitempty"`
}

// Evaluate decides what happens to a video for the kid described by p.
func Evaluate(p Policy, m Meta) Decision {
	h := evaluateTier(p, TierHide, defaultOr(p.HideDefault), m)
	if h.Verdict == ListDeny {
		return Decision{Outcome: Hide, Hide: h, Reason: h.Reason}
	}
	b := evaluateTier(p, TierBlock, defaultOr(p.BlockDefault), m)
	d := Decision{Outcome: Play, Hide: h, Block: &b, Reason: b.Reason}
	if b.Verdict == ListDeny {
		d.Outcome = Block
	}
	return d
}

func defaultOr(s string) string {
	if s == ListDeny {
		return ListDeny
	}
	return ListAllow
}

func evaluateTier(p Policy, tier, def string, m Meta) TierResult {
	res := TierResult{Tier: tier}
	for _, level := range Levels {
		// Kid-scoped rules first, then rules for all kids.
		for _, kidScoped := range []bool{true, false} {
			var allow, deny *Rule
			for i := range p.Rules {
				r := &p.Rules[i]
				if r.Tier != tier || r.Type != level || (r.KidID != 0) != kidScoped {
					continue
				}
				if kidScoped && r.KidID != p.KidID {
					continue
				}
				ok, known := Matches(*r, m)
				if !known {
					res.Unchecked = append(res.Unchecked, r.Describe())
					continue
				}
				if !ok {
					continue
				}
				if r.List == ListDeny && deny == nil {
					deny = r
				} else if r.List == ListAllow && allow == nil {
					allow = r
				}
			}
			if deny != nil {
				res.Verdict, res.Rule = ListDeny, deny
				res.Reason = deny.Describe()
				return res
			}
			if allow != nil {
				res.Verdict, res.Rule = ListAllow, allow
				res.Reason = allow.Describe()
				return res
			}
		}
	}
	res.Verdict, res.Default = def, true
	if def == ListDeny {
		res.Reason = fmt.Sprintf("%s default: not on an allow list", title(tier))
	} else {
		res.Reason = fmt.Sprintf("%s default: allowed", title(tier))
	}
	return res
}

// Matches reports whether rule r matches m. known is false when the
// metadata needed to check the rule is missing (e.g. description on a feed
// tile); such rules are skipped and re-checked when the video is opened.
func Matches(r Rule, m Meta) (ok, known bool) {
	switch r.Type {
	case TypeVideo:
		return m.VideoID != "" && m.VideoID == r.Value, m.VideoID != ""
	case TypeChannel:
		if m.ChannelID != "" && strings.EqualFold(m.ChannelID, r.Value) {
			return true, true
		}
		if h := normHandle(m.ChannelHandle); h != "" && r.Extra != "" && h == normHandle(r.Extra) {
			return true, true
		}
		return false, m.ChannelID != "" || (m.ChannelHandle != "" && r.Extra != "")
	case TypeKeyword:
		re, err := keywordRegexp(r.Value, r.Match)
		if err != nil {
			return false, true
		}
		allKnown := true
		for _, f := range r.fields() {
			var text string
			var have bool
			switch f {
			case FieldTitle:
				text, have = m.Title, m.Title != ""
			case FieldDescription:
				text, have = m.Description, m.Full || m.Description != ""
			case FieldTags:
				text, have = strings.Join(m.Tags, "\n"), m.Full || len(m.Tags) > 0
			case FieldChannel:
				text, have = m.ChannelName, m.ChannelName != ""
			}
			if !have {
				allKnown = false
				continue
			}
			if re.MatchString(text) {
				return true, true
			}
		}
		return false, allKnown
	case TypeCategory:
		return m.Category != "" && strings.EqualFold(m.Category, r.Value), m.Category != "" || m.Full
	case TypeAttribute:
		switch {
		case r.Value == AttrShorts:
			return m.IsShort, true
		case r.Value == AttrLive:
			return m.IsLive, m.Full || m.IsLive
		case r.Value == AttrNotFamilySafe:
			return m.FamilySafe != nil && !*m.FamilySafe, m.FamilySafe != nil
		case r.Value == AttrNotMadeForKids:
			return m.MadeForKids != nil && !*m.MadeForKids, m.MadeForKids != nil
		case strings.HasPrefix(r.Value, AttrLongerThan+":"):
			min, err := strconv.Atoi(strings.TrimPrefix(r.Value, AttrLongerThan+":"))
			if err != nil {
				return false, true
			}
			return m.LengthSeconds > min*60, m.LengthSeconds > 0
		}
	}
	return false, true
}

func normHandle(h string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(h), "@"))
}

var (
	reMu    sync.Mutex
	reCache = map[string]*regexp.Regexp{}
)

// keywordRegexp compiles (and caches) the matcher for a keyword rule.
func keywordRegexp(value, match string) (*regexp.Regexp, error) {
	key := match + "\x00" + value
	reMu.Lock()
	defer reMu.Unlock()
	if re, ok := reCache[key]; ok {
		return re, nil
	}
	var expr string
	switch match {
	case MatchRegex:
		expr = "(?i)" + value
	case MatchSubstring:
		expr = "(?i)" + regexp.QuoteMeta(value)
	default: // whole word(s), unicode aware
		expr = `(?i)(?:^|[^\p{L}\p{N}])` + regexp.QuoteMeta(strings.TrimSpace(value)) + `(?:$|[^\p{L}\p{N}])`
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, err
	}
	reCache[key] = re
	return re, nil
}

// ValidateKeyword checks a keyword rule's value before saving it.
func ValidateKeyword(value, match string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("keyword is empty")
	}
	_, err := keywordRegexp(value, match)
	return err
}

func quoteLabel(label, id string) string {
	if label != "" {
		return strconv.Quote(label)
	}
	return id
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
