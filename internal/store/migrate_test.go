package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// openEmpty opens a new database that ran no migrations.
func openEmpty(t *testing.T) *Store {
	t.Helper()
	s, err := open(filepath.Join(t.TempDir(), "germes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func ranVersions(t *testing.T, s *Store) map[int]bool {
	t.Helper()
	ran, err := listRanMigrations(context.Background(), s.db)
	if err != nil {
		t.Fatal(err)
	}
	return ran
}

func file(body string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(body)} }

func TestContractMigrationWaitsForItsTurn(t *testing.T) {
	ctx := context.Background()
	s := openEmpty(t)
	files := fstest.MapFS{
		"001_t.sql":                 file(`CREATE TABLE t (id INTEGER PRIMARY KEY, old TEXT);`),
		"002_drop_old.contract.sql": file(`ALTER TABLE t DROP COLUMN old;`),
		"003_add_n.sql":             file(`ALTER TABLE t ADD COLUMN n INTEGER NOT NULL DEFAULT 0;`),
	}
	// The daemon starts: the later additive one runs, the contract one waits.
	if err := s.migrate(ctx, files, false); err != nil {
		t.Fatal(err)
	}
	if ran := ranVersions(t, s); !ran[1] || ran[2] || !ran[3] {
		t.Fatalf("after start: %v", ran)
	}
	if _, err := s.db.Exec(`INSERT INTO t (old, n) VALUES ('x', 1)`); err != nil {
		t.Fatalf("old column gone before older daemons exited: %v", err)
	}
	// No older daemon runs any more: the contract one runs.
	if err := s.migrate(ctx, files, true); err != nil {
		t.Fatal(err)
	}
	if ran := ranVersions(t, s); !ran[2] {
		t.Fatalf("contract migration did not run: %v", ran)
	}
	if _, err := s.db.Exec(`INSERT INTO t (old) VALUES ('x')`); err == nil {
		t.Fatal("old column still there")
	}
}

func TestFailedContractMigrationStaysPending(t *testing.T) {
	ctx := context.Background()
	s := openEmpty(t)
	files := fstest.MapFS{
		"001_t.sql":          file(`CREATE TABLE t (id INTEGER PRIMARY KEY, old TEXT);`),
		"002_x.contract.sql": file(`UPDATE t SET old = 'gone'; ALTER TABLE missing DROP COLUMN old;`),
	}
	if err := s.migrate(ctx, files, false); err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`INSERT INTO t (old) VALUES ('kept')`)
	if err := s.migrate(ctx, files, true); err == nil {
		t.Fatal("a broken contract migration succeeded")
	}
	var old string
	s.db.QueryRow(`SELECT old FROM t`).Scan(&old)
	if ran := ranVersions(t, s); ran[2] || old != "kept" {
		t.Fatalf("not rolled back: ran %v, old %q", ran, old)
	}
	// Fixed (a later start retries): it runs.
	files["002_x.contract.sql"] = file(`ALTER TABLE t DROP COLUMN old;`)
	if err := s.migrate(ctx, files, true); err != nil {
		t.Fatal(err)
	}
	if ran := ranVersions(t, s); !ran[2] {
		t.Fatalf("retry did not run: %v", ran)
	}
}

