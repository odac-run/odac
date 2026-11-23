package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFetchIgnoresWhatTheCloneWrote runs the fetch command with a real git
// against a clone whose config, hooks and fsmonitor were written the way an
// install script in the compile step can write them. None of them may run
// with the token in the environment, and the fetch must still land.
func TestFetchIgnoresWhatTheCloneWrote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	leak := filepath.Join(root, "leak")
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	app := filepath.Join(root, "app")

	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}

	git(root, "init", "-q", "--bare", "-b", "main", remote)
	git(root, "clone", "-q", remote, seed)
	write(filepath.Join(seed, "f"), "one\n", 0o644)
	git(seed, "add", "f")
	git(seed, "commit", "-qm", "one")
	git(seed, "push", "-q", "origin", "main")
	git(root, "clone", "-q", "--depth", "1", "--branch", "main", "file://"+remote, app)
	write(filepath.Join(seed, "f"), "two\n", 0o644)
	git(seed, "commit", "-qam", "two")
	git(seed, "push", "-q", "origin", "main")

	planted := "echo $GIT_TOKEN >> " + leak
	cfg, err := os.OpenFile(filepath.Join(app, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	cfg.WriteString("[core]\n\tfsmonitor = \"sh -c '" + planted + "; echo'\"\n" +
		"[credential]\n\thelper = \"!f() { " + planted + "; }; f\"\n" +
		"[include]\n\tpath = " + filepath.Join(root, "extra") + "\n")
	cfg.Close()
	write(filepath.Join(root, "extra"), "[core]\n\tsshCommand = sh -c '"+planted+"'\n", 0o644)
	for _, hook := range []string{"reference-transaction", "post-checkout", "pre-auto-gc"} {
		write(filepath.Join(app, ".git", "hooks", hook), "#!/bin/sh\n"+planted+"\n", 0o755)
	}

	cmd := exec.Command("sh", "-c", resetConfigScript+`git fetch -q --depth 1 origin "$GIT_BRANCH" && git reset -q --hard "origin/$GIT_BRANCH"`)
	cmd.Dir = app
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_BRANCH=main", "GIT_REMOTE_URL=file://"+remote, "GIT_TOKEN=sekret")
	cmd.Env = append(cmd.Env, gitEnv("file://"+remote, "")...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}

	if got, err := os.ReadFile(leak); err == nil {
		t.Errorf("the clone's own config or hooks ran with the token: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(app, "f")); string(got) != "two\n" {
		t.Errorf("fetch did not land: f = %q", got)
	}
	got, _ := os.ReadFile(filepath.Join(app, ".git", "config"))
	if strings.Contains(string(got), "fsmonitor") || strings.Contains(string(got), "include") ||
		!strings.Contains(string(got), "url = file://"+remote) {
		t.Errorf("config not rebuilt:\n%s", got)
	}
}

// TestFetchRefusesARedirectedGitDir pins the refusal: a .git that is a
// symlink or points at another git directory would hand git a config the
// reset never touched.
func TestFetchRefusesARedirectedGitDir(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, dir string){
		"symlink": func(t *testing.T, dir string) {
			other := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.Mkdir(other, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, filepath.Join(dir, ".git")); err != nil {
				t.Fatal(err)
			}
		},
		"gitfile": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"commondir": func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".git", "commondir"), []byte("/elsewhere\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			setup(t, dir)
			cmd := exec.Command("sh", "-c", resetConfigScript+"echo fetched")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err == nil || strings.Contains(string(out), "fetched") {
				t.Errorf("redirected .git accepted: %v %q", err, out)
			}
		})
	}
}
