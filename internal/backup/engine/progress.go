package engine

import (
	"sync/atomic"

	"github.com/kopia/kopia/snapshot/upload"
)

// Progress counts the work a backup has done so far. It is safe to read
// while the backup runs. A live directory is read more than once when its
// files move, so the counts measure work, not a fraction of the source.
type Progress struct {
	files atomic.Int64
	bytes atomic.Int64
}

// Files is how many files have been read or found unchanged.
func (p *Progress) Files() int64 { return p.files.Load() }

// Bytes is how many bytes have been read or found unchanged.
func (p *Progress) Bytes() int64 { return p.bytes.Load() }

// uploadProgress feeds Kopia's callbacks into a Progress. Enabled stays
// false (from the embedded null progress), which keeps Kopia from walking
// the whole source up front to estimate its size.
type uploadProgress struct {
	upload.NullUploadProgress
	p *Progress
}

func (u *uploadProgress) HashedBytes(n int64)            { u.p.bytes.Add(n) }
func (u *uploadProgress) CachedFile(_ string, n int64)   { u.p.bytes.Add(n) }
func (u *uploadProgress) FinishedFile(_ string, _ error) { u.p.files.Add(1) }
