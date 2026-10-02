package ytmeta

import "testing"

func TestParseRef(t *testing.T) {
	tests := []struct{ in, kind, id string }{
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=10s", "video", "dQw4w9WgXcQ"},
		{"youtube.com/watch?v=dQw4w9WgXcQ", "video", "dQw4w9WgXcQ"},
		{"https://youtu.be/dQw4w9WgXcQ?si=abc", "video", "dQw4w9WgXcQ"},
		{"https://m.youtube.com/shorts/dQw4w9WgXcQ", "video", "dQw4w9WgXcQ"},
		{"https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ", "video", "dQw4w9WgXcQ"},
		{"dQw4w9WgXcQ", "video", "dQw4w9WgXcQ"},
		{"https://www.youtube.com/channel/UCuAXFkgsw1L7xaCfnd5JJOw", "channel", "UCuAXFkgsw1L7xaCfnd5JJOw"},
		{"UCuAXFkgsw1L7xaCfnd5JJOw", "channel", "UCuAXFkgsw1L7xaCfnd5JJOw"},
		{"https://www.youtube.com/@MrBeast/videos", "handle", "@MrBeast"},
		{"@MrBeast", "handle", "@MrBeast"},
	}
	for _, tt := range tests {
		r, err := ParseRef(tt.in)
		if err != nil || r.Kind != tt.kind || r.ID != tt.id {
			t.Errorf("ParseRef(%q) = %+v, %v; want %s %s", tt.in, r, err, tt.kind, tt.id)
		}
	}
	for _, bad := range []string{"https://example.com/watch?v=dQw4w9WgXcQ", "hello world", ""} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) should fail", bad)
		}
	}
}

func TestParseWatchPage(t *testing.T) {
	page := `<html><script>var ytInitialPlayerResponse = {"videoDetails":{"videoId":"abcdefghijk","title":"T","lengthSeconds":"125",
"keywords":["k1","k2"],"channelId":"UCxxxxxxxxxxxxxxxxxxxxxx","shortDescription":"D","author":"A"},
"microformat":{"playerMicroformatRenderer":{"category":"Gaming","isFamilySafe":true,"ownerProfileUrl":"http://www.youtube.com/@chan"}}};var meta = 1;</script>`
	m, err := ParseWatchPage([]byte(page))
	if err != nil {
		t.Fatal(err)
	}
	if m.VideoID != "abcdefghijk" || m.LengthSeconds != 125 || m.Category != "Gaming" || m.ChannelHandle != "@chan" ||
		len(m.Tags) != 2 || m.FamilySafe == nil || !*m.FamilySafe || !m.Full {
		t.Fatalf("%+v", m)
	}
}

func TestISODuration(t *testing.T) {
	for in, want := range map[string]int{"PT1H2M3S": 3723, "PT45S": 45, "P1DT1S": 86401, "PT10M": 600, "bad": 0} {
		if got := ParseISODuration(in); got != want {
			t.Errorf("%s = %d want %d", in, got, want)
		}
	}
}