func TestRenameOverlapKeepsBothVersionsWrites(t *testing.T) {
	// A rename in two steps (docs/RULEBOOK.md R46): the additive step adds
	// and copies, the contract step copies what the older daemon wrote in
	// the meantime, without overwriting newer values, then drops.
	ctx := context.Background()
	s := openEmpty(t)
	files := fstest.MapFS{
		"001_t.sql":                  file(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT);`),
		"002_add_title.sql":          file(`ALTER TABLE t ADD COLUMN title TEXT NOT NULL DEFAULT ''; UPDATE t SET title = name WHERE name IS NOT NULL;`),
		"003_drop_name.contract.sql": file(`UPDATE t SET title = name WHERE title = '' AND name IS NOT NULL; ALTER TABLE t DROP COLUMN name;`),
	}
	exec := func(q string) {
		t.Helper()
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrate(ctx, fstest.MapFS{"001_t.sql": files["001_t.sql"]}, false); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO t (id, name) VALUES (1, 'before')`)
	if err := s.migrate(ctx, files, false); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO t (id, name) VALUES (2, 'old daemon')`)  // knows only name
	exec(`INSERT INTO t (id, title) VALUES (3, 'new daemon')`) // knows only title
	exec(`UPDATE t SET title = 'renamed by new' WHERE id = 1`)
	if err := s.migrate(ctx, files, true); err != nil {
		t.Fatal(err)
	}
	want := map[int]string{1: "renamed by new", 2: "old daemon", 3: "new daemon"}
	for id, title := range want {
		var got string
		if err := s.db.QueryRow(`SELECT title FROM t WHERE id = ?`, id).Scan(&got); err != nil || got != title {
			t.Errorf("row %d: %q %v, want %q", id, got, err, title)
		}
	}
}

func TestMigrationsWithOneNumberAreRefused(t *testing.T) {
	_, err := listMigrations(fstest.MapFS{"004_a.sql": file(``), "004_b.contract.sql": file(``)})
	if err == nil || !strings.Contains(err.Error(), "share the number 4") {
		t.Fatalf("got %v", err)
	}
}

func TestGetPendingMigrationsChangesNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.db")
	if _, err := GetPendingMigrations(ctx, missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing database: %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("GetPendingMigrations created the database")
	}

	// A database where only migration 1 ran.
	path := filepath.Join(dir, "germes.db")
	s, err := open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() // stays open, like a running daemon's
	if _, err := s.db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY); INSERT INTO schema_migrations VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	all, err := listMigrations(migrationFiles())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := GetPendingMigrations(ctx, path); err != nil || n != len(all)-1 {
		t.Fatalf("pending = %d, %v; want %d", n, err, len(all)-1)
	}
	if ran := ranVersions(t, s); len(ran) != 1 {
		t.Fatalf("GetPendingMigrations changed the database: %v", ran)
	}
}

// sqlCode is sql without its comments and string contents, so neither can
// break or excuse a policy rule.
func sqlCode(sql string) string {
	var b strings.Builder
	for i := 0; i < len(sql); i++ {
		switch {
		case strings.HasPrefix(sql[i:], "--"):
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case strings.HasPrefix(sql[i:], "/*"):
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 3
		case sql[i] == '\'':
			b.WriteString("''")
			for i++; i < len(sql); i++ {
				if sql[i] == '\'' && !(i+1 < len(sql) && sql[i+1] == '\'') {
					break
				}
				if sql[i] == '\'' {
					i++ // '' is a quote inside the string
				}
			}
		default:
			b.WriteByte(sql[i])
		}
	}
	return b.String()
}

var (
	dropOrRename = regexp.MustCompile(`(?i)\b(DROP|RENAME)\b`)
	addColumn    = regexp.MustCompile(`(?i)^\s*ALTER\s+TABLE\b.*\bADD\b`)
	notNull      = regexp.MustCompile(`(?i)\bNOT\s+NULL\b`)
	hasDefault   = regexp.MustCompile(`(?i)\bDEFAULT\b`)
)

// policyViolation says why an additive migration could break an older
// daemon still running during a hand-over, or "" (docs/RULEBOOK.md R46).
func policyViolation(sql string) string {
	for _, stmt := range strings.Split(sqlCode(sql), ";") {
		switch {
		case dropOrRename.MatchString(stmt):
			return "DROP or RENAME belongs in a .contract.sql file: " + strings.TrimSpace(stmt)
		case addColumn.MatchString(stmt) && notNull.MatchString(stmt) && !hasDefault.MatchString(stmt):
			return "a NOT NULL column needs a DEFAULT, or older daemons' inserts fail: " + strings.TrimSpace(stmt)
		}
	}
	return ""
}

func TestAdditiveMigrationsKeepOlderDaemonsWorking(t *testing.T) {
	all, err := listMigrations(migrationFiles())
	if err != nil {
		t.Fatal(err)
	}
	// Every existing file (001-013) is additive: new tables, and new
	// columns that have a DEFAULT.
	for _, m := range all {
		if why := policyViolation(m.body); !m.contract && why != "" {
			t.Errorf("%s: %s", m.name, why)
		}
	}
	for sql, bad := range map[string]bool{
		"-- DROP TABLE x\nCREATE TABLE y (a INT NOT NULL);":                  false,
		"/* RENAME */ INSERT INTO t VALUES ('DROP TABLE t', 'it''s; DROP');": false,
		"ALTER TABLE t ADD COLUMN a INT NOT NULL DEFAULT 0;":                 false,
		"DROP TABLE t;":                            true,
		"ALTER TABLE t RENAME COLUMN a TO b;":      true,
		"ALTER TABLE t ADD COLUMN a INT NOT NULL;": true,
		"ALTER TABLE t ADD COLUMN a INT NOT NULL; ALTER TABLE t ADD COLUMN b INT DEFAULT 0;": true, // a DEFAULT elsewhere excuses nothing
	} {
		if got := policyViolation(sql) != ""; got != bad {
			t.Errorf("policyViolation(%q) = %v, want %v", sql, got, bad)
		}
	}
}
