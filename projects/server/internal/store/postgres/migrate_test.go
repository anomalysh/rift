package postgres

import (
	"testing"
	"testing/fstest"

	"github.com/anomalysh/rift/projects/server/internal/store/migrations"
)

func TestParseMigrationsOrdersByVersion(t *testing.T) {
	fsys := fstest.MapFS{
		"0002_second.sql": {Data: []byte("SELECT 2;")},
		"0001_first.sql":  {Data: []byte("SELECT 1;")},
		"0010_tenth.sql":  {Data: []byte("SELECT 10;")},
		"README.md":       {Data: []byte("ignored")},
	}

	migs, err := parseMigrations(fsys)
	if err != nil {
		t.Fatalf("parseMigrations: %v", err)
	}
	if len(migs) != 3 {
		t.Fatalf("want 3 migrations, got %d", len(migs))
	}

	// Ascending version order, not the lexical map order and not "0010" < "0002".
	want := []int64{1, 2, 10}
	for i, m := range migs {
		if m.version != want[i] {
			t.Fatalf("migration %d: want version %d, got %d (%s)", i, want[i], m.version, m.name)
		}
	}
	if migs[0].sql != "SELECT 1;" {
		t.Fatalf("body mismatch: got %q", migs[0].sql)
	}
}

func TestParseMigrationsRejectsDuplicateVersion(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_a.sql": {Data: []byte("SELECT 1;")},
		"0001_b.sql": {Data: []byte("SELECT 2;")},
	}
	if _, err := parseMigrations(fsys); err == nil {
		t.Fatal("want error for duplicate version, got nil")
	}
}

func TestParseMigrationsRejectsNonNumericPrefix(t *testing.T) {
	fsys := fstest.MapFS{
		"init_schema.sql": {Data: []byte("SELECT 1;")},
	}
	if _, err := parseMigrations(fsys); err == nil {
		t.Fatal("want error for non-numeric prefix, got nil")
	}
}

func TestEmbeddedMigrationsParse(t *testing.T) {
	migs, err := parseMigrations(migrations.FS)
	if err != nil {
		t.Fatalf("parse embedded migrations: %v", err)
	}
	if len(migs) == 0 {
		t.Fatal("no embedded migrations found")
	}
	for i := 1; i < len(migs); i++ {
		if migs[i].version <= migs[i-1].version {
			t.Fatalf("embedded migrations not strictly increasing: %d then %d", migs[i-1].version, migs[i].version)
		}
	}
}
