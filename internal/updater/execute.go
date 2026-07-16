package updater

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/pkg/stdcopy"

	"odac/internal/logx"
)

// download ports Updater.download(): build mode builds from source; image
// mode was already pulled by checkForUpdates.
func (u *Updater) download() error {
	if u.isBuildMode() {
		return u.buildFromSource()
	}
	return nil
}

// buildFromSource ports #buildFromSource: clone the target branch via an
// alpine/git sidecar sharing the host bind of ~/.odac, build the update
// image, clean up. Deviation from Node (documented in the package comment):
// the `docker build` runs in a socket-mounted docker:cli runner instead of
// exec'ing a host docker CLI — same policy as internal/docker's builder.
func (u *Updater) buildFromSource() error {
	u.log.Log("Starting Build from Source (Beta/Dev)...")

	err := func() error {
		repo := "https://github.com/odac-run/odac.git"
		branch := u.targetBranch()
		downloadPath := u.downloadPath()

		if _, err := os.Stat(downloadPath); err == nil {
			u.log.Log("Removing previous download...")
			if err := os.RemoveAll(downloadPath); err != nil {
				return err
			}
		}

		u.log.Log("Cloning repository via Docker Sidecar...")
		// The sidecar mounts the host bind of baseDir at /git_target and
		// clones into /git_target/tmp/odac_source == downloadPath here.
		hostBind, err := u.gitCloneWithDocker(repo, branch, downloadPath)
		if err != nil {
			return err
		}

		if _, err := os.Stat(filepath.Join(downloadPath, "package.json")); err != nil {
			return fmt.Errorf("Clone failed: package.json not found")
		}

		u.log.Log("Building Docker Image...")
		if err := u.buildImage(hostBind); err != nil {
			return err
		}

		u.log.Log("Cleaning up source files...")
		os.RemoveAll(downloadPath)

		u.log.Log("Build complete.")
		return nil
	}()
	if err != nil {
		return fmt.Errorf("Build failed: %s", err.Error())
	}
	return nil
}

// gitCloneWithDocker ports #gitCloneWithDocker. Returns the resolved host
// bind so the build step can reuse it.
func (u *Updater) gitCloneWithDocker(repo, branch, targetDir string) (string, error) {
	u.log.Log("Using Docker Sidecar for git operations using target: %s", targetDir)

	hostBind := u.resolveHostBind(u.baseDir)
	if hostBind == "" {
		return "", fmt.Errorf("Could not determine host path for storage volume. Cannot run git sidecar.")
	}

	// Best-effort pull (maybe offline/cached — Node ignores pull errors here).
	_ = u.deps.Docker.Pull("alpine/git")

	id, err := u.deps.Docker.Create(CreateOptions{
		Image: "alpine/git",
		Cmd:   []string{"clone", "-b", branch, "--depth", "1", repo, "/git_target/tmp/odac_source"},
		Binds: []string{hostBind + ":/git_target"},
	})
	if err != nil {
		return "", err
	}
	if err := u.deps.Docker.Start(id); err != nil {
		return "", err
	}
	if rc, lerr := u.deps.Docker.Logs(id, true, ""); lerr == nil {
		// Node pipes the raw multiplexed stream to stdout; demuxing is the
		// cosmetic-only cleanup of that (headers never reach the log).
		go func() {
			defer rc.Close()
			stdcopy.StdCopy(logx.Stdout, logx.Stderr, rc)
		}()
	}

	status, err := u.deps.Docker.Wait(id)
	if err != nil {
		return "", err
	}
	if rerr := u.deps.Docker.Remove(id, false); rerr != nil && !isNotFound(rerr) {
		u.log.Log("Warning: could not remove git sidecar: %s", rerr.Error())
	}

	if status != 0 {
		return "", fmt.Errorf("Git clone failed with exit code %d", status)
	}
	u.log.Log("Git clone via Docker Sidecar successful.")
	return hostBind, nil
}

