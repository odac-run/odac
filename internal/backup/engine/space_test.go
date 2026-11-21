package engine

import (
	"path/filepath"
	"testing"
)

func TestSpaceAttributesContentsToRuns(t *testing.T) {
	f := newFixture(t)
	r := f.create()
	shared := seededBytes(1, 2*mib)
	store := func(job, run string, files map[string][]byte) string {
		t.Helper()
		dir := t.TempDir()
		for name, data := range files {
			writeFile(t, filepath.Join(dir, name), data)
		}
		res, err := r.BackupDir(f.ctx, Source{Job: job, App: "web", Name: "files"}, dir, Meta{Run: run, Trigger: "schedule", Consistency: Live})
		if err != nil {
			t.Fatal(err)
		}
		return res.ID
	}
	x, y, z := seededBytes(2, 4*mib), seededBytes(3, 3*mib), seededBytes(4, 5*mib)
	r1 := store("a", "r1", map[string][]byte{"x": x, "s": shared})
	r2 := store("a", "r2", map[string][]byte{"y": y, "s": shared})
	store("b", "r3", map[string][]byte{"z": z, "s": shared})

	sp, err := r.Space(f.ctx, [][]string{{r1}, {r2}})
	if err != nil {
		t.Fatal(err)
	}
	near := func(what string, got, want int64) {
		t.Helper()
		if got < want*9/10 || got > want*11/10+64<<10 {
			t.Errorf("%s: %d bytes, want about %d", what, got, want)
		}
	}
	if !sp.Detailed || sp.Free <= 0 || sp.Total < sp.Free {
		t.Fatalf("space: %+v", sp)
	}
	near("garbage", sp.Garbage, 0)
	near("job a", sp.Jobs["a"], 9*mib)
	near("job b", sp.Jobs["b"], 7*mib)
	if len(sp.Freed) != 2 {
		t.Fatalf("freed: %+v", sp.Freed)
	}
	// The shared file stays referenced by job b, so the repository never
	// gets it back; job a does once both of its runs are gone.
	near("repo after r1", sp.Freed[0].Repo, 4*mib)
	near("job after r1", sp.Freed[0].Job, 4*mib)
	near("repo after r2", sp.Freed[1].Repo, 7*mib)
	near("job after r2", sp.Freed[1].Job, 9*mib)

	if err := r.Delete(f.ctx, []string{r1}); err != nil {
		t.Fatal(err)
	}
	sp, err = r.Space(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	near("garbage after the delete", sp.Garbage, 4*mib)
	if sp.Freed != nil {
		t.Fatalf("freed without an order: %+v", sp.Freed)
	}
}

func TestFreeSpaceWithoutAFilesystem(t *testing.T) {
	r := &Repo{cfg: Config{Storage: Storage{S3: &S3Storage{Bucket: "b"}}}}
	if sp := r.FreeSpace(); sp.Free != -1 || sp.Total != -1 {
		t.Fatalf("free space of a bucket: %+v", sp)
	}
}
