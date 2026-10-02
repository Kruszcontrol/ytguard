package store

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"ytguard/internal/rules"
)

// FilterList is a subscribed public filter list.
type FilterList struct {
	ID           int64    `json:"id"`
	URL          string   `json:"url"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Ages         string   `json:"ages"`
	Homepage     string   `json:"homepage"`
	License      string   `json:"license"`
	Version      string   `json:"version"`
	Kids         []int64  `json:"kids"` // empty = all kids
	Enabled      bool     `json:"enabled"`
	ETag         string   `json:"-"`
	LastModified string   `json:"-"`
	FetchedAt    int64    `json:"fetchedAt"` // last time the content was downloaded
	CheckedAt    int64    `json:"checkedAt"` // last time we asked for updates
	Error        string   `json:"error,omitempty"`
	RuleCount    int      `json:"ruleCount"`
	AllowCount   int      `json:"allowCount"`
	Warnings     []string `json:"warnings,omitempty"`
	AddedAt      int64    `json:"addedAt"`
}

// AppliesTo reports whether the list is used for a kid.
func (l FilterList) AppliesTo(kidID int64) bool {
	if len(l.Kids) == 0 {
		return true
	}
	for _, k := range l.Kids {
		if k == kidID {
			return true
		}
	}
	return false
}

const listCols = `id, url, title, description, ages, homepage, license, version, kids, enabled, etag, last_modified,
	fetched_at, checked_at, error, rule_count, allow_count, warnings, added_at`

func scanList(row interface{ Scan(...any) error }) (FilterList, error) {
	var l FilterList
	var kids, warnings string
	err := row.Scan(&l.ID, &l.URL, &l.Title, &l.Description, &l.Ages, &l.Homepage, &l.License, &l.Version, &kids, &l.Enabled,
		&l.ETag, &l.LastModified, &l.FetchedAt, &l.CheckedAt, &l.Error, &l.RuleCount, &l.AllowCount, &warnings, &l.AddedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	l.Kids = parseIDs(kids)
	if warnings != "" {
		l.Warnings = strings.Split(warnings, "\n")
	}
	return l, err
}

func parseIDs(s string) []int64 {
	var out []int64
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

// Lists returns subscribed lists.
func (s *Store) Lists() ([]FilterList, error) {
	rows, err := s.DB.Query(`SELECT ` + listCols + ` FROM lists ORDER BY lower(title), id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FilterList
	for rows.Next() {
		l, err := scanList(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// List returns one subscribed list.
func (s *Store) List(id int64) (FilterList, error) {
	return scanList(s.DB.QueryRow(`SELECT `+listCols+` FROM lists WHERE id=?`, id))
}

// ListByURL finds a subscription by URL.
func (s *Store) ListByURL(url string) (FilterList, error) {
	return scanList(s.DB.QueryRow(`SELECT `+listCols+` FROM lists WHERE url=?`, url))
}

// AddList subscribes to a URL (no rules until it's fetched).
func (s *Store) AddList(l *FilterList) error {
	l.AddedAt = now()
	res, err := s.DB.Exec(`INSERT INTO lists(url, title, kids, enabled, added_at) VALUES(?,?,?,?,?)`,
		l.URL, l.Title, joinIDs(l.Kids), l.Enabled, l.AddedAt)
	if err != nil {
		return err
	}
	l.ID, _ = res.LastInsertId()
	return nil
}

// SetListOptions changes which kids a list applies to and whether it's on.
func (s *Store) SetListOptions(id int64, kids []int64, enabled bool) error {
	_, err := s.DB.Exec(`UPDATE lists SET kids=?, enabled=? WHERE id=?`, joinIDs(kids), enabled, id)
	return err
}

// SetListChecked records an update check that didn't change the content.
func (s *Store) SetListChecked(id int64, checked int64, errMsg string) error {
	_, err := s.DB.Exec(`UPDATE lists SET checked_at=?, error=? WHERE id=?`, checked, errMsg, id)
	return err
}

// ReplaceListContent stores a freshly downloaded list: its header, its
// rules (replacing the old ones) and cache validators, in one transaction.
func (s *Store) ReplaceListContent(l FilterList, rs []rules.Rule) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM rules WHERE list_id=?`, l.ID); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO rules(tier, list, type, value, extra, fields, match, kid_id, label, note, created_at, list_id)
		VALUES(?,?,?,?,?,?,?,0,?,'',?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	allow := 0
	ts := now()
	for _, r := range rs {
		if r.List == rules.ListAllow {
			allow++
		}
		if _, err := stmt.Exec(r.Tier, r.List, r.Type, r.Value, r.Extra, strings.Join(r.Fields, ","), r.Match, r.Label, ts, l.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE lists SET title=?, description=?, ages=?, homepage=?, license=?, version=?, etag=?, last_modified=?,
		fetched_at=?, checked_at=?, error='', rule_count=?, allow_count=?, warnings=? WHERE id=?`,
		l.Title, l.Description, l.Ages, l.Homepage, l.License, l.Version, l.ETag, l.LastModified,
		l.FetchedAt, l.CheckedAt, len(rs), allow, strings.Join(l.Warnings, "\n"), l.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteList unsubscribes and removes the list's rules.
func (s *Store) DeleteList(id int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM rules WHERE list_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM lists WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// removeKidFromLists drops a deleted kid from list subscriptions (so a
// future kid reusing the ID doesn't inherit them). A list left with no kids
// is switched off rather than applying to everyone.
func (s *Store) removeKidFromLists(kidID int64) error {
	ls, err := s.Lists()
	if err != nil {
		return err
	}
	for _, l := range ls {
		if len(l.Kids) == 0 || !l.AppliesTo(kidID) {
			continue
		}
		var keep []int64
		for _, k := range l.Kids {
			if k != kidID {
				keep = append(keep, k)
			}
		}
		if err := s.SetListOptions(l.ID, keep, l.Enabled && len(keep) > 0); err != nil {
			return err
		}
	}
	return nil
}
