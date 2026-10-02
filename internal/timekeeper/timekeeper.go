// Package timekeeper does server-side accounting of watch time: daily
// limits, allowed time windows, parent locks and enforced breaks.
//
// Play time is counted from extension heartbeats. Each heartbeat adds the
// wall-clock time since the previous counted heartbeat (capped), so several
// tabs playing at once are counted once, not per tab.
package timekeeper

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// HeartbeatInterval is how often the extension reports while playing.
	HeartbeatInterval = 15
	// maxCount caps seconds credited per heartbeat (covers jitter).
	maxCount = 30
	// staleAfter: if the last counted heartbeat is older than this, the
	// gap is treated as not watching and only one interval is credited.
	staleAfter = 60

	ReasonLocked        = "locked"
	ReasonBreak         = "break"
	ReasonOutsideWindow = "outside_window"
	ReasonTimeUp        = "time_up"
	ReasonNoneToday     = "none_today"
)

// Window is an allowed period of the day, in minutes since midnight.
type Window struct{ Start, End int }

// Schedule is one weekday's rules for a kid.
type Schedule struct {
	DailyMinutes int      `json:"dailyMinutes"` // < 0 = unlimited
	Windows      []Window `json:"windows"`      // empty = any time
	BreakAfter   int      `json:"breakAfter"`   // minutes of continuous watching before a break; 0 = no breaks
	BreakLen     int      `json:"breakLen"`     // break length, minutes
}

// State is a kid's persisted counters.
type State struct {
	Day           string `json:"day"`
	UsedSec       int    `json:"usedSec"`
	ContinuousSec int    `json:"continuousSec"`
	LastCounted   int64  `json:"lastCounted"`
	BreakUntil    int64  `json:"breakUntil"`
	LockedUntil   int64  `json:"lockedUntil"`
}

// Status is what the extension and the dashboard see.
type Status struct {
	Allowed      bool   `json:"allowed"`
	Reason       string `json:"reason,omitempty"`
	Message      string `json:"message,omitempty"`
	UsedSec      int    `json:"usedSec"`
	LimitSec     int    `json:"limitSec"`     // -1 unlimited
	RemainingSec int    `json:"remainingSec"` // -1 unlimited
	BreakInSec   int    `json:"breakInSec"`   // -1 no breaks
	BreakUntil   int64  `json:"breakUntil,omitempty"`
	LockedUntil  int64  `json:"lockedUntil,omitempty"`
	NextOpen     string `json:"nextOpen,omitempty"`
}

// Day returns the accounting day key for t (local time).
func Day(t time.Time) string { return t.Format("2006-01-02") }

// Roll applies day rollover and break expiry. Call before using st.
func Roll(st *State, sch Schedule, now time.Time) {
	n := now.Unix()
	if today := Day(now); st.Day != today {
		st.Day = today
		st.UsedSec, st.ContinuousSec, st.LastCounted, st.BreakUntil = 0, 0, 0, 0
	}
	if st.BreakUntil != 0 && n >= st.BreakUntil {
		st.BreakUntil, st.ContinuousSec = 0, 0
	}
	// A pause at least as long as a break counts as a break.
	if sch.BreakLen > 0 && st.LastCounted > 0 && n-st.LastCounted >= int64(sch.BreakLen*60) {
		st.ContinuousSec = 0
	}
	if st.LockedUntil != 0 && n >= st.LockedUntil {
		st.LockedUntil = 0
	}
}

// Heartbeat records a report from the extension and returns the new status.
// start marks the first heartbeat of a play session (nothing is counted for
// the time before it).
func Heartbeat(st *State, sch Schedule, bonusMin int, now time.Time, playing, start bool) (Status, int) {
	Roll(st, sch, now)
	s := Evaluate(st, sch, bonusMin, now)
	if !s.Allowed || !playing {
		return s, 0
	}
	n := now.Unix()
	delta := 0
	switch gap := n - st.LastCounted; {
	case st.LastCounted == 0:
	case gap <= 0:
	case gap > staleAfter:
		if !start {
			delta = HeartbeatInterval
		}
	case start && gap > HeartbeatInterval+5:
		// New play session after a short pause: don't credit the pause.
	default:
		delta = int(min(gap, maxCount))
	}
	if n > st.LastCounted {
		st.LastCounted = n
	}
	st.UsedSec += delta
	st.ContinuousSec += delta
	if sch.BreakAfter > 0 && st.ContinuousSec >= sch.BreakAfter*60 && sch.BreakLen > 0 {
		st.BreakUntil = n + int64(sch.BreakLen*60)
		st.ContinuousSec = 0
	}
	return Evaluate(st, sch, bonusMin, now), delta
}

