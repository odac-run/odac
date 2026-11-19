// Package proto is the wire contract between the server and odac-backup,
// which the server runs once per operation inside a helper container. Both
// sides import it, so it must never import the backup engine: the server
// would link Kopia through it (cmd/odac-backup/boundary_test.go).
//
// One run is one request. The server reads a hello event and writes the
// request as a length-prefixed JSON header to stdin. For a backup with a
// stream item, odac-backup sends a ready event once the repository is
// open, and only then does the server start the dump and send it as
// framed chunks after the header. odac-backup answers on
// stdout with one JSON event per line and ends with exactly one result or
// error event. Secrets travel in the header only, never in the
// environment, because a container's environment is visible to anyone who
// can inspect it.
//
// The two sides can come from different releases during an update, so the
// protocol is versioned. Version changes when framing changes or when a
// field the receiver would ignore changes the outcome; new optional fields
// and new event types do not change it, and receivers ignore what they do
// not know. odac-backup serves every version from MinVersion to Version, so
// keep MinVersion one release behind after a bump.
package proto

import (
	"fmt"
	"path/filepath"
	"time"
)

// Version is the protocol version this build speaks; MinVersion is the
// oldest one odac-backup still serves.
const (
	Version    = 1
	MinVersion = 1
)

// Operations.
const (
	OpInit     = "init"
	OpBackup   = "backup"
	OpList     = "list"
	OpDelete   = "delete"
	OpMaintain = "maintain"
	OpVerify   = "verify"
	OpRestore  = "restore"
	OpUsage    = "usage"
)

// Request is the header of a run. Exactly the argument member that
// belongs to Op is set; init and usage take none.
type Request struct {
	Version  int       `json:"version"`
	Op       string    `json:"op"`
	Repo     Repo      `json:"repo"`
	Backup   *Backup   `json:"backup,omitempty"`
	List     *List     `json:"list,omitempty"`
	Delete   *Delete   `json:"delete,omitempty"`
	Maintain *Maintain `json:"maintain,omitempty"`
	Verify   *Verify   `json:"verify,omitempty"`
	Restore  *Restore  `json:"restore,omitempty"`
}

// Repo says which repository the run opens and as whom.
type Repo struct {
	Storage  Storage `json:"storage"`
	Password string  `json:"password"`
	ServerID string  `json:"serverId"`
	// CacheDir is the cache's path inside the helper; the server mounts a
	// directory that outlives the helper there.
	CacheDir   string `json:"cacheDir"`
	CacheBytes int64  `json:"cacheBytes,omitempty"`
	// ReadOnly opens another server's repository for list and restore.
	ReadOnly bool `json:"readOnly,omitempty"`
}

// Storage names where the repository lives, as the helper sees it: a
// local disk, SMB share or NFS export is a mounted Filesystem path.
// Exactly one member is set.
type Storage struct {
	Filesystem *FilesystemStorage `json:"filesystem,omitempty"`
	S3         *S3Storage         `json:"s3,omitempty"`
	WebDAV     *WebDAVStorage     `json:"webdav,omitempty"`
}

// FilesystemStorage is a directory inside the helper.
type FilesystemStorage struct {
	Path string `json:"path"`
}

// S3Storage is any S3-compatible bucket.
type S3Storage struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region,omitempty"`
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix,omitempty"`
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken,omitempty"`
	DisableTLS      bool   `json:"disableTLS,omitempty"`
}

// WebDAVStorage is a WebDAV collection.
type WebDAVStorage struct {
	URL      string `json:"url"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// Backup stores one job run. A run may read several directories but only
// one stream, which must be the first item: a dump holds its database's
// transaction open while it is read, so it never waits behind a directory.
type Backup struct {
	Run     string `json:"run"`
	Trigger string `json:"trigger"`
	Items   []Item `json:"items"`
}

// Item is one source of a run. Exactly one of Dir and Stream is set.
type Item struct {
	Source      Source `json:"source"`
	Consistency string `json:"consistency"`
	// Dir is a directory mounted into the helper.
	Dir string `json:"dir,omitempty"`
	// Stream is the file name the stdin stream is stored under.
	Stream string `json:"stream,omitempty"`
}

// Source is what one snapshot covers.
type Source struct {
	Job  string `json:"job"`
	App  string `json:"app"`
	Name string `json:"name"`
}

// List selects snapshots; empty fields match everything, except Host,
// which defaults to the requesting server.
type List struct {
	Host string `json:"host,omitempty"`
	Job  string `json:"job,omitempty"`
	App  string `json:"app,omitempty"`
	Name string `json:"name,omitempty"`
	Run  string `json:"run,omitempty"`
}

// Delete removes snapshots by id.
type Delete struct {
	IDs []string `json:"ids"`
}

// Maintain runs maintenance; full also collects garbage.
type Maintain struct {
	Full bool `json:"full,omitempty"`
}

// Verify checks every snapshot's objects and reads Percent of the files.
type Verify struct {
	Percent float64 `json:"percent"`
}

// Restore writes a snapshot back. A directory snapshot needs Dir, an empty
// or absent directory inside the helper; a stream snapshot leaves Dir
// empty and comes back as data events.
type Restore struct {
	ID  string `json:"id"`
	Dir string `json:"dir,omitempty"`
}

// Snapshot is one stored snapshot. TornPaths, StalePaths and SQLitePaths
// (databases stored as a consistent copy) are only known to the run that
// stored it.
type Snapshot struct {
	ID          string    `json:"id"`
	Host        string    `json:"host"`
	Source      Source    `json:"source"`
	Kind        string    `json:"kind"`
	Run         string    `json:"run"`
	Trigger     string    `json:"trigger"`
	Consistency string    `json:"consistency"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Files       int64     `json:"files"`
	Bytes       int64     `json:"bytes"`
	Torn        int       `json:"torn,omitempty"`
	Stale       int       `json:"stale,omitempty"`
	TornPaths   []string  `json:"tornPaths,omitempty"`
	StalePaths  []string  `json:"stalePaths,omitempty"`
	SQLitePaths []string  `json:"sqlitePaths,omitempty"`
}

