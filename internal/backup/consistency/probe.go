package consistency

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"odac/internal/docker"
)

// Prober runs a short command the way a dump runs (the app's image, its
// network namespace, entrypoint replaced) and returns its stdout. It is
// how the planner verifies a tool and asks a database about itself.
type Prober interface {
	Run(ctx context.Context, spec docker.DumpSpec) ([]byte, error)
}

// ErrToolMissing is a command whose executable is not in the image.
var ErrToolMissing = errors.New("consistency: tool not in the image")

// CommandError is a command that ran and failed; Stderr is the end of what
// it wrote there.
type CommandError struct{ Msg string }

func (e *CommandError) Error() string { return "consistency: command failed: " + e.Msg }

// Dumper starts dump containers; runner.Docker is one.
type Dumper interface {
	StartDump(ctx context.Context, spec docker.DumpSpec) (io.ReadCloser, error)
}

// probeOutput bounds what a probe may print: a table listing of a large
// ClickHouse server fits many times over.
const probeOutput = 8 << 20

// probeTimeout bounds one probe, which only starts a client and asks a
// question.
const probeTimeout = 2 * time.Minute

// NewProber runs probes as dump containers with limits.
func NewProber(d Dumper, limits docker.Limits) Prober {
	return &dumpProber{d: d, limits: limits}
}

type dumpProber struct {
	d      Dumper
	limits docker.Limits
}

func (p *dumpProber) Run(ctx context.Context, spec docker.DumpSpec) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	spec.Limits = p.limits
	rc, err := p.d.StartDump(ctx, spec)
	if err != nil {
		if toolMissing(err) {
			return nil, fmt.Errorf("%w: %s", ErrToolMissing, spec.Cmd[0])
		}
		return nil, err
	}
	defer rc.Close()
	out, err := io.ReadAll(io.LimitReader(rc, probeOutput+1))
	if err != nil {
		if toolMissing(err) {
			return nil, fmt.Errorf("%w: %s", ErrToolMissing, spec.Cmd[0])
		}
		return nil, &CommandError{Msg: err.Error()}
	}
	if len(out) > probeOutput {
		return nil, &CommandError{Msg: "output too large"}
	}
	return out, nil
}

// toolMissing reads the runtime's refusal to start an executable it cannot
// find, which is how an image without the tool answers.
func toolMissing(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "executable file not found") ||
		(strings.Contains(msg, "exec:") && strings.Contains(msg, "no such file or directory"))
}

// tool returns the first of candidates the image has, with what it printed
// for --version. Results are kept per image: a tool does not appear in an
// image that did not have it.
func (p *Planner) tool(ctx context.Context, app App, candidates ...string) (string, string, error) {
	for _, name := range candidates {
		key := app.image() + "\x00" + name
		p.mu.Lock()
		r, ok := p.tools[key]
		p.mu.Unlock()
		if !ok {
			out, err := p.cfg.Prober.Run(ctx, docker.DumpSpec{Image: app.image(), App: app.Container, Cmd: []string{name, "--version"}})
			var ce *CommandError
			switch {
			case errors.Is(err, ErrToolMissing):
				r = toolResult{missing: true}
			case errors.As(err, &ce):
				// It exists but refuses --version; the dump will tell.
				r = toolResult{}
			case err != nil:
				return "", "", err
			default:
				r = toolResult{version: strings.TrimSpace(string(out))}
			}
			p.mu.Lock()
			p.tools[key] = r
			p.mu.Unlock()
		}
		if !r.missing {
			return name, r.version, nil
		}
	}
	return "", "", nil
}

// probe runs a command and turns a failure the database caused into a
// warning on plan; only an infrastructure failure is returned.
func (p *Planner) probe(ctx context.Context, app App, plan *Plan, c command, secrets ...string) ([]byte, bool, error) {
	out, err := p.cfg.Prober.Run(ctx, c.spec(app))
	if err == nil {
		return out, true, nil
	}
	var ce *CommandError
	if errors.As(err, &ce) || errors.Is(err, ErrToolMissing) {
		plan.warn(WarnProbe, "%s", mask(err.Error(), secrets))
		return nil, false, nil
	}
	return nil, false, err
}

func mask(s string, secrets []string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "****")
		}
	}
	return s
}
