package console

import (
	"context"
	"errors"
	"sort"
	"time"
)

// Kid is a kid's status as a PC reports it (GET /api/v1/state).
type Kid struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	State            string `json:"state"` // watching | idle | break | time_up | locked | outside_window | none_today
	Allowed          bool   `json:"allowed"`
	Message          string `json:"message"`
	UsedMinutes      int    `json:"used_minutes"`
	LimitMinutes     int    `json:"limit_minutes"`     // -1 unlimited
	RemainingMinutes int    `json:"remaining_minutes"` // -1 unlimited
	BreakInMinutes   int    `json:"break_in_minutes"`  // -1 no breaks
	BreakUntil       string `json:"break_until"`
	LockedUntil      string `json:"locked_until"`
	TodayVideos      int    `json:"today_videos"`
	PendingRequests  int    `json:"pending_requests"`
	VideoTitle       string `json:"video_title"`
	VideoChannel     string `json:"video_channel"`
	VideoURL         string `json:"video_url"`
}

// Percent is the share of today's limit used.
func (k Kid) Percent() int {
	if k.LimitMinutes <= 0 {
		return 0
	}
	return min(100, k.UsedMinutes*100/k.LimitMinutes)
}

// Finding is another browser or video app a PC noticed.
type Finding struct {
	Key       string `json:"key"`
	Kind      string `json:"kind"`
	App       string `json:"app"`
	Location  string `json:"location"`
	How       string `json:"how"`
	FirstSeen int64  `json:"firstSeen"`
	LastSeen  int64  `json:"lastSeen"`
	Kid       string `json:"kid"`
	Running   bool   `json:"runningNow"`
}

type pcState struct {
	PC        string    `json:"pc"`
	PCID      string    `json:"pc_id"`
	Kids      []Kid     `json:"kids"`
	Pending   int       `json:"pending_requests"`
	OtherApps []Finding `json:"other_apps_list"`
	YTGuard   struct {
		Version         string `json:"version"`
		Latest          string `json:"latest_version"`
		UpdateAvailable bool   `json:"update_available"`
		ReleaseURL      string `json:"release_url"`
	} `json:"ytguard"`
}

// Request is a kid asking to watch a blocked video.
type Request struct {
	ID          int64  `json:"id"`
	KidID       int64  `json:"kidId"`
	KidName     string `json:"kidName"`
	VideoID     string `json:"videoId"`
	Title       string `json:"title"`
	ChannelID   string `json:"channelId"`
	ChannelName string `json:"channelName"`
	Reason      string `json:"reason"`
	Message     string `json:"message"`
	CreatedAt   int64  `json:"createdAt"`
	// Filled in by the console.
	PC PC `json:"-"`
}

// PCView is one PC's live status.
type PCView struct {
	PC
	Online      bool
	Err         string
	CertChanged string // the new fingerprint, if the certificate changed
	State       pcState
	Requests    []Request
}

// Status is a short word for the PC's connection state.
func (v PCView) Status() string {
	switch {
	case v.CertChanged != "":
		return "cert"
	case v.Online:
		return "online"
	}
	return "offline"
}

// KidView is a kid with the PC it's on.
type KidView struct {
	Kid
	PC PC
}

// Snapshot is the live state of every PC.
type Snapshot struct {
	At       time.Time
	PCs      []PCView
	Kids     []KidView
	Requests []Request
	Offline  int
}

// fetch asks every PC for its state and pending requests, in parallel.
func (s *Server) fetch(ctx context.Context) Snapshot {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	views := forEach(s.pcs(), func(p PC) PCView {
		v := PCView{PC: p}
		err := s.call(ctx, p, "GET", "/api/v1/state", nil, &v.State)
		if err == nil {
			var rq struct{ Requests []Request }
			err = s.call(ctx, p, "GET", "/api/v1/requests", nil, &rq)
			v.Requests = rq.Requests
		}
		var cc CertChangedError
		switch {
		case errors.As(err, &cc):
			v.CertChanged, v.Err = cc.Got, err.Error()
		case err != nil:
			v.Err = err.Error()
		default:
			v.Online = true
		}
		return v
	})
	snap := Snapshot{At: time.Now(), PCs: views}
	for _, v := range views {
		if !v.Online {
			snap.Offline++
			continue
		}
		for _, k := range v.State.Kids {
			snap.Kids = append(snap.Kids, KidView{Kid: k, PC: v.PC})
		}
		for _, r := range v.Requests {
			r.PC = v.PC
			snap.Requests = append(snap.Requests, r)
		}
	}
	sort.SliceStable(snap.Requests, func(i, j int) bool { return snap.Requests[i].CreatedAt > snap.Requests[j].CreatedAt })
	s.smu.Lock()
	s.last = snap
	s.smu.Unlock()
	return snap
}

// cached returns the last snapshot if it's recent enough (for the nav
// badge on pages that don't need live data).
func (s *Server) cached() (Snapshot, bool) {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.last, !s.last.At.IsZero() && time.Since(s.last.At) < 2*time.Minute
}
