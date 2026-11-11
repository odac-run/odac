package docker

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// gitImage is the sandbox image for clone/fetch operations. It is a
// community image that receives the repository token, so it is pinned by
// digest (alpine/git:2.54.0) rather than trusted by tag.
const gitImage = "alpine/git@sha256:6a1fdb962814ca11508ca8d1bab94375b5b7ac4e4a9a40981c29fc32c02f095d"

// credentialHelper answers git's credential "get" with the token from the
// environment. Store and erase requests are ignored, so nothing is saved.
const credentialHelper = `!f() { test "$1" = get && echo username=oauth2 && echo "password=$GIT_TOKEN"; }; f`

// credentialEnv hands the token to git through a credential helper set in
// the environment (GIT_CONFIG_*) and scoped to the remote's origin. A token
// inside the remote URL is written to .git/config, where the compile step and
// a custom Dockerfile's `COPY . .` both pick it up.
func credentialEnv(remote, token string) []string {
	if token == "" || !strings.HasPrefix(remote, "https://") {
		return nil
	}
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" {
		return nil
	}
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=credential." + u.Scheme + "://" + u.Host + ".helper",
		"GIT_CONFIG_VALUE_0=" + credentialHelper,
		"GIT_TOKEN=" + token,
	}
}

// CloneRepo ports Container.cloneRepo: shallow-clones the repository into
// targetDir using an ephemeral alpine/git sandbox container. All
// dynamic values reach the shell only via "$VAR" env expansion, never
// string interpolation, so quote/metachars in them are not re-parsed.
func (c *Client) CloneRepo(url, branch, targetDir, token string, buildLog BuildLog) error {
	if !c.available {
		return fmt.Errorf("Docker is not available")
	}

	if buildLog != nil {
		buildLog.StartPhase("pull_git_image")
	}
	if err := c.EnsureImage(gitImage, nil); err != nil {
		return err
	}
	if buildLog != nil {
		buildLog.EndPhase("pull_git_image", true)
	}

	branchLabel := branch
	if branchLabel == "" {
		branchLabel = "default"
	}
	c.log.Log("[Git] Cloning %s (branch: %s) into isolated sandbox...", url, branchLabel)

	env := append([]string{"GIT_REMOTE_URL=" + url}, credentialEnv(url, token)...)
	gitCmd := `git clone --depth 1 "$GIT_REMOTE_URL" .`
	if branch != "" {
		env = append(env, "GIT_BRANCH="+branch)
		gitCmd = `git clone --depth 1 --branch "$GIT_BRANCH" "$GIT_REMOTE_URL" .`
	}

	if err := c.runGitSandbox(gitCmd, env, targetDir, token, buildLog, "clone"); err != nil {
		c.log.Error("[Git] Clone failed: %s", err.Error())
		return err
	}
	c.log.Log("[Git] Clone successful.")
	return nil
}

// FetchRepo ports Container.fetchRepo: fetches the branch head (or a
// pinned commit) into an existing clone and hard-resets to it. The remote is
// reset to the plain URL first, which also scrubs a token that a clone made
// before credentialEnv left in .git/config.
func (c *Client) FetchRepo(url, branch, targetDir, token, commitSha string, buildLog BuildLog) error {
	if !c.available {
		return fmt.Errorf("Docker is not available")
	}

	if buildLog != nil {
		buildLog.StartPhase("pull_git_image")
	}
	if err := c.EnsureImage(gitImage, nil); err != nil {
		return err
	}
	if buildLog != nil {
		buildLog.EndPhase("pull_git_image", true)
	}

	branchLabel := branch
	if branchLabel == "" {
		branchLabel = "default"
	}
	commitLabel := commitSha
	if commitLabel == "" {
		commitLabel = "HEAD"
	}
	c.log.Log("[Git] Fetching updates (branch: %s, commit: %s)...", branchLabel, commitLabel)

	env := []string{"GIT_BRANCH=" + branch}
	if commitSha != "" {
		env = append(env, "GIT_COMMIT_SHA="+commitSha)
	}
	env = append(env, credentialEnv(url, token)...)

	gitCmd := `git fetch --depth 1 origin "$GIT_BRANCH" && git reset --hard "origin/$GIT_BRANCH"`
	if commitSha != "" {
		gitCmd = `git fetch --depth 1 origin "$GIT_COMMIT_SHA" && git reset --hard "$GIT_COMMIT_SHA"`
	}
	if url != "" {
		env = append(env, "GIT_REMOTE_URL="+url)
		gitCmd = `git remote set-url origin "$GIT_REMOTE_URL" && ` + gitCmd
	}

	if err := c.runGitSandbox(gitCmd, env, targetDir, token, buildLog, "fetch"); err != nil {
		c.log.Error("[Git] Fetch failed: %s", err.Error())
		return err
	}
	c.log.Log("[Git] Fetch and reset successful.")
	return nil
}

