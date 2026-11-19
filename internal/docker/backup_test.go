package docker

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

func helperLimits() Limits { return Limits{Memory: 512 << 20, PidsLimit: 64} }

func validHelper() HelperSpec {
	return HelperSpec{
		Image:      "sha256:abc",
		Entrypoint: []string{"/app/bin/odac-backup", "run"},
		Labels:     map[string]string{"odac.backup.run": "r1"},
		Mounts: []HelperMount{
			{Source: "/app/.odac/apps/db/data", Target: "/backup/src/0", ReadOnly: true},
			{Source: "/mnt/disk", HostSource: true, Target: "/backup/target"},
		},
		Caps:   []string{"DAC_READ_SEARCH"},
		Limits: helperLimits(),
		Follow: true,
	}
}

func startHelper(t *testing.T, f *fakeAPI, spec HelperSpec) createCall {
	t.Helper()
	c := New(f, Options{HostRoot: "/var/lib/odac"})
	h, err := c.StartHelper(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	h.Remove()
	if len(f.created) != 1 {
		t.Fatalf("created %d containers", len(f.created))
	}
	return f.created[0]
}

// The helper holds the repository key and target credentials and reads
// every app's data, so what it is denied matters as much as what it gets.
func TestHelperContainment(t *testing.T) {
	f := newFakeAPI()
	call := startHelper(t, f, validHelper())
	hc, cfg := call.HostConfig, call.Config

	if !strings.HasPrefix(call.Name, "odac-bk-") || call.Name == "odac-backup" {
		t.Errorf("name %q: the updater owns odac-backup", call.Name)
	}
	if len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" || len(hc.CapAdd) != 1 {
		t.Errorf("caps drop %v add %v", hc.CapDrop, hc.CapAdd)
	}
	if hc.Privileged || !hc.ReadonlyRootfs || len(hc.SecurityOpt) != 1 || hc.SecurityOpt[0] != "no-new-privileges" {
		t.Errorf("privileged %v, read-only root %v, security %v", hc.Privileged, hc.ReadonlyRootfs, hc.SecurityOpt)
	}
	if hc.NetworkMode != "none" {
		t.Errorf("a helper with a mounted target got network %q", hc.NetworkMode)
	}
	if hc.Memory != 512<<20 || hc.MemorySwap != hc.Memory || hc.PidsLimit == nil || *hc.PidsLimit != 64 {
		t.Errorf("limits: memory %d swap %d pids %v", hc.Memory, hc.MemorySwap, hc.PidsLimit)
	}
	if len(hc.Binds) != 0 {
		t.Errorf("binds %v: every mount goes through Mounts", hc.Binds)
	}
	for _, m := range hc.Mounts {
		if strings.Contains(m.Source, "docker.sock") || strings.Contains(m.Target, "docker.sock") {
			t.Errorf("the Docker socket reached a helper: %+v", m)
		}
	}
	want := []mount.Mount{
		{Type: mount.TypeBind, Source: "/var/lib/odac/.odac/apps/db/data", Target: "/backup/src/0", ReadOnly: true},
		{Type: mount.TypeBind, Source: "/mnt/disk", Target: "/backup/target"},
	}
	if len(hc.Mounts) != len(want) {
		t.Fatalf("mounts %+v", hc.Mounts)
	}
	for i := range want {
		if hc.Mounts[i] != want[i] {
			t.Errorf("mount %d = %+v, want %+v", i, hc.Mounts[i], want[i])
		}
	}
	if cfg.User != "0:0" || !cfg.OpenStdin || !cfg.StdinOnce {
		t.Errorf("user %q, stdin open %v once %v", cfg.User, cfg.OpenStdin, cfg.StdinOnce)
	}
	if cfg.Labels[LabelBackupRole] != RoleHelper || cfg.Labels[LabelBackupFollow] != "true" || cfg.Labels["odac.backup.run"] != "r1" {
		t.Errorf("labels %v", cfg.Labels)
	}
	if hc.LogConfig.Type != "json-file" {
		t.Errorf("a followable helper logs to %q", hc.LogConfig.Type)
	}
	if cfg.Healthcheck == nil || cfg.Healthcheck.Test[0] != "NONE" {
		t.Error("the image's health check would probe an API the helper does not serve")
	}
	if len(f.removed) != 1 {
		t.Errorf("removed %v", f.removed)
	}
}

