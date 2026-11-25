package engine

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openWAL(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func rowCount(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var check string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check: %q, %v", check, err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSQLiteStoredAsConsistentCopy backs up a WAL database whose commits
// sit in its WAL, never checkpointed: the database file's size and mtime do
// not move between the two runs, so only a copy taken through SQLite sees
// the second run's rows.
func TestSQLiteStoredAsConsistentCopy(t *testing.T) {
	old := scratchReserve
	scratchReserve = func(int64) int64 { return 0 }
	t.Cleanup(func() { scratchReserve = old })

	f := newFixture(t)
	r := f.create()
	dir := t.TempDir()
	p := filepath.Join(dir, "data", "app.db")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	db := openWAL(t, p)
	if _, err := db.Exec("CREATE TABLE t (v BLOB)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	insert := func(n int) {
		for range n {
			if _, err := db.Exec("INSERT INTO t VALUES (randomblob(100))"); err != nil {
				t.Fatal(err)
			}
		}
	}
	insert(10)
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "notes.txt"), []byte("plain"))
	before, _ := os.Stat(p)

	src := Source{Job: "j", App: "a", Name: "files"}
	res := backupDir(t, r, src, dir)
	if !slices.Equal(res.SQLitePaths, []string{"data/app.db"}) {
		t.Fatalf("SQLitePaths = %v", res.SQLitePaths)
	}
	if res.Torn != 0 || res.Stale != 0 {
		t.Fatalf("torn %d, stale %d", res.Torn, res.Stale)
	}

	time.Sleep(10 * time.Millisecond)
	insert(15)
	after, _ := os.Stat(p)
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Skip("the database file moved; the test needs commits that stay in the WAL")
	}
	res2 := backupDir(t, r, src, dir)

	for _, c := range []struct {
		id   string
		rows int
	}{{res.ID, 10}, {res2.ID, 25}} {
		out := filepath.Join(t.TempDir(), "out")
		if _, err := r.RestoreDir(context.Background(), c.id, out); err != nil {
			t.Fatal(err)
		}
		got := filepath.Join(out, "data", "app.db")
		for _, s := range []string{"-wal", "-shm", "-journal"} {
			if _, err := os.Stat(got + s); !os.IsNotExist(err) {
				t.Fatalf("restored %s next to the copy", s)
			}
		}
		if fi, err := os.Stat(got); err != nil || fi.Mode().Perm() != 0o640 {
			t.Fatalf("restored mode: %v, %v", fi, err)
		}
		if n := rowCount(t, got); n != c.rows {
			t.Fatalf("snapshot %s restored %d rows, want %d", c.id, n, c.rows)
		}
		if b, _ := os.ReadFile(filepath.Join(out, "notes.txt")); string(b) != "plain" {
			t.Fatalf("notes.txt = %q", b)
		}
	}
	if entries, _ := os.ReadDir(r.scratchDir()); len(entries) != 0 {
		t.Fatalf("scratch left behind: %v", entries)
	}
}

// TestSQLiteWithoutRoomIsReadLive leaves a database the scratch disk has
// no room for to the live copy, which stores it and its WAL as read.
func TestSQLiteWithoutRoomIsReadLive(t *testing.T) {
	old := scratchReserve
	scratchReserve = func(int64) int64 { return 1 << 62 }
	t.Cleanup(func() { scratchReserve = old })

	f := newFixture(t)
	r := f.create()
	dir := t.TempDir()
	db := openWAL(t, filepath.Join(dir, "app.db"))
	if _, err := db.Exec("CREATE TABLE t (v BLOB); INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	res := backupDir(t, r, Source{Job: "j", App: "a", Name: "files"}, dir)
	if len(res.SQLitePaths) != 0 {
		t.Fatalf("copied without room: %v", res.SQLitePaths)
	}
	out := filepath.Join(t.TempDir(), "out")
	if _, err := r.RestoreDir(context.Background(), res.ID, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "app.db-wal")); err != nil {
		t.Fatalf("the live copy dropped the WAL: %v", err)
	}
}
