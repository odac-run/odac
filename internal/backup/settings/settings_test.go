package settings

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"odac/internal/backup/proto"
	"odac/internal/backup/thin"
	"odac/internal/config"
	"odac/internal/logx"
)

const serverID = "0123456789abcdef0123456789abcdef"

// fakeInit stands in for odac-backup's init: it keeps one ownership
// number per server directory and applies the same rule.
type fakeInit struct {
	mu    sync.Mutex
	files map[string]int64
	calls []Access
	fail  error
}

func (f *fakeInit) Init(_ context.Context, a Access) (*proto.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, a)
	if f.fail != nil {
		return nil, f.fail
	}
	b, _ := json.Marshal(a.Repo.Storage)
	place := string(b)
	if a.Target != nil {
		place += a.Target.Root
	}
	o := a.Repo.Owner
	if o == nil {
		return nil, errors.New("init without an owner claim")
	}
	seq := o.Seq
	if cur, ok := f.files[place]; ok && cur >= seq {
		if !o.Adopt {
			return nil, &proto.Error{Code: proto.CodeOwnerConflict, Message: "the ownership file is ahead"}
		}
		seq = cur + 1
	}
	f.files[place] = seq
	return &proto.Result{Created: true, Seq: seq, SameDisk: a.Repo.Storage.Filesystem != nil}, nil
}

type fakeApps []App

type fakeLocator map[string]bool

func (f fakeLocator) IsLocal(_ context.Context, host string) bool { return f[host] }

type fixture struct {
	s     *Settings
	store *config.Store
	init  *fakeInit
	now   time.Time
	apps  fakeApps
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	store, err := config.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	fx := &fixture{
		store: store,
		init:  &fakeInit{files: map[string]int64{}},
		now:   time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
		apps: fakeApps{
			{Name: "web", Linked: []string{"db", "cache"}},
			{Name: "db", Stateful: true},
			{Name: "cache"},
			{Name: "files", Stateful: true},
		},
	}
	fx.s = fx.open(t)
	return fx
}

