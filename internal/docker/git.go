package docker

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// gitImage is the sandbox image for clone/fetch operations.
const gitImage = "alpine/git"

// secureURL injects an oauth2 basic-auth token into an https remote URL.
func secureURL(url, token string) string {
	if token != "" && strings.HasPrefix(url, "https://") {
		return "https://oauth2:" + token + "@" + url[len("https://"):]
	}
	return url
}

// CloneRepo ports Container.cloneRepo: shallow-clones the repository into
// targetDir using an isolated, unprivileged alpine/git container. All
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

	env := []string{"GIT_REMOTE_URL=" + secureURL(url, token)}
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
// pinned commit) into an existing clone and hard-resets to it. For private
// repos the authenticated URL is set only for the fetch and restored after.
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
	if token != "" {
		env = append(env, "GIT_REMOTE_URL="+secureURL(url, token), "GIT_ORIGINAL_URL="+url)
	}

	gitCmd := `git fetch --depth 1 origin "$GIT_BRANCH" && git reset --hard "origin/$GIT_BRANCH"`
	if commitSha != "" {
		gitCmd = `git fetch --depth 1 origin "$GIT_COMMIT_SHA" && git reset --hard "$GIT_COMMIT_SHA"`
	}
	if token != "" {
		gitCmd = `git remote set-url origin "$GIT_REMOTE_URL" && ` + gitCmd + ` && git remote set-url origin "$GIT_ORIGINAL_URL"`
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
// buildLog, and force-removing the container afterwards.
func (c *Client) runGitSandbox(gitCmd string, env []string, targetDir, token string, buildLog BuildLog, op string) error {
	ctx := context.Background()
	hostPath := c.ResolveHostPath(targetDir)

	created, err := c.api.ContainerCreate(ctx, &container.Config{
		Image:      gitImage,
		Entrypoint: []string{"sh", "-c"},
		Cmd:        []string{gitCmd},
		Env:        env,
		WorkingDir: "/git",
	}, &container.HostConfig{
		Binds:      []string{hostPath + ":/git"},
		Privileged: false, // SECURITY: rootless git
	}, nil, nil, "")
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
