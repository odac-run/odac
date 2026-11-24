package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

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

// headSHARe is the only HEAD answer reported: a full SHA-1 object name, the
// shape the Cloud stores. A SHA-256 repository resolves to nothing.
var headSHARe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// maxHeadOutput bounds what is kept of the sandbox's stdout, which carries
// only the HEAD line.
const maxHeadOutput = 128

// withHead wraps a git step so the sandbox also reports the commit it left
// checked out. The step's own stdout moves to stderr (the build log still
// gets both), which leaves stdout carrying nothing but rev-parse's line, so
// no remote message or checkout output can pass for the answer. A rev-parse
// that cannot answer never fails the step.
func withHead(gitCmd string) string {
	return "{ " + gitCmd + "; } 1>&2 && { git rev-parse --verify -q HEAD || true; }"
}

// parseHead returns the commit the sandbox printed, "" when it is not one.
func parseHead(out []byte) string {
	sha := strings.TrimSpace(string(out))
	if headSHARe.MatchString(sha) {
		return sha
	}
	return ""
}

// gitHardening is command-level config for every sandbox run, which outranks
// the clone's own. The compile step and a dev container write the clone as
// root, so a hook in .git/hooks or an fsmonitor would otherwise run with the
// token in its environment.
var gitHardening = [][2]string{
	{"core.hooksPath", "/dev/null"},
	{"core.fsmonitor", "false"},
}

// gitEnv configures git through the environment (GIT_CONFIG_*): the hardening
// above and, with a token, a credential helper scoped to the remote's origin.
// A token inside the remote URL is written to .git/config, where the compile
// step and a custom Dockerfile's `COPY . .` both pick it up.
func gitEnv(remote, token string) []string {
	pairs := append([][2]string(nil), gitHardening...)
	origin := credentialOrigin(remote, token)
	if origin != "" {
		pairs = append(pairs, [2]string{"credential." + origin + ".helper", credentialHelper})
	}
	env := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(pairs))}
	for i, p := range pairs {
		env = append(env, "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+p[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+p[1])
	}
	if origin != "" {
		env = append(env, "GIT_TOKEN="+token)
	}
	return env
}

