package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"

	"odac/internal/docker"
)

// localProcess is odac-backup as a child process, for a server that is not
// in a container. It has no limits and no isolation: that is development.
type localProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *docker.StderrTail

	waitOnce sync.Once
	waitErr  error
}

func startLocal(binary string) (*localProcess, error) {
	cmd := exec.Command(binary, "run")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &localProcess{cmd: cmd, stdin: stdin, stdout: stdout, stderr: docker.NewStderrTail(4 << 10)}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("runner: start %s: %w", binary, err)
	}
	return p, nil
}

func (p *localProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *localProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *localProcess) Stderr() string        { return p.stderr.String() }

func (p *localProcess) wait() error {
	p.waitOnce.Do(func() { p.waitErr = p.cmd.Wait() })
	return p.waitErr
}

func (p *localProcess) Wait(ctx context.Context) (docker.ExitStatus, error) {
	done := make(chan error, 1)
	go func() { done <- p.wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if err == nil || errors.As(err, &ee) {
			return docker.ExitStatus{Code: int64(p.cmd.ProcessState.ExitCode())}, nil
		}
		return docker.ExitStatus{}, err
	case <-ctx.Done():
		return docker.ExitStatus{}, ctx.Err()
	}
}

func (p *localProcess) Stop() {
	p.cmd.Process.Signal(syscall.SIGTERM)
}

func (p *localProcess) Remove() {
	// Killing a process that already exited is a harmless error.
	p.cmd.Process.Kill()
	p.wait()
}

// Detach leaves the child running; it ends on its own when its stdout
// breaks. There is no next server to adopt it outside a container.
func (p *localProcess) Detach() {}
