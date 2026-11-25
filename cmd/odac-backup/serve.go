package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"odac/internal/backup/engine"
	"odac/internal/backup/proto"
	"odac/internal/sysinfo"
)

// progressEvery is how often a backup item reports progress.
const progressEvery = 2 * time.Second

// serve answers one request. Exit code 1 always comes with an error event,
// so the server never has to guess from the code alone.
func serve(ctx context.Context, stdin io.Reader, stdout io.Writer) int {
	em := proto.NewEmitter(stdout)
	if em.Hello(sysinfo.Version) != nil {
		return 1
	}
	// One buffered reader for the header and the stream behind it.
	in := bufio.NewReaderSize(stdin, 256<<10)
	req, err := proto.ReadRequest(in)
	if err != nil {
		return fail(em, err)
	}
	em.Mask(req.Secrets()...)
	res, err := handle(ctx, req, in, em)
	if err != nil {
		return fail(em, err)
	}
	if em.Result(*res) != nil {
		return 1
	}
	return 0
}

func fail(em *proto.Emitter, err error) int {
	var pe *proto.Error
	if errors.As(err, &pe) {
		em.Error(pe.Code, pe.Message)
		return 1
	}
	em.Error(errorCode(err), err.Error())
	return 1
}

var errorCodes = []struct {
	err  error
	code string
}{
	{context.Canceled, proto.CodeCanceled},
	{context.DeadlineExceeded, proto.CodeCanceled},
	{proto.ErrTruncated, proto.CodeStream},
	{engine.ErrNotInitialized, proto.CodeNotInitialized},
	{engine.ErrExists, proto.CodeExists},
	{engine.ErrWrongPassword, proto.CodeWrongPassword},
	{engine.ErrNotOwner, proto.CodeNotOwner},
	{engine.ErrReadOnly, proto.CodeReadOnly},
	{engine.ErrNotFound, proto.CodeNotFound},
	{engine.ErrForeign, proto.CodeForeign},
	{engine.ErrKind, proto.CodeKind},
	{engine.ErrTargetNotEmpty, proto.CodeTargetNotEmpty},
	{engine.ErrOwnerConflict, proto.CodeOwnerConflict},
	{engine.ErrUnsafeEntry, proto.CodeUnsafeEntry},
}

