package backup

import (
	"context"
	"io"
	"time"

	"odac/internal/backup/consistency"
	"odac/internal/backup/proto"
	"odac/internal/backup/runner"
	"odac/internal/backup/settings"
)

// rememberDump records the dump a run just stored, so that a run of another
// job moments later reads it back instead of dumping the database again.
func (m *Manager) rememberDump(app, target string, d *consistency.Dump, snaps []proto.Snapshot) {
	for _, s := range snaps {
		if s.Source.App != app || s.Source.Name != d.Name {
			continue
		}
		rec := &dumpRecord{Target: target, Snapshot: s.ID, Name: d.Name, File: d.File, Consistency: s.Consistency, At: ms(s.End)}
		if err := m.state.update(func(st *stateData) { st.Dumps[app] = rec }); err != nil {
			m.log.Error("Failed to save the backup state: %s", err.Error())
		}
		return
	}
}

// reusable returns a stored dump of app recent enough to stand in for a
// new one: within the reuse window and a quarter of the job's interval, so
// a frequent job is never handed a dump older than its schedule promises.
// ClickHouse is never reused: its parts are frozen by this run, and an
// older stream of definitions would not match them.
func (m *Manager) reusable(app string, plan *consistency.Plan, rs runSpec) *dumpRecord {
	if plan.Dump == nil || plan.Engine == consistency.ClickHouse {
		return nil
	}
	var rec *dumpRecord
	m.state.view(func(st *stateData) {
		if r := st.Dumps[app]; r != nil {
			c := *r
			rec = &c
		}
	})
	if rec == nil || rec.Name != plan.Dump.Name || rec.File != plan.Dump.File {
		return nil
	}
	window := min(m.t.reuseWindow, time.Duration(rs.job.Every)*time.Minute/4)
	if age := m.now().Sub(fromMs(rec.At)); age < 0 || age > window {
		return nil
	}
	return rec
}

// storedDump reads a stored dump back as a stream source: a restore run on
// its repository, piped into the backup. The pipe ends with io.EOF only
// once the restore has succeeded, so a restore that fails midway fails the
// backup instead of storing a cut dump.
func (m *Manager) storedDump(rec *dumpRecord) func(context.Context) (io.ReadCloser, error) {
	return func(ctx context.Context) (io.ReadCloser, error) {
		r, err := m.currentRunner()
		if err != nil {
			return nil, err
		}
		a, err := m.settings().Access(rec.Target, settings.AccessOptions{})
		if err != nil {
			return nil, err
		}
		req, err := m.request(proto.OpRestore, a, true)
		if err != nil {
			return nil, err
		}
		req.Restore = &proto.Restore{ID: rec.Snapshot}
		pr, pw := io.Pipe()
		rctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, err := r.Run(rctx, runner.Invocation{
				Request: req, Target: a.Target, Handler: proto.Handler{Output: pw},
				Labels: map[string]string{labelKind: proto.OpRestore, labelTarget: rec.Target},
			})
			pw.CloseWithError(err)
		}()
		return &restoreStream{PipeReader: pr, cancel: cancel, done: done}, nil
	}
}

type restoreStream struct {
	*io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
}

// Close ends the restore and waits for it, so no run outlives the backup
// that read it.
func (s *restoreStream) Close() error {
	s.cancel()
	s.PipeReader.Close()
	<-s.done
	return nil
}
