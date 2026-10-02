package rules

import "testing"

func kw(tier, list, value string, fields ...string) Rule {
	return Rule{Tier: tier, List: list, Type: TypeKeyword, Value: value, Match: MatchWord, Fields: fields}
}

func TestEvaluate(t *testing.T) {
	const kid = 7
	tru, fls := true, false
	full := Meta{VideoID: "vid1", Title: "Creepy Minecraft house", Description: "a tour", Tags: []string{"mc"},
		ChannelID: "UCabc", ChannelHandle: "@builder", ChannelName: "Builder", Category: "Gaming", LengthSeconds: 600, Full: true}

	tests := []struct {
		name     string
		policy   Policy
		meta     Meta
		want     string
		wantRule string // expected deciding tier/verdict "hide:deny" etc.
	}{
		{
			name:   "defaults allow everything",
			policy: Policy{KidID: kid},
			meta:   full,
			want:   Play,
		},
		{
			name: "user example 1: block-allow minecraft, hide-deny creepy -> hidden",
			policy: Policy{KidID: kid, Rules: []Rule{
				kw(TierBlock, ListAllow, "minecraft"),
				kw(TierHide, ListDeny, "creepy"),
			}},
			meta: full,
			want: Hide,
		},
		{
			name: "user example 2: hide-allow video, block-deny video -> shown but blocked",
			policy: Policy{KidID: kid, HideDefault: ListDeny, Rules: []Rule{
				{Tier: TierHide, List: ListAllow, Type: TypeVideo, Value: "vid1"},
				{Tier: TierBlock, List: ListDeny, Type: TypeVideo, Value: "vid1"},
			}},
			meta: full,
			want: Block,
		},
		{
			name: "deny beats allow at same level",
			policy: Policy{KidID: kid, Rules: []Rule{
				kw(TierBlock, ListAllow, "minecraft"),
				kw(TierBlock, ListDeny, "creepy"),
			}},
			meta: full,
			want: Block,
		},
		{
			name: "more specific level wins: channel allow over keyword deny",
			policy: Policy{KidID: kid, Rules: []Rule{
				kw(TierBlock, ListDeny, "creepy"),
				{Tier: TierBlock, List: ListAllow, Type: TypeChannel, Value: "UCabc"},
			}},
			meta: full,
			want: Play,
		},
		{
			name: "video allow over channel deny",
			policy: Policy{KidID: kid, Rules: []Rule{
				{Tier: TierBlock, List: ListDeny, Type: TypeChannel, Value: "UCabc"},
				{Tier: TierBlock, List: ListAllow, Type: TypeVideo, Value: "vid1"},
			}},
			meta: full,
			want: Play,
		},
		{
			name: "kid-scoped allow beats global deny at same level (approved request)",
			policy: Policy{KidID: kid, Rules: []Rule{
				{Tier: TierBlock, List: ListDeny, Type: TypeVideo, Value: "vid1"},
				{Tier: TierBlock, List: ListAllow, Type: TypeVideo, Value: "vid1", KidID: kid},
			}},
			meta: full,
			want: Play,
		},
		{
			name: "other kid's rules ignored",
			policy: Policy{KidID: kid, Rules: []Rule{
				{Tier: TierHide, List: ListDeny, Type: TypeVideo, Value: "vid1", KidID: 99},
			}},
			meta: full,
			want: Play,
		},
		{
			name:   "block default deny",
			policy: Policy{KidID: kid, BlockDefault: ListDeny},
			meta:   full,
			want:   Block,
		},
		{
			name:   "hide default deny",
			policy: Policy{KidID: kid, HideDefault: ListDeny},
			meta:   full,
			want:   Hide,
		},
		{
			name: "channel matched by handle on feed tile",
			policy: Policy{KidID: kid, Rules: []Rule{
				{Tier: TierHide, List: ListDeny, Type: TypeChannel, Value: "UCzzz", Extra: "@Builder"},
			}},
			meta: Meta{VideoID: "x", Title: "t", ChannelHandle: "@builder"},
			want: Hide,
		},
		{
			name: "description rule unchecked on partial meta",
			policy: Policy{KidID: kid, Rules: []Rule{
				kw(TierHide, ListDeny, "tour", FieldDescription),
			}},
			meta: Meta{VideoID: "vid1", Title: "Creepy Minecraft house"},
			want: Play,
		},
		{
			name: "description rule applies on full meta",
			policy: Policy{KidID: kid, Rules: []Rule{
				kw(TierHide, ListDeny, "tour", FieldDescription),
			}},
			meta: full,
			want: Hide,
		},
		{
			name: "word match does not match inside words",
			policy: Policy{KidID: kid, Rules: []Rule{
				kw(TierBlock, ListDeny, "mine"),
			}},
			meta: full,
			want: Play,
		},
		{
			name: "substring match does",
			policy: Policy{KidID: kid, Rules: []Rule{
				{Tier: TierBlock, List: ListDeny, Type: TypeKeyword, Value: "mine", Match: MatchSubstring},
			}},
			meta: full,
			want: Block,
		},
		{
			name: "regex match",
			policy: Policy{KidID: kid, Rules: []Rule{
				{Tier: TierBlock, List: ListDeny, Type: TypeKeyword, Value: `cr(ee|ea)py`, Match: MatchRegex},
			}},
			meta: full,
			want: Block,
		},
		{
			name: "tags field",
			policy: Policy{KidID: kid, Rules: []Rule{
				kw(TierBlock, ListDeny, "mc", FieldTags),
			}},
			meta: full,
			want: Block,
		},
		{
			name: "category",
			policy: Policy{KidID: kid, Rules: []Rule{
				{Tier: TierBlock, List: ListDeny, Type: TypeCategory, Value: "gaming"},
			}},
			meta: full,
			want: Block,
		},
		{
			name: "attribute shorts hidden",
			policy: Policy{KidID: kid, Rules: []Rule{
				{Tier: TierHide, List: ListDeny, Type: TypeAttribute, Value: AttrShorts},
			}},
			meta: Meta{VideoID: "s", IsShort: true},
			want: Hide,
		},
		{
			name: "attribute longer than",
			policy: Policy{KidID: kid, Rules: []Rule{
				{Tier: TierBlock, List: ListDeny, Type: TypeAttribute, Value: "longer_than:5"},
			}},
			meta: full,
			want: Block,
		},
		{
			name: "attribute not family safe",
			policy: Policy{KidID: kid, Rules: []Rule{
				{Tier: TierHide, List: ListDeny, Type: TypeAttribute, Value: AttrNotFamilySafe},
			}},
			meta: Meta{VideoID: "v", FamilySafe: &fls, Full: true},
			want: Hide,
		},
		{
			name: "attribute family safe passes",
			policy: Policy{KidID: kid, Rules: []Rule{
				{Tier: TierHide, List: ListDeny, Type: TypeAttribute, Value: AttrNotFamilySafe},
			}},
			meta: Meta{VideoID: "v", FamilySafe: &tru, Full: true},
			want: Play,
		},
		{
			name: "allowlist mode: block default deny + channel allow",
			policy: Policy{KidID: kid, BlockDefault: ListDeny, Rules: []Rule{
				{Tier: TierBlock, List: ListAllow, Type: TypeChannel, Value: "UCabc"},
			}},
			meta: full,
			want: Play,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Evaluate(tt.policy, tt.meta)
			if d.Outcome != tt.want {
				t.Fatalf("outcome = %s, want %s (reason: %s)", d.Outcome, tt.want, d.Reason)
			}
		})
	}
}

func TestUncheckedReported(t *testing.T) {
	p := Policy{Rules: []Rule{kw(TierHide, ListDeny, "x", FieldDescription)}}
	d := Evaluate(p, Meta{VideoID: "a", Title: "t"})
	if len(d.Hide.Unchecked) != 1 {
		t.Fatalf("unchecked = %v", d.Hide.Unchecked)
	}
}

func TestMergeKeepsFull(t *testing.T) {
	m := Meta{VideoID: "a", Title: "tile"}
	m.Merge(Meta{VideoID: "a", Title: "full", Description: "d", Full: true})
	if !m.Full || m.Title != "tile" || m.Description != "d" {
		t.Fatalf("merge = %+v", m)
	}
}

func TestValidateKeyword(t *testing.T) {
	if err := ValidateKeyword("(", MatchRegex); err == nil {
		t.Fatal("expected bad regex error")
	}
	if err := ValidateKeyword("(", MatchWord); err != nil {
		t.Fatal(err)
	}
}
