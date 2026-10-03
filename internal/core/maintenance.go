package core

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"ytguard/internal/timekeeper"
)

// Storage limits. History is kept for Settings.HistoryDays; the row caps
// are a safety net so the database can't grow without bound whatever
// happens (e.g. a runaway client).
const (
	DefaultHistoryDays = 365
	maxWatchRows       = 200_000
	maxEventRows       = 200_000
	maxAuditRows       = 5_000
	maxVideoMetaRows   = 50_000
	videoMetaDays      = 90
	findingDays        = 90
	pendingRequestDays = 30
	keepBackups        = 3
)

// CleanupResult says what a cleanup removed.
type CleanupResult struct {
	Rows      int64
	Backups   int
	Compacted bool
}

func (r CleanupResult) String() string {
	s := fmt.Sprintf("removed %d old records", r.Rows)
	if r.Backups > 0 {
		s += fmt.Sprintf(" and %d old database backups", r.Backups)
	}
	if r.Compacted {
		s += "; database compacted"
	}
	return s
}

// HistoryDays is the retention setting, clamped to a sane range.
func HistoryDays(days int) int {
	if days <= 0 {
		return DefaultHistoryDays
	}
	return min(max(days, 7), 3650)
}

// Cleanup removes old history and caps table sizes.
func (a *App) Cleanup() (CleanupResult, error) {
	var res CleanupResult
	s, err := a.St.Settings()
	if err != nil {
		return res, err
	}
	now := a.Now()
	cutoff := now.AddDate(0, 0, -HistoryDays(s.HistoryDays))
	day := timekeeper.Day(cutoff)
	stmts := []struct {
		q    string
		args []any
	}{
		{`DELETE FROM watch_log WHERE day < ?`, []any{day}},
		{`DELETE FROM events WHERE ts < ?`, []any{cutoff.Unix()}},
		{`DELETE FROM requests WHERE status <> 'pending' AND created_at < ?`, []any{cutoff.Unix()}},
		{`DELETE FROM requests WHERE status = 'pending' AND created_at < ?`, []any{now.AddDate(0, 0, -pendingRequestDays).Unix()}},
		{`DELETE FROM grants WHERE day < ?`, []any{day}},
		{`DELETE FROM reports_sent WHERE day < ?`, []any{day}},
		{`DELETE FROM audit WHERE ts < ?`, []any{cutoff.Unix()}},
		{`DELETE FROM video_meta WHERE fetched_at < ?`, []any{now.AddDate(0, 0, -videoMetaDays).Unix()}},
		{`DELETE FROM findings WHERE last_seen < ?`, []any{now.AddDate(0, 0, -findingDays).Unix()}},
		{`DELETE FROM sessions WHERE expires_at <= ?`, []any{now.Unix()}},
		// Row caps: keep the newest N.
		{`DELETE FROM watch_log WHERE rowid IN (SELECT rowid FROM watch_log ORDER BY last_seen DESC LIMIT -1 OFFSET ?)`, []any{maxWatchRows}},
		{`DELETE FROM events WHERE id IN (SELECT id FROM events ORDER BY id DESC LIMIT -1 OFFSET ?)`, []any{maxEventRows}},
		{`DELETE FROM audit WHERE id IN (SELECT id FROM audit ORDER BY id DESC LIMIT -1 OFFSET ?)`, []any{maxAuditRows}},
		{`DELETE FROM video_meta WHERE video_id IN (SELECT video_id FROM video_meta ORDER BY fetched_at DESC LIMIT -1 OFFSET ?)`, []any{maxVideoMetaRows}},
	}
	for _, st := range stmts {
		r, err := a.St.DB.Exec(st.q, st.args...)
		if err != nil {
			return res, fmt.Errorf("cleanup: %w", err)
		}
		n, _ := r.RowsAffected()
		res.Rows += n
	}
	if a.DataDir != "" {
		res.Backups = pruneBackups(a.DataDir, keepBackups)
	}
	// Give space back to the disk when a lot is free.
	var pages, free int64
	_ = a.St.DB.QueryRow(`PRAGMA page_count`).Scan(&pages)
	_ = a.St.DB.QueryRow(`PRAGMA freelist_count`).Scan(&free)
	if free > 1000 && free*5 > pages {
		if _, err := a.St.DB.Exec(`VACUUM`); err == nil {
			res.Compacted = true
		}
	}
	_, _ = a.St.DB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return res, nil
}

// pruneBackups keeps the newest n pre-migration database backups.
func pruneBackups(dir string, n int) int {
	matches, _ := filepath.Glob(filepath.Join(dir, "ytguard.db.backup-*"))
	if len(matches) <= n {
		return 0
	}
	type f struct {
		path string
		mod  time.Time
	}
	var fs []f
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil {
			fs = append(fs, f{m, st.ModTime()})
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].mod.After(fs[j].mod) })
	removed := 0
	for _, old := range fs[min(n, len(fs)):] {
		if os.Remove(old.path) == nil {
			removed++
		}
	}
	return removed
}

// DatabaseSize is the database's size on disk (including its WAL).
func (a *App) DatabaseSize() int64 {
	if a.DataDir == "" {
		return 0
	}
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if st, err := os.Stat(filepath.Join(a.DataDir, "ytguard.db"+suffix)); err == nil {
			total += st.Size()
		}
	}
	return total
}

// HumanSize formats bytes.
func HumanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// RunMaintenance cleans up shortly after start and then daily.
func (a *App) RunMaintenance(ctx context.Context) {
	t := time.NewTimer(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if res, err := a.Cleanup(); err != nil {
			slog.Error("maintenance", "err", err)
		} else if res.Rows > 0 || res.Backups > 0 {
			slog.Info("maintenance: " + res.String())
		}
		t.Reset(24 * time.Hour)
	}
}
