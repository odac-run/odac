package sqlitecopy

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// writerEnv makes the test binary a writer process: SQLite's locks are
// per process, so only another process shows what an app's writes do to a
// copy.
const writerEnv = "SQLITECOPY_TEST_WRITER"

func TestMain(m *testing.M) {
	if p := os.Getenv(writerEnv); p != "" {
		os.Exit(write(p))
	}
	os.Exit(m.Run())
}

// write commits pairs of rows forever, so every consistent state has as
// many rows in a as in b.
func write(path string) int {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return 1
	}
	db.SetMaxOpenConns(1)
	for i := 0; ; i++ {
		tx, err := db.Begin()
		if err != nil {
			return 1
		}
		tx.Exec("INSERT INTO a (v) VALUES (randomblob(300))")
		tx.Exec("INSERT INTO b (v) VALUES (randomblob(300))")
		if err := tx.Commit(); err != nil {
			return 1
		}
	}
}

func create(t *testing.T, path, journal string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode("+journal+")&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		"CREATE TABLE a (id INTEGER PRIMARY KEY, v BLOB)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY, v BLOB)",
		"INSERT INTO a (v) VALUES (randomblob(300))",
		"INSERT INTO b (v) VALUES (randomblob(300))",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func counts(t *testing.T, path string) (int, int) {
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
	var a, b int
	if err := db.QueryRow("SELECT (SELECT count(*) FROM a), (SELECT count(*) FROM b)").Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func TestParse(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.db")
	create(t, p, "wal").Close()
	hd, ok, err := Detect(p)
	if err != nil || !ok || !hd.WAL || hd.PageSize != 4096 {
		t.Fatalf("Detect = %+v, %v, %v", hd, ok, err)
	}
	q := filepath.Join(dir, "y.db")
	create(t, q, "delete").Close()
	if hd, ok, _ := Detect(q); !ok || hd.WAL {
		t.Fatalf("rollback database: %+v, %v", hd, ok)
	}

	h, _ := os.ReadFile(p)
	for name, mutate := range map[string]func([]byte){
		"magic":     func(b []byte) { b[0] = 'X' },
		"page size": func(b []byte) { b[16], b[17] = 0x03, 0x00 },
		"fraction":  func(b []byte) { b[21] = 63 },
	} {
		b := append([]byte(nil), h[:HeaderSize]...)
		mutate(b)
		if _, ok := Parse(b, int64(len(h))); ok {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, ok := Parse(h[:HeaderSize], int64(len(h))+1); ok {
		t.Error("a size that is not a page multiple was accepted")
	}
	if _, ok := Parse(h[:50], int64(len(h))); ok {
		t.Error("a short header was accepted")
	}
	other := filepath.Join(dir, "text")
	os.WriteFile(other, make([]byte, 4096), 0o644)
	if _, ok, err := Detect(other); ok || err != nil {
		t.Fatalf("zeros detected: %v, %v", ok, err)
	}
}

// TestCopyUnderWrites copies a WAL database while another process commits
// to it, with the WAL index read-only to the copier as it is through a
// read-only mount. Every copy must pass integrity_check and hold a state
// the writer committed.
func TestCopyUnderWrites(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.db")
	create(t, p, "wal").Close()

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), writerEnv+"="+p)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })

	deadline := time.Now().Add(10 * time.Second)
	for {
		a, _ := counts(t, p)
		if a > 50 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer made no progress")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, f := range []string{p, p + "-wal", p + "-shm"} {
		if err := os.Chmod(f, 0o444); err != nil {
			t.Fatal(err)
		}
	}

	out := t.TempDir()
	last := 0
	for i := range 5 {
		dst := filepath.Join(out, "copy"+string(rune('0'+i))+".db")
		info, err := Copy(context.Background(), p, dst, 10*time.Second)
		if err != nil {
			t.Fatalf("copy %d: %v", i, err)
		}
		if !info.WAL || info.BlocksWriters || info.Bytes == 0 {
			t.Fatalf("info = %+v", info)
		}
		a, b := counts(t, dst)
		if a != b || a < last {
			t.Fatalf("copy %d is torn: a=%d b=%d (previous %d)", i, a, b, last)
		}
		last = a
		for _, s := range Siblings(dst) {
			if s == dst+"-journal" {
				continue
			}
			os.Remove(s)
		}
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o444 {
		t.Fatalf("source changed mode: %v %v", fi, err)
	}
}

func TestCopyRollbackWaitsBounded(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.db")
	db := create(t, p, "delete")
	defer db.Close()

	info, err := Copy(context.Background(), p, filepath.Join(dir, "free.db"), time.Second)
	if err != nil || !info.BlocksWriters {
		t.Fatalf("copy = %+v, %v", info, err)
	}

	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "locked.db")
	start := time.Now()
	_, err = Copy(context.Background(), p, dst, 300*time.Millisecond)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("copy under an exclusive lock = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("waited %s", time.Since(start))
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("a failed copy left its destination")
	}
	conn.ExecContext(context.Background(), "ROLLBACK")
}

func TestCopyRefuses(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.db")
	create(t, p, "wal").Close()
	exists := filepath.Join(dir, "exists")
	os.WriteFile(exists, nil, 0o644)
	if _, err := Copy(context.Background(), p, exists, time.Second); err == nil {
		t.Fatal("overwrote an existing file")
	}
	text := filepath.Join(dir, "text")
	os.WriteFile(text, make([]byte, 1024), 0o644)
	if _, err := Copy(context.Background(), text, filepath.Join(dir, "out"), time.Second); err == nil {
		t.Fatal("copied a file that is not a database")
	}
	odd := filepath.Join(dir, "we?ird #name.db")
	if err := os.Rename(p, odd); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(context.Background(), odd, filepath.Join(dir, "odd-copy.db"), time.Second); err != nil {
		t.Fatalf("a path with URI characters: %v", err)
	}
}
