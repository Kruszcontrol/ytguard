package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"ytguard/internal/rules"
	"ytguard/internal/store"
)

func TestCleanup(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := New(st)
	a.DataDir = dir
	k := store.Kid{LinuxUser: "k", Name: "K"}
	st.SaveKid(&k)
	s, _ := st.Settings()
	s.HistoryDays = 30
	st.SaveSettings(s)

	now := time.Now()
	old, recent := now.AddDate(0, 0, -40), now.AddDate(0, 0, -5)
	for _, ts := range []time.Time{old, recent} {
		st.AddWatch(k.ID, ts.Format("2006-01-02"), rules.Meta{VideoID: "v" + ts.Format("0102")}, 60, ts.Unix())
		st.DB.Exec(`INSERT INTO events(kid_id, ts, day, kind, video_id) VALUES(?,?,?,?,?)`, k.ID, ts.Unix(), ts.Format("2006-01-02"), "blocked", "x")
		st.DB.Exec(`INSERT INTO audit(ts, actor, ip, action) VALUES(?,?,?,?)`, ts.Unix(), "a", "ip", "x")
	}
	st.DB.Exec(`INSERT INTO requests(kid_id, video_id, status, created_at) VALUES(?,?,?,?)`, k.ID, "p", "pending", now.AddDate(0, 0, -31).Unix())
	st.DB.Exec(`INSERT INTO requests(kid_id, video_id, status, created_at) VALUES(?,?,?,?)`, k.ID, "q", "pending", recent.Unix())
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, "ytguard.db.backup-schema1-"+string(rune('a'+i)))
		os.WriteFile(p, []byte("x"), 0o600)
		os.Chtimes(p, now.Add(time.Duration(i)*time.Minute), now.Add(time.Duration(i)*time.Minute))
	}

	res, err := a.Cleanup()
	if err != nil {
		t.Fatal(err)
	}
	count := func(q string) (n int) { st.DB.QueryRow(q).Scan(&n); return }
	if count(`SELECT COUNT(*) FROM watch_log`) != 1 || count(`SELECT COUNT(*) FROM events`) != 1 ||
		count(`SELECT COUNT(*) FROM audit`) != 1 || count(`SELECT COUNT(*) FROM requests`) != 1 {
		t.Fatalf("old rows not removed: %+v", res)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "ytguard.db.backup-*"))
	if len(backups) != keepBackups || res.Backups != 2 {
		t.Fatalf("backups left %d, removed %d", len(backups), res.Backups)
	}
	if a.DatabaseSize() <= 0 {
		t.Fatal("database size")
	}
	if HistoryDays(0) != DefaultHistoryDays || HistoryDays(1) != 7 || HistoryDays(99999) != 3650 {
		t.Fatal("HistoryDays clamp")
	}
}