// buildImage runs `docker build -t <image>` over the cloned source through a
// socket-mounted docker:cli runner (see buildFromSource's deviation note).
func (u *Updater) buildImage(hostBind string) error {
	if err := u.deps.Docker.Pull(runnerImage); err != nil {
		u.log.Log("Warning: could not pull %s: %s", runnerImage, err.Error())
	}
	id, err := u.deps.Docker.Create(CreateOptions{
		Image: runnerImage,
		Cmd:   []string{"sh", "-c", fmt.Sprintf("docker build -t %s /git_target/tmp/odac_source", u.image)},
		Binds: []string{
			"/var/run/docker.sock:/var/run/docker.sock",
			hostBind + ":/git_target",
		},
	})
	if err != nil {
		return err
	}
	if err := u.deps.Docker.Start(id); err != nil {
		return err
	}
	if rc, lerr := u.deps.Docker.Logs(id, true, ""); lerr == nil {
		go func() {
			defer rc.Close()
			stdcopy.StdCopy(logx.Stdout, logx.Stderr, rc)
		}()
	}
	status, err := u.deps.Docker.Wait(id)
	if err != nil {
		return err
	}
	if rerr := u.deps.Docker.Remove(id, false); rerr != nil && !isNotFound(rerr) {
		u.log.Log("Warning: could not remove build runner: %s", rerr.Error())
	}
	if status != 0 {
		return fmt.Errorf("Docker build failed with exit code %d", status)
	}
	return nil
}

// resolveHostBind ports #resolveHostBind: the host-side path for a container
// mount point, read from the 'odac' container's binds (Node uses the literal
// name here even after 2189336 — only execute()/getLocalImageId resolve
// dynamically). Named-volume Mounts are the fallback. "" when not found.
func (u *Updater) resolveHostBind(containerPath string) string {
	info, err := u.deps.Docker.Inspect(containerName)
	if err != nil {
		u.log.Log("Could not resolve host bind for %s: %s", containerPath, err.Error())
		return ""
	}

	for _, bind := range info.Binds {
		parts := strings.Split(bind, ":")
		if len(parts) >= 2 && parts[1] == containerPath {
			return parts[0]
		}
	}

	for _, mount := range info.Mounts {
		if mount.Destination == containerPath {
			if mount.Name != "" {
				return mount.Name
			}
			return mount.Source
		}
	}

	u.log.Log("No host bind found for container path: %s", containerPath)
	return ""
}

// execute ports Updater.execute(): the Linux zero-downtime strategy (listener
// + handshake + rollback) or the container-swap strategy elsewhere.
func (u *Updater) execute() error {
	u.log.Log("Launching update process...")

	// Clean up previous update logs to avoid confusion.
	logDir := filepath.Join(u.baseDir, "logs")
	for _, f := range []string{"." + updateContainerName + ".log", "." + updateContainerName + "_err.log"} {
		p := filepath.Join(logDir, f)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := os.Remove(p); err != nil {
			u.log.Log("Warning: Could not clean old logs: %s", err.Error())
			break
		}
		u.log.Log("Removed old log file: %s", f)
	}

	if err := u.executeInner(); err != nil {
		return fmt.Errorf("Failed to execute update: %s", err.Error())
	}
	return nil
}

