// Package ytmeta parses YouTube URLs and fetches video/channel metadata,
// from the YouTube Data API when a key is configured or by reading public
// pages otherwise.
package ytmeta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ytguard/internal/rules"
)

// Ref is a parsed reference to a video or channel.
type Ref struct {
	Kind string // video | channel | handle
	ID   string // video ID, UC… channel ID, or @handle
}

var (
	reVideoID   = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	reChannelID = regexp.MustCompile(`^UC[A-Za-z0-9_-]{22}$`)
	reHandle    = regexp.MustCompile(`^@[A-Za-z0-9._·-]{3,100}$`)
)

// ParseRef accepts a pasted URL, video ID, channel ID or @handle.
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	switch {
	case reChannelID.MatchString(s):
		return Ref{"channel", s}, nil
	case reHandle.MatchString(s):
		return Ref{"handle", s}, nil
	case reVideoID.MatchString(s):
		return Ref{"video", s}, nil
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return Ref{}, err
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	host = strings.TrimPrefix(host, "m.")
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch host {
	case "youtu.be":
		if reVideoID.MatchString(parts[0]) {
			return Ref{"video", parts[0]}, nil
		}
	case "youtube.com", "music.youtube.com", "youtube-nocookie.com":
		if v := u.Query().Get("v"); reVideoID.MatchString(v) {
			return Ref{"video", v}, nil
		}
		if len(parts) >= 2 {
			switch parts[0] {
			case "shorts", "embed", "live", "v":
				if reVideoID.MatchString(parts[1]) {
					return Ref{"video", parts[1]}, nil
				}
			case "channel":
				if reChannelID.MatchString(parts[1]) {
					return Ref{"channel", parts[1]}, nil
				}
			}
		}
		if len(parts) >= 1 && strings.HasPrefix(parts[0], "@") {
			h, _ := url.PathUnescape(parts[0])
			return Ref{"handle", h}, nil
		}
	}
	return Ref{}, fmt.Errorf("not a YouTube video or channel link: %q", s)
}

// Client fetches metadata. APIKey may return "" (no key configured).
type Client struct {
	APIKey func() string
	HTTP   *http.Client
}

// New returns a client with sensible timeouts.
func New(apiKey func() string) *Client {
	return &Client{APIKey: apiKey, HTTP: &http.Client{Timeout: 8 * time.Second}}
}

func (c *Client) key() string {
	if c.APIKey == nil {
		return ""
	}
	return strings.TrimSpace(c.APIKey())
}

// HasAPIKey reports whether Data API enrichment is available.
func (c *Client) HasAPIKey() bool { return c.key() != "" }

func (c *Client) get(ctx context.Context, u string, v any) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Language", "en-US,en;q=0.8")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36")
	req.AddCookie(&http.Cookie{Name: "CONSENT", Value: "YES+1"})
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: HTTP %d", strings.SplitN(u, "?", 2)[0], resp.StatusCode)
	}
	if v != nil {
		return body, json.Unmarshal(body, v)
	}
	return body, nil
}

// Video fetches full metadata for one video.
func (c *Client) Video(ctx context.Context, id string) (rules.Meta, error) {
	if c.HasAPIKey() {
		m, err := c.Videos(ctx, []string{id})
		if err == nil {
			if v, ok := m[id]; ok {
				return v, nil
			}
			return rules.Meta{}, fmt.Errorf("video %s not found", id)
		}
		// Fall through to page scraping on API errors (quota etc.).
	}
	body, err := c.get(ctx, "https://www.youtube.com/watch?v="+url.QueryEscape(id)+"&hl=en", nil)
	if err != nil {
		return rules.Meta{}, err
	}
	m, err := ParseWatchPage(body)
	if err != nil {
		return rules.Meta{}, err
	}
	if m.VideoID != id {
		return rules.Meta{}, fmt.Errorf("video %s unavailable", id)
	}
	return m, nil
}

