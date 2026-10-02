package filterlist

import (
	"strings"
	"testing"

	"ytguard/internal/rules"
)

const sample = `! Title: Test list
! Description: For tests.
! Ages: 0-12
! Homepage: https://example.com/lists
! License: CC0-1.0
! Version: 1

# comment
[hide deny]
keyword: creepypasta
keyword(title,description): jump scare
keyword(regex): \bfnaf\b
channel: UCabcdefghijklmnopqrstuv @scary Scary Channel
channel: @justhandle
video: dQw4w9WgXcQ Some video
attribute: not_family_safe
attribute: longer_than:30

[block deny]
category: News & Politics
attribute: live

[block allow]
channel: UCzzzzzzzzzzzzzzzzzzzzzz Good channel

[hide deny]
keyword(bogus): x
keyword(regex): (
channel: notachannel
video: short
attribute: purple
widget: thing
`

func TestParse(t *testing.T) {
	l, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	if l.Meta.Title != "Test list" || l.Meta.Ages != "0-12" || l.Meta.Homepage != "https://example.com/lists" {
		t.Fatalf("meta %+v", l.Meta)
	}
	if len(l.Rules) != 11 {
		t.Fatalf("got %d rules: %+v", len(l.Rules), l.Rules)
	}
	if len(l.Warnings) != 6 {
		t.Fatalf("warnings: %v", l.Warnings)
	}
	r := l.Rules[1]
	if r.Tier != "hide" || r.List != "deny" || r.Value != "jump scare" || strings.Join(r.Fields, ",") != "title,description" || r.Match != "word" {
		t.Fatalf("keyword: %+v", r)
	}
	if r := l.Rules[3]; r.Value != "UCabcdefghijklmnopqrstuv" || r.Extra != "@scary" || r.Label != "Scary Channel" {
		t.Fatalf("channel: %+v", r)
	}
	if r := l.Rules[4]; r.Value != "@justhandle" || r.Extra != "@justhandle" {
		t.Fatalf("handle channel: %+v", r)
	}
	if r := l.Rules[10]; r.Tier != "block" || r.List != "allow" || r.Type != rules.TypeChannel {
		t.Fatalf("allow: %+v", r)
	}
}

func TestParseRejects(t *testing.T) {
	if _, err := Parse("<html>not a list</html>"); err == nil {
		t.Fatal("html accepted")
	}
	if _, err := Parse(strings.Repeat("x", MaxBytes+1)); err == nil {
		t.Fatal("huge list accepted")
	}
	l, _ := Parse("! Title: t\nkeyword: before section\n")
	if len(l.Rules) != 0 || len(l.Warnings) != 1 {
		t.Fatalf("%+v", l)
	}
}

func TestFormatRoundTrip(t *testing.T) {
	l, _ := Parse(sample)
	out := Format(l.Meta, l.Rules)
	l2, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(l2.Rules) != len(l.Rules) || len(l2.Warnings) != 0 {
		t.Fatalf("round trip: %d vs %d rules, warnings %v\n%s", len(l2.Rules), len(l.Rules), l2.Warnings, out)
	}
	for i := range l.Rules {
		a, b := l.Rules[i], l2.Rules[i]
		if a.Tier != b.Tier || a.List != b.List || a.Type != b.Type || a.Value != b.Value || a.Extra != b.Extra ||
			a.Match != b.Match || strings.Join(a.Fields, ",") != strings.Join(b.Fields, ",") {
			t.Errorf("rule %d: %+v != %+v", i, a, b)
		}
	}
}
