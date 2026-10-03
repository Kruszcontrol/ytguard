package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestHAPayloadIsSmall(t *testing.T) {
	r := Report{Kid: "Alice", Date: "2026-10-03", PC: "pc"}
	for i := 0; i < 400; i++ {
		r.Videos = append(r.Videos, Video{Title: strings.Repeat("Long video title ", 5) + fmt.Sprint(i), Channel: "Some channel",
			URL: "https://www.youtube.com/watch?v=abcdefghijk", Thumb: "https://i.ytimg.com/vi/abcdefghijk/mqdefault.jpg"})
	}
	p := HAPayload("pc", r)
	b, _ := json.Marshal(p)
	if p["videos_not_included"] != 300 {
		t.Fatalf("videos_not_included = %v", p["videos_not_included"])
	}
	if len(r.Videos[0].Thumb) == 0 {
		t.Fatal("original report modified")
	}
	if len(p["text"].(string)) > haMaxText+200 {
		t.Fatal("text not shortened")
	}
	t.Logf("payload %d bytes", len(b))
	if len(b) > 40_000 {
		t.Fatalf("payload too large: %d bytes", len(b))
	}
}