// runGitSandbox creates and runs one ephemeral alpine/git container with
// targetDir bind-mounted at /git, streaming (token-sanitized) output into
// buildLog, and force-removing the container afterwards. Git never prompts:
// a missing or wrong credential fails the step instead of waiting for input.
func (c *Client) runGitSandbox(gitCmd string, env []string, targetDir, token string, buildLog BuildLog, op string) error {
	ctx := context.Background()
	hostPath := c.ResolveHostPath(targetDir)
	if err := c.ensureNetwork(ctx, BuildNetwork); err != nil {
		return err
	}

	created, err := c.api.ContainerCreate(ctx, &container.Config{
		Image:      gitImage,
		Entrypoint: []string{"sh", "-c"},
		Cmd:        []string{gitCmd},
		Env:        append(env, "GIT_TERMINAL_PROMPT=0"),
		WorkingDir: "/git",
	}, sandboxHostConfig([]string{hostPath + ":/git"}), nil, nil, "")
	if err != nil {
		return err
	}
	defer c.api.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})

	status, err := c.runToCompletion(ctx, created.ID, buildLog, token)
	if err != nil {
		return err
	}
	if status != 0 {
		// Node capitalizes the op in the message ("Git clone failed…").
		return fmt.Errorf("Git %s failed with exit code %d.", op, status)
	}
	return nil
}

// runToCompletion starts a created container, streams its demuxed output
// into logw (both streams to the same writer, token occurrences masked)
// and waits for its exit code. The wait channel is registered before the
// start so fast-exiting containers cannot be missed.
func (c *Client) runToCompletion(ctx context.Context, id string, logw io.Writer, token string) (int64, error) {
	waitCh, errCh := c.api.ContainerWait(ctx, id, container.WaitConditionNextExit)

	if err := c.api.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return 0, err
	}

	if logw != nil {
		if rc, err := c.api.ContainerLogs(ctx, id, container.LogsOptions{
			ShowStdout: true, ShowStderr: true, Follow: true,
		}); err == nil {
			w := logw
			if token != "" {
				w = &maskingWriter{w: logw, secret: token}
			}
			// Both stdout and stderr land in the same build log, like
			// Node's #streamContainerLogs single handler.
			stdcopy.StdCopy(w, w, rc)
			rc.Close()
		} else if !client.IsErrNotFound(err) {
			c.log.Error("Failed to get logs for %s: %s", id, err.Error())
		}
	}

	select {
	case res := <-waitCh:
		if res.Error != nil {
			return res.StatusCode, fmt.Errorf("%s", res.Error.Message)
		}
		return res.StatusCode, nil
	case err := <-errCh:
		return 0, err
	}
}

// maskingWriter replaces every occurrence of secret with '*****' per write
// (chunk-wise, like Node — a secret split across two chunks is not caught).
type maskingWriter struct {
	w      io.Writer
	secret string
}

func (m *maskingWriter) Write(p []byte) (int, error) {
	masked := strings.ReplaceAll(string(p), m.secret, "*****")
	if _, err := io.WriteString(m.w, masked); err != nil {
		return 0, err
	}
	return len(p), nil
}