func TestHelperWithoutFollowLogsNothing(t *testing.T) {
	spec := validHelper()
	spec.Follow = false
	spec.Network = true
	f := newFakeAPI()
	call := startHelper(t, f, spec)
	if call.HostConfig.LogConfig.Type != "none" {
		t.Errorf("a stream restore's bytes would reach the log (%q)", call.HostConfig.LogConfig.Type)
	}
	if call.HostConfig.NetworkMode != BackupNetwork {
		t.Errorf("network %q", call.HostConfig.NetworkMode)
	}
	if opts := f.netCreateOpts[BackupNetwork]; opts.Options[iccOption] != "false" {
		t.Errorf("%s created with %v", BackupNetwork, opts.Options)
	}
}

func TestHelperRefusals(t *testing.T) {
	for name, mutate := range map[string]func(*HelperSpec){
		"no memory limit":       func(s *HelperSpec) { s.Limits.Memory = 0 },
		"no pid limit":          func(s *HelperSpec) { s.Limits.PidsLimit = 0 },
		"SYS_ADMIN":             func(s *HelperSpec) { s.Caps = []string{"SYS_ADMIN"} },
		"foreign label":         func(s *HelperSpec) { s.Labels = map[string]string{"com.example": "x"} },
		"spoofed role":          func(s *HelperSpec) { s.Labels = map[string]string{LabelBackupRole: RoleDump} },
		"relative target":       func(s *HelperSpec) { s.Mounts[0].Target = "backup" },
		"root target":           func(s *HelperSpec) { s.Mounts[0].Target = "/" },
		"duplicate target":      func(s *HelperSpec) { s.Mounts[1].Target = s.Mounts[0].Target },
		"relative source":       func(s *HelperSpec) { s.Mounts[0].Source = "data" },
		"no image":              func(s *HelperSpec) { s.Image = "" },
		"share on mount target": func(s *HelperSpec) { s.Share = &Share{Type: "nfs", Addr: "nas", Path: "/x", Target: "/backup/target"} },
	} {
		spec := validHelper()
		spec.Mounts = append([]HelperMount(nil), spec.Mounts...)
		mutate(&spec)
		f := newFakeAPI()
		if _, err := New(f, Options{}).StartHelper(context.Background(), spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if len(f.created) != 0 || len(f.volumes) != len(f.removedVolumes) {
			t.Errorf("%s: left containers %d, volumes %d of %d removed", name, len(f.created), len(f.removedVolumes), len(f.volumes))
		}
	}
}

func TestShareOptions(t *testing.T) {
	for _, c := range []struct {
		share Share
		want  map[string]string
	}{
		{
			Share{Type: "cifs", Addr: "nas.local", Path: "backups", Username: "odac", Password: "p=ss word", Version: "3.0"},
			map[string]string{"type": "cifs", "device": "//nas.local/backups", "o": "addr=nas.local,username=odac,password=p=ss word,vers=3.0"},
		},
		{
			Share{Type: "cifs", Addr: "fd00::5", Path: "b"},
			map[string]string{"type": "cifs", "device": "//[fd00::5]/b", "o": "addr=fd00::5,guest"},
		},
		{
			Share{Type: "nfs", Addr: "10.0.0.2", Path: "/srv/backups/", Version: "4.1"},
			map[string]string{"type": "nfs", "device": ":/srv/backups", "o": "addr=10.0.0.2,nfsvers=4.1"},
		},
	} {
		got, err := c.share.driverOpts()
		if err != nil {
			t.Errorf("%+v: %v", c.share, err)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%+v: %s = %q, want %q", c.share, k, got[k], v)
			}
		}
	}
}

