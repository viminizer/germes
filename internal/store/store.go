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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"modernc.org/sqlite" // the "sqlite" driver
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed migrations/*.sql
var embedded embed.FS

// Store is the database.
type Store struct {
	db *sql.DB
}

// New opens the database at path and runs its pending additive migrations.
// Only the daemon opens the store; `germes doctor` uses GetPendingMigrations.
func New(path string) (*Store, error) {
	s, err := open(path)
	if err != nil {
		return nil, err
	}
	if err := s.migrate(context.Background(), migrationFiles(), false); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// open opens the database at path, creating it when missing.
func open(path string) (*Store, error) {
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
	return &Store{db: db}, nil
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

// RunContractMigrations runs the pending contract migrations (NNN_x.contract.sql):
// the ones that remove or rename what an older daemon may still use. The
// daemon calls it once no older daemon runs (#21).
func (s *Store) RunContractMigrations(ctx context.Context) error {
	return s.migrate(ctx, migrationFiles(), true)
}

// GetPendingMigrations counts the migrations that have not run on the database at
// path. It opens the file read-only and never creates it: a missing file is
// an error that wraps fs.ErrNotExist.
func GetPendingMigrations(ctx context.Context, path string) (int, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return 0, err
	}
	defer db.Close()
	all, err := listMigrations(migrationFiles())
	if err != nil {
		return 0, err
	}
	var tracked bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations')`).Scan(&tracked); err != nil {
		return 0, err
	}
	if !tracked {
		return len(all), nil
	}
	ran, err := listRanMigrations(ctx, db)
	if err != nil {
		return 0, err
	}
	pending := 0
	for _, m := range all {
		if !ran[m.version] {
			pending++
		}
	}
	return pending, nil
}

// migrationFiles are the embedded migration files.
func migrationFiles() fs.FS {
	sub, err := fs.Sub(embedded, "migrations")
	if err != nil {
		panic(err) // a bug: the embed pattern names this folder
	}
	return sub
}

// migration is one numbered SQL file.
type migration struct {
	name     string
	version  int
	contract bool // NNN_x.contract.sql: removes or renames, so it waits for older daemons to exit
	body     string
}

// listMigrations reads the migration files in order. Two files with one
// number are refused: one would hide the other.
func listMigrations(files fs.FS) ([]migration, error) {
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	seen := map[int]string{}
	var out []migration
	for _, name := range names {
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return nil, fmt.Errorf("migration %s: bad number: %w", name, err)
		}
		if other, ok := seen[version]; ok {
			return nil, fmt.Errorf("migrations %s and %s share the number %d", other, name, version)
		}
		seen[version] = name
		body, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{name: name, version: version, contract: strings.HasSuffix(name, ".contract.sql"), body: string(body)})
	}
	return out, nil
}

// migrate runs the pending migrations of one kind, additive or contract,
// each in its own transaction. A failed one rolls back and stays pending.
func (s *Store) migrate(ctx context.Context, files fs.FS, contract bool) error {
	all, err := listMigrations(files)
	if err != nil {
		return err
	}
	// One connection: BEGIN IMMEDIATE and its COMMIT must run on the same one.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	// Read what already ran without the write lock, so an up-to-date
	// database opens even while another process writes for a long time.
	ran, err := listRanMigrations(ctx, conn)
	if err != nil {
		return fmt.Errorf("read schema versions: %w", err)
	}
	for _, m := range all {
		if m.contract != contract || ran[m.version] {
			continue
		}
		if err := runMigration(ctx, conn, m.version, m.body); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}

// listRanMigrations returns the versions of the migrations that already ran.
func listRanMigrations(ctx context.Context, db interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
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