func errorCode(err error) string {
	for _, c := range errorCodes {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return proto.CodeFailed
}

// handle claims the server directory before an operation that writes, so
// a clone of this server stops before it touches the repository.
func handle(ctx context.Context, req *proto.Request, in io.Reader, em *proto.Emitter) (*proto.Result, error) {
	var seq int64
	if o := req.Repo.Owner; o != nil && req.Writes() {
		var err error
		seq, err = engine.ClaimOwner(ctx, convert[engine.Storage](req.Repo.Storage), engine.OwnerClaim{
			ServerID: req.Repo.ServerID, Name: o.Name, Seq: o.Seq, Adopt: o.Adopt,
		})
		if err != nil {
			return nil, err
		}
	}
	res, err := operate(ctx, req, in, em)
	if res != nil {
		res.Seq = seq
	}
	return res, err
}

func operate(ctx context.Context, req *proto.Request, in io.Reader, em *proto.Emitter) (*proto.Result, error) {
	cfg := engine.Config{
		Storage:    convert[engine.Storage](req.Repo.Storage),
		Password:   req.Repo.Password,
		ServerID:   req.Repo.ServerID,
		CacheDir:   req.Repo.CacheDir,
		CacheBytes: req.Repo.CacheBytes,
		ReadOnly:   req.Repo.ReadOnly,
	}
	switch req.Op {
	case proto.OpInit:
		return initRepo(ctx, cfg)
	case proto.OpServers:
		dirs, err := engine.ListServers(ctx, cfg.Storage, cfg.ServerID)
		if err != nil {
			return nil, err
		}
		return &proto.Result{Servers: convert[[]proto.ServerDir](dirs)}, nil
	}
	r, err := engine.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// A canceled run still closes the repository and removes the
	// connection file. What was written is already committed, so a close
	// failure is not the run's failure.
	defer r.Close(context.WithoutCancel(ctx))

	switch req.Op {
	case proto.OpBackup:
		return backup(ctx, r, req.Backup, in, em)
	case proto.OpList:
		f := req.List
		snaps, err := r.List(ctx, engine.Filter{Host: f.Host, Job: f.Job, App: f.App, Name: f.Name, Run: f.Run})
		if err != nil {
			return nil, err
		}
		for _, s := range snaps {
			if err := em.Snapshot(convert[proto.Snapshot](s)); err != nil {
				return nil, err
			}
		}
		return &proto.Result{Count: len(snaps)}, nil
	case proto.OpDelete:
		return &proto.Result{}, r.Delete(ctx, req.Delete.IDs)
	case proto.OpMaintain:
		if err := r.Maintain(ctx, req.Maintain.Full); err != nil {
			return nil, err
		}
		res, err := usage(ctx, r, nil)
		return share(ctx, req, res, err)
	case proto.OpVerify:
		v, err := r.Verify(ctx, req.Verify.Percent)
		if err != nil {
			return nil, err
		}
		out := convert[proto.VerifyResult](v)
		return &proto.Result{Verify: &out}, nil
	case proto.OpRestore:
		return restore(ctx, r, req.Restore, em)
	case proto.OpUsage:
		res, err := usage(ctx, r, req.Usage)
		return share(ctx, req, res, err)
	}
	return nil, fmt.Errorf("operation %q has no handler", req.Op)
}

// initRepo creates the repository, or opens the one already there, which
// also proves the password: a target added again after a reinstall must
// be given the key it was created with.
func initRepo(ctx context.Context, cfg engine.Config) (*proto.Result, error) {
	created := true
	r, err := engine.Create(ctx, cfg)
	if errors.Is(err, engine.ErrExists) {
		created = false
		r, err = engine.Open(ctx, cfg)
	}
	if err != nil {
		return nil, err
	}
	r.Close(context.WithoutCancel(ctx))
	res := &proto.Result{Created: created}
	if fs := cfg.Storage.Filesystem; fs != nil {
		// Unknown counts as another disk: the label is a warning, and a
		// false one would be worse than a missing one.
		res.SameDisk, _ = engine.SameFileSystem(fs.Path, cfg.CacheDir)
	}
	return res, nil
}

// usage measures the repository. The walk of every snapshot behind a
// detailed answer is only made when asked for.
func usage(ctx context.Context, r *engine.Repo, u *proto.Usage) (*proto.Result, error) {
	n, err := r.Size(ctx)
	if err != nil {
		return nil, err
	}
	sp := r.FreeSpace()
	if u != nil {
		if sp, err = r.Space(ctx, u.Order); err != nil {
			return nil, err
		}
	}
	out := convert[proto.Space](sp)
	return &proto.Result{Bytes: n, Space: &out}, nil
}

// share writes this server's usage file from a measurement and adds what
// the other servers' files say, when the request asks to share the target.
// Garbage is known only from a detailed measurement; without one the file
// says none, which only makes the others count less space as coming back.
func share(ctx context.Context, req *proto.Request, res *proto.Result, err error) (*proto.Result, error) {
	if err != nil || !req.Share {
		return res, err
	}
	st := convert[engine.Storage](req.Repo.Storage)
	rec := engine.UsageRecord{ServerID: req.Repo.ServerID, Name: req.Repo.Owner.Name, Bytes: res.Bytes, Updated: time.Now()}
	if res.Space != nil && res.Space.Detailed {
		rec.Garbage = res.Space.Garbage
	}
	if err := engine.WriteUsage(st, rec); err != nil {
		return nil, err
	}
	peers, err := engine.ReadPeers(ctx, st, req.Repo.ServerID)
	if err != nil {
		return nil, err
	}
	if res.Space == nil {
		res.Space = &proto.Space{Free: -1, Total: -1}
	}
	res.Space.Peers = convert[[]proto.PeerUsage](peers)
	return res, nil
}

// backup stores the run's items in order and reports each snapshot as
// soon as it is stored, so a run that fails later still accounts for what
// it left in the repository.
func backup(ctx context.Context, r *engine.Repo, b *proto.Backup, in io.Reader, em *proto.Emitter) (*proto.Result, error) {
	for i, it := range b.Items {
		src := engine.Source{Job: it.Source.Job, App: it.Source.App, Name: it.Source.Name}
		meta := engine.Meta{Run: b.Run, Trigger: b.Trigger, Consistency: it.Consistency, Progress: &engine.Progress{}}
		stop := reportProgress(em, i, meta.Progress)
		var (
			res *engine.BackupResult
			err error
		)
		if it.Stream != "" {
			if err := em.Ready(); err != nil {
				stop()
				return nil, err
			}
			sr := proto.NewStreamReader(in)
			res, err = r.BackupStream(ctx, src, it.Stream, sr, meta)
			// The engine reports a reader's failure as text only.
			if err != nil && sr.Err() != nil && ctx.Err() == nil {
				err = fmt.Errorf("%w: %v", sr.Err(), err)
			}
		} else {
			res, err = r.BackupDir(ctx, src, it.Dir, meta)
		}
		stop()
		if err != nil {
			return nil, fmt.Errorf("item %d (%s/%s): %w", i, it.Source.App, it.Source.Name, err)
		}
		if err := em.Snapshot(convert[proto.Snapshot](res)); err != nil {
			return nil, err
		}
	}
	return &proto.Result{Count: len(b.Items)}, nil
}

// reportProgress emits p every progressEvery until the returned function
// is called, which waits for the reporter to stop.
func reportProgress(em *proto.Emitter, item int, p *engine.Progress) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(progressEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				em.Progress(proto.Progress{Item: item, Files: p.Files(), Bytes: p.Bytes()})
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

func restore(ctx context.Context, r *engine.Repo, rs *proto.Restore, em *proto.Emitter) (*proto.Result, error) {
	if rs.Dir != "" {
		st, err := r.RestoreDir(ctx, rs.ID, rs.Dir)
		if err != nil {
			return nil, err
		}
		return &proto.Result{Files: st.Files, Bytes: st.Bytes}, nil
	}
	w := em.DataWriter()
	n, err := r.RestoreStream(ctx, rs.ID, w)
	if err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return &proto.Result{Bytes: n}, nil
}

// convert maps between the engine's types and their wire twins by JSON
// name. The two are kept apart on purpose (the server must not link the
// engine, and the wire must not move when the engine does);
// TestWireTypesMatchEngine fails when a field exists on one side only.
func convert[T any](src any) T {
	var out T
	b, err := json.Marshal(src)
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	if err != nil {
		// Both sides are plain data; this is a programming error.
		panic(fmt.Sprintf("odac-backup: convert %T: %v", src, err))
	}
	return out
}