// Mount options are one comma-separated string, so a value that carries a
// comma would add options of its own (uid=0, a second addr=).
func TestShareOptionInjection(t *testing.T) {
	for name, s := range map[string]Share{
		"comma in password":  {Type: "cifs", Addr: "nas", Path: "b", Username: "u", Password: "x,uid=0"},
		"comma in username":  {Type: "cifs", Addr: "nas", Path: "b", Username: "u,uid=0"},
		"equals in username": {Type: "cifs", Addr: "nas", Path: "b", Username: "u=x"},
		"comma in domain":    {Type: "cifs", Addr: "nas", Path: "b", Username: "u", Domain: "d,sec=none"},
		"comma in path":      {Type: "cifs", Addr: "nas", Path: "b,uid=0"},
		"comma in address":   {Type: "nfs", Addr: "nas,nolock", Path: "/x"},
		"option as version":  {Type: "nfs", Addr: "nas", Path: "/x", Version: "4,nolock"},
		"relative export":    {Type: "nfs", Addr: "nas", Path: "x"},
		"nfs credentials":    {Type: "nfs", Addr: "nas", Path: "/x", Username: "u"},
		"slash share":        {Type: "cifs", Addr: "nas", Path: "/b"},
		"unknown type":       {Type: "sshfs", Addr: "nas", Path: "/x"},
		"newline":            {Type: "cifs", Addr: "nas", Path: "b", Username: "u", Password: "a\nb"},
	} {
		if _, err := s.driverOpts(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHelperShareVolumeGoesWithIt(t *testing.T) {
	spec := validHelper()
	spec.Mounts = spec.Mounts[:1]
	spec.Share = &Share{Type: "cifs", Addr: "nas", Path: "b", Username: "u", Password: "secret", Target: "/backup/target"}
	f := newFakeAPI()
	call := startHelper(t, f, spec)
	if len(f.volumes) != 1 {
		t.Fatalf("volumes %+v", f.volumes)
	}
	v := f.volumes[0]
	if v.Driver != "local" || v.Labels[LabelBackupRole] != RoleShare {
		t.Errorf("volume %+v", v)
	}
	if call.Config.Labels[LabelBackupVolume] != v.Name {
		t.Errorf("the helper does not name its volume: %v", call.Config.Labels)
	}
	for _, e := range call.Config.Env {
		if strings.Contains(e, "secret") {
			t.Error("the share password reached the helper's environment")
		}
	}
	if len(f.removedVolumes) != 1 || f.removedVolumes[0] != v.Name {
		t.Errorf("removed volumes %v", f.removedVolumes)
	}
}

func TestDumpContainment(t *testing.T) {
	f := newFakeAPI()
	c := New(f, Options{})
	d, err := c.StartDump(context.Background(), DumpSpec{
		Image:  "postgres:17",
		App:    "db",
		Cmd:    []string{"pg_dump", "--lock-wait-timeout=30s", "app"},
		Env:    []string{"PGPASSWORD=x"},
		Limits: helperLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	call := f.created[0]
	hc, cfg := call.HostConfig, call.Config
	if hc.LogConfig.Type != "none" {
		t.Errorf("the database's contents would reach a log (%q)", hc.LogConfig.Type)
	}
	if hc.NetworkMode != "container:db" {
		t.Errorf("network %q", hc.NetworkMode)
	}
	if cfg.Entrypoint[0] != "pg_dump" || len(cfg.Cmd) != 2 {
		t.Errorf("entrypoint %v cmd %v: the image's own entrypoint would start a database", cfg.Entrypoint, cfg.Cmd)
	}
	if len(hc.CapAdd) != 0 || hc.CapDrop[0] != "ALL" || !hc.ReadonlyRootfs || len(hc.Mounts) != 0 {
		t.Errorf("caps %v/%v, read-only %v, mounts %v", hc.CapDrop, hc.CapAdd, hc.ReadonlyRootfs, hc.Mounts)
	}
	if cfg.OpenStdin {
		t.Error("a dump has no stdin")
	}
}

// A credential reaches the dump through stdin, which nothing the daemon
// records about the container shows.
func TestDumpSecretOnStdin(t *testing.T) {
	f := newFakeAPI()
	f.captureStdin = true
	c := New(f, Options{})
	d, err := c.StartDump(context.Background(), DumpSpec{
		Image:  "postgres:17",
		App:    "db",
		Cmd:    []string{"sh", "-c", "cat >/tmp/s && exec \"$0\" \"$@\"", "pg_dumpall"},
		Env:    []string{"PGPASSFILE=/tmp/s"},
		Stdin:  []byte("*:*:*:postgres:hunter2\n"),
		Limits: helperLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(d)
	d.Close()
	call := f.created[0]
	if !call.Config.OpenStdin || !call.Config.StdinOnce || !call.Config.AttachStdin {
		t.Errorf("stdin open %v once %v attach %v", call.Config.OpenStdin, call.Config.StdinOnce, call.Config.AttachStdin)
	}
	f.mu.Lock()
	got := string(f.stdin[call.ID])
	f.mu.Unlock()
	if got != "*:*:*:postgres:hunter2\n" {
		t.Errorf("stdin %q", got)
	}
	for _, v := range append(append([]string{}, call.Config.Env...), append(call.Config.Entrypoint, call.Config.Cmd...)...) {
		if strings.Contains(v, "hunter2") {
			t.Errorf("the secret reached the container config: %q", v)
		}
	}
}

// A dump that exits non-zero must never read as a complete stream.
func TestDumpFailureIsNotEOF(t *testing.T) {
	f := newFakeAPI()
	c := New(f, Options{})
	d, err := c.StartDump(context.Background(), DumpSpec{Image: "postgres:17", App: "db", Cmd: []string{"pg_dump"}, Limits: helperLimits()})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	f.mu.Lock()
	f.waitCodes[f.created[0].ID] = 1
	f.inspects[f.created[0].ID] = container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{OOMKilled: true}}}
	f.mu.Unlock()
	_, err = io.ReadAll(d)
	if err == nil || !strings.Contains(err.Error(), "memory limit") {
		t.Fatalf("ReadAll = %v, want the dump's failure", err)
	}
}

func TestSelfCandidates(t *testing.T) {
	id := strings.Repeat("ab", 32)
	files := map[string]string{
		"/proc/self/mountinfo": "620 610 0:52 /docker/containers/" + id + "/hostname /etc/hostname rw\n",
		"/proc/self/cgroup":    "0::/\n",
	}
	read := func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, io.EOF
	}
	got := SelfCandidates("0123456789ab", read)
	if len(got) != 2 || got[0] != "0123456789ab" || got[1] != id {
		t.Fatalf("candidates %v", got)
	}
	if got := SelfCandidates("my-host", read); len(got) != 1 || got[0] != id {
		t.Fatalf("a host-network hostname became a candidate: %v", got)
	}
}

// Most cloud disks have no I/O weights; asking for one there fails the
// start itself, which would fail every backup.
func TestIOWeightFallsBack(t *testing.T) {
	f := newFakeAPI()
	f.startErrs = []error{errors.New("OCI runtime create failed: openat2 /sys/fs/cgroup/docker/x/io.weight: no such file or directory")}
	c := New(f, Options{})
	spec := validHelper()
	spec.Limits.BlkioWeight = 100
	h, err := c.StartHelper(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	h.Remove()
	if len(f.created) != 2 || f.created[0].HostConfig.BlkioWeight != 100 || f.created[1].HostConfig.BlkioWeight != 0 {
		t.Fatalf("creates %d, weights %v", len(f.created), f.created)
	}
	if f.removed[0] != f.created[0].ID {
		t.Errorf("the failed container was not removed: %v", f.removed)
	}
	if _, err := c.StartDump(context.Background(), DumpSpec{Image: "p", App: "db", Cmd: []string{"x"}, Limits: Limits{Memory: 1, PidsLimit: 1, BlkioWeight: 100}}); err != nil {
		t.Fatal(err)
	}
	if f.created[2].HostConfig.BlkioWeight != 0 {
		t.Error("the client asked for a weight again after learning the host has none")
	}

	f = newFakeAPI()
	f.startErrs = []error{errors.New("image not found")}
	if _, err := New(f, Options{}).StartHelper(context.Background(), spec); err == nil || len(f.created) != 1 {
		t.Fatalf("an unrelated failure was retried: %v, %d creates", err, len(f.created))
	}
}