// PlayerResponse is the subset of ytInitialPlayerResponse we use.
type PlayerResponse struct {
	VideoDetails struct {
		VideoID          string   `json:"videoId"`
		Title            string   `json:"title"`
		LengthSeconds    string   `json:"lengthSeconds"`
		Keywords         []string `json:"keywords"`
		ChannelID        string   `json:"channelId"`
		ShortDescription string   `json:"shortDescription"`
		Author           string   `json:"author"`
		IsLiveContent    bool     `json:"isLiveContent"`
		IsLive           bool     `json:"isLive"`
	} `json:"videoDetails"`
	Microformat struct {
		R struct {
			Category        string `json:"category"`
			IsFamilySafe    *bool  `json:"isFamilySafe"`
			OwnerProfileURL string `json:"ownerProfileUrl"`
			LiveDetails     *struct {
				IsLiveNow bool `json:"isLiveNow"`
			} `json:"liveBroadcastDetails"`
		} `json:"playerMicroformatRenderer"`
	} `json:"microformat"`
}

// Meta converts a player response to rule metadata.
func (p PlayerResponse) Meta() rules.Meta {
	d, mf := p.VideoDetails, p.Microformat.R
	n, _ := strconv.Atoi(d.LengthSeconds)
	m := rules.Meta{
		VideoID: d.VideoID, Title: d.Title, Description: d.ShortDescription, Tags: d.Keywords,
		ChannelID: d.ChannelID, ChannelName: d.Author, Category: mf.Category, LengthSeconds: n,
		IsLive: d.IsLive || (mf.LiveDetails != nil && mf.LiveDetails.IsLiveNow), FamilySafe: mf.IsFamilySafe, Full: true,
	}
	if i := strings.Index(mf.OwnerProfileURL, "/@"); i >= 0 {
		m.ChannelHandle = mf.OwnerProfileURL[i+1:]
	}
	return m
}

// ParseWatchPage extracts metadata from a watch page's HTML.
func ParseWatchPage(body []byte) (rules.Meta, error) {
	const marker = "ytInitialPlayerResponse = "
	i := strings.Index(string(body), marker)
	if i < 0 {
		return rules.Meta{}, errors.New("player response not found")
	}
	var p PlayerResponse
	dec := json.NewDecoder(strings.NewReader(string(body[i+len(marker):])))
	if err := dec.Decode(&p); err != nil {
		return rules.Meta{}, err
	}
	if p.VideoDetails.VideoID == "" {
		return rules.Meta{}, errors.New("video unavailable")
	}
	return p.Meta(), nil
}

// Videos batch-fetches metadata via the Data API (needs a key).
func (c *Client) Videos(ctx context.Context, ids []string) (map[string]rules.Meta, error) {
	key := c.key()
	if key == "" {
		return nil, errors.New("no YouTube API key configured")
	}
	out := map[string]rules.Meta{}
	for start := 0; start < len(ids); start += 50 {
		batch := ids[start:min(start+50, len(ids))]
		var resp struct {
			Items []struct {
				ID      string `json:"id"`
				Snippet struct {
					Title                string   `json:"title"`
					Description          string   `json:"description"`
					Tags                 []string `json:"tags"`
					ChannelID            string   `json:"channelId"`
					ChannelTitle         string   `json:"channelTitle"`
					CategoryID           string   `json:"categoryId"`
					LiveBroadcastContent string   `json:"liveBroadcastContent"`
				} `json:"snippet"`
				ContentDetails struct {
					Duration string `json:"duration"`
				} `json:"contentDetails"`
				Status struct {
					MadeForKids *bool `json:"madeForKids"`
				} `json:"status"`
			} `json:"items"`
		}
		u := "https://www.googleapis.com/youtube/v3/videos?part=snippet,contentDetails,status&id=" +
			url.QueryEscape(strings.Join(batch, ",")) + "&key=" + url.QueryEscape(key)
		if _, err := c.get(ctx, u, &resp); err != nil {
			return out, err
		}
		for _, it := range resp.Items {
			out[it.ID] = rules.Meta{
				VideoID: it.ID, Title: it.Snippet.Title, Description: it.Snippet.Description, Tags: it.Snippet.Tags,
				ChannelID: it.Snippet.ChannelID, ChannelName: it.Snippet.ChannelTitle,
				Category: Categories[it.Snippet.CategoryID], LengthSeconds: ParseISODuration(it.ContentDetails.Duration),
				IsLive: it.Snippet.LiveBroadcastContent == "live", MadeForKids: it.Status.MadeForKids, Full: true,
			}
		}
	}
	return out, nil
}

