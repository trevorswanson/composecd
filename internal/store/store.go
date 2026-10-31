// Package store keeps composecd's own state in SQLite: the history of sync and
// rollback operations, which applications are paused, and the image digests
// registries last served. Desired state lives in
// Git and release history in composetpl's storage, not here.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Operation kinds, triggers and phases.
const (
	KindSync     = "sync"
	KindRollback = "rollback"
	// KindHeal re-applies the deployed revision to drifted containers.
	KindHeal = "heal"
	// KindPrune uninstalls a release whose Application left Git.
	KindPrune = "prune"

	TriggerAuto   = "auto"
	TriggerManual = "manual"

	PhaseRunning   = "Running"
	PhaseSucceeded = "Succeeded"
	PhaseFailed    = "Failed"
)

// Operation is one sync or rollback of an application.
type Operation struct {
	ID      int64  `json:"id"`
	App     string `json:"app"`
	Kind    string `json:"kind"`
	Trigger string `json:"trigger"`
	// User is who started a manual operation ("" for automatic ones).
	User string `json:"user,omitempty"`
	// SourceRevision is the Git commit synced ("" for an oci:// chart or a rollback).
	SourceRevision string `json:"sourceRevision,omitempty"`
	// ReleaseRevision is the composetpl revision the operation created.
	ReleaseRevision int    `json:"releaseRevision,omitempty"`
	Phase           string `json:"phase"`
	Message         string `json:"message,omitempty"`
	Log             string `json:"log,omitempty"`
	// Helper is the container applying composecd's own release, for an
	// operation on composecd itself. It outlives this process, so the next one
	// finishes the operation.
	Helper     string     `json:"helper,omitempty"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

// AppState is what composecd remembers about an application between restarts.
type AppState struct {
	Paused       bool   `json:"paused"`
	PausedReason string `json:"pausedReason,omitempty"`
}

// Store is the database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS operations (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	app              TEXT NOT NULL,
	kind             TEXT NOT NULL,
	trigger          TEXT NOT NULL,
	source_revision  TEXT NOT NULL DEFAULT '',
	release_revision INTEGER NOT NULL DEFAULT 0,
	phase            TEXT NOT NULL,
	message          TEXT NOT NULL DEFAULT '',
	log              TEXT NOT NULL DEFAULT '',
	started_at       TEXT NOT NULL,
	finished_at      TEXT
);
CREATE INDEX IF NOT EXISTS operations_app ON operations (app, id);
CREATE TABLE IF NOT EXISTS users (
	name          TEXT PRIMARY KEY,
	role          TEXT NOT NULL,
	password_hash TEXT NOT NULL,
	created_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL UNIQUE,
	role         TEXT NOT NULL,
	secret_hash  TEXT NOT NULL,
	created_by   TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	last_used_at TEXT
);
CREATE TABLE IF NOT EXISTS sessions (
	id_hash      TEXT PRIMARY KEY,
	user         TEXT NOT NULL,
	role         TEXT NOT NULL,
	via          TEXT NOT NULL,
	csrf         TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	last_seen_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS host_state (
	host               TEXT PRIMARY KEY,
	maintenance        INTEGER NOT NULL DEFAULT 0,
	maintenance_reason TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS image_digests (
	ref         TEXT PRIMARY KEY,
	digest      TEXT NOT NULL DEFAULT '',
	resolved_at TEXT,
	checked_at  TEXT NOT NULL,
	error       TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS app_state (
	app           TEXT PRIMARY KEY,
	paused        INTEGER NOT NULL DEFAULT 0,
	paused_reason TEXT NOT NULL DEFAULT ''
);
`

// Open opens (creating if needed) the database at path. Operations left
// running by a previous process are marked failed, except those a helper
// container is still running (HelperOperations).
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// One connection: SQLite serializes writers anyway, and this avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initializing %s: %w", path, err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if _, err := db.Exec(`UPDATE operations SET phase = ?, message = ?, finished_at = ? WHERE phase = ? AND helper = ''`,
		PhaseFailed, "interrupted: composecd restarted during the operation", formatTime(time.Now()), PhaseRunning); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrate brings databases created by older versions up to the current schema.
