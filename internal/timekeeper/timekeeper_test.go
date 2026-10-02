package timekeeper

import (
	"testing"
	"time"
)

var base = time.Date(2026, 10, 2, 12, 0, 0, 0, time.Local)

// play simulates continuous playback with heartbeats every 15 s for d.
func play(st *State, sch Schedule, bonus int, from time.Time, d time.Duration) (time.Time, Status) {
	now := from
	s, _ := Heartbeat(st, sch, bonus, now, true, true)
	for end := from.Add(d); now.Before(end); {
		now = now.Add(HeartbeatInterval * time.Second)
		s, _ = Heartbeat(st, sch, bonus, now, true, false)
	}
	return now, s
}

func TestDailyLimit(t *testing.T) {
	st := &State{}
	sch := Schedule{DailyMinutes: 10}
	_, s := play(st, sch, 0, base, 9*time.Minute)
	if !s.Allowed || st.UsedSec != 540 {
		t.Fatalf("after 9 min: allowed=%v used=%d", s.Allowed, st.UsedSec)
	}
	_, s = play(st, sch, 0, base.Add(9*time.Minute+15*time.Second), 2*time.Minute)
	if s.Allowed || s.Reason != ReasonTimeUp {
		t.Fatalf("want time up, got %+v", s)
	}
	if st.UsedSec > 600 {
		t.Fatalf("counted past limit: %d", st.UsedSec)
	}
}

func TestBonus(t *testing.T) {
	st := &State{Day: Day(base), UsedSec: 600}
	sch := Schedule{DailyMinutes: 10}
	Roll(st, sch, base)
	if Evaluate(st, sch, 0, base).Allowed {
		t.Fatal("should be time up")
	}
	s := Evaluate(st, sch, 15, base)
	if !s.Allowed || s.RemainingSec != 900 {
		t.Fatalf("bonus: %+v", s)
	}
}

func TestMultiTabCountedOnce(t *testing.T) {
	st := &State{}
	sch := Schedule{DailyMinutes: -1}
	now := base
	Heartbeat(st, sch, 0, now, true, true)
	// Two tabs, offset by 7 s, both heartbeating every 15 s for 5 minutes.
	for i := 0; i < 20; i++ {
		Heartbeat(st, sch, 0, now.Add(7*time.Second), true, false)
		now = now.Add(15 * time.Second)
		Heartbeat(st, sch, 0, now, true, false)
	}
	if st.UsedSec != 300 {
		t.Fatalf("used = %d, want 300", st.UsedSec)
	}
}

func TestPauseNotCounted(t *testing.T) {
	st := &State{}
	sch := Schedule{DailyMinutes: -1}
	end, _ := play(st, sch, 0, base, time.Minute)
	used := st.UsedSec
	// Paused for 5 minutes, then resumes.
	Heartbeat(st, sch, 0, end.Add(5*time.Minute), true, true)
	if st.UsedSec != used {
		t.Fatalf("pause counted: %d -> %d", used, st.UsedSec)
	}
}

func TestBreakCycle(t *testing.T) {
	st := &State{}
	sch := Schedule{DailyMinutes: 60, BreakAfter: 20, BreakLen: 10}
	now, s := play(st, sch, 0, base, 20*time.Minute)
	if s.Allowed || s.Reason != ReasonBreak {
		t.Fatalf("want break after 20 min, got %+v", s)
	}
	// During break nothing counts.
	used := st.UsedSec
	Heartbeat(st, sch, 0, now.Add(5*time.Minute), true, true)
	if st.UsedSec != used {
		t.Fatal("counted during break")
	}
	// After break, allowed again with full break budget.
	after := now.Add(10*time.Minute + time.Second)
	s, _ = Heartbeat(st, sch, 0, after, true, true)
	if !s.Allowed || s.BreakInSec != 1200 {
		t.Fatalf("after break: %+v", s)
	}
	// Three full cycles exhaust the hour.
	now, _ = play(st, sch, 0, after, 20*time.Minute)
	now, _ = play(st, sch, 0, now.Add(10*time.Minute+time.Second), 20*time.Minute)
	if st.UsedSec < 3600 {
		t.Fatalf("used %d", st.UsedSec)
	}
	s, _ = Heartbeat(st, sch, 0, now.Add(11*time.Minute), true, true)
	if s.Allowed || s.Reason != ReasonTimeUp {
		t.Fatalf("want time up, got %+v", s)
	}
}

func TestNaturalBreakResetsContinuous(t *testing.T) {
	st := &State{}
	sch := Schedule{DailyMinutes: -1, BreakAfter: 20, BreakLen: 10}
	now, _ := play(st, sch, 0, base, 15*time.Minute)
	s, _ := Heartbeat(st, sch, 0, now.Add(11*time.Minute), true, true)
	if s.BreakInSec != 1200 {
		t.Fatalf("break-in = %d, want reset to 1200", s.BreakInSec)
	}
}

func TestWindows(t *testing.T) {
	ws, err := ParseWindows("07:00-08:00, 15:30-20:00")
	if err != nil {
		t.Fatal(err)
	}
	sch := Schedule{DailyMinutes: -1, Windows: ws}
	st := &State{}
	at := func(h, m int) time.Time { return time.Date(2026, 10, 2, h, m, 0, 0, time.Local) }
	if s := Evaluate(st, sch, 0, at(12, 0)); s.Allowed || s.NextOpen != "15:30" {
		t.Fatalf("noon: %+v", s)
	}
	if s := Evaluate(st, sch, 0, at(19, 50)); !s.Allowed || s.RemainingSec != 600 {
		t.Fatalf("19:50: %+v", s)
	}
	if s := Evaluate(st, sch, 0, at(21, 0)); s.Allowed || s.NextOpen != "" {
		t.Fatalf("21:00: %+v", s)
	}
	if FormatWindows(ws) != "07:00-08:00, 15:30-20:00" {
		t.Fatal(FormatWindows(ws))
	}
	if _, err := ParseWindows("9-8"); err == nil {
		t.Fatal("want error")
	}
}

func TestMidnightRollover(t *testing.T) {
	st := &State{}
	sch := Schedule{DailyMinutes: 10}
	late := time.Date(2026, 10, 2, 23, 50, 0, 0, time.Local)
	play(st, sch, 0, late, 9*time.Minute)
	next := time.Date(2026, 10, 3, 8, 0, 0, 0, time.Local)
	s, _ := Heartbeat(st, sch, 0, next, true, true)
	if !s.Allowed || st.UsedSec != 0 || st.Day != "2026-10-03" {
		t.Fatalf("rollover: %+v %+v", s, st)
	}
}

func TestLock(t *testing.T) {
	st := &State{LockedUntil: base.Add(time.Hour).Unix()}
	sch := Schedule{DailyMinutes: -1}
	Roll(st, sch, base)
	if s := Evaluate(st, sch, 0, base); s.Allowed || s.Reason != ReasonLocked {
		t.Fatalf("%+v", s)
	}
	Roll(st, sch, base.Add(2*time.Hour))
	if s := Evaluate(st, sch, 0, base.Add(2*time.Hour)); !s.Allowed {
		t.Fatalf("%+v", s)
	}
}

func TestNoneToday(t *testing.T) {
	st := &State{}
	if s := Evaluate(st, Schedule{DailyMinutes: 0}, 0, base); s.Allowed || s.Reason != ReasonNoneToday {
		t.Fatalf("%+v", s)
	}
	if s := Evaluate(st, Schedule{DailyMinutes: 0}, 20, base); !s.Allowed {
		t.Fatalf("bonus on none day: %+v", s)
	}
}