func (u *Updater) executeInner() error {
	// 1. Get current container info. Resolve our own identity (2189336): a
	// container running under a non-canonical name can still update.
	containerID := u.resolveSelfName()
	if containerID == "" {
		containerID = containerName
	}
	info, err := u.deps.Docker.Inspect(containerID)
	if err != nil {
		return err
	}

	u.log.Log("Current container found: %s (%s)", info.Name, containerID)

	// 2. Prepare configuration for the new container.
	newName := updateContainerName
	// Clean up previous update attempt if it exists.
	_ = u.deps.Docker.Remove(newName, true)

	env := make([]string, 0, len(info.Env)+6)
	for _, e := range info.Env {
		stale := false
		for _, k := range updateEnvKeys {
			if strings.HasPrefix(e, k+"=") {
				stale = true
				break
			}
		}
		if !stale {
			env = append(env, e)
		}
	}

	createOpts := CreateOptions{
		Name:          newName,
		Image:         u.image,
		Env:           env,
		Binds:         info.Binds,
		Privileged:    true,
		CapAdd:        []string{"NET_ADMIN", "NET_BIND_SERVICE"},
		RestartPolicy: "unless-stopped", // default policy for production
		Tty:           true,
	}

	if u.platform == "linux" {
		// Linux: zero-downtime update via socket handover.
		u.log.Log("Platform: Linux. Using Zero Downtime Update Strategy.")

		currentInstanceID := os.Getenv("ODAC_INSTANCE_ID")
		if currentInstanceID == "" {
			currentInstanceID = "default"
		}
		createOpts.Env = append(createOpts.Env,
			"ODAC_UPDATE_MODE=true",
			"ODAC_INSTANCE_ID="+randomUUID(),
			"ODAC_PREVIOUS_INSTANCE_ID="+currentInstanceID,
			"ODAC_PREVIOUS_CONTAINER_NAME="+containerID,
			"ODAC_UPDATE_SOCKET_PATH="+u.socketPath(),
			"ODAC_LOG_NAME=."+newName, // separate log file for the update
		)
		createOpts.NetworkMode = "host"
		createOpts.PidMode = "host"
		createOpts.RestartPolicy = "no"

		// Initialize the listener FIRST to avoid a race with fast containers.
		listener, err := u.createUpdateListener()
		if err != nil {
			return err
		}

		u.log.Log("Creating new container: %s", newName)
		if _, err := u.deps.Docker.Create(createOpts); err != nil {
			return err
		}

		u.log.Log("Starting new container...")
		if err := u.deps.Docker.Start(newName); err != nil {
			return err
		}

		// Stream logs from the new container for better observability.
		go func() {
			if err := u.streamNewLogs(newName); err != nil {
				u.log.Log("Warning: Failed to attach logs for %s: %s", newName, err.Error())
			}
		}()

		u.log.Log("Update container started successfully. Waiting for handover...")
		if err := <-listener.completion; err != nil {
			u.log.Log("Handover failed: %s. Rolling back...", err.Error())
			_ = u.deps.Docker.Stop(newName)
			_ = u.deps.Docker.Remove(newName, false)
			return err
		}
		return nil
	}

	// Windows/Mac: container-swap strategy via a helper container.
	u.log.Log(fmt.Sprintf("Platform: %s. Using Container Swap Strategy.", u.platform))

	if len(info.PortBindings) > 0 {
		createOpts.PortBindings = info.PortBindings
	}

	u.log.Log("Creating new container (STOPPED): %s", newName)
	if _, err := u.deps.Docker.Create(createOpts); err != nil {
		return err
	}

	u.log.Log("Spawning runner container to perform swap...")

	// Wait 5s, stop old, remove old, rename new, start new. The stop/rm
	// target the resolved self name (2189336); the rename/start targets stay
	// the literal 'odac'.
	cmd := fmt.Sprintf("sleep 5 && docker stop %s && docker rm %s && docker rename %s %s && docker start %s",
		containerID, containerID, updateContainerName, containerName, containerName)

	if err := u.deps.Docker.Pull(runnerImage); err != nil {
		return err
	}

	runnerID, err := u.deps.Docker.Create(CreateOptions{
		Image:      runnerImage,
		Cmd:        []string{"sh", "-c", cmd},
		Binds:      []string{"/var/run/docker.sock:/var/run/docker.sock"},
		AutoRemove: true, // remove the runner after execution
	})
	if err != nil {
		return err
	}
	if err := u.deps.Docker.Start(runnerID); err != nil {
		return err
	}

	u.log.Log("Runner spawned. Handing over control and exiting...")
	u.exit(0)
	return nil // reached only under a test exit seam
}

// streamNewLogs ports #streamLogs: relay the new container's output into our
// own log, line-buffered, prefixed [NEW_VERSION]. The container is created
// with a TTY, so the stream is not multiplexed (see the package comment's
// deviation note on Node's demux of it).
func (u *Updater) streamNewLogs(name string) error {
	rc, err := u.deps.Docker.Logs(name, true, "")
	if err != nil {
		return err
	}
	defer rc.Close()
	reader := bufio.NewReader(rc)
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimSuffix(line, "\n")
		if strings.TrimSpace(line) != "" {
			u.log.Log("[NEW_VERSION] " + line)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return nil // stream torn down (container stopped) — Node ignores too
		}
	}
}
