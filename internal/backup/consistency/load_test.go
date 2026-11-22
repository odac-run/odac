package consistency

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// noSecretIn fails when a credential shows in what the daemon records.
func noSecretIn(t *testing.T, cmd, env []string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		for _, v := range append(slices.Clone(cmd), env...) {
			if strings.Contains(v, s) {
				t.Fatalf("secret %q reaches the container's command or environment: %q", s, v)
			}
		}
	}
}

func TestLoadPostgres(t *testing.T) {
	f := &fakeProber{answer: has("psql")}
	p := newPlanner(t, f, 8<<30)
	app := App{Name: "db", Container: "db", Image: "postgres:16", Env: map[string]string{"POSTGRES_PASSWORD": "pw:x", "POSTGRES_USER": "app"},
		Volumes: []Volume{{Host: "/srv/db", Container: "/var/lib/postgresql/data"}, {Host: "/srv/up", Container: "/uploads"}}}
	l, err := p.Load(context.Background(), app, "postgres", Consistent)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Fresh || l.Spec == nil || l.Spec.Cmd[0] != "sh" || l.Spec.Cmd[3] != "1" || !slices.Contains(l.Spec.Cmd, "--dbname=postgres") {
		t.Fatalf("load %+v", l.Spec)
	}
	if string(l.Spec.Stdin) != `*:*:*:app:pw\:x`+"\n" {
		t.Fatalf("stdin %q", l.Spec.Stdin)
	}
	noSecretIn(t, l.Spec.Cmd, l.Spec.Env, "pw:x")
	if err := l.Judge("psql:<stdin>:12: ERROR:  role \"app\" already exists\n"); err != nil {
		t.Fatalf("expected error judged fatal: %v", err)
	}
	if err := l.Judge("psql:<stdin>:40: ERROR:  could not extend file: No space left on device\n"); err == nil {
		t.Fatal("a real error passed")
	}
	if vols := DataVolumes(app, Postgres); len(vols) != 1 || vols[0].Host != "/srv/db" {
		t.Fatalf("data volumes %+v", vols)
	}
	if _, err := p.Load(context.Background(), App{Name: "db", Container: "db", Image: "postgres:16"}, "postgres", Consistent); err == nil {
		t.Fatal("a load without credentials")
	}
}

func TestLoadMySQLAndMongo(t *testing.T) {
	f := &fakeProber{answer: has("mysql", "mongorestore", "mongosh")}
	p := newPlanner(t, f, 8<<30)
	my := App{Name: "db", Container: "db", Image: "mysql:8.4", Env: map[string]string{"MYSQL_ROOT_PASSWORD": "r00t\"x"}}
	l, err := p.Load(context.Background(), my, "mysql", Consistent)
	if err != nil {
		t.Fatal(err)
	}
	if l.Spec.Cmd[4] != "mysql" || l.finish == nil || !strings.Contains(string(l.Spec.Stdin), `password="r00t\"x"`) {
		t.Fatalf("mysql %+v", l.Spec)
	}
	if lines := strings.Count(string(l.Spec.Stdin), "\n"); l.Spec.Cmd[3] != strconv.Itoa(lines) {
		t.Fatalf("line count %s for %d lines", l.Spec.Cmd[3], lines)
	}
	noSecretIn(t, l.Spec.Cmd, l.Spec.Env, `r00t"x`)

	mongo := App{Name: "m", Container: "m", Image: "mongo:7", Env: map[string]string{"MONGO_INITDB_ROOT_USERNAME": "root", "MONGO_INITDB_ROOT_PASSWORD": "s3cret"}}
	for level, replay := range map[string]bool{Consistent: true, Live: false} {
		l, err := p.Load(context.Background(), mongo, "mongo", level)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(l.Spec.Cmd, "--oplogReplay") != replay || !slices.Contains(l.Spec.Cmd, "--drop") {
			t.Fatalf("%s: %q", level, l.Spec.Cmd)
		}
		noSecretIn(t, l.Spec.Cmd, l.Spec.Env, "s3cret")
	}
}