func (fx *fixture) open(t *testing.T) *Settings {
	t.Helper()
	s, err := New(Config{
		Store:      fx.store,
		ServerID:   serverID,
		ServerName: func() string { return "web-1" },
		Init:       fx.init,
		Apps:       &fx.apps,
		Locate:     fakeLocator{"127.0.0.1": true, "nas.lan": false},
		Protected:  ProtectedPaths("/app/.odac", "/var/lib/odac"),
		Log:        logx.New("Test"),
		Now:        func() time.Time { return fx.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *fakeApps) BackupApps() []App { return *f }

func s3Spec(name string) TargetSpec {
	return TargetSpec{Name: name, S3: &S3{
		Endpoint: "s3.eu-central-1.amazonaws.com", Region: "eu-central-1", Bucket: "odac-backups",
		Prefix: "/prod/", AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "SECRET-S3-KEY-VALUE",
	}}
}

func (fx *fixture) addTarget(t *testing.T, spec TargetSpec) TargetView {
	t.Helper()
	p, err := fx.s.PrepareTarget(context.Background(), spec)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	v, err := fx.s.ConfirmTarget(context.Background(), p.Token, false)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	return v
}

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return "error: " + err.Error()
}

func TestTargetIsSavedOnlyOnceConfirmed(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	p, err := fx.s.PrepareTarget(ctx, s3Spec("Offsite"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^([a-z2-7]{4}-){7}[a-z2-7]{4}$`).MatchString(p.Recovery.Key) {
		t.Fatalf("generated key %q", p.Recovery.Key)
	}
	if p.Recovery.Target.KeySource != KeyGenerated || p.Recovery.Directory != serverID+"/repository" {
		t.Fatalf("recovery: %+v", p.Recovery)
	}
	if list, _ := fx.s.Targets(); len(list) != 0 || len(fx.init.calls) != 0 {
		t.Fatalf("saved or initialized before confirmation: %v, %d", list, len(fx.init.calls))
	}

	v, err := fx.s.ConfirmTarget(ctx, p.Token, false)
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != KindS3 || v.Location != Remote || v.S3.Prefix != "prod" || v.SameDisk {
		t.Fatalf("view: %+v", v)
	}
	if _, err := fx.s.ConfirmTarget(ctx, p.Token, false); code(err) != CodeExpired {
		t.Fatalf("second confirm: %v", err)
	}

	// The key and the secret are on disk, in the module only.
	raw, err := os.ReadFile(filepath.Join(fx.store.BaseDir(), "config", "backup.json"))
	if err != nil || !strings.Contains(string(raw), p.Recovery.Key) || !strings.Contains(string(raw), "SECRET-S3-KEY-VALUE") {
		t.Fatalf("module file: %s, %v", raw, err)
	}
	list, _ := fx.s.Targets()
	out, _ := json.Marshal(list)
	if strings.Contains(string(out), p.Recovery.Key) || strings.Contains(string(out), "SECRET") {
		t.Fatalf("a secret reached the list: %s", out)
	}
	rec, err := fx.s.Key(v.ID)
	if err != nil || rec.Key != p.Recovery.Key || rec.ServerName != "web-1" {
		t.Fatalf("key: %+v, %v", rec, err)
	}
	if b, _ := json.Marshal(rec); strings.Contains(string(b), "SECRET") {
		t.Fatalf("the recovery file carries the storage secret: %s", b)
	}
}

func TestViewsCannotCarrySecrets(t *testing.T) {
	tg := &Target{
		Name: "x", Key: "SECRET-1",
		SMB:    &SMB{Addr: "nas", Share: "b", Username: "u", Password: "SECRET-2"},
		S3:     &S3{Endpoint: "e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "SECRET-3"},
		WebDAV: &WebDAV{URL: "https://d", Username: "u", Password: "SECRET-4"},
		NFS:    &NFS{Addr: "nas", Export: "/e"},
		Disk:   &Disk{Path: "/mnt/b"},
	}
	s := &Settings{owners: newOwners(t.TempDir())}
	b, _ := json.Marshal(s.view(newID(), tg))
	if strings.Contains(string(b), "SECRET") {
		t.Fatalf("view: %s", b)
	}
}

func TestUserKey(t *testing.T) {
	fx := newFixture(t)
	for key, want := range map[string]string{
		"short":                         CodeInvalid,
		" sixteen chars ok but spaced ": CodeInvalid,
		"tab\tinside a long passphrase": CodeInvalid,
		"a passphrase of my own choice": "",
	} {
		spec := s3Spec("K " + key)
		spec.Key = key
		p, err := fx.s.PrepareTarget(context.Background(), spec)
		if code(err) != want {
			t.Errorf("%q: %v", key, err)
		}
		if err == nil && (p.Recovery.Key != key || p.Recovery.Target.KeySource != KeyEntered) {
			t.Errorf("%q: %+v", key, p.Recovery)
		}
	}
}

func TestTargetValidation(t *testing.T) {
	fx := newFixture(t)
	disk := func(p string) TargetSpec { return TargetSpec{Name: "d", Disk: &Disk{Path: p}} }
	for name, c := range map[string]struct {
		spec TargetSpec
		code string
	}{
		"root":            {disk("/"), CodeInvalid},
		"etc":             {disk("/etc/backups"), CodeInvalid},
		"docker":          {disk("/var/lib/docker/x"), CodeInvalid},
		"var lib":         {disk("/var/lib/"), CodeInvalid},
		"odac data":       {disk("/var/lib/odac/backups"), CodeInvalid},
		"odac base":       {disk("/app/.odac"), CodeInvalid},
		"tmp":             {disk("/tmp/b"), CodeInvalid},
		"relative":        {disk("backups"), CodeInvalid},
		"climb":           {disk("/mnt/../etc"), CodeInvalid},
		"mnt":             {disk("/mnt/backup"), ""},
		"var lib sibling": {disk("/var/lib/backups"), ""},
		"none":            {TargetSpec{Name: "n"}, CodeInvalid},
		"two": {TargetSpec{Name: "two", Disk: &Disk{Path: "/mnt/b"},
			NFS: &NFS{Addr: "nas", Export: "/e"}}, CodeInvalid},
		"no name":          {TargetSpec{Disk: &Disk{Path: "/mnt/b"}}, CodeInvalid},
		"smb comma":        {TargetSpec{Name: "s", SMB: &SMB{Addr: "nas.lan", Share: "b", Username: "u", Password: "a,b"}}, CodeInvalid},
		"smb ok":           {TargetSpec{Name: "s", SMB: &SMB{Addr: "nas.lan", Share: "b", Path: "odac/x", Username: "u", Password: "p"}}, ""},
		"smb climb":        {TargetSpec{Name: "s", SMB: &SMB{Addr: "nas.lan", Share: "b", Path: "../x"}}, ""},
		"nfs relative":     {TargetSpec{Name: "n", NFS: &NFS{Addr: "nas.lan", Export: "srv"}}, CodeInvalid},
		"s3 scheme":        {TargetSpec{Name: "s", S3: &S3{Endpoint: "https://s3.x", Bucket: "bucket", AccessKeyID: "a", SecretAccessKey: "b"}}, CodeInvalid},
		"s3 no secret":     {TargetSpec{Name: "s", S3: &S3{Endpoint: "s3.x", Bucket: "bucket", AccessKeyID: "a"}}, CodeInvalid},
		"s3 prefix climb":  {TargetSpec{Name: "s", S3: &S3{Endpoint: "s3.x", Bucket: "bucket", Prefix: "a/../b", AccessKeyID: "a", SecretAccessKey: "b"}}, CodeInvalid},
		"webdav userinfo":  {TargetSpec{Name: "w", WebDAV: &WebDAV{URL: "https://u:p@dav.x/b"}}, CodeInvalid},
		"webdav ftp":       {TargetSpec{Name: "w", WebDAV: &WebDAV{URL: "ftp://dav.x/b"}}, CodeInvalid},
		"webdav ok":        {TargetSpec{Name: "w", WebDAV: &WebDAV{URL: "https://dav.x/b/"}}, ""},
		"control in name":  {TargetSpec{Name: "a\nb", Disk: &Disk{Path: "/mnt/b"}}, CodeInvalid},
		"name too long":    {TargetSpec{Name: strings.Repeat("x", 65), Disk: &Disk{Path: "/mnt/b"}}, CodeInvalid},
		"name at the edge": {TargetSpec{Name: strings.Repeat("x", 64), Disk: &Disk{Path: "/mnt/b"}}, ""},
	} {
		if _, err := fx.s.PrepareTarget(context.Background(), c.spec); code(err) != c.code {
			t.Errorf("%s: %v, want %q", name, err, c.code)
		}
	}
}

func TestOneStorageOneTarget(t *testing.T) {
	fx := newFixture(t)
	fx.addTarget(t, s3Spec("Offsite"))
	same := s3Spec("Other name")
	same.S3.Prefix = "prod"
	if _, err := fx.s.PrepareTarget(context.Background(), same); code(err) != CodeExists {
		t.Fatalf("same storage: %v", err)
	}
	other := s3Spec("offsite")
	other.S3.Prefix = "staging"
	if _, err := fx.s.PrepareTarget(context.Background(), other); code(err) != CodeExists {
		t.Fatalf("same name: %v", err)
	}
}

func TestLocationLabels(t *testing.T) {
	fx := newFixture(t)
	if v := fx.addTarget(t, TargetSpec{Name: "disk", Disk: &Disk{Path: "/mnt/backup"}}); v.Location != Local || !v.SameDisk {
		t.Fatalf("disk: %+v", v)
	}
	if v := fx.addTarget(t, TargetSpec{Name: "minio", S3: &S3{Endpoint: "127.0.0.1:9000", Bucket: "bucket", AccessKeyID: "a", SecretAccessKey: "b", DisableTLS: true}}); v.Location != Local || v.SameDisk {
		t.Fatalf("minio: %+v", v)
	}
	c, _ := fx.s.Coverage()
	if !c.OnlyLocal || len(c.SameDisk) != 1 {
		t.Fatalf("coverage: %+v", c)
	}
	fx.addTarget(t, TargetSpec{Name: "nas", NFS: &NFS{Addr: "nas.lan", Export: "/srv/b"}})
	if c, _ := fx.s.Coverage(); c.OnlyLocal {
		t.Fatalf("a remote target left only local: %+v", c)
	}
}

func TestAccessLayout(t *testing.T) {
	fx := newFixture(t)
	disk := fx.addTarget(t, TargetSpec{Name: "disk", Disk: &Disk{Path: "/mnt/backup/"}})
	smb := fx.addTarget(t, TargetSpec{Name: "smb", SMB: &SMB{Addr: "nas.lan", Share: "b", Path: "/odac/", Username: "u", Password: "p"}})
	s3 := fx.addTarget(t, s3Spec("s3"))
	dav := fx.addTarget(t, TargetSpec{Name: "dav", WebDAV: &WebDAV{URL: "https://dav.x/b/", Username: "u", Password: "p"}})
	dir := serverID + "/repository"

	a, err := fx.s.Access(disk.ID, AccessOptions{})
	if err != nil || a.Target.Root != "/mnt/backup" || a.Repo.Storage.Filesystem.Path != "/"+dir || a.Repo.Owner != nil || a.Repo.ServerID != serverID {
		t.Fatalf("disk: %+v, %v", a, err)
	}
	a, _ = fx.s.Access(smb.ID, AccessOptions{Write: true})
	if a.Target.Share.Type != "cifs" || a.Target.Share.Password != "p" || a.Repo.Storage.Filesystem.Path != "/odac/"+dir || a.Repo.Owner == nil {
		t.Fatalf("smb: %+v", a)
	}
	a, _ = fx.s.Access(s3.ID, AccessOptions{})
	if a.Target != nil || a.Repo.Storage.S3.Prefix != "prod/"+dir+"/" || a.Repo.Storage.S3.SecretAccessKey != "SECRET-S3-KEY-VALUE" || a.Repo.Password == "" {
		t.Fatalf("s3: %+v", a)
	}
	a, _ = fx.s.Access(dav.ID, AccessOptions{})
	if a.Repo.Storage.WebDAV.URL != "https://dav.x/b/"+dir {
		t.Fatalf("webdav: %+v", a.Repo.Storage.WebDAV)
	}

	other := "ffffffffffffffffffffffffffffffff"
	a, err = fx.s.Access(disk.ID, AccessOptions{Server: other})
	if err != nil || !a.Repo.ReadOnly || a.Repo.ServerID != serverID || a.Repo.Storage.Filesystem.Path != "/"+other+"/repository" {
		t.Fatalf("other server: %+v, %v", a, err)
	}
	if _, err := fx.s.Access(disk.ID, AccessOptions{Server: other, Write: true}); code(err) != CodeInvalid {
		t.Fatalf("write to another server: %v", err)
	}
	if _, err := fx.s.Access(disk.ID, AccessOptions{Server: "../x"}); code(err) != CodeInvalid {
		t.Fatalf("bad server id: %v", err)
	}
	// Each kind's request passes the protocol's own check.
	for _, id := range []string{disk.ID, smb.ID, s3.ID, dav.ID} {
		a, _ := fx.s.Access(id, AccessOptions{Write: true})
		r := proto.Request{Version: proto.Version, Op: proto.OpInit, Repo: a.Repo}
		if err := r.Validate(); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

func TestOwnerSequenceSurvivesRestartAndStopsOnConflict(t *testing.T) {
	fx := newFixture(t)
	v := fx.addTarget(t, TargetSpec{Name: "disk", Disk: &Disk{Path: "/mnt/backup"}})
	seqs := []int64{}
	for range 3 {
		a, err := fx.s.Access(v.ID, AccessOptions{Write: true})
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, a.Repo.Owner.Seq)
	}
	if seqs[0] != 2 || seqs[2] != 4 {
		t.Fatalf("sequence after init at 1: %v", seqs)
	}
	// A restarted server continues from the number it persisted.
	s2 := fx.open(t)
	a, err := s2.Access(v.ID, AccessOptions{Write: true})
	if err != nil || a.Repo.Owner.Seq != 5 || a.Repo.Owner.Name != "web-1" {
		t.Fatalf("after restart: %+v, %v", a.Repo.Owner, err)
	}

	s2.Observe(v.ID, errors.New("unrelated"))
	if _, err := s2.Access(v.ID, AccessOptions{Write: true}); err != nil {
		t.Fatalf("an unrelated failure stopped writes: %v", err)
	}
	s2.Observe(v.ID, &proto.Error{Code: proto.CodeOwnerConflict, Message: "file holds 9"})
	if _, err := s2.Access(v.ID, AccessOptions{Write: true}); code(err) != CodeOwnerConflict {
		t.Fatalf("write after a conflict: %v", err)
	}
	if _, err := s2.Access(v.ID, AccessOptions{}); err != nil {
		t.Fatalf("a read after a conflict: %v", err)
	}
	if tv, _ := s2.Target(v.ID); tv.Conflict == nil || tv.Conflict.Message != "file holds 9" {
		t.Fatalf("view: %+v", tv)
	}
	if c, _ := s2.Coverage(); len(c.Conflicts) != 1 {
		t.Fatalf("coverage: %+v", c)
	}

	if _, err := s2.Adopt(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	a, err = s2.Access(v.ID, AccessOptions{Write: true})
	if err != nil || a.Repo.Owner.Seq <= 7 {
		t.Fatalf("after adopt: %+v, %v", a.Repo.Owner, err)
	}
	if tv, _ := s2.Target(v.ID); tv.Conflict != nil {
		t.Fatalf("conflict kept: %+v", tv.Conflict)
	}
}

func TestConfirmFailures(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	spec := TargetSpec{Name: "disk", Disk: &Disk{Path: "/mnt/backup"}}

	// The directory already holds this server's data from before.
	place, _ := json.Marshal(proto.Storage{Filesystem: &proto.FilesystemStorage{Path: "/" + serverID + "/repository"}})
	fx.init.files[string(place)+"/mnt/backup"] = 12
	p, _ := fx.s.PrepareTarget(ctx, spec)
	if _, err := fx.s.ConfirmTarget(ctx, p.Token, false); code(err) != CodeOwnerConflict {
		t.Fatalf("claimed directory: %v", err)
	}
	if list, _ := fx.s.Targets(); len(list) != 0 {
		t.Fatal("saved despite the conflict")
	}
	v, err := fx.s.ConfirmTarget(ctx, p.Token, true)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := fx.s.Access(v.ID, AccessOptions{Write: true})
	if a.Repo.Owner.Seq != 14 {
		t.Fatalf("after adopting at 13: %d", a.Repo.Owner.Seq)
	}

	fx.init.fail = &proto.Error{Code: proto.CodeWrongPassword, Message: "wrong"}
	p, _ = fx.s.PrepareTarget(ctx, TargetSpec{Name: "other", Disk: &Disk{Path: "/mnt/other"}})
	if _, err := fx.s.ConfirmTarget(ctx, p.Token, false); code(err) != CodeUnreachable {
		t.Fatalf("wrong key: %v", err)
	}
	fx.init.fail = errors.New("dial tcp: SECRET-S3-KEY-VALUE refused")
	p2, _ := fx.s.PrepareTarget(ctx, s3Spec("s3"))
	_, err = fx.s.ConfirmTarget(ctx, p2.Token, false)
	if code(err) != CodeUnreachable || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unreachable: %v", err)
	}
	fx.init.fail = nil
	if _, err := fx.s.ConfirmTarget(ctx, p.Token, false); err != nil {
		t.Fatalf("retry after the cause was fixed: %v", err)
	}

	p, _ = fx.s.PrepareTarget(ctx, TargetSpec{Name: "late", Disk: &Disk{Path: "/mnt/late"}})
	fx.now = fx.now.Add(pendingTTL)
	if _, err := fx.s.ConfirmTarget(ctx, p.Token, false); code(err) != CodeExpired {
		t.Fatalf("expired: %v", err)
	}
}

func TestPendingIsBounded(t *testing.T) {
	fx := newFixture(t)
	var first Pending
	for i := range maxPending + 3 {
		p, err := fx.s.PrepareTarget(context.Background(), TargetSpec{Name: "t" + string(rune('a'+i)), Disk: &Disk{Path: "/mnt/" + string(rune('a'+i))}})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = p
		}
		fx.now = fx.now.Add(time.Second)
	}
	fx.s.mu.Lock()
	n := len(fx.s.pending)
	fx.s.mu.Unlock()
	if n != maxPending {
		t.Fatalf("%d pending", n)
	}
	if _, err := fx.s.ConfirmTarget(context.Background(), first.Token, false); code(err) != CodeExpired {
		t.Fatalf("oldest kept: %v", err)
	}
}

func TestUpdateTarget(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	v := fx.addTarget(t, s3Spec("Offsite"))
	calls := len(fx.init.calls)

	rename := s3Spec("Renamed")
	rename.S3.SecretAccessKey = ""
	got, err := fx.s.UpdateTarget(ctx, v.ID, rename)
	if err != nil || got.Name != "Renamed" || len(fx.init.calls) != calls {
		t.Fatalf("rename: %+v, %v, init calls %d", got, err, len(fx.init.calls)-calls)
	}
	a, _ := fx.s.Access(v.ID, AccessOptions{})
	if a.Repo.Storage.S3.SecretAccessKey != "SECRET-S3-KEY-VALUE" {
		t.Fatal("an empty secret replaced the saved one")
	}

	rotate := s3Spec("Renamed")
	rotate.S3.SecretAccessKey = "NEW-SECRET"
	fx.init.fail = errors.New("denied")
	if _, err := fx.s.UpdateTarget(ctx, v.ID, rotate); code(err) != CodeUnreachable {
		t.Fatalf("failed rotation: %v", err)
	}
	if a, _ := fx.s.Access(v.ID, AccessOptions{}); a.Repo.Storage.S3.SecretAccessKey != "SECRET-S3-KEY-VALUE" {
		t.Fatal("a secret that did not open the target was saved")
	}
	fx.init.fail = nil
	if _, err := fx.s.UpdateTarget(ctx, v.ID, rotate); err != nil {
		t.Fatal(err)
	}
	if a, _ := fx.s.Access(v.ID, AccessOptions{}); a.Repo.Storage.S3.SecretAccessKey != "NEW-SECRET" {
		t.Fatal("rotation not saved")
	}

	moved := s3Spec("Renamed")
	moved.S3.Bucket = "another-bucket"
	if _, err := fx.s.UpdateTarget(ctx, v.ID, moved); code(err) != CodeInvalid {
		t.Fatalf("moved: %v", err)
	}
	keyed := s3Spec("Renamed")
	keyed.Key = "a new passphrase of mine"
	if _, err := fx.s.UpdateTarget(ctx, v.ID, keyed); code(err) != CodeInvalid {
		t.Fatalf("key change: %v", err)
	}
}

func TestJobs(t *testing.T) {
	fx := newFixture(t)
	tg := fx.addTarget(t, s3Spec("Offsite"))

	j, err := fx.s.CreateJob(JobSpec{Name: "Nightly", Target: tg.ID, Apps: []string{"web", "db", "web"}})
	if err != nil {
		t.Fatal(err)
	}
	if j.Every != DefaultEvery || j.Density != thin.Normal || len(j.Apps) != 2 || j.Created == 0 {
		t.Fatalf("defaults: %+v", j)
	}
	for name, c := range map[string]struct {
		spec JobSpec
		code string
	}{
		"unknown app":  {JobSpec{Name: "a", Target: tg.ID, Apps: []string{"nope"}}, CodeNotFound},
		"bad app name": {JobSpec{Name: "a", Target: tg.ID, Apps: []string{"../x"}}, CodeInvalid},
		"no target":    {JobSpec{Name: "a", Target: "0000000000000000", AllApps: true}, CodeNotFound},
		"both":         {JobSpec{Name: "a", Target: tg.ID, AllApps: true, Apps: []string{"web"}}, CodeInvalid},
		"nothing":      {JobSpec{Name: "a", Target: tg.ID}, CodeInvalid},
		"too often":    {JobSpec{Name: "a", Target: tg.ID, Server: true, Every: 5}, CodeInvalid},
		"density":      {JobSpec{Name: "a", Target: tg.ID, Server: true, Density: -1}, CodeInvalid},
		"max age":      {JobSpec{Name: "a", Target: tg.ID, Server: true, MaxAgeDays: -1}, CodeInvalid},
		"same name":    {JobSpec{Name: "nightly", Target: tg.ID, Server: true}, CodeExists},
		"server only":  {JobSpec{Name: "State", Target: tg.ID, Server: true, Every: 60, Density: thin.High}, ""},
	} {
		if _, err := fx.s.CreateJob(c.spec); code(err) != c.code {
			t.Errorf("%s: %v, want %q", name, err, c.code)
		}
	}

	// An app that is gone stays on a job until the operator takes it off.
	fx.apps = fx.apps[1:]
	up, err := fx.s.UpdateJob(j.ID, JobSpec{Name: "Nightly", Apps: []string{"web", "db"}, Every: 120})
	if err != nil || up.Every != 120 || up.Target != tg.ID || up.Created != j.Created {
		t.Fatalf("update: %+v, %v", up, err)
	}
	other := fx.addTarget(t, TargetSpec{Name: "disk", Disk: &Disk{Path: "/mnt/b"}})
	if _, err := fx.s.UpdateJob(j.ID, JobSpec{Name: "Nightly", Target: other.ID, AllApps: true}); code(err) != CodeInvalid {
		t.Fatalf("target change: %v", err)
	}
	if err := fx.s.DeleteTarget(tg.ID); code(err) != CodeInUse {
		t.Fatalf("delete target in use: %v", err)
	}

	jobs, _ := fx.s.Jobs()
	for _, jv := range jobs {
		if err := fx.s.DeleteJob(jv.ID, jv.Name == "State"); err != nil {
			t.Fatal(err)
		}
	}
	retired, _ := fx.s.Retired()
	if len(retired) != 2 {
		t.Fatalf("retired: %+v", retired)
	}
	var kept, purged string
	for _, r := range retired {
		if r.Purge {
			purged = r.ID
		} else {
			kept = r.ID
		}
	}
	if kept == "" || purged == "" {
		t.Fatalf("keeping is the default: %+v", retired)
	}
	if err := fx.s.DeleteTarget(tg.ID); code(err) != CodeInUse {
		t.Fatalf("delete target with a purge pending: %v", err)
	}
	fx.s.Purged(kept)
	fx.s.Purged(purged)
	if retired, _ := fx.s.Retired(); len(retired) != 1 || retired[0].ID != kept {
		t.Fatalf("after purge: %+v", retired)
	}
	if err := fx.s.DeleteTarget(tg.ID); err != nil {
		t.Fatal(err)
	}
	if retired, _ := fx.s.Retired(); len(retired) != 0 {
		t.Fatalf("kept backups of a deleted target listed: %+v", retired)
	}
	if _, err := os.Stat(filepath.Join(ownerDir(fx.store.BaseDir()), tg.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("owner state left: %v", err)
	}
}

func TestCoverage(t *testing.T) {
	fx := newFixture(t)
	c, err := fx.s.Coverage()
	if err != nil || !c.NoJobs || strings.Join(c.Unprotected, ",") != "db,files" || c.OnlyLocal {
		t.Fatalf("empty: %+v, %v", c, err)
	}
	tg := fx.addTarget(t, s3Spec("Offsite"))
	fx.s.CreateJob(JobSpec{Name: "Web", Target: tg.ID, Apps: []string{"web"}})
	c, _ = fx.s.Coverage()
	// cache is linked but holds nothing; only the database is suggested.
	if c.NoJobs || len(c.MissingLinks) != 1 || c.MissingLinks[0].Linked != "db" || c.MissingLinks[0].App != "web" {
		t.Fatalf("missing link: %+v", c)
	}
	if strings.Join(c.Unprotected, ",") != "db,files" {
		t.Fatalf("unprotected: %+v", c.Unprotected)
	}
	fx.s.CreateJob(JobSpec{Name: "All", Target: tg.ID, AllApps: true})
	c, _ = fx.s.Coverage()
	if len(c.Unprotected) != 0 || len(c.MissingLinks) != 1 {
		t.Fatalf("all apps: %+v", c)
	}
	b, _ := json.Marshal(Coverage{})
	if string(b) != "{}" {
		t.Fatalf("empty coverage serializes as %s", b)
	}
}

func TestSnapshots(t *testing.T) {
	fx := newFixture(t)
	if s, _ := fx.s.Snapshots(); !s.Enabled || s.MaxDiskPct != DefaultMaxDiskPct {
		t.Fatalf("defaults: %+v", s)
	}
	off, pct := false, 10
	if s, err := fx.s.SetSnapshots(SnapshotsPatch{Enabled: &off}); err != nil || s.Enabled || s.MaxDiskPct != DefaultMaxDiskPct {
		t.Fatalf("off: %+v, %v", s, err)
	}
	if s, _ := fx.s.SetSnapshots(SnapshotsPatch{MaxDiskPct: &pct}); s.Enabled || s.MaxDiskPct != 10 {
		t.Fatalf("pct: %+v", s)
	}
	big := 90
	if _, err := fx.s.SetSnapshots(SnapshotsPatch{MaxDiskPct: &big}); code(err) != CodeInvalid {
		t.Fatalf("bound: %v", err)
	}
	fx.store.Reload()
	if s, _ := fx.s.Snapshots(); s.Enabled || s.MaxDiskPct != 10 {
		t.Fatalf("after reload: %+v", s)
	}
}

func TestMalformedModuleIsNotOverwritten(t *testing.T) {
	fx := newFixture(t)
	fx.store.Mutate(func() { fx.store.Set(Module, map[string]any{"targets": "not a map"}) })
	if _, err := fx.s.CreateJob(JobSpec{Name: "x", Server: true}); err == nil {
		t.Fatal("a change was applied over a malformed module")
	}
	var raw any
	fx.store.View(func() { raw = fx.store.Get(Module) })
	if m, _ := raw.(map[string]any); m["targets"] != "not a map" {
		t.Fatalf("module rewritten: %v", raw)
	}
}

func TestHostLocator(t *testing.T) {
	l := HostLocator{}
	ctx := context.Background()
	for host, want := range map[string]bool{
		"localhost": true, "LOCALHOST.": true, "minio.localhost": true,
		"127.0.0.1": true, "::1": true, "203.0.113.7": false, "": false,
	} {
		if got := l.IsLocal(ctx, host); got != want {
			t.Errorf("%q: %v", host, got)
		}
	}
	own, _ := localNets()
	for _, ip := range own {
		if !ip.IsLoopback() && !l.IsLocal(ctx, ip.String()) {
			t.Errorf("own address %s read as remote", ip)
		}
	}
}
