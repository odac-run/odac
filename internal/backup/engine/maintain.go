package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/kopia/kopia/snapshot/snapshotmaintenance"
)

// maintenanceSafety is SafetyFull and nothing else in production:
// SafetyNone may delete the not yet referenced contents of a backup that
// is being written at that moment. Only tests replace it, to see garbage
// collection finish without waiting days of wall time.
var maintenanceSafety = maintenance.SafetyFull

// maxVerifyErrors caps how many corruption reports one verification keeps.
const maxVerifyErrors = 100

// Maintain runs Kopia maintenance. Quick maintenance compacts indexes and
// is cheap; full maintenance also collects the contents no snapshot
// references any more, which is what returns the space of deleted
// snapshots. Under SafetyFull that takes more than one full run spread
// over time, so a size decision belongs after maintenance, on Size.
func (r *Repo) Maintain(ctx context.Context, full bool) error {
	if err := r.writable(); err != nil {
		return err
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	mode := maintenance.ModeQuick
	if full {
		mode = maintenance.ModeFull
	}
	err := repo.DirectWriteSession(ctx, r.rep, repo.WriteSessionOptions{Purpose: "odac:maintenance"},
		func(ctx context.Context, w repo.DirectRepositoryWriter) error {
			return snapshotmaintenance.Run(ctx, w, mode, false, maintenanceSafety)
		})
	var notOwned maintenance.NotOwnedError
	if errors.As(err, &notOwned) {
		return fmt.Errorf("%w (%s)", ErrNotOwner, notOwned.Owner)
	}
	if err != nil {
		return fmt.Errorf("engine: maintenance: %w", err)
	}
	return nil
}

// Size is the repository's measured footprint on its storage: the sum of
// every blob, which is what the target's disk or bill actually carries.
func (r *Repo) Size(ctx context.Context) (int64, error) {
	if err := r.err(); err != nil {
		return 0, err
	}
	var total int64
	err := r.rep.BlobReader().ListBlobs(ctx, "", func(bm blob.Metadata) error {
		total += bm.Length
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("engine: size: %w", err)
	}
	return total, nil
}

// VerifyResult reports a verification. Errors lists damage found; the
// call itself fails only when verification could not run.
type VerifyResult struct {
	Snapshots int      `json:"snapshots"`
	Objects   int64    `json:"objects"`
	FilesRead int64    `json:"filesRead"`
	BytesRead int64    `json:"bytesRead"`
	Errors    []string `json:"errors,omitempty"`
}

// Verify checks that every object of this server's snapshots is present,
// and reads filesPercent of the files in full, which decrypts them and
// checks their hashes. Sampling a fraction per run covers the whole
// repository over successive runs without reading it all each time.
func (r *Repo) Verify(ctx context.Context, filesPercent float64) (VerifyResult, error) {
	if err := r.err(); err != nil {
		return VerifyResult{}, err
	}
	if filesPercent < 0 || filesPercent > 100 {
		return VerifyResult{}, errors.New("engine: verify percent must be within 0-100")
	}
	blobs, err := blob.ReadBlobMap(ctx, r.rep.BlobReader())
	if err != nil {
		return VerifyResult{}, fmt.Errorf("engine: verify: %w", err)
	}
	ms, err := r.find(ctx, r.rep, Filter{})
	if err != nil {
		return VerifyResult{}, err
	}

	out := VerifyResult{}
	v := snapshotfs.NewVerifier(ctx, r.rep, snapshotfs.VerifierOptions{
		VerifyFilesPercent: filesPercent,
		MaxErrors:          maxVerifyErrors,
		BlobMap:            blobs,
	})
	res, err := v.InParallel(ctx, func(tw *snapshotfs.TreeWalker) error {
		for _, m := range ms {
			if _, ok := fromManifest(m); !ok {
				continue
			}
			root, err := snapshotfs.SnapshotRoot(r.rep, m)
			if err != nil {
				tw.ReportError(ctx, string(m.ID), err)
				continue
			}
			out.Snapshots++
			// Errors land in the walker and come back in the result.
			tw.Process(ctx, root, string(m.ID))
		}
		return nil
	})
	out.Objects = res.Stats.ProcessedObjectCount
	out.FilesRead = res.Stats.ReadFileCount
	out.BytesRead = res.Stats.ReadBytes
	out.Errors = res.ErrorStrings
	if res.ErrorCount == 0 && err != nil {
		return out, fmt.Errorf("engine: verify: %w", err)
	}
	if res.ErrorCount > len(out.Errors) {
		out.Errors = append(out.Errors, fmt.Sprintf("%d more errors not listed", res.ErrorCount-len(out.Errors)))
	}
	return out, ctx.Err()
}