// Channel describes a channel.
type Channel struct {
	ID, Name, Handle string
}

var (
	reCanonical  = regexp.MustCompile(`<link rel="canonical" href="https://www\.youtube\.com/channel/(UC[A-Za-z0-9_-]{22})"`)
	reOGTitle    = regexp.MustCompile(`<meta property="og:title" content="([^"]*)"`)
	reVanityURL  = regexp.MustCompile(`"vanityChannelUrl":"https?://www\.youtube\.com/(@[^"]+)"`)
	reCanonicalB = regexp.MustCompile(`"canonicalBaseUrl":"/(@[^"]+)"`)
)

// ResolveChannel looks up a channel by ID or @handle.
func (c *Client) ResolveChannel(ctx context.Context, ref Ref) (Channel, error) {
	if key := c.key(); key != "" {
		q := "id=" + url.QueryEscape(ref.ID)
		if ref.Kind == "handle" {
			q = "forHandle=" + url.QueryEscape(ref.ID)
		}
		var resp struct {
			Items []struct {
				ID      string `json:"id"`
				Snippet struct {
					Title     string `json:"title"`
					CustomURL string `json:"customUrl"`
				} `json:"snippet"`
			} `json:"items"`
		}
		if _, err := c.get(ctx, "https://www.googleapis.com/youtube/v3/channels?part=snippet&"+q+"&key="+url.QueryEscape(key), &resp); err == nil && len(resp.Items) > 0 {
			it := resp.Items[0]
			return Channel{ID: it.ID, Name: it.Snippet.Title, Handle: it.Snippet.CustomURL}, nil
		}
	}
	path := "channel/" + ref.ID
	if ref.Kind == "handle" {
		path = url.PathEscape(ref.ID)
	}
	body, err := c.get(ctx, "https://www.youtube.com/"+path+"?hl=en", nil)
	if err != nil {
		return Channel{}, err
	}
	ch := Channel{}
	if m := reCanonical.FindSubmatch(body); m != nil {
		ch.ID = string(m[1])
	}
	if m := reOGTitle.FindSubmatch(body); m != nil {
		ch.Name = html.UnescapeString(string(m[1]))
	}
	if m := reVanityURL.FindSubmatch(body); m != nil {
		ch.Handle = string(m[1])
	} else if m := reCanonicalB.FindSubmatch(body); m != nil {
		ch.Handle = string(m[1])
	}
	if ref.Kind == "handle" && ch.Handle == "" {
		ch.Handle = ref.ID
	}
	if ch.ID == "" {
		return ch, fmt.Errorf("channel %s not found", ref.ID)
	}
	return ch, nil
}

// OEmbed returns a video's title and channel name without an API key.
func (c *Client) OEmbed(ctx context.Context, id string) (title, author string, err error) {
	var r struct {
		Title      string `json:"title"`
		AuthorName string `json:"author_name"`
	}
	_, err = c.get(ctx, "https://www.youtube.com/oembed?format=json&url="+url.QueryEscape("https://www.youtube.com/watch?v="+id), &r)
	return r.Title, r.AuthorName, err
}

var reISO = regexp.MustCompile(`^P(?:(\d+)D)?T?(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?$`)

// ParseISODuration parses durations like PT1H2M3S.
func ParseISODuration(s string) int {
	m := reISO.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	return n(1)*86400 + n(2)*3600 + n(3)*60 + n(4)
}

// Categories maps Data API category IDs to the names the watch page uses.
var Categories = map[string]string{
	"1": "Film & Animation", "2": "Autos & Vehicles", "10": "Music", "15": "Pets & Animals",
	"17": "Sports", "19": "Travel & Events", "20": "Gaming", "22": "People & Blogs",
	"23": "Comedy", "24": "Entertainment", "25": "News & Politics", "26": "Howto & Style",
	"27": "Education", "28": "Science & Technology", "29": "Nonprofits & Activism",
}

// CategoryNames lists category names for the UI.
func CategoryNames() []string {
	return []string{"Autos & Vehicles", "Comedy", "Education", "Entertainment", "Film & Animation", "Gaming",
		"Howto & Style", "Music", "News & Politics", "Nonprofits & Activism", "People & Blogs",
		"Pets & Animals", "Science & Technology", "Sports", "Travel & Events"}
}