// VerifyResult reports a verification; Errors lists damage found.
type VerifyResult struct {
	Snapshots int      `json:"snapshots"`
	Objects   int64    `json:"objects"`
	FilesRead int64    `json:"filesRead"`
	BytesRead int64    `json:"bytesRead"`
	Errors    []string `json:"errors,omitempty"`
}

// Result closes a successful run. Which members are set depends on the
// operation: Created for init, Count for backup and list (snapshot events
// sent), Bytes for maintain and usage (repository size) and for restore
// (bytes written), Files for a directory restore, Verify for verify.
type Result struct {
	Created bool          `json:"created,omitempty"`
	Count   int           `json:"count,omitempty"`
	Bytes   int64         `json:"bytes,omitempty"`
	Files   int64         `json:"files,omitempty"`
	Verify  *VerifyResult `json:"verify,omitempty"`
}

// Progress reports the work done on one backup item or restore so far.
type Progress struct {
	Item  int   `json:"item"`
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

// Error codes. The server branches on these, so they are part of the
// contract.
const (
	CodeVersion        = "unsupported_version"
	CodeBadRequest     = "bad_request"
	CodeNotInitialized = "not_initialized"
	CodeExists         = "exists"
	CodeWrongPassword  = "wrong_password"
	CodeNotOwner       = "not_owner"
	CodeReadOnly       = "read_only"
	CodeNotFound       = "not_found"
	CodeForeign        = "foreign"
	CodeKind           = "kind"
	CodeTargetNotEmpty = "target_not_empty"
	CodeStream         = "stream"
	CodeCanceled       = "canceled"
	CodeFailed         = "failed"
)

// Error is a run that failed on the odac-backup side.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return "odac-backup: " + e.Code + ": " + e.Message }

func badRequest(format string, a ...any) *Error {
	return &Error{Code: CodeBadRequest, Message: fmt.Sprintf(format, a...)}
}

// Validate checks the shape of a request. What only the engine can judge
// (names, passwords, server ids) is left to it.
func (r *Request) Validate() error {
	if r.Version < MinVersion || r.Version > Version {
		return &Error{Code: CodeVersion, Message: fmt.Sprintf("request speaks protocol %d, odac-backup speaks %d to %d", r.Version, MinVersion, Version)}
	}
	args := map[string]bool{
		OpBackup:   r.Backup != nil,
		OpList:     r.List != nil,
		OpDelete:   r.Delete != nil,
		OpMaintain: r.Maintain != nil,
		OpVerify:   r.Verify != nil,
		OpRestore:  r.Restore != nil,
	}
	switch r.Op {
	case OpInit, OpUsage, OpBackup, OpList, OpDelete, OpMaintain, OpVerify, OpRestore:
	default:
		return badRequest("unknown operation %q", r.Op)
	}
	for op, set := range args {
		if set != (op == r.Op) {
			if set {
				return badRequest("%s carries %s arguments", r.Op, op)
			}
			return badRequest("%s needs its arguments", r.Op)
		}
	}
	switch {
	case r.Backup != nil:
		return r.Backup.validate()
	case r.Delete != nil:
		if len(r.Delete.IDs) == 0 {
			return badRequest("delete names no snapshot")
		}
	case r.Verify != nil:
		if r.Verify.Percent < 0 || r.Verify.Percent > 100 {
			return badRequest("verify percent must be within 0-100")
		}
	case r.Restore != nil:
		if r.Restore.ID == "" {
			return badRequest("restore names no snapshot")
		}
		if r.Restore.Dir != "" && !filepath.IsAbs(r.Restore.Dir) {
			return badRequest("restore directory must be absolute")
		}
	}
	return nil
}

func (b *Backup) validate() error {
	if b.Run == "" || b.Trigger == "" {
		return badRequest("backup needs a run and a trigger")
	}
	if len(b.Items) == 0 {
		return badRequest("backup has no items")
	}
	for i, it := range b.Items {
		if (it.Dir == "") == (it.Stream == "") {
			return badRequest("item %d must set exactly one of dir and stream", i)
		}
		if it.Stream != "" && i != 0 {
			return badRequest("item %d: only the first item may be a stream", i)
		}
		if it.Dir != "" && !filepath.IsAbs(it.Dir) {
			return badRequest("item %d: directory must be absolute", i)
		}
	}
	return nil
}

// HasStream reports whether the run reads a stream from stdin.
func (r *Request) HasStream() bool {
	return r.Backup != nil && len(r.Backup.Items) > 0 && r.Backup.Items[0].Stream != ""
}

// streamsOut reports whether the run answers with data events.
func (r *Request) streamsOut() bool {
	return r.Restore != nil && r.Restore.Dir == ""
}

// Secrets lists the request's secret values, for masking.
func (r *Request) Secrets() []string {
	s := []string{r.Repo.Password}
	if st := r.Repo.Storage.S3; st != nil {
		s = append(s, st.SecretAccessKey, st.SessionToken)
	}
	if st := r.Repo.Storage.WebDAV; st != nil {
		s = append(s, st.Password)
	}
	return s
}
