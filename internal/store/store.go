// Package store persists Shopify discount export snapshots in a local SQLite
// database and answers the questions the dashboard and reports need: what does
// each code look like now, how has its usage changed over time, and which codes
// appeared or disappeared between exports.
package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/OptimumMeans/ShopifyDiscount/internal/csvimport"
	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and runs migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite + serial writes; keeps things simple and safe
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS snapshots (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	taken_at    TEXT NOT NULL,
	imported_at TEXT NOT NULL,
	source_file TEXT NOT NULL,
	file_hash   TEXT NOT NULL UNIQUE,
	row_count   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS discount_rows (
	snapshot_id        INTEGER NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
	name               TEXT NOT NULL,
	value              REAL,
	value_type         TEXT,
	type               TEXT,
	discount_class     TEXT,
	min_purchase       TEXT,
	combines_order     TEXT,
	combines_product   TEXT,
	combines_shipping  TEXT,
	customer_selection TEXT,
	context            TEXT,
	times_used         INTEGER NOT NULL,
	applies_once       TEXT,
	usage_limit        TEXT,
	status             TEXT,
	start_at           TEXT,
	end_at             TEXT,
	PRIMARY KEY (snapshot_id, name)
);

CREATE INDEX IF NOT EXISTS idx_rows_name ON discount_rows(name);

CREATE TABLE IF NOT EXISTS notes (
	name       TEXT PRIMARY KEY,
	note       TEXT NOT NULL DEFAULT '',
	tags       TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS revenue (
	name           TEXT PRIMARY KEY,
	total_discount REAL NOT NULL,
	order_count    INTEGER NOT NULL,
	currency       TEXT NOT NULL DEFAULT '',
	computed_at    TEXT NOT NULL
);
`
	_, err := s.db.Exec(schema)
	return err
}

// SnapshotMeta describes a single import.
type SnapshotMeta struct {
	ID         int64
	TakenAt    time.Time
	ImportedAt time.Time
	SourceFile string
	FileHash   string
	RowCount   int
}

// FindByHash returns the snapshot previously imported from an identical file, or
// (nil, nil) if none exists. Used to keep imports idempotent.
func (s *Store) FindByHash(hash string) (*SnapshotMeta, error) {
	row := s.db.QueryRow(`SELECT id, taken_at, imported_at, source_file, file_hash, row_count
		FROM snapshots WHERE file_hash = ?`, hash)
	m, err := scanSnapshot(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// Import stores a parsed file as a new snapshot. The caller is responsible for
// checking FindByHash first; Import enforces uniqueness on file_hash regardless.
func (s *Store) Import(meta SnapshotMeta, rows []csvimport.Row) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`INSERT INTO snapshots (taken_at, imported_at, source_file, file_hash, row_count)
		VALUES (?, ?, ?, ?, ?)`,
		meta.TakenAt.UTC().Format(time.RFC3339), meta.ImportedAt.UTC().Format(time.RFC3339),
		meta.SourceFile, meta.FileHash, len(rows))
	if err != nil {
		return 0, fmt.Errorf("insert snapshot: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	stmt, err := tx.Prepare(`INSERT INTO discount_rows
		(snapshot_id, name, value, value_type, type, discount_class, min_purchase,
		 combines_order, combines_product, combines_shipping, customer_selection,
		 context, times_used, applies_once, usage_limit, status, start_at, end_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.Exec(id, r.Name, r.Value, r.ValueType, r.Type, r.DiscountClass,
			r.MinPurchase, r.CombinesOrder, r.CombinesProduct, r.CombinesShipping,
			r.CustomerSelection, r.Context, r.TimesUsed, r.AppliesOnce, r.UsageLimit,
			r.Status, nullable(r.StartAt), nullable(r.EndAt)); err != nil {
			return 0, fmt.Errorf("insert row %q: %w", r.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// Snapshots returns all snapshots, newest first.
func (s *Store) Snapshots() ([]SnapshotMeta, error) {
	rows, err := s.db.Query(`SELECT id, taken_at, imported_at, source_file, file_hash, row_count
		FROM snapshots ORDER BY taken_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotMeta
	for rows.Next() {
		m, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// LatestSnapshot returns the most recent snapshot by taken_at, or nil if empty.
func (s *Store) LatestSnapshot() (*SnapshotMeta, error) {
	row := s.db.QueryRow(`SELECT id, taken_at, imported_at, source_file, file_hash, row_count
		FROM snapshots ORDER BY taken_at DESC, id DESC LIMIT 1`)
	m, err := scanSnapshot(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// previousSnapshotID returns the id of the snapshot taken immediately before the
// given one, or (0, false) when there is no earlier snapshot.
func (s *Store) previousSnapshotID(snapshotID int64) (int64, bool, error) {
	row := s.db.QueryRow(`SELECT prev.id FROM snapshots cur
		JOIN snapshots prev ON (prev.taken_at, prev.id) < (cur.taken_at, cur.id)
		WHERE cur.id = ?
		ORDER BY prev.taken_at DESC, prev.id DESC LIMIT 1`, snapshotID)
	var id int64
	switch err := row.Scan(&id); err {
	case nil:
		return id, true, nil
	case sql.ErrNoRows:
		return 0, false, nil
	default:
		return 0, false, err
	}
}

// DiscountView is one code within a snapshot, enriched with its delta versus the
// previous snapshot.
type DiscountView struct {
	csvimport.Row
	PrevTimesUsed int  // times_used in the previous snapshot
	HadPrev       bool // whether the code existed in the previous snapshot
	Delta         int  // TimesUsed - PrevTimesUsed (0 when no previous)
	IsNew         bool // appeared since the previous snapshot
}

// SnapshotDiscounts returns every code in a snapshot, enriched with deltas
// against the previous snapshot, sorted by times-used descending.
func (s *Store) SnapshotDiscounts(snapshotID int64) ([]DiscountView, error) {
	prevID, hasPrev, err := s.previousSnapshotID(snapshotID)
	if err != nil {
		return nil, err
	}
	prevUsed := map[string]int{}
	if hasPrev {
		pr, err := s.db.Query(`SELECT name, times_used FROM discount_rows WHERE snapshot_id = ?`, prevID)
		if err != nil {
			return nil, err
		}
		for pr.Next() {
			var name string
			var used int
			if err := pr.Scan(&name, &used); err != nil {
				pr.Close()
				return nil, err
			}
			prevUsed[name] = used
		}
		pr.Close()
		if err := pr.Err(); err != nil {
			return nil, err
		}
	}

	rows, err := s.queryRows(`SELECT name, value, value_type, type, discount_class, min_purchase,
		combines_order, combines_product, combines_shipping, customer_selection, context,
		times_used, applies_once, usage_limit, status, start_at, end_at
		FROM discount_rows WHERE snapshot_id = ? ORDER BY times_used DESC, name ASC`, snapshotID)
	if err != nil {
		return nil, err
	}

	out := make([]DiscountView, 0, len(rows))
	for _, r := range rows {
		v := DiscountView{Row: r}
		if hasPrev {
			if prev, ok := prevUsed[r.Name]; ok {
				v.HadPrev = true
				v.PrevTimesUsed = prev
				v.Delta = r.TimesUsed - prev
			} else {
				v.IsNew = true
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// DisappearedCodes lists codes present in the snapshot immediately before
// snapshotID but absent from it (deleted in Shopify between exports).
func (s *Store) DisappearedCodes(snapshotID int64) ([]string, error) {
	prevID, hasPrev, err := s.previousSnapshotID(snapshotID)
	if err != nil || !hasPrev {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT name FROM discount_rows WHERE snapshot_id = ?
		AND name NOT IN (SELECT name FROM discount_rows WHERE snapshot_id = ?)
		ORDER BY name`, prevID, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// HistoryPoint is one code's state in one snapshot, for trend display.
type HistoryPoint struct {
	SnapshotID int64
	TakenAt    time.Time
	TimesUsed  int
	Status     string
}

// CodeHistory returns the full usage history of a single code, oldest first.
func (s *Store) CodeHistory(name string) ([]HistoryPoint, error) {
	rows, err := s.db.Query(`SELECT r.snapshot_id, sn.taken_at, r.times_used, r.status
		FROM discount_rows r JOIN snapshots sn ON sn.id = r.snapshot_id
		WHERE r.name = ? ORDER BY sn.taken_at ASC, sn.id ASC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryPoint
	for rows.Next() {
		var p HistoryPoint
		var takenAt string
		if err := rows.Scan(&p.SnapshotID, &takenAt, &p.TimesUsed, &p.Status); err != nil {
			return nil, err
		}
		p.TakenAt, _ = time.Parse(time.RFC3339, takenAt)
		out = append(out, p)
	}
	return out, rows.Err()
}

// BackupTo writes a consistent copy of the database to path (which must not yet
// exist). Uses VACUUM INTO so it captures committed WAL content safely while the
// database is open.
func (s *Store) BackupTo(path string) error {
	_, err := s.db.Exec(`VACUUM INTO ?`, path)
	return err
}

// ArchivedCode is a code's latest-known state across the whole archive, whether
// or not it still exists in Shopify.
type ArchivedCode struct {
	Name       string
	Value      float64
	ValueType  string
	Status     string
	TimesUsed  int
	EndAt      string
	UsageLimit string
	FirstSeen  time.Time
	LastSeen   time.Time
	Snapshots  int  // how many snapshots contained this code
	Live       bool // present in the most recent snapshot overall
}

// AllCodes returns every code ever recorded, each with its most recent known
// state, sorted by most-recently-seen. Codes deleted from Shopify remain here
// (marked Live=false) for as long as the local database keeps their snapshots.
func (s *Store) AllCodes() ([]ArchivedCode, error) {
	latest, err := s.LatestSnapshot()
	if err != nil {
		return nil, err
	}
	var latestID int64
	if latest != nil {
		latestID = latest.ID
	}

	rows, err := s.db.Query(`
SELECT name, value, value_type, status, times_used, end_at, usage_limit,
       snapshot_id, first_seen, last_seen, snapshots
FROM (
  SELECT r.name, r.value, r.value_type, r.status, r.times_used, r.end_at, r.usage_limit,
    r.snapshot_id,
    ROW_NUMBER() OVER (PARTITION BY r.name ORDER BY sn.taken_at DESC, sn.id DESC) AS rn,
    MIN(sn.taken_at) OVER (PARTITION BY r.name) AS first_seen,
    MAX(sn.taken_at) OVER (PARTITION BY r.name) AS last_seen,
    COUNT(*)         OVER (PARTITION BY r.name) AS snapshots
  FROM discount_rows r JOIN snapshots sn ON sn.id = r.snapshot_id
) t
WHERE rn = 1
ORDER BY last_seen DESC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ArchivedCode
	for rows.Next() {
		var c ArchivedCode
		var endAt sql.NullString
		var snapshotID int64
		var firstSeen, lastSeen string
		if err := rows.Scan(&c.Name, &c.Value, &c.ValueType, &c.Status, &c.TimesUsed,
			&endAt, &c.UsageLimit, &snapshotID, &firstSeen, &lastSeen, &c.Snapshots); err != nil {
			return nil, err
		}
		c.EndAt = endAt.String
		c.FirstSeen, _ = time.Parse(time.RFC3339, firstSeen)
		c.LastSeen, _ = time.Parse(time.RFC3339, lastSeen)
		c.Live = snapshotID == latestID
		out = append(out, c)
	}
	return out, rows.Err()
}

// SnapshotTotal is the aggregate state of one snapshot, for a trends chart.
type SnapshotTotal struct {
	ID        int64
	TakenAt   time.Time
	CodeCount int
	TotalUses int
}

// SnapshotTotals returns one row per snapshot, oldest first.
func (s *Store) SnapshotTotals() ([]SnapshotTotal, error) {
	rows, err := s.db.Query(`SELECT sn.id, sn.taken_at, COUNT(r.name), COALESCE(SUM(r.times_used),0)
		FROM snapshots sn LEFT JOIN discount_rows r ON r.snapshot_id = sn.id
		GROUP BY sn.id, sn.taken_at
		ORDER BY sn.taken_at ASC, sn.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotTotal
	for rows.Next() {
		var t SnapshotTotal
		var takenAt string
		if err := rows.Scan(&t.ID, &takenAt, &t.CodeCount, &t.TotalUses); err != nil {
			return nil, err
		}
		t.TakenAt, _ = time.Parse(time.RFC3339, takenAt)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Note is a free-form note plus tags attached to a single code.
type Note struct {
	Name      string
	Note      string
	Tags      string
	UpdatedAt time.Time
}

// GetNote returns the note for a code (zero-value Note, nil error if none).
func (s *Store) GetNote(name string) (Note, error) {
	row := s.db.QueryRow(`SELECT name, note, tags, updated_at FROM notes WHERE name = ?`, name)
	var n Note
	var updatedAt string
	switch err := row.Scan(&n.Name, &n.Note, &n.Tags, &updatedAt); err {
	case nil:
		n.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		return n, nil
	case sql.ErrNoRows:
		return Note{}, nil
	default:
		return Note{}, err
	}
}

// SetNote upserts a note + tags for a code. Deletes the row if both are empty.
func (s *Store) SetNote(name, note, tags string) error {
	if note == "" && tags == "" {
		_, err := s.db.Exec(`DELETE FROM notes WHERE name = ?`, name)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO notes (name, note, tags, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET note=excluded.note, tags=excluded.tags, updated_at=excluded.updated_at`,
		name, note, tags, time.Now().UTC().Format(time.RFC3339))
	return err
}

// AllNotes returns every note keyed by code name.
func (s *Store) AllNotes() (map[string]Note, error) {
	rows, err := s.db.Query(`SELECT name, note, tags, updated_at FROM notes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Note{}
	for rows.Next() {
		var n Note
		var updatedAt string
		if err := rows.Scan(&n.Name, &n.Note, &n.Tags, &updatedAt); err != nil {
			return nil, err
		}
		n.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		out[n.Name] = n
	}
	return out, rows.Err()
}

// SnapshotRowsByName returns the discount rows of one snapshot keyed by code name.
func (s *Store) SnapshotRowsByName(snapshotID int64) (map[string]csvimport.Row, error) {
	rows, err := s.queryRows(`SELECT name, value, value_type, type, discount_class, min_purchase,
		combines_order, combines_product, combines_shipping, customer_selection, context,
		times_used, applies_once, usage_limit, status, start_at, end_at
		FROM discount_rows WHERE snapshot_id = ?`, snapshotID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]csvimport.Row, len(rows))
	for _, r := range rows {
		out[r.Name] = r
	}
	return out, nil
}

// Revenue is the discount spend attributed to one code, derived from order data.
type Revenue struct {
	Name          string
	TotalDiscount float64
	OrderCount    int
	Currency      string
	ComputedAt    time.Time
}

// SetRevenue replaces the entire revenue table with the given rows in one tx.
func (s *Store) SetRevenue(rev []Revenue, computedAt time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM revenue`); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO revenue (name, total_discount, order_count, currency, computed_at)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	at := computedAt.UTC().Format(time.RFC3339)
	for _, r := range rev {
		if _, err := stmt.Exec(r.Name, r.TotalDiscount, r.OrderCount, r.Currency, at); err != nil {
			return fmt.Errorf("insert revenue %q: %w", r.Name, err)
		}
	}
	return tx.Commit()
}

// AllRevenue returns revenue keyed by code name.
func (s *Store) AllRevenue() (map[string]Revenue, error) {
	rows, err := s.db.Query(`SELECT name, total_discount, order_count, currency, computed_at FROM revenue`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Revenue{}
	for rows.Next() {
		var r Revenue
		var computedAt string
		if err := rows.Scan(&r.Name, &r.TotalDiscount, &r.OrderCount, &r.Currency, &computedAt); err != nil {
			return nil, err
		}
		r.ComputedAt, _ = time.Parse(time.RFC3339, computedAt)
		out[r.Name] = r
	}
	return out, rows.Err()
}

// queryRows runs a SELECT returning the standard discount column set.
func (s *Store) queryRows(query string, args ...any) ([]csvimport.Row, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []csvimport.Row
	for rows.Next() {
		var r csvimport.Row
		var start, end sql.NullString
		if err := rows.Scan(&r.Name, &r.Value, &r.ValueType, &r.Type, &r.DiscountClass,
			&r.MinPurchase, &r.CombinesOrder, &r.CombinesProduct, &r.CombinesShipping,
			&r.CustomerSelection, &r.Context, &r.TimesUsed, &r.AppliesOnce, &r.UsageLimit,
			&r.Status, &start, &end); err != nil {
			return nil, err
		}
		r.StartAt = start.String
		r.EndAt = end.String
		out = append(out, r)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanSnapshot(row scanner) (*SnapshotMeta, error) {
	var m SnapshotMeta
	var takenAt, importedAt string
	if err := row.Scan(&m.ID, &takenAt, &importedAt, &m.SourceFile, &m.FileHash, &m.RowCount); err != nil {
		return nil, err
	}
	m.TakenAt, _ = time.Parse(time.RFC3339, takenAt)
	m.ImportedAt, _ = time.Parse(time.RFC3339, importedAt)
	return &m, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
