// Package database opens the SQLite log store and applies its migrations.
package database

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, keeps CGO_ENABLED=0 builds
)

// migrations run in order; PRAGMA user_version records how many have run.
var migrations = []string{
	`CREATE TABLE logs (
		id          INTEGER PRIMARY KEY,
		ts          INTEGER NOT NULL,           -- unix ms
		level       TEXT    NOT NULL,
		kind        TEXT    NOT NULL DEFAULT 'app',
		direction   TEXT,                        -- inbound | outbound | NULL (system/admin)
		msg         TEXT    NOT NULL,
		client      TEXT, method TEXT, host TEXT, url TEXT,
		status      INTEGER, bytes_in INTEGER, bytes_out INTEGER, duration_ms INTEGER,
		source_ip   TEXT, route TEXT, trace_id TEXT,
		attrs       TEXT                         -- JSON object of remaining attributes
	);
	CREATE INDEX logs_ts     ON logs(ts);
	CREATE INDEX logs_kind   ON logs(kind, ts);
	CREATE INDEX logs_dir    ON logs(direction, ts);
	CREATE INDEX logs_route  ON logs(route, ts);
	CREATE INDEX logs_client ON logs(client, ts);

	CREATE TABLE events (
		id          TEXT PRIMARY KEY,            -- time-sortable
		ts          INTEGER NOT NULL,
		source      TEXT    NOT NULL,            -- webhook | ingest
		route       TEXT    NOT NULL,            -- webhook id or ingest rule id
		vendor      TEXT, event_type TEXT,
		method      TEXT    NOT NULL,
		path        TEXT    NOT NULL,
		query       TEXT,
		source_ip   TEXT,
		headers     TEXT    NOT NULL,            -- JSON, credentials removed
		body        BLOB,
		body_size   INTEGER NOT NULL,
		traceparent TEXT
	);
	CREATE INDEX events_ts    ON events(ts);
	CREATE INDEX events_route ON events(route, ts);

	CREATE TABLE deliveries (
		id           INTEGER PRIMARY KEY,
		event_id     TEXT    NOT NULL REFERENCES events(id) ON DELETE CASCADE,
		route        TEXT    NOT NULL,
		target       TEXT    NOT NULL,         -- full URL
		mode         TEXT    NOT NULL,         -- sync | async
		status       TEXT    NOT NULL,         -- pending | inflight | delivered | failed | dead
		attempts     INTEGER NOT NULL DEFAULT 0,
		max_attempts INTEGER NOT NULL,
		timeout_sec  INTEGER NOT NULL,
		next_at      INTEGER NOT NULL,
		last_status  INTEGER,
		last_error   TEXT,
		duration_ms  INTEGER,
		created_at   INTEGER NOT NULL,
		updated_at   INTEGER NOT NULL
	);
	CREATE INDEX deliveries_due   ON deliveries(status, next_at);
	CREATE INDEX deliveries_event ON deliveries(event_id);
	CREATE INDEX deliveries_route ON deliveries(route, created_at);`,

	// 2: full request/response capture for logged API calls (RQH, RQB, RSH, RSB).
	`ALTER TABLE logs ADD COLUMN rq_headers TEXT;
	ALTER TABLE logs ADD COLUMN rq_body BLOB;
	ALTER TABLE logs ADD COLUMN rq_size INTEGER;
	ALTER TABLE logs ADD COLUMN rs_headers TEXT;
	ALTER TABLE logs ADD COLUMN rs_body BLOB;
	ALTER TABLE logs ADD COLUMN rs_size INTEGER;`,
}

// DB is the SQLite database.
type DB struct {
	*sql.DB
	path string
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(5000)", "foreign_keys(ON)"} {
		q.Add("_pragma", p)
	}
	if readOnly {
		q.Set("mode", "ro")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
		q.Set("_txlock", "immediate") // take the write lock at BEGIN: no lock-upgrade deadlocks
	}
	return "file:" + path + "?" + q.Encode()
}

// Open opens (creating if needed) and migrates the database at path.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
	}
	sdb, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(8)
	db := &DB{DB: sdb, path: path}
	if err := db.migrate(); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return db, nil
}

// OpenReadOnly is used by CLI queries while the server is running.
func OpenReadOnly(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	sdb, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, err
	}
	return &DB{DB: sdb, path: path}, sdb.Ping()
}

// Path is the database file.
func (db *DB) Path() string { return db.path }

func (db *DB) migrate() error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// NowMs is the current time in unix milliseconds.
func NowMs() int64 { return time.Now().UnixMilli() }

// NewID returns a time-sortable id: 12 hex chars of unix ms + 8 random hex chars.
func NewID() string {
	var b [4]byte
	rand.Read(b[:])
	return fmt.Sprintf("%012x%s", NowMs(), hex.EncodeToString(b[:]))
}