func TestLoadRedisHookAndUnknown(t *testing.T) {
	p := newPlanner(t, &fakeProber{answer: has()}, 8<<30)
	l, err := p.Load(context.Background(), App{Name: "r", Container: "r", Image: "redis:7", Cmd: []string{"redis-server", "--dbfilename", "x.rdb"}}, "redis", Consistent)
	if err != nil || l.Spec != nil || l.File != "/data/x.rdb" || !l.Fresh {
		t.Fatalf("redis %+v, %v", l, err)
	}
	hook := App{Name: "h", Container: "h", Image: "x", Env: map[string]string{"K": "v"}, Spec: &Spec{Hook: &Hook{Dump: []string{"dump"}, Restore: []string{"import", "-"}}}}
	l, err = p.Load(context.Background(), hook, "hook", Live)
	if err != nil || l.Fresh || l.Spec.Cmd[0] != "import" || !slices.Contains(l.Spec.Env, "K=v") {
		t.Fatalf("hook %+v, %v", l, err)
	}
	hook.Spec.Hook.Restore = nil
	if _, err := p.Load(context.Background(), hook, "hook", Live); err == nil {
		t.Fatal("a hook without a restore command loaded")
	}
	if _, err := p.Load(context.Background(), hook, "files-data", Live); err == nil {
		t.Fatal("a directory loaded as a dump")
	}
}

// TestSecretLinesScript runs the wrapper in a real shell: the secret's
// lines land in the file and the tool reads exactly what follows.
func TestSecretLinesScript(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "secret")
	script := strings.ReplaceAll(secretLinesScript, secretFile, file)
	cmd := exec.Command(sh, "-c", script, "2", "cat")
	cmd.Stdin = strings.NewReader("[client]\n password=\"a b\\\\c\" \n-- dump line 1\n-- dump line 2\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "-- dump line 1\n-- dump line 2\n" {
		t.Fatalf("tool read %q", out)
	}
	got, _ := os.ReadFile(file)
	if string(got) != "[client]\n password=\"a b\\\\c\" \n" {
		t.Fatalf("secret file %q", got)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode %v", st.Mode())
	}
	cmd = exec.Command(sh, "-c", script, "3", "cat")
	cmd.Stdin = strings.NewReader("only\none\n")
	if err := cmd.Run(); err == nil {
		t.Fatal("a stream shorter than its secret ran the tool")
	}
}

func TestRDBTrailerSize(t *testing.T) {
	rdb := append([]byte("REDIS0011...data..."), 0xFF, 1, 2, 3, 4, 5, 6, 7, 8)
	mark := []byte(strings.Repeat("0123456789", 4)[:40])
	if n := RDBTrailerSize(append(slices.Clone(rdb), mark...)); n != 40 {
		t.Fatalf("with the mark: %d", n)
	}
	if n := RDBTrailerSize(rdb); n != 0 {
		t.Fatalf("a clean RDB lost %d bytes", n)
	}
	bad := append(slices.Clone(rdb), []byte(strings.Repeat("Z", 40))...)
	if n := RDBTrailerSize(bad); n != 0 {
		t.Fatalf("not a mark: %d", n)
	}
}

func TestWaitReadyEndsWhenTheContainerCrashes(t *testing.T) {
	f := &fakeProber{answer: func(tool string, args []string, stdin string) (string, error) {
		if len(args) == 1 && args[0] == "--version" {
			return "psql 1.0", nil
		}
		return "", &CommandError{Msg: "connection refused"}
	}}
	p := newPlanner(t, f, 8<<30)
	app := App{Name: "db", Container: "db", Image: "postgres:16", Env: map[string]string{"POSTGRES_PASSWORD": "pw"},
		Volumes: []Volume{{Host: "/srv/db", Container: "/var/lib/postgresql/data"}}}
	l, err := p.Load(context.Background(), app, "postgres", Consistent)
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	alive := func(context.Context) error {
		if asked++; asked < 2 {
			return nil
		}
		return errors.New("restarting")
	}
	// Without the crash the wait would last until the context ends.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	err = l.WaitReady(ctx, alive)
	if err == nil || !strings.Contains(err.Error(), "restarting") || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("wait: %v", err)
	}
	if time.Since(start) > 3*readyEvery {
		t.Fatalf("the wait took %s", time.Since(start))
	}
}
