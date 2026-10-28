// Package store persists scraped data in SQLite.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// TimeFormat is the format of every timestamp stored by the scraper.
const TimeFormat = "2006-01-02T15:04:05Z"

// FormatTime renders t in TimeFormat (UTC).
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// DB wraps a single-connection writer pool and a reader pool. SQLite allows
// one writer at a time; funnelling every write through one connection avoids
// SQLITE_BUSY churn and keeps transactions short and serialized.
type DB struct {
	W    *sql.DB
	R    *sql.DB
	Path string
	Now  func() time.Time
}

// Open opens (creating if needed) the database at path and applies pending
// migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := func(readonly bool) string {
		q := url.Values{}
		q.Add("_pragma", "busy_timeout(30000)")
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
		q.Add("_pragma", "foreign_keys(OFF)")
		q.Add("_txlock", "immediate")
		if readonly {
			q.Add("_pragma", "query_only(1)")
		}
		return "file:" + filepath.ToSlash(abs) + "?" + q.Encode()
	}
	w, err := sql.Open("sqlite", dsn(false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetConnMaxIdleTime(0)
	if err := w.PingContext(ctx); err != nil {
		w.Close()
		return nil, fmt.Errorf("store: open %s: %w", abs, err)
	}
	db := &DB{W: w, Path: abs, Now: time.Now}
	if err := db.migrate(ctx); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn(true))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	db.R = r
	return db, nil
}

// Close checkpoints the WAL into the main file and closes both pools.
func (db *DB) Close() error {
	_, _ = db.W.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return errors.Join(db.R.Close(), db.W.Close())
}

func (db *DB) NowString() string { return FormatTime(db.Now()) }

func (db *DB) migrate(ctx context.Context) error {
	var version int
	if err := db.W.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		n, err := strconv.Atoi(strings.SplitN(filepath.Base(name), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("store: bad migration name %s", name)
		}
		if n <= version {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		err = db.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", n))
			return err
		})
		if err != nil {
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
	}
	return nil
}

// Tx runs fn inside a write transaction.
func (db *DB) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Checkpoint returns the stored value for key, or "" when absent.
func (db *DB) Checkpoint(ctx context.Context, key string) (value, updatedAt string, err error) {
	err = db.W.QueryRowContext(ctx, "SELECT value, updated_at FROM checkpoints WHERE key = ?", key).Scan(&value, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return value, updatedAt, err
}

func (db *DB) SetCheckpoint(ctx context.Context, key, value string) error {
	_, err := db.W.ExecContext(ctx, `INSERT INTO checkpoints (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, db.NowString())
	return err
}

func (db *DB) DeleteCheckpoint(ctx context.Context, key string) error {
	_, err := db.W.ExecContext(ctx, "DELETE FROM checkpoints WHERE key = ?", key)
	return err
}

// nullStr maps "" to NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func ptrStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
