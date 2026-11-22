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
	// OpServers lists the server directories on the target of the
	// request's storage, which names this server's repository: what a
	// restore from another server (a migration) chooses from. It opens no
	// repository, so it needs no password.
	OpServers = "servers"
)

// Request is the header of a run. Exactly the argument member that
// belongs to Op is set; init and servers take none, and usage takes Usage
// only when it is asked for more than sizes.
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
	Usage    *Usage    `json:"usage,omitempty"`
	// Share, for maintain and usage on a filesystem target, writes this
	// server's usage file (UsageFile) next to its repository and answers
	// with the usage files of the other servers on the target
	// (Space.Peers): servers that share one disk without a protocol
	// between them read each other's sizes there to split its space
	// fairly. It writes, so it needs an owner claim.
	Share bool `json:"share,omitempty"`
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
	// Owner is the clone check of a target repository: the operations
	// that write (init, backup, delete, maintain) claim the server
	// directory before touching the repository. Nil skips the check,
	// which only a repository no other machine can reach may do.
	Owner *Owner `json:"owner,omitempty"`
}

// Layout of a server's directory on a target: the repository lives in
// RepositoryDir and the ownership file next to it, so a target shared by
// several servers holds one directory per server id. With an Owner the
// storage must name <server id>/RepositoryDir (a filesystem path or a
// WebDAV URL ending in it, an S3 prefix ending in it and a slash).
const (
	RepositoryDir = "repository"
	OwnerFile     = "server.json"
	// UsageFile, beside OwnerFile, is what the server last measured its
	// repository at; see Request.Share.
	UsageFile = "usage.json"
)

// Owner claims a server directory. Seq is the number this run writes into
// the ownership file; the server persists it before the run and never
// sends a number twice. A file already holding Seq or more means another
// machine writes under this server id (a cloned VM), and the run fails
// with CodeOwnerConflict before writing anything. Adopt, for init only,
// takes the directory over whatever it holds and writes the number after
// the one found, which the result reports: the operator's answer once a
// conflict is understood, such as a target added again.
type Owner struct {
	Name  string `json:"name"`
	Seq   int64  `json:"seq"`
	Adopt bool   `json:"adopt,omitempty"`
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

// Usage asks usage to walk every snapshot, which costs about what garbage
// collection costs and is why it is optional: the answer then says how
// much of the repository no snapshot references (garbage maintenance is
// still returning), how much each job references, and what deleting the
// runs in Order would free.
type Usage struct {
	// Order lists runs in the order the server would delete them, each as
	// the IDs of its snapshots.
	Order [][]string `json:"order,omitempty"`
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
// (bytes written), Files for a directory restore, Verify for verify,
// Space for maintain and usage, Servers for servers.
// Seq is the number an owner claim wrote. SameDisk, for init on a
// filesystem, says the repository shares a file system with the cache
// directory, which the server keeps on its own data disk: such a target
// only protects against deleting by mistake.
type Result struct {
	Created  bool          `json:"created,omitempty"`
	Count    int           `json:"count,omitempty"`
	Bytes    int64         `json:"bytes,omitempty"`
	Files    int64         `json:"files,omitempty"`
	Verify   *VerifyResult `json:"verify,omitempty"`
	Seq      int64         `json:"seq,omitempty"`
	SameDisk bool          `json:"sameDisk,omitempty"`
	Space    *Space        `json:"space,omitempty"`
	Servers  []ServerDir   `json:"servers,omitempty"`
}

// ServerDir is one server's directory on a target, as its ownership file
// describes it. Repository says a repository lives in it; a directory
// whose ownership file is unreadable is listed with Error and nothing
// else, so the operator sees it rather than a gap.
type ServerDir struct {
	ID         string    `json:"id"`
	Name       string    `json:"name,omitempty"`
	Seq        int64     `json:"seq,omitempty"`
	Updated    time.Time `json:"updated,omitempty"`
	Repository bool      `json:"repository,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// Space is what a repository's storage looks like. Free and Total
// describe a filesystem target's disk and are -1 where storage has no
// notion of free space (S3, WebDAV). The other members come only from a
// usage run that asked for them (Detailed).
type Space struct {
	Free     int64 `json:"free"`
	Total    int64 `json:"total"`
	Detailed bool  `json:"detailed,omitempty"`
	// Garbage is the packed size of the contents no snapshot references:
	// space that maintenance returns over its next full runs.
	Garbage int64 `json:"garbage,omitempty"`
	// Jobs is the packed size each job's snapshots reference, which is
	// what a job's budget caps.
	Jobs map[string]int64 `json:"jobs,omitempty"`
	// Freed has one entry per run of Usage.Order: what deleting it and
	// every run before it would turn into garbage.
	Freed []Freed `json:"freed,omitempty"`
	// Peers are the other server directories on the target, with what
	// their usage files say, when the request asked to Share.
	Peers []PeerUsage `json:"peers,omitempty"`
}

// PeerUsage is another server's directory on a shared filesystem target.
// Updated is when its usage file was written (zero without one) and Seen
// when its ownership file was, by that server's own clock. Error says the
// usage file could not be read, which counts as no usage file.
type PeerUsage struct {
	ID      string    `json:"id"`
	Name    string    `json:"name,omitempty"`
	Bytes   int64     `json:"bytes,omitempty"`
	Garbage int64     `json:"garbage,omitempty"`
	Updated time.Time `json:"updated,omitempty"`
	Seen    time.Time `json:"seen,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// Freed is what deleting a prefix of an order frees. Repo counts the
// contents no remaining snapshot references; Job counts, for the job of
// the run at this position, the contents none of that job's remaining
// snapshots reference, which is what lowers the job's referenced size.
type Freed struct {
	Repo int64 `json:"repo"`
	Job  int64 `json:"job"`
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
	// CodeOwnerConflict: the server directory's ownership file is ahead
	// of the claim, or names another server.
	CodeOwnerConflict = "owner_conflict"
	// CodeUnsafeEntry: a snapshot names an entry outside the restore
	// target, which a repository written by this server never does.
	CodeUnsafeEntry = "unsafe_entry"
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
	case OpInit, OpUsage, OpServers, OpBackup, OpList, OpDelete, OpMaintain, OpVerify, OpRestore:
	default:
		return badRequest("unknown operation %q", r.Op)
	}
	if r.Usage != nil && r.Op != OpUsage {
		return badRequest("%s carries usage arguments", r.Op)
	}
	if o := r.Repo.Owner; o != nil {
		if r.Repo.ReadOnly {
			return badRequest("a read-only run claims no owner")
		}
		if o.Seq < 1 {
			return badRequest("owner sequence must be positive")
		}
		if o.Adopt && r.Op != OpInit {
			return badRequest("only init adopts a server directory")
		}
		if r.Op == OpServers {
			return badRequest("servers claims no owner")
		}
	}
	if r.Share {
		if r.Op != OpMaintain && r.Op != OpUsage {
			return badRequest("only maintain and usage share a target")
		}
		if r.Repo.Owner == nil || r.Repo.Storage.Filesystem == nil {
			return badRequest("sharing needs an owner claim on a filesystem target")
		}
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

// Writes reports whether the operation writes the repository or the
// server directory around it, which is when an owner claim is checked
// and written.
func (r *Request) Writes() bool {
	switch r.Op {
	case OpInit, OpBackup, OpDelete, OpMaintain:
		return true
	case OpUsage:
		return r.Share
	}
	return false
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