// Evaluate computes the status without changing counters (call Roll first).
func Evaluate(st *State, sch Schedule, bonusMin int, now time.Time) Status {
	n := now.Unix()
	s := Status{UsedSec: st.UsedSec, LimitSec: -1, RemainingSec: -1, BreakInSec: -1}
	if sch.DailyMinutes >= 0 {
		s.LimitSec = (sch.DailyMinutes + bonusMin) * 60
		s.RemainingSec = max(0, s.LimitSec-st.UsedSec)
	}
	if sch.BreakAfter > 0 && sch.BreakLen > 0 {
		s.BreakInSec = max(0, sch.BreakAfter*60-st.ContinuousSec)
	}
	// Time until the current window closes also limits what's left.
	if len(sch.Windows) > 0 {
		mins := now.Hour()*60 + now.Minute()
		in := false
		for _, w := range sch.Windows {
			if mins >= w.Start && mins < w.End {
				in = true
				left := (w.End-mins)*60 - now.Second()
				if s.RemainingSec < 0 || left < s.RemainingSec {
					s.RemainingSec = left
				}
			}
		}
		if !in {
			s.Reason = ReasonOutsideWindow
			s.NextOpen = nextOpen(sch.Windows, mins)
			if s.NextOpen != "" {
				s.Message = "YouTube is available again at " + s.NextOpen + "."
			} else {
				s.Message = "YouTube time is over for today."
			}
		}
	}
	switch {
	case st.LockedUntil > n:
		s.Reason, s.LockedUntil = ReasonLocked, st.LockedUntil
		s.Message = "A parent has paused YouTube."
	case st.BreakUntil > n:
		s.Reason, s.BreakUntil = ReasonBreak, st.BreakUntil
		s.Message = "Stretch, get a drink, look out a window."
	case s.Reason != "":
	case sch.DailyMinutes == 0 && bonusMin == 0:
		s.Reason, s.Message = ReasonNoneToday, "No YouTube today."
	case s.LimitSec >= 0 && st.UsedSec >= s.LimitSec:
		s.Reason, s.Message = ReasonTimeUp, "That's all your YouTube time for today."
	default:
		s.Allowed = true
	}
	return s
}

func nextOpen(ws []Window, mins int) string {
	best := -1
	for _, w := range ws {
		if w.Start > mins && (best < 0 || w.Start < best) {
			best = w.Start
		}
	}
	if best < 0 {
		return ""
	}
	return fmt.Sprintf("%d:%02d", best/60, best%60)
}

// ParseWindows parses "07:00-12:00, 15:30-20:00". Empty means any time.
func ParseWindows(s string) ([]Window, error) {
	var out []Window
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		a, b, ok := strings.Cut(part, "-")
		if !ok {
			return nil, fmt.Errorf("window %q: want HH:MM-HH:MM", part)
		}
		start, err := parseClock(a)
		if err != nil {
			return nil, err
		}
		end, err := parseClock(b)
		if err != nil {
			return nil, err
		}
		if end <= start {
			return nil, fmt.Errorf("window %q ends before it starts", part)
		}
		out = append(out, Window{start, end})
	}
	return out, nil
}

// FormatWindows is the inverse of ParseWindows.
func FormatWindows(ws []Window) string {
	parts := make([]string, len(ws))
	for i, w := range ws {
		parts[i] = fmt.Sprintf("%02d:%02d-%02d:%02d", w.Start/60, w.Start%60, w.End/60, w.End%60)
	}
	return strings.Join(parts, ", ")
}

func parseClock(s string) (int, error) {
	s = strings.TrimSpace(s)
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		m = "0"
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 24 || mm < 0 || mm > 59 || (hh == 24 && mm != 0) {
		return 0, fmt.Errorf("bad time %q", s)
	}
	return hh*60 + mm, nil
}

// EndOfDay returns local midnight after t.
func EndOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, t.Location())
}
