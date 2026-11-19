package runner

import (
	"context"
	"errors"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"

	"odac/internal/backup/proto"
	"odac/internal/docker"
)

// ErrGone is a followed helper whose container disappeared: another server
// (the one being replaced, finishing the run after all) removed it.
var ErrGone = errors.New("runner: the helper was removed by another server")

// Orphan is a helper the previous server started and handed over.
type Orphan struct {
	r *Runner
	c docker.BackupContainer
}

// Labels are the labels the run was started with.
func (o *Orphan) Labels() map[string]string { return o.c.Labels }

// Adopt takes over what the previous server left: dumps are removed
// (their stream ended with that server, so the helper reading it stores
// nothing), as are helpers whose output was not kept, and share volumes
// no container uses. The followable helpers are returned, running or
// finished. Call it once this server is the live one, after an update's
// handover and before its first Run: until then the previous server may
// still be reading the same helpers.
func (r *Runner) Adopt() ([]*Orphan, error) {
	if r.cfg.Docker == nil {
		return nil, nil
	}
	list, err := r.cfg.Docker.BackupContainers()
	if err != nil {
		return nil, err
	}
	var out []*Orphan
	for _, c := range list {
		if c.Role == docker.RoleHelper && c.Labels[docker.LabelBackupFollow] == "true" {
			out = append(out, &Orphan{r: r, c: c})
			continue
		}
		r.cfg.Docker.RemoveBackupContainer(c)
	}
	r.cfg.Docker.SweepBackupVolumes()
	return out, nil
}

// Follow replays the run from its first event and waits for its result,
// then removes the helper. Canceling ctx stops following and leaves the
// helper as it is.
func (o *Orphan) Follow(ctx context.Context, h proto.Handler) (*proto.Result, error) {
	d := o.r.cfg.Docker
	rc, err := d.FollowHelper(ctx, o.c.ID)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, ErrGone
		}
		return nil, fmt.Errorf("runner: follow %s: %w", o.c.Name, err)
	}
	res, err := proto.Follow(ctx, rc, h)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, proto.ErrNoResult) {
		wctx, cancel := context.WithTimeout(ctx, reapTimeout)
		st, werr := d.WaitBackupContainer(wctx, o.c.ID)
		cancel()
		switch {
		case cerrdefs.IsNotFound(werr):
			return nil, ErrGone
		case werr == nil:
			err = fmt.Errorf("%w: odac-backup %s", err, st)
		}
	}
	d.RemoveBackupContainer(o.c)
	return res, err
}