func migrate(db *sql.DB) error {
	cols, err := columns(db, "operations")
	if err != nil {
		return err
	}
	if !cols["user"] {
		if _, err := db.Exec(`ALTER TABLE operations ADD COLUMN user TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !cols["helper"] {
		if _, err := db.Exec(`ALTER TABLE operations ADD COLUMN helper TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

func columns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// StartOperation records a running operation and sets its ID and StartedAt.
func (s *Store) StartOperation(op *Operation) error {
	op.Phase = PhaseRunning
	if op.StartedAt.IsZero() {
		op.StartedAt = time.Now()
	}
	res, err := s.db.Exec(`INSERT INTO operations (app, kind, trigger, user, source_revision, phase, started_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		op.App, op.Kind, op.Trigger, op.User, op.SourceRevision, op.Phase, formatTime(op.StartedAt))
	if err != nil {
		return err
	}
	op.ID, err = res.LastInsertId()
	return err
}

// FinishOperation records op's outcome (Phase, Message, Log, ReleaseRevision).
func (s *Store) FinishOperation(op *Operation) error {
	now := time.Now()
	op.FinishedAt = &now
	_, err := s.db.Exec(`UPDATE operations SET phase = ?, message = ?, log = ?, release_revision = ?, source_revision = ?, finished_at = ? WHERE id = ?`,
		op.Phase, op.Message, op.Log, op.ReleaseRevision, op.SourceRevision, formatTime(now), op.ID)
	return err
}

// SetHelper records the helper container running operation id.
func (s *Store) SetHelper(id int64, helper string) error {
	_, err := s.db.Exec(`UPDATE operations SET helper = ? WHERE id = ?`, helper, id)
	return err
}

// HelperOperations returns the running operations a helper container is
// carrying out, oldest first.
func (s *Store) HelperOperations() ([]*Operation, error) {
	return s.query(false, `WHERE phase = ? AND helper != '' ORDER BY id`, PhaseRunning)
}

// Operation returns one operation, with its log.
func (s *Store) Operation(id int64) (*Operation, error) {
	ops, err := s.query(true, `WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(ops) == 0 {
		return nil, ErrNotFound
	}
	return ops[0], nil
}

// ErrNotFound is returned for a missing operation.
var ErrNotFound = errors.New("not found")

// Operations returns app's newest operations first, without logs ("" app: every app).
func (s *Store) Operations(app string, limit int) ([]*Operation, error) {
	if limit <= 0 {
		limit = 50
	}
	if app == "" {
		return s.query(false, `ORDER BY id DESC LIMIT ?`, limit)
	}
	return s.query(false, `WHERE app = ? ORDER BY id DESC LIMIT ?`, app, limit)
}

// LastOperation returns app's newest operation, without its log, or nil.
func (s *Store) LastOperation(app string) (*Operation, error) {
	ops, err := s.Operations(app, 1)
	if err != nil || len(ops) == 0 {
		return nil, err
	}
	return ops[0], nil
}

func (s *Store) query(withLog bool, where string, args ...interface{}) ([]*Operation, error) {
	logCol := `''`
	if withLog {
		logCol = `log`
	}
	// logCol and where are constants from this file; values are bound parameters.
	rows, err := s.db.Query(`SELECT id, app, kind, trigger, user, source_revision, release_revision, phase, message, `+logCol+ //nolint:gosec // see above
		`, helper, started_at, finished_at FROM operations `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Operation
	for rows.Next() {
		op := &Operation{}
		var started string
		var finished sql.NullString
		if err := rows.Scan(&op.ID, &op.App, &op.Kind, &op.Trigger, &op.User, &op.SourceRevision, &op.ReleaseRevision,
			&op.Phase, &op.Message, &op.Log, &op.Helper, &started, &finished); err != nil {
			return nil, err
		}
		if op.StartedAt, err = parseTime(started); err != nil {
			return nil, err
		}
		if finished.Valid {
			t, err := parseTime(finished.String)
			if err != nil {
				return nil, err
			}
			op.FinishedAt = &t
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// AppState returns what is stored for app (the zero value if nothing is).
func (s *Store) AppState(app string) (AppState, error) {
	var st AppState
	var paused int
	err := s.db.QueryRow(`SELECT paused, paused_reason FROM app_state WHERE app = ?`, app).Scan(&paused, &st.PausedReason)
	if errors.Is(err, sql.ErrNoRows) {
		return AppState{}, nil
	}
	st.Paused = paused != 0
	return st, err
}

// SetPaused pauses or resumes automatic sync of app.
func (s *Store) SetPaused(app string, paused bool, reason string) error {
	if !paused {
		reason = ""
	}
	_, err := s.db.Exec(`INSERT INTO app_state (app, paused, paused_reason) VALUES (?, ?, ?)
		ON CONFLICT (app) DO UPDATE SET paused = excluded.paused, paused_reason = excluded.paused_reason`,
		app, boolInt(paused), reason)
	return err
}

// HostState is what composecd remembers about a host.
type HostState struct {
	// Maintenance stops automatic syncs, self-heal and prune on the host.
	Maintenance       bool   `json:"maintenance"`
	MaintenanceReason string `json:"maintenanceReason,omitempty"`
}

// HostState returns what is stored for host (the zero value if nothing is).
func (s *Store) HostState(host string) (HostState, error) {
	var st HostState
	var on int
	err := s.db.QueryRow(`SELECT maintenance, maintenance_reason FROM host_state WHERE host = ?`, host).Scan(&on, &st.MaintenanceReason)
	if errors.Is(err, sql.ErrNoRows) {
		return HostState{}, nil
	}
	st.Maintenance = on != 0
	return st, err
}

// SetMaintenance turns maintenance mode on or off for host.
func (s *Store) SetMaintenance(host string, on bool, reason string) error {
	if !on {
		reason = ""
	}
	_, err := s.db.Exec(`INSERT INTO host_state (host, maintenance, maintenance_reason) VALUES (?, ?, ?)
		ON CONFLICT (host) DO UPDATE SET maintenance = excluded.maintenance, maintenance_reason = excluded.maintenance_reason`,
		host, boolInt(on), reason)
	return err
}

// ImageDigest is the digest a registry last served for an image tag.
type ImageDigest struct {
	// Ref is registry/repository:tag.
	Ref string `json:"ref"`
	// Digest is the last digest resolved ("" if none ever was).
	Digest     string    `json:"digest,omitempty"`
	ResolvedAt time.Time `json:"resolvedAt,omitzero"`
	// CheckedAt is the last check, which failed with Error if it is set.
	CheckedAt time.Time `json:"checkedAt"`
	Error     string    `json:"error,omitempty"`
}

// ImageDigests returns every stored image digest, by ref.
func (s *Store) ImageDigests() (map[string]ImageDigest, error) {
	rows, err := s.db.Query(`SELECT ref, digest, resolved_at, checked_at, error FROM image_digests`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ImageDigest{}
	for rows.Next() {
		var d ImageDigest
		var resolved sql.NullString
		var checked string
		if err := rows.Scan(&d.Ref, &d.Digest, &resolved, &checked, &d.Error); err != nil {
			return nil, err
		}
		if d.CheckedAt, err = parseTime(checked); err != nil {
			return nil, err
		}
		if resolved.Valid {
			if d.ResolvedAt, err = parseTime(resolved.String); err != nil {
				return nil, err
			}
		}
		out[d.Ref] = d
	}
	return out, rows.Err()
}

// SetImageDigest records a successful check of ref.
func (s *Store) SetImageDigest(ref, digest string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO image_digests (ref, digest, resolved_at, checked_at, error) VALUES (?, ?, ?, ?, '')
		ON CONFLICT (ref) DO UPDATE SET digest = excluded.digest, resolved_at = excluded.resolved_at,
		checked_at = excluded.checked_at, error = ''`, ref, digest, formatTime(at), formatTime(at))
	return err
}

// SetImageError records a failed check of ref, keeping its last digest.
func (s *Store) SetImageError(ref, msg string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO image_digests (ref, checked_at, error) VALUES (?, ?, ?)
		ON CONFLICT (ref) DO UPDATE SET checked_at = excluded.checked_at, error = excluded.error`, ref, formatTime(at), msg)
	return err
}

// DeleteImageDigests forgets every stored digest.
func (s *Store) DeleteImageDigests() error {
	_, err := s.db.Exec(`DELETE FROM image_digests`)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }
