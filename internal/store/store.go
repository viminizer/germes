// Package store keeps everything in germes.db (SQLite).
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

	"modernc.org/sqlite" // the "sqlite" driver
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the database.
type Store struct {
	db *sql.DB
}

// New opens the database at path and runs its migrations.
func New(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path %s: %w", path, err)
	}
	// Encode only the filesystem path, so URI characters cannot change the
	// selected database or the connection settings. Absolute paths avoid a
	// relative first component being interpreted as a URI scheme or host.
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// IsBusy reports whether err means another process held the database (for
// example it was running a migration), so trying again later can work.
func IsBusy(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	code := e.Code() & 0xff // extended codes keep the primary code in the low byte
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}

// migrate runs the numbered migration files that have not run yet, each in
// its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	// One connection: BEGIN IMMEDIATE and its COMMIT must run on the same one.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Read what already ran without the write lock, so an up-to-date
	// database opens even while another process writes for a long time.
	ran, err := listRanMigrations(ctx, conn)
	if err != nil {
		return fmt.Errorf("read schema versions: %w", err)
	}
	for _, name := range names {
		base := strings.TrimPrefix(name, "migrations/")
		version, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: bad number: %w", base, err)
		}
		if ran[version] {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		if err := runMigration(ctx, conn, version, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", base, err)
		}
	}
	return nil
}

// listRanMigrations returns the versions of the migrations that already ran.
func listRanMigrations(ctx context.Context, conn *sql.Conn) (map[int]bool, error) {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ran := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		ran[v] = true
	}
	return ran, rows.Err()
}

// runMigration runs one pending migration under the write lock, unless it
// ran meanwhile. The check is made again after taking the lock: the daemon
// and `germes doctor` may migrate at once after an update, and the one that
// waited must see what the other did (#21).
func runMigration(ctx context.Context, conn *sql.Conn, version int, body string) (err error) {
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var done bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = ?)`, version).Scan(&done); err != nil {
		return err
	}
	if !done {
		if _, err := conn.ExecContext(ctx, body); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
			return err
		}
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return err
}
