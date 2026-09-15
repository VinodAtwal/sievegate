package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sievegate/models"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS requests (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id            TEXT NOT NULL,
	method            TEXT NOT NULL,
	path              TEXT NOT NULL,
	query             TEXT,
	status_original   INTEGER,
	status_migrated   INTEGER,
	status_match      INTEGER,
	header_match      INTEGER,
	header_mismatch   TEXT,
	body_match        INTEGER,
	preview_original  TEXT,
	preview_migrated  TEXT,
	full_original     TEXT,
	full_migrated     TEXT,
	diff_paths        TEXT,
	duration_original REAL,
	duration_migrated REAL,
	size_original     INTEGER,
	size_migrated     INTEGER,
	error_original    TEXT,
	error_migrated    TEXT,
	is_match          INTEGER,
	created_at        TEXT
);`

const dropSQL = `DROP TABLE IF EXISTS requests`

// Store wraps the SQLite database used during a single run.
//
// The database is flushed on every run so it never grows large: data from a
// previous run is dropped when New is called and recreated fresh.
type Store struct {
	db *sql.DB
}

// New opens (or creates) the SQLite database at path and flushes previous
// run data so every run starts clean.
func New(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil && filepath.Dir(dbPath) != "." {
		return nil, fmt.Errorf("creating db directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}
	// modernc sqlite handles a single writer; keep a small pool.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.Flush(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Flush drops all data and recreates the requests table so the store starts
// from a clean slate. It is called on every run and exposed through the reset
// endpoint.
func (s *Store) Flush() error {
	if _, err := s.db.Exec(dropSQL); err != nil {
		return fmt.Errorf("flushing db: %w", err)
	}
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("flushing db: recreating schema: %w", err)
	}
	return nil
}

// Reset discards all stored comparison data. The table (and its schema) is
// recreated so the store is immediately ready to accept new records.
func (s *Store) Reset() error {
	return s.Flush()
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Save persists a single comparison record.
func (s *Store) Save(c *models.Comparison) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}

	var statusOrig, statusMig sql.NullInt64
	if c.StatusOriginal != 0 {
		statusOrig = sql.NullInt64{Int64: int64(c.StatusOriginal), Valid: true}
	}
	if c.StatusMigrated != 0 {
		statusMig = sql.NullInt64{Int64: int64(c.StatusMigrated), Valid: true}
	}

	var durOrig, durMig sql.NullFloat64
	if c.DurationOriginal > 0 {
		durOrig = sql.NullFloat64{Float64: c.DurationOriginal.Seconds() * 1000, Valid: true}
	}
	if c.DurationMigrated > 0 {
		durMig = sql.NullFloat64{Float64: c.DurationMigrated.Seconds() * 1000, Valid: true}
	}

	var sizeOrig, sizeMig sql.NullInt64
	if c.SizeOriginal > 0 {
		sizeOrig = sql.NullInt64{Int64: int64(c.SizeOriginal), Valid: true}
	}
	if c.SizeMigrated > 0 {
		sizeMig = sql.NullInt64{Int64: int64(c.SizeMigrated), Valid: true}
	}

	headerMismatch := strings.Join(c.HeaderMismatches, ", ")
	diffPaths := strings.Join(c.DiffPaths, "\n")

	_, err := s.db.Exec(`
		INSERT INTO requests (
			run_id, method, path, query,
			status_original, status_migrated, status_match, header_match,
			header_mismatch, body_match, preview_original, preview_migrated,
			full_original, full_migrated,
			diff_paths, duration_original, duration_migrated,
			size_original, size_migrated, error_original, error_migrated,
			is_match, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.RunID, c.Method, c.Path, c.Query,
		statusOrig, statusMig, boolToInt(c.StatusMatch), boolToInt(c.HeaderMatch),
		headerMismatch, boolToInt(c.BodyMatch), c.BodyPreviewOriginal, c.BodyPreviewMigrated,
		nullStr(c.BodyOriginalFull), nullStr(c.BodyMigratedFull),
		diffPaths, durOrig, durMig,
		sizeOrig, sizeMig, nullStr(c.ErrorOriginal), nullStr(c.ErrorMigrated),
		boolToInt(c.IsMatch), c.CreatedAt.Format(time.RFC3339),
	)
	return err
}

// List returns all comparison records for the current run.
func (s *Store) List() ([]*models.Comparison, error) {
	rows, err := s.db.Query(`
		SELECT run_id, method, path, query,
			status_original, status_migrated, status_match, header_match,
			header_mismatch, body_match, preview_original, preview_migrated,
			full_original, full_migrated,
			diff_paths, duration_original, duration_migrated,
			size_original, size_migrated, error_original, error_migrated,
			is_match, created_at
		FROM requests ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*models.Comparison
	for rows.Next() {
		c := &models.Comparison{}
		var statusOrig, statusMig sql.NullInt64
		var statusMatch, headerMatch, bodyMatch, isMatch int
		var headerMismatch, previewOrig, previewMig, fullOrig, fullMig, diffPaths, query sql.NullString
		var durOrig, durMig sql.NullFloat64
		var sizeOrig, sizeMig sql.NullInt64
		var errOrig, errMig sql.NullString
		var createdAt string

		if err := rows.Scan(
			&c.RunID, &c.Method, &c.Path, &query,
			&statusOrig, &statusMig, &statusMatch, &headerMatch,
			&headerMismatch, &bodyMatch, &previewOrig, &previewMig,
			&fullOrig, &fullMig,
			&diffPaths, &durOrig, &durMig,
			&sizeOrig, &sizeMig, &errOrig, &errMig,
			&isMatch, &createdAt,
		); err != nil {
			return nil, err
		}

		c.Query = query.String
		if statusOrig.Valid {
			c.StatusOriginal = int(statusOrig.Int64)
		}
		if statusMig.Valid {
			c.StatusMigrated = int(statusMig.Int64)
		}
		c.StatusMatch = statusMatch == 1
		c.HeaderMatch = headerMatch == 1
		if headerMismatch.Valid {
			c.HeaderMismatches = splitNonEmpty(headerMismatch.String, ", ")
		}
		c.BodyMatch = bodyMatch == 1
		c.BodyPreviewOriginal = previewOrig.String
		c.BodyPreviewMigrated = previewMig.String
		c.BodyOriginalFull = fullOrig.String
		c.BodyMigratedFull = fullMig.String
		if diffPaths.Valid {
			c.DiffPaths = splitNonEmpty(diffPaths.String, "\n")
		}
		if durOrig.Valid {
			c.DurationOriginal = msToDuration(durOrig.Float64)
		}
		if durMig.Valid {
			c.DurationMigrated = msToDuration(durMig.Float64)
		}
		if sizeOrig.Valid {
			c.SizeOriginal = int(sizeOrig.Int64)
		}
		if sizeMig.Valid {
			c.SizeMigrated = int(sizeMig.Int64)
		}
		c.ErrorOriginal = errOrig.String
		c.ErrorMigrated = errMig.String
		c.IsMatch = isMatch == 1
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			c.CreatedAt = t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, part := range strings.Split(s, sep) {
		if strings.TrimSpace(part) != "" {
			out = append(out, part)
		}
	}
	return out
}

func msToDuration(ms float64) time.Duration {
	return time.Duration(ms * float64(time.Millisecond))
}