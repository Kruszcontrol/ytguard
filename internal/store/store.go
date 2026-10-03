// Package store persists ytguard state in SQLite.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"ytguard/internal/rules"
	"ytguard/internal/timekeeper"
)

// ErrNotFound is returned when a row doesn't exist.
var ErrNotFound = errors.New("not found")

// Store wraps the database.
type Store struct {
	DB *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS kids (
	id INTEGER PRIMARY KEY,
	linux_user TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL,
	hide_default TEXT NOT NULL DEFAULT 'allow',
	block_default TEXT NOT NULL DEFAULT 'allow',
	options TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS rules (
	id INTEGER PRIMARY KEY,
	tier TEXT NOT NULL, list TEXT NOT NULL, type TEXT NOT NULL,
	value TEXT NOT NULL, extra TEXT NOT NULL DEFAULT '',
	fields TEXT NOT NULL DEFAULT '', match TEXT NOT NULL DEFAULT '',
	kid_id INTEGER NOT NULL DEFAULT 0,
	label TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS rules_lookup ON rules(tier, type, value);
CREATE TABLE IF NOT EXISTS schedules (
	kid_id INTEGER NOT NULL, weekday INTEGER NOT NULL,
	daily_minutes INTEGER NOT NULL, windows TEXT NOT NULL DEFAULT '',
	break_after INTEGER NOT NULL DEFAULT 0, break_len INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (kid_id, weekday)
);
CREATE TABLE IF NOT EXISTS grants (
	id INTEGER PRIMARY KEY, kid_id INTEGER NOT NULL, day TEXT NOT NULL,
	minutes INTEGER NOT NULL, note TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS kid_state (kid_id INTEGER PRIMARY KEY, state TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS video_meta (video_id TEXT PRIMARY KEY, meta TEXT NOT NULL, fetched_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS watch_log (
	kid_id INTEGER NOT NULL, day TEXT NOT NULL, video_id TEXT NOT NULL,
	title TEXT NOT NULL DEFAULT '', channel_id TEXT NOT NULL DEFAULT '', channel_name TEXT NOT NULL DEFAULT '',
	first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL, seconds INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (kid_id, day, video_id)
);
CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY, kid_id INTEGER NOT NULL, ts INTEGER NOT NULL, day TEXT NOT NULL,
	kind TEXT NOT NULL, video_id TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '',
	channel_id TEXT NOT NULL DEFAULT '', channel_name TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_day ON events(kid_id, day);
CREATE TABLE IF NOT EXISTS requests (
	id INTEGER PRIMARY KEY, kid_id INTEGER NOT NULL, video_id TEXT NOT NULL,
	title TEXT NOT NULL DEFAULT '', channel_id TEXT NOT NULL DEFAULT '', channel_name TEXT NOT NULL DEFAULT '',
	reason TEXT NOT NULL DEFAULT '', message TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending', decision TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL, decided_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS admin (id INTEGER PRIMARY KEY CHECK (id = 1), username TEXT NOT NULL, pw_hash TEXT NOT NULL, changed_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (
	id INTEGER PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, csrf TEXT NOT NULL,
	name TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '', user_agent TEXT NOT NULL DEFAULT '',
	remember INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL, last_used INTEGER NOT NULL, expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS api_tokens (
	id INTEGER PRIMARY KEY, name TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE, scopes TEXT NOT NULL,
	created_at INTEGER NOT NULL, last_used INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS audit (id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, actor TEXT NOT NULL, ip TEXT NOT NULL, action TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS reports_sent (kid_id INTEGER NOT NULL, day TEXT NOT NULL, sent_at INTEGER NOT NULL, PRIMARY KEY (kid_id, day));
`

// Open opens (creating if needed) the database in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.Join(dir, "ytguard.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db, dir); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{DB: db}, nil
}

// migrations upgrade the schema step by step. migrations[i] takes the
// database from version i+1 to i+2. Never edit a released migration; add a
// new one at the end.
var migrations = []func(tx *sql.Tx) error{
	// 2: other browsers / video apps found on the PC.
	func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE findings (
			key TEXT PRIMARY KEY, kind TEXT NOT NULL, uid INTEGER NOT NULL, app TEXT NOT NULL,
			location TEXT NOT NULL, how TEXT NOT NULL DEFAULT '',
			first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL,
			notified INTEGER NOT NULL DEFAULT 0, dismissed INTEGER NOT NULL DEFAULT 0)`)
		return err
	},
	// 3: subscribed filter lists.
	func(tx *sql.Tx) error {
		for _, q := range []string{
			`CREATE TABLE lists (
				id INTEGER PRIMARY KEY, url TEXT NOT NULL UNIQUE,
				title TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '', ages TEXT NOT NULL DEFAULT '',
				homepage TEXT NOT NULL DEFAULT '', license TEXT NOT NULL DEFAULT '', version TEXT NOT NULL DEFAULT '',
				kids TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
				etag TEXT NOT NULL DEFAULT '', last_modified TEXT NOT NULL DEFAULT '',
				fetched_at INTEGER NOT NULL DEFAULT 0, checked_at INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '',
				rule_count INTEGER NOT NULL DEFAULT 0, allow_count INTEGER NOT NULL DEFAULT 0, warnings TEXT NOT NULL DEFAULT '',
				added_at INTEGER NOT NULL)`,
			`ALTER TABLE rules ADD COLUMN list_id INTEGER NOT NULL DEFAULT 0`,
			`CREATE INDEX rules_list ON rules(list_id)`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		return nil
	},
}

// SchemaVersion is the schema version this build expects.
func SchemaVersion() int { return 1 + len(migrations) }

// migrate creates the base schema (version 1) and applies newer migrations,
// backing the database up first. A database newer than this build is
// refused so a downgrade can't corrupt it.
func migrate(db *sql.DB, dir string) error {
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		return err
	}
	fresh := ver == 0
	if ver == 0 {
		// Fresh database, or one from before versioning (same schema).
		if _, err := db.Exec(schema); err != nil {
			return err
		}
		if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
			return err
		}
		ver = 1
	}
	if ver > SchemaVersion() {
		return fmt.Errorf("database is schema version %d but this ytguard only knows %d; install a newer ytguard (backups are in %s)", ver, SchemaVersion(), dir)
	}
	if ver == SchemaVersion() {
		return nil
	}
	backup := "(none: new database)"
	if !fresh {
		backup = filepath.Join(dir, fmt.Sprintf("ytguard.db.backup-schema%d-%s", ver, time.Now().Format("20060102-150405")))
		if _, err := db.Exec(`VACUUM INTO ?`, backup); err != nil {
			return fmt.Errorf("backup before migration: %w", err)
		}
	}
	for ; ver < SchemaVersion(); ver++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if err := migrations[ver-1](tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration to schema %d: %w (backup: %s)", ver+1, err, backup)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, ver+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.DB.Close() }

func now() int64 { return time.Now().Unix() }

// ---- settings ----

// Settings are global configuration values edited in the UI.
type Settings struct {
	ReportTime     string `json:"reportTime"` // HH:MM local
	ReportEmail    bool   `json:"reportEmail"`
	SMTPHost       string `json:"smtpHost"`
	SMTPPort       int    `json:"smtpPort"`
	SMTPUser       string `json:"smtpUser"`
	SMTPPass       string `json:"smtpPass"`
	SMTPFrom       string `json:"smtpFrom"`
	SMTPTo         string `json:"smtpTo"`  // comma separated
	SMTPTLS        string `json:"smtpTLS"` // starttls | tls | none
	ReportHA       bool   `json:"reportHA"`
	HAWebhookURL   string `json:"haWebhookURL"`
	HAEvents       bool   `json:"haEvents"`      // approval requests, time up, new logins
	HAInsecureTLS  bool   `json:"haInsecureTLS"` // accept a self-signed HA certificate
	PublicURL      string `json:"publicURL"`
	YouTubeAPIKey  string `json:"youtubeAPIKey"`
	UnmappedPolicy string `json:"unmappedPolicy"` // allow | block: Linux users that aren't kids
	EmbedOrigins   string `json:"embedOrigins"`   // space separated origins allowed to frame the UI
	SessionDays    int    `json:"sessionDays"`
	PCName         string `json:"pcName"`
	UpdateCheck    bool   `json:"updateCheck"`    // check GitHub for new releases
	AppScan        bool   `json:"appScan"`        // look for other browsers / video apps
	ListCatalogURL string `json:"listCatalogURL"` // "" = this build's default catalog

	// Home Assistant over MQTT.
	MQTTEnabled   bool   `json:"mqttEnabled"`
	MQTTHost      string `json:"mqttHost"`
	MQTTPort      int    `json:"mqttPort"`
	MQTTTLS       bool   `json:"mqttTLS"`
	MQTTInsecure  bool   `json:"mqttInsecure"` // accept a self-signed broker certificate
	MQTTUser      string `json:"mqttUser"`
	MQTTPass      string `json:"mqttPass"`
	MQTTBase      string `json:"mqttBase"`      // topic prefix, default "ytguard"
	MQTTDiscovery string `json:"mqttDiscovery"` // HA discovery prefix, default "homeassistant"
}

// DefaultSettings for a fresh install.
func DefaultSettings() Settings {
	host, _ := os.Hostname()
	return Settings{ReportTime: "20:30", SMTPPort: 587, SMTPTLS: "starttls", UnmappedPolicy: "allow", SessionDays: 90, PCName: host, HAEvents: true, UpdateCheck: true, AppScan: true,
		MQTTPort: 1883, MQTTBase: "ytguard", MQTTDiscovery: "homeassistant"}
}

// Settings returns the global settings.
func (s *Store) Settings() (Settings, error) {
	st := DefaultSettings()
	var raw string
	err := s.DB.QueryRow(`SELECT value FROM settings WHERE key='config'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal([]byte(raw), &st)
}

// SaveSettings stores the global settings.
func (s *Store) SaveSettings(st Settings) error {
	b, _ := json.Marshal(st)
	_, err := s.DB.Exec(`INSERT INTO settings(key,value) VALUES('config',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(b))
	return err
}

// GetJSON loads a stored value into v; ok is false if it isn't set.
func (s *Store) GetJSON(key string, v any) (ok bool, err error) {
	var raw string
	err = s.DB.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), v)
}

// SetJSON stores a value.
func (s *Store) SetJSON(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, string(b))
	return err
}

// ---- kids ----

// Shorts modes. These override every filter rule.
const (
	ShortsFilter = "filter" // Shorts follow the normal Hide/Block rules
	ShortsBlock  = "block"  // shown locked; never play; kid can't ask
	ShortsHide   = "hide"   // removed everywhere
)

// KidOptions are per-kid YouTube page tweaks.
type KidOptions struct {
	HideComments    bool   `json:"hideComments"`
	Shorts          string `json:"shorts"`               // filter | block | hide
	HideShorts      bool   `json:"hideShorts,omitempty"` // legacy; read as Shorts=hide
	DisableAutoplay bool   `json:"disableAutoplay"`
}

// ShortsMode returns the effective Shorts setting.
func (o KidOptions) ShortsMode() string {
	switch {
	case o.Shorts == ShortsBlock || o.Shorts == ShortsHide:
		return o.Shorts
	case o.Shorts == "" && o.HideShorts:
		return ShortsHide
	}
	return ShortsFilter
}

// Kid is a child account.
type Kid struct {
	ID           int64      `json:"id"`
	LinuxUser    string     `json:"linuxUser"`
	Name         string     `json:"name"`
	HideDefault  string     `json:"hideDefault"`
	BlockDefault string     `json:"blockDefault"`
	Options      KidOptions `json:"options"`
}

func scanKid(row interface{ Scan(...any) error }) (Kid, error) {
	var k Kid
	var opts string
	if err := row.Scan(&k.ID, &k.LinuxUser, &k.Name, &k.HideDefault, &k.BlockDefault, &opts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return k, ErrNotFound
		}
		return k, err
	}
	_ = json.Unmarshal([]byte(opts), &k.Options)
	k.Options.Shorts, k.Options.HideShorts = k.Options.ShortsMode(), false
	return k, nil
}

const kidCols = `id, linux_user, name, hide_default, block_default, options`

// Kids lists all kids.
func (s *Store) Kids() ([]Kid, error) {
	rows, err := s.DB.Query(`SELECT ` + kidCols + ` FROM kids ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Kid
	for rows.Next() {
		k, err := scanKid(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Kid returns one kid.
func (s *Store) Kid(id int64) (Kid, error) {
	return scanKid(s.DB.QueryRow(`SELECT `+kidCols+` FROM kids WHERE id=?`, id))
}

// KidByUser finds a kid by Linux login name.
func (s *Store) KidByUser(u string) (Kid, error) {
	return scanKid(s.DB.QueryRow(`SELECT `+kidCols+` FROM kids WHERE linux_user=?`, u))
}

// KidByName finds a kid by display name (case-insensitive) or Linux user.
func (s *Store) KidByName(n string) (Kid, error) {
	return scanKid(s.DB.QueryRow(`SELECT `+kidCols+` FROM kids WHERE lower(name)=lower(?) OR linux_user=? LIMIT 1`, n, n))
}

// SaveKid inserts (ID 0) or updates a kid. New kids get a default schedule.
func (s *Store) SaveKid(k *Kid) error {
	if k.HideDefault == "" {
		k.HideDefault = rules.ListAllow
	}
	if k.BlockDefault == "" {
		k.BlockDefault = rules.ListAllow
	}
	opts, _ := json.Marshal(k.Options)
	if k.ID != 0 {
		_, err := s.DB.Exec(`UPDATE kids SET linux_user=?, name=?, hide_default=?, block_default=?, options=? WHERE id=?`,
			k.LinuxUser, k.Name, k.HideDefault, k.BlockDefault, string(opts), k.ID)
		return err
	}
	res, err := s.DB.Exec(`INSERT INTO kids(linux_user, name, hide_default, block_default, options) VALUES(?,?,?,?,?)`,
		k.LinuxUser, k.Name, k.HideDefault, k.BlockDefault, string(opts))
	if err != nil {
		return err
	}
	k.ID, _ = res.LastInsertId()
	def := timekeeper.Schedule{DailyMinutes: 60, Windows: []timekeeper.Window{{Start: 7 * 60, End: 20*60 + 30}}}
	for wd := 0; wd < 7; wd++ {
		if err := s.SaveSchedule(k.ID, wd, def); err != nil {
			return err
		}
	}
	return nil
}

// DeleteKid removes a kid and their data.
func (s *Store) DeleteKid(id int64) error {
	for _, q := range []string{
		`DELETE FROM kids WHERE id=?`, `DELETE FROM rules WHERE kid_id=? AND list_id=0`, `DELETE FROM schedules WHERE kid_id=?`,
		`DELETE FROM grants WHERE kid_id=?`, `DELETE FROM kid_state WHERE kid_id=?`, `DELETE FROM watch_log WHERE kid_id=?`,
		`DELETE FROM events WHERE kid_id=?`, `DELETE FROM requests WHERE kid_id=?`, `DELETE FROM reports_sent WHERE kid_id=?`,
	} {
		if _, err := s.DB.Exec(q, id); err != nil {
			return err
		}
	}
	return s.removeKidFromLists(id)
}

// ---- schedules ----

// Schedule returns a kid's schedule for a weekday (0 = Sunday).
func (s *Store) Schedule(kidID int64, weekday int) (timekeeper.Schedule, error) {
	var sch timekeeper.Schedule
	var win string
	err := s.DB.QueryRow(`SELECT daily_minutes, windows, break_after, break_len FROM schedules WHERE kid_id=? AND weekday=?`, kidID, weekday).
		Scan(&sch.DailyMinutes, &win, &sch.BreakAfter, &sch.BreakLen)
	if errors.Is(err, sql.ErrNoRows) {
		return timekeeper.Schedule{DailyMinutes: -1}, nil
	}
	if err != nil {
		return sch, err
	}
	sch.Windows, _ = timekeeper.ParseWindows(win)
	return sch, nil
}

// Schedules returns all seven weekdays.
func (s *Store) Schedules(kidID int64) ([7]timekeeper.Schedule, error) {
	var out [7]timekeeper.Schedule
	for wd := range out {
		sch, err := s.Schedule(kidID, wd)
		if err != nil {
			return out, err
		}
		out[wd] = sch
	}
	return out, nil
}

// SaveSchedule stores one weekday.
func (s *Store) SaveSchedule(kidID int64, weekday int, sch timekeeper.Schedule) error {
	_, err := s.DB.Exec(`INSERT INTO schedules(kid_id, weekday, daily_minutes, windows, break_after, break_len) VALUES(?,?,?,?,?,?)
		ON CONFLICT(kid_id, weekday) DO UPDATE SET daily_minutes=excluded.daily_minutes, windows=excluded.windows,
		break_after=excluded.break_after, break_len=excluded.break_len`,
		kidID, weekday, sch.DailyMinutes, timekeeper.FormatWindows(sch.Windows), sch.BreakAfter, sch.BreakLen)
	return err
}

// ---- time state & grants ----

// State loads a kid's time counters.
func (s *Store) State(kidID int64) (timekeeper.State, error) {
	var st timekeeper.State
	var raw string
	err := s.DB.QueryRow(`SELECT state FROM kid_state WHERE kid_id=?`, kidID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal([]byte(raw), &st)
}

// SaveState stores a kid's time counters.
func (s *Store) SaveState(kidID int64, st timekeeper.State) error {
	b, _ := json.Marshal(st)
	_, err := s.DB.Exec(`INSERT INTO kid_state(kid_id, state) VALUES(?,?) ON CONFLICT(kid_id) DO UPDATE SET state=excluded.state`, kidID, string(b))
	return err
}

// AddGrant gives bonus minutes for a day (negative to take away).
func (s *Store) AddGrant(kidID int64, day string, minutes int, note string) error {
	_, err := s.DB.Exec(`INSERT INTO grants(kid_id, day, minutes, note, created_at) VALUES(?,?,?,?,?)`, kidID, day, minutes, note, now())
	return err
}

// BonusMinutes sums grants for a day.
func (s *Store) BonusMinutes(kidID int64, day string) (int, error) {
	var n sql.NullInt64
	err := s.DB.QueryRow(`SELECT SUM(minutes) FROM grants WHERE kid_id=? AND day=?`, kidID, day).Scan(&n)
	return int(n.Int64), err
}

// ---- rules ----

// ruleCols/ruleFrom select rules with the name of the list they came from.
const (
	ruleCols = `r.id, r.tier, r.list, r.type, r.value, r.extra, r.fields, r.match, r.kid_id, r.label, r.note, r.list_id, coalesce(l.title, '')`
	ruleFrom = ` FROM rules r LEFT JOIN lists l ON l.id = r.list_id`
)

func scanRule(row interface{ Scan(...any) error }) (rules.Rule, error) {
	var r rules.Rule
	var fields string
	err := row.Scan(&r.ID, &r.Tier, &r.List, &r.Type, &r.Value, &r.Extra, &fields, &r.Match, &r.KidID, &r.Label, &r.Note, &r.Source, &r.SourceName)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if fields != "" {
		r.Fields = strings.Split(fields, ",")
	}
	return r, err
}

func scanRules(rows *sql.Rows, err error) ([]rules.Rule, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rules.Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RuleFilter selects rules. Empty fields match anything; KidID -1 = any.
// ListID 0 (the default) selects the parent's own rules; >0 a list's rules.
type RuleFilter struct {
	Tier, List, Type string
	KidID            int64
	ListID           int64
	Search           string
}

// Rules lists rules matching f.
func (s *Store) Rules(f RuleFilter) ([]rules.Rule, error) {
	q := `SELECT ` + ruleCols + ruleFrom + ` WHERE r.list_id=?`
	args := []any{f.ListID}
	if f.Tier != "" {
		q += ` AND r.tier=?`
		args = append(args, f.Tier)
	}
	if f.List != "" {
		q += ` AND r.list=?`
		args = append(args, f.List)
	}
	if f.Type != "" {
		q += ` AND r.type=?`
		args = append(args, f.Type)
	}
	if f.KidID >= 0 {
		q += ` AND r.kid_id=?`
		args = append(args, f.KidID)
	}
	if f.Search != "" {
		q += ` AND (r.value LIKE ? OR r.label LIKE ? OR r.extra LIKE ? OR r.note LIKE ?)`
		p := "%" + f.Search + "%"
		args = append(args, p, p, p, p)
	}
	q += ` ORDER BY r.type, lower(coalesce(nullif(r.label,''), r.value))`
	return scanRules(s.DB.Query(q, args...))
}

// RulesForKid returns everything that applies to a kid: the parent's rules
// for the kid and for all kids, plus rules from enabled lists the kid is
// subscribed to.
func (s *Store) RulesForKid(kidID int64) ([]rules.Rule, error) {
	return scanRules(s.DB.Query(`SELECT `+ruleCols+ruleFrom+`
		WHERE (r.list_id=0 AND (r.kid_id=0 OR r.kid_id=?))
		   OR (r.list_id>0 AND l.enabled=1 AND (l.kids='' OR (','||l.kids||',') LIKE ?))`,
		kidID, fmt.Sprintf("%%,%d,%%", kidID)))
}

// Rule returns one rule.
func (s *Store) Rule(id int64) (rules.Rule, error) {
	return scanRule(s.DB.QueryRow(`SELECT `+ruleCols+ruleFrom+` WHERE r.id=?`, id))
}

// AddRule inserts a parent rule unless an identical one (tier, list, type,
// value, kid, match, fields) exists; returns the rule's ID either way.
func (s *Store) AddRule(r *rules.Rule) (created bool, err error) {
	fields := strings.Join(r.Fields, ",")
	var id int64
	err = s.DB.QueryRow(`SELECT id FROM rules WHERE list_id=0 AND tier=? AND list=? AND type=? AND value=? AND kid_id=? AND match=? AND fields=?`,
		r.Tier, r.List, r.Type, r.Value, r.KidID, r.Match, fields).Scan(&id)
	if err == nil {
		r.ID = id
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	res, err := s.DB.Exec(`INSERT INTO rules(tier, list, type, value, extra, fields, match, kid_id, label, note, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		r.Tier, r.List, r.Type, r.Value, r.Extra, fields, r.Match, r.KidID, r.Label, r.Note, now())
	if err != nil {
		return false, err
	}
	r.Source = 0
	r.ID, _ = res.LastInsertId()
	return true, nil
}

// UpdateRuleTierList moves a parent rule to another tier and/or list.
func (s *Store) UpdateRuleTierList(id int64, tier, list string) error {
	_, err := s.DB.Exec(`UPDATE rules SET tier=?, list=? WHERE id=? AND list_id=0`, tier, list, id)
	return err
}

// DeleteRule removes a parent rule (list rules change only with the list).
func (s *Store) DeleteRule(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM rules WHERE id=? AND list_id=0`, id)
	return err
}

// DeleteRulesWhere removes parent rules matching exactly (used when approving).
func (s *Store) DeleteRulesWhere(tier, list, typ, value string, kidID int64) error {
	_, err := s.DB.Exec(`DELETE FROM rules WHERE list_id=0 AND tier=? AND list=? AND type=? AND value=? AND kid_id=?`, tier, list, typ, value, kidID)
	return err
}

// ---- video metadata cache ----

// Meta returns cached metadata for a video.
func (s *Store) Meta(videoID string) (rules.Meta, bool) {
	var raw string
	if err := s.DB.QueryRow(`SELECT meta FROM video_meta WHERE video_id=?`, videoID).Scan(&raw); err != nil {
		return rules.Meta{}, false
	}
	var m rules.Meta
	if json.Unmarshal([]byte(raw), &m) != nil {
		return m, false
	}
	return m, true
}

// PutMeta caches metadata (only overwrites with equal or better data).
func (s *Store) PutMeta(m rules.Meta) error {
	if m.VideoID == "" {
		return nil
	}
	if old, ok := s.Meta(m.VideoID); ok {
		if old.Full && !m.Full {
			if !m.IsShort || old.IsShort {
				return nil
			}
			old.IsShort = true // only learn that it's a Short
			m = old
		}
		m.Merge(old)
	}
	b, _ := json.Marshal(m)
	_, err := s.DB.Exec(`INSERT INTO video_meta(video_id, meta, fetched_at) VALUES(?,?,?) ON CONFLICT(video_id) DO UPDATE SET meta=excluded.meta, fetched_at=excluded.fetched_at`,
		m.VideoID, string(b), now())
	return err
}

// ---- watch log & events ----

// Watch is one video in a kid's day.
type Watch struct {
	VideoID     string `json:"videoId"`
	Title       string `json:"title"`
	ChannelID   string `json:"channelId"`
	ChannelName string `json:"channelName"`
	FirstSeen   int64  `json:"firstSeen"`
	LastSeen    int64  `json:"lastSeen"`
	Seconds     int    `json:"seconds"`
}

// AddWatch records play time for a video.
func (s *Store) AddWatch(kidID int64, day string, m rules.Meta, seconds int, ts int64) error {
	_, err := s.DB.Exec(`INSERT INTO watch_log(kid_id, day, video_id, title, channel_id, channel_name, first_seen, last_seen, seconds)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(kid_id, day, video_id) DO UPDATE SET last_seen=excluded.last_seen, seconds=seconds+excluded.seconds,
		title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE title END,
		channel_id=CASE WHEN excluded.channel_id<>'' THEN excluded.channel_id ELSE channel_id END,
		channel_name=CASE WHEN excluded.channel_name<>'' THEN excluded.channel_name ELSE channel_name END`,
		kidID, day, m.VideoID, m.Title, m.ChannelID, m.ChannelName, ts, ts, seconds)
	return err
}

// Watches lists a kid's videos for a day, oldest first.
func (s *Store) Watches(kidID int64, day string) ([]Watch, error) {
	rows, err := s.DB.Query(`SELECT video_id, title, channel_id, channel_name, first_seen, last_seen, seconds FROM watch_log WHERE kid_id=? AND day=? ORDER BY first_seen`, kidID, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Watch
	for rows.Next() {
		var w Watch
		if err := rows.Scan(&w.VideoID, &w.Title, &w.ChannelID, &w.ChannelName, &w.FirstSeen, &w.LastSeen, &w.Seconds); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// LastWatch returns the most recently played video for a kid.
func (s *Store) LastWatch(kidID int64) (Watch, bool) {
	var w Watch
	err := s.DB.QueryRow(`SELECT video_id, title, channel_id, channel_name, first_seen, last_seen, seconds FROM watch_log WHERE kid_id=? ORDER BY last_seen DESC LIMIT 1`, kidID).
		Scan(&w.VideoID, &w.Title, &w.ChannelID, &w.ChannelName, &w.FirstSeen, &w.LastSeen, &w.Seconds)
	return w, err == nil
}

// Event kinds.
const (
	EventBlocked = "blocked"
	EventHidden  = "hidden"
)

// Event is a blocked or hidden attempt.
type Event struct {
	ID          int64  `json:"id"`
	TS          int64  `json:"ts"`
	Kind        string `json:"kind"`
	VideoID     string `json:"videoId"`
	Title       string `json:"title"`
	ChannelID   string `json:"channelId"`
	ChannelName string `json:"channelName"`
	Reason      string `json:"reason"`
}

// AddEvent records an attempt, skipping duplicates within 10 minutes.
func (s *Store) AddEvent(kidID int64, day, kind string, m rules.Meta, reason string) error {
	ts := now()
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM events WHERE kid_id=? AND kind=? AND video_id=? AND ts>?`, kidID, kind, m.VideoID, ts-600).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.DB.Exec(`INSERT INTO events(kid_id, ts, day, kind, video_id, title, channel_id, channel_name, reason) VALUES(?,?,?,?,?,?,?,?,?)`,
		kidID, ts, day, kind, m.VideoID, m.Title, m.ChannelID, m.ChannelName, reason)
	return err
}

// LatestEvent returns a kid's most recent blocked/hidden event for a video
// after t.
func (s *Store) LatestEvent(kidID int64, videoID string, t time.Time) (Event, error) {
	var e Event
	err := s.DB.QueryRow(`SELECT id, ts, kind, video_id, title, channel_id, channel_name, reason FROM events
		WHERE kid_id=? AND video_id=? AND ts>? ORDER BY id DESC LIMIT 1`, kidID, videoID, t.Unix()).
		Scan(&e.ID, &e.TS, &e.Kind, &e.VideoID, &e.Title, &e.ChannelID, &e.ChannelName, &e.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

// Events lists a kid's events for a day.
func (s *Store) Events(kidID int64, day string) ([]Event, error) {
	rows, err := s.DB.Query(`SELECT id, ts, kind, video_id, title, channel_id, channel_name, reason FROM events WHERE kid_id=? AND day=? ORDER BY ts`, kidID, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.VideoID, &e.Title, &e.ChannelID, &e.ChannelName, &e.Reason); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- approval requests ----

// Request statuses.
const (
	RequestPending  = "pending"
	RequestApproved = "approved"
	RequestDenied   = "denied"
)

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
	Status      string `json:"status"`
	Decision    string `json:"decision"`
	CreatedAt   int64  `json:"createdAt"`
	DecidedAt   int64  `json:"decidedAt"`
}

const reqCols = `r.id, r.kid_id, coalesce(k.name,''), r.video_id, r.title, r.channel_id, r.channel_name, r.reason, r.message, r.status, r.decision, r.created_at, r.decided_at`

func scanRequest(row interface{ Scan(...any) error }) (Request, error) {
	var r Request
	err := row.Scan(&r.ID, &r.KidID, &r.KidName, &r.VideoID, &r.Title, &r.ChannelID, &r.ChannelName, &r.Reason, &r.Message, &r.Status, &r.Decision, &r.CreatedAt, &r.DecidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// CreateRequest adds a pending request (or returns the existing pending one).
func (s *Store) CreateRequest(r *Request) (created bool, err error) {
	if old, err := s.LatestRequest(r.KidID, r.VideoID); err == nil && old.Status == RequestPending {
		*r = old
		return false, nil
	}
	r.Status, r.CreatedAt = RequestPending, now()
	res, err := s.DB.Exec(`INSERT INTO requests(kid_id, video_id, title, channel_id, channel_name, reason, message, status, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		r.KidID, r.VideoID, r.Title, r.ChannelID, r.ChannelName, r.Reason, r.Message, r.Status, r.CreatedAt)
	if err != nil {
		return false, err
	}
	r.ID, _ = res.LastInsertId()
	return true, nil
}

// CountRequestsSince counts requests a kid created after t.
func (s *Store) CountRequestsSince(kidID int64, t time.Time) (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM requests WHERE kid_id=? AND created_at>?`, kidID, t.Unix()).Scan(&n)
	return n, err
}

// Request returns one request.
func (s *Store) Request(id int64) (Request, error) {
	return scanRequest(s.DB.QueryRow(`SELECT `+reqCols+` FROM requests r LEFT JOIN kids k ON k.id=r.kid_id WHERE r.id=?`, id))
}

// LatestRequest returns the newest request by a kid for a video.
func (s *Store) LatestRequest(kidID int64, videoID string) (Request, error) {
	return scanRequest(s.DB.QueryRow(`SELECT `+reqCols+` FROM requests r LEFT JOIN kids k ON k.id=r.kid_id WHERE r.kid_id=? AND r.video_id=? ORDER BY r.id DESC LIMIT 1`, kidID, videoID))
}

// Requests lists requests with a status ("" = all), newest first.
func (s *Store) Requests(status string, limit int) ([]Request, error) {
	q := `SELECT ` + reqCols + ` FROM requests r LEFT JOIN kids k ON k.id=r.kid_id`
	var args []any
	if status != "" {
		q += ` WHERE r.status=?`
		args = append(args, status)
	}
	q += ` ORDER BY r.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RequestsForDay lists a kid's requests made on a day.
func (s *Store) RequestsForDay(kidID int64, from, to time.Time) ([]Request, error) {
	rows, err := s.DB.Query(`SELECT `+reqCols+` FROM requests r LEFT JOIN kids k ON k.id=r.kid_id WHERE r.kid_id=? AND r.created_at>=? AND r.created_at<? ORDER BY r.id`,
		kidID, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DecideRequest marks a request approved/denied.
func (s *Store) DecideRequest(id int64, status, decision string) error {
	_, err := s.DB.Exec(`UPDATE requests SET status=?, decision=?, decided_at=? WHERE id=?`, status, decision, now(), id)
	return err
}

// ---- reports ----

// ReportSent reports whether a kid's report for day was sent.
func (s *Store) ReportSent(kidID int64, day string) bool {
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM reports_sent WHERE kid_id=? AND day=?`, kidID, day).Scan(&n)
	return n > 0
}

// MarkReportSent records that a report went out.
func (s *Store) MarkReportSent(kidID int64, day string) error {
	_, err := s.DB.Exec(`INSERT OR REPLACE INTO reports_sent(kid_id, day, sent_at) VALUES(?,?,?)`, kidID, day, now())
	return err
}

// ---- other browsers / apps ----

// Finding is a browser or video app other than the managed Chrome.
type Finding struct {
	Key       string `json:"key"`
	Kind      string `json:"kind"` // running | installed
	UID       int    `json:"uid"`  // -1 = everyone
	App       string `json:"app"`
	Location  string `json:"location"`
	How       string `json:"how"`
	FirstSeen int64  `json:"firstSeen"`
	LastSeen  int64  `json:"lastSeen"`
	Notified  bool   `json:"notified"`
	Dismissed bool   `json:"dismissed"`
}

// UpsertFinding records a sighting.
func (s *Store) UpsertFinding(f Finding, ts int64) error {
	_, err := s.DB.Exec(`INSERT INTO findings(key, kind, uid, app, location, how, first_seen, last_seen) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET last_seen=excluded.last_seen, app=excluded.app, how=excluded.how`,
		f.Key, f.Kind, f.UID, f.App, f.Location, f.How, ts, ts)
	return err
}

// Findings lists findings seen after since, newest first.
func (s *Store) Findings(since int64, includeDismissed bool) ([]Finding, error) {
	q := `SELECT key, kind, uid, app, location, how, first_seen, last_seen, notified, dismissed FROM findings WHERE last_seen>?`
	if !includeDismissed {
		q += ` AND dismissed=0`
	}
	rows, err := s.DB.Query(q+` ORDER BY last_seen DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var f Finding
		if err := rows.Scan(&f.Key, &f.Kind, &f.UID, &f.App, &f.Location, &f.How, &f.FirstSeen, &f.LastSeen, &f.Notified, &f.Dismissed); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetFindingFlags marks a finding notified and/or dismissed.
func (s *Store) SetFindingFlags(key string, notified, dismissed *bool) error {
	if notified != nil {
		if _, err := s.DB.Exec(`UPDATE findings SET notified=? WHERE key=?`, *notified, key); err != nil {
			return err
		}
	}
	if dismissed != nil {
		_, err := s.DB.Exec(`UPDATE findings SET dismissed=? WHERE key=?`, *dismissed, key)
		return err
	}
	return nil
}

// ---- audit ----

// AuditEntry is one logged admin action.
type AuditEntry struct {
	TS     int64
	Actor  string
	IP     string
	Action string
	Detail string
}

// Audit logs an admin action.
func (s *Store) Audit(actor, ip, action, detail string) {
	_, _ = s.DB.Exec(`INSERT INTO audit(ts, actor, ip, action, detail) VALUES(?,?,?,?,?)`, now(), actor, ip, action, detail)
}

// AuditLog returns recent entries.
func (s *Store) AuditLog(limit int) ([]AuditEntry, error) {
	rows, err := s.DB.Query(`SELECT ts, actor, ip, action, detail FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.TS, &e.Actor, &e.IP, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
