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