// credentialOrigin returns the scheme and host the token is offered to, or ""
// when there is no token or the remote is not HTTPS.
func credentialOrigin(remote, token string) string {
	if token == "" || !strings.HasPrefix(remote, "https://") {
		return ""
	}
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// resetConfigScript replaces the clone's config with one that names only the
// remote ODAC knows, before git runs with the token. The command-level config
// cannot cover it: a credential helper or an include.path there is added to
// ours, not replaced. A .git that is not a plain directory of its own (a
// symlink, a gitfile, a linked worktree's commondir) is refused, since git
// would read another config through it.
const resetConfigScript = `test -d .git && test ! -L .git && test ! -e .git/commondir ` +
	`|| { echo "Refusing to fetch: .git is not a plain repository directory." >&2; exit 1; }; ` +
	`rm -f .git/config .git/config.worktree && ` +
	`printf '[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n' > .git/config && ` +
	`git config remote.origin.url "$GIT_REMOTE_URL" && ` +
	`git config remote.origin.fetch '+refs/heads/*:refs/remotes/origin/*' && `

// CloneRepo ports Container.cloneRepo: shallow-clones the repository into
// targetDir using an ephemeral alpine/git sandbox container. All
// dynamic values reach the shell only via "$VAR" env expansion, never
// string interpolation, so quote/metachars in them are not re-parsed.
// It returns the commit checked out, "" when git could not name one.
func (c *Client) CloneRepo(url, branch, targetDir, token string, buildLog BuildLog) (string, error) {
	if !c.available {
		return "", fmt.Errorf("Docker is not available")
	}

	if buildLog != nil {
		buildLog.StartPhase("pull_git_image")
	}
	if err := c.EnsureImage(gitImage, nil); err != nil {
		return "", err
	}
	if buildLog != nil {
		buildLog.EndPhase("pull_git_image", true)
	}

	branchLabel := branch
	if branchLabel == "" {
		branchLabel = "default"
	}
	c.log.Log("[Git] Cloning %s (branch: %s) into isolated sandbox...", url, branchLabel)

	env := append([]string{"GIT_REMOTE_URL=" + url}, gitEnv(url, token)...)
	gitCmd := `git clone --depth 1 "$GIT_REMOTE_URL" .`
	if branch != "" {
		env = append(env, "GIT_BRANCH="+branch)
		gitCmd = `git clone --depth 1 --branch "$GIT_BRANCH" "$GIT_REMOTE_URL" .`
	}

	head, err := c.runGitSandbox(context.Background(), gitCmd, env, targetDir, token, buildLog, "clone")
	if err != nil {
		c.log.Error("[Git] Clone failed: %s", err.Error())
		return "", err
	}
	c.log.Log("[Git] Clone successful (HEAD: %s).", headLabel(head))
	return head, nil
}

func headLabel(head string) string {
	if head == "" {
		return "unknown"
	}
	return head
}

// FetchRepo ports Container.fetchRepo: fetches the branch head (or a
// pinned commit) into an existing clone and hard-resets to it. With a URL the
// clone's config is rebuilt first (resetConfigScript), which also scrubs a
// token an older clone left in the remote URL. It returns the commit checked
// out, "" when git could not name one.
func (c *Client) FetchRepo(url, branch, targetDir, token, commitSha string, buildLog BuildLog) (string, error) {
	if !c.available {
		return "", fmt.Errorf("Docker is not available")
	}

	if buildLog != nil {
		buildLog.StartPhase("pull_git_image")
	}
	if err := c.EnsureImage(gitImage, nil); err != nil {
		return "", err
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
	env = append(env, gitEnv(url, token)...)

	gitCmd := `git fetch --depth 1 origin "$GIT_BRANCH" && git reset --hard "origin/$GIT_BRANCH"`
	if commitSha != "" {
		gitCmd = `git fetch --depth 1 origin "$GIT_COMMIT_SHA" && git reset --hard "$GIT_COMMIT_SHA"`
	}
	if url != "" {
		env = append(env, "GIT_REMOTE_URL="+url)
		gitCmd = resetConfigScript + gitCmd
	}

	head, err := c.runGitSandbox(context.Background(), gitCmd, env, targetDir, token, buildLog, "fetch")
	if err != nil {
		c.log.Error("[Git] Fetch failed: %s", err.Error())
		return "", err
	}
	c.log.Log("[Git] Fetch and reset successful (HEAD: %s).", headLabel(head))
	return head, nil
}

// CheckoutHead names the commit a checkout has, "" when git cannot name one.
// It asks the git sandbox like every other HEAD read, because the tree is
// written by repository code, and it never pulls: a host without the sandbox
// image answers an error rather than a download nobody asked for.
func (c *Client) CheckoutHead(targetDir string) (string, error) {
	if !c.available {
		return "", fmt.Errorf("Docker is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), headProbeTimeout)
	defer cancel()
	if _, _, err := c.api.ImageInspectWithRaw(ctx, gitImage); err != nil {
		return "", err
	}
	return c.runGitSandbox(ctx, "true", nil, targetDir, "", nil, "rev-parse")
}

// headProbeTimeout bounds CheckoutHead, container start included.
const headProbeTimeout = time.Minute

// runGitSandbox creates and runs one ephemeral alpine/git container with
// targetDir bind-mounted at /git, streaming (token-sanitized) output into
// buildLog, and force-removing the container afterwards. Git never prompts:
// a missing or wrong credential fails the step instead of waiting for input.
// The commit left checked out is resolved in the same container (withHead),
// inside the sandbox rather than by reading .git from here, since the tree
// is written by repository code. The container is removed even when ctx has
// ended, so a deadline never leaks one.
func (c *Client) runGitSandbox(ctx context.Context, gitCmd string, env []string, targetDir, token string, buildLog BuildLog, op string) (string, error) {
	hostPath := c.ResolveHostPath(targetDir)
	if err := c.ensureNetwork(ctx, BuildNetwork); err != nil {
		return "", err
	}

	created, err := c.api.ContainerCreate(ctx, &container.Config{
		Image:      gitImage,
		Entrypoint: []string{"sh", "-c"},
		Cmd:        []string{withHead(gitCmd)},
		Env:        append(env, "GIT_TERMINAL_PROMPT=0"),
		WorkingDir: "/git",
	}, sandboxHostConfig([]string{hostPath + ":/git"}), nil, nil, "")
	if err != nil {
		return "", err
	}
	defer c.api.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})

	head := &boundedBuffer{max: maxHeadOutput}
	status, err := c.runToCompletionSplit(ctx, created.ID, head, writerOrNil(buildLog), token)
	if err != nil {
		return "", err
	}
	if status != 0 {
		// Node capitalizes the op in the message ("Git clone failed…").
		return "", fmt.Errorf("Git %s failed with exit code %d.", op, status)
	}
	return parseHead(head.Bytes()), nil
}

// boundedBuffer keeps the first max bytes written to it and drops the rest,
// so a stream that should carry one line cannot grow without bound.
type boundedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }

// runToCompletion starts a created container, streams its demuxed output
// into logw (both streams to the same writer, token occurrences masked)
// and waits for its exit code. The wait channel is registered before the
// start so fast-exiting containers cannot be missed.
func (c *Client) runToCompletion(ctx context.Context, id string, logw io.Writer, token string) (int64, error) {
	// Both stdout and stderr land in the same build log, like Node's
	// #streamContainerLogs single handler.
	return c.runToCompletionSplit(ctx, id, logw, logw, token)
}

// runToCompletionSplit is runToCompletion with the two streams kept apart,
// for a step whose stdout is an answer rather than log output. Either
// writer may be nil; output is fetched when at least one is set.
func (c *Client) runToCompletionSplit(ctx context.Context, id string, stdout, stderr io.Writer, token string) (int64, error) {
	waitCh, errCh := c.api.ContainerWait(ctx, id, container.WaitConditionNextExit)

	if err := c.api.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return 0, err
	}

	if stdout != nil || stderr != nil {
		if rc, err := c.api.ContainerLogs(ctx, id, container.LogsOptions{
			ShowStdout: true, ShowStderr: true, Follow: true,
		}); err == nil {
			stdcopy.StdCopy(maskedOrDiscard(stdout, token), maskedOrDiscard(stderr, token), rc)
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

// maskedOrDiscard wraps w in a maskingWriter when there is a secret to
// mask, and stands in io.Discard for a nil writer.
func maskedOrDiscard(w io.Writer, token string) io.Writer {
	if w == nil {
		return io.Discard
	}
	if token != "" {
		return &maskingWriter{w: w, secret: token}
	}
	return w
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
