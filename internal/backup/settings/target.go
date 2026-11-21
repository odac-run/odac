package settings

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"odac/internal/backup/proto"
	"odac/internal/backup/runner"
	"odac/internal/docker"
)

// Target kinds, by which member of a Target is set.
const (
	KindDisk   = "disk"
	KindSMB    = "smb"
	KindNFS    = "nfs"
	KindS3     = "s3"
	KindWebDAV = "webdav"
)

// Locations a target is labelled with. Only remote targets survive the
// loss of the server, the account or a ransomware run on it.
const (
	Local  = "local"
	Remote = "remote"
)

// Key sources.
const (
	KeyGenerated = "generated"
	KeyEntered   = "entered"
)

// Target is a saved target, secrets included. It never leaves this
// package except through Access; views are TargetView.
type Target struct {
	Name   string  `json:"name"`
	Disk   *Disk   `json:"disk,omitempty"`
	SMB    *SMB    `json:"smb,omitempty"`
	NFS    *NFS    `json:"nfs,omitempty"`
	S3     *S3     `json:"s3,omitempty"`
	WebDAV *WebDAV `json:"webdav,omitempty"`
	// Key is the repository password, the same for every job on this
	// target, because the repository is per target.
	Key       string `json:"key"`
	KeySource string `json:"keySource"`
	Location  string `json:"location"`
	// SameDisk: a disk target on the file system that holds ODAC's data,
	// which protects against deleting by mistake and nothing else.
	SameDisk bool  `json:"sameDisk,omitempty"`
	Created  int64 `json:"created"`
}

// Disk is a directory on this server's host, named as the host sees it.
type Disk struct {
	Path string `json:"path"`
}

// SMB is a share the daemon mounts for each run.
type SMB struct {
	Addr  string `json:"addr"`
	Share string `json:"share"`
	// Path is a directory inside the share; empty is its root.
	Path     string `json:"path,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Domain   string `json:"domain,omitempty"`
	Version  string `json:"version,omitempty"`
}

// NFS is an export the daemon mounts for each run.
type NFS struct {
	Addr    string `json:"addr"`
	Export  string `json:"export"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
}

// S3 is a bucket on any S3-compatible service.
type S3 struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region,omitempty"`
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix,omitempty"`
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	DisableTLS      bool   `json:"disableTLS,omitempty"`
}

// WebDAV is a WebDAV collection.
type WebDAV struct {
	URL      string `json:"url"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// TargetView is a target as lists show it. Its types have no secret
// fields at all, so no list can carry one.
type TargetView struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Kind      string      `json:"kind"`
	Disk      *Disk       `json:"disk,omitempty"`
	SMB       *SMBView    `json:"smb,omitempty"`
	NFS       *NFS        `json:"nfs,omitempty"`
	S3        *S3View     `json:"s3,omitempty"`
	WebDAV    *WebDAVView `json:"webdav,omitempty"`
	KeySource string      `json:"keySource"`
	Location  string      `json:"location"`
	SameDisk  bool        `json:"sameDisk,omitempty"`
	Created   int64       `json:"created"`
	// Conflict is set while writes to the target are stopped because
	// another machine writes under this server id.
	Conflict *Conflict `json:"conflict,omitempty"`
}

// SMBView is SMB without its password.
type SMBView struct {
	Addr     string `json:"addr"`
	Share    string `json:"share"`
	Path     string `json:"path,omitempty"`
	Username string `json:"username,omitempty"`
	Domain   string `json:"domain,omitempty"`
	Version  string `json:"version,omitempty"`
}

// S3View is S3 without its secret key. The access key id names the
// credential rather than proving it, and the operator needs it to tell
// two credentials apart.
type S3View struct {
	Endpoint    string `json:"endpoint"`
	Region      string `json:"region,omitempty"`
	Bucket      string `json:"bucket"`
	Prefix      string `json:"prefix,omitempty"`
	AccessKeyID string `json:"accessKeyId"`
	DisableTLS  bool   `json:"disableTLS,omitempty"`
}

// WebDAVView is WebDAV without its password.
type WebDAVView struct {
	URL      string `json:"url"`
	Username string `json:"username,omitempty"`
}

// TargetSpec is what the operator gives to add or change a target.
type TargetSpec struct {
	Name   string  `json:"name"`
	Disk   *Disk   `json:"disk,omitempty"`
	SMB    *SMB    `json:"smb,omitempty"`
	NFS    *NFS    `json:"nfs,omitempty"`
	S3     *S3     `json:"s3,omitempty"`
	WebDAV *WebDAV `json:"webdav,omitempty"`
	// Key is the operator's own passphrase; empty has the server generate
	// one. A change cannot set it.
	Key string `json:"key,omitempty"`
}

func (t *Target) kind() string {
	switch {
	case t.Disk != nil:
		return KindDisk
	case t.SMB != nil:
		return KindSMB
	case t.NFS != nil:
		return KindNFS
	case t.S3 != nil:
		return KindS3
	case t.WebDAV != nil:
		return KindWebDAV
	}
	return ""
}

// convert copies between a type and its view by JSON name, which is what
// drops the secret fields.
func convert[T any](src any) T {
	var out T
	b, err := json.Marshal(src)
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	if err != nil {
		panic(fmt.Sprintf("settings: convert %T: %v", src, err))
	}
	return out
}

func (s *Settings) view(id string, t *Target) TargetView {
	v := convert[TargetView](t)
	v.ID = id
	v.Kind = t.kind()
	v.Conflict = s.owners.conflict(id)
	return v
}

var (
	idPattern      = regexp.MustCompile(`^[0-9a-f]{16}$`)
	addrPattern    = regexp.MustCompile(`^[A-Za-z0-9.:\[\]-]+$`)
	bucketPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	prefixPattern  = regexp.MustCompile(`^[A-Za-z0-9!_.*'()/-]*$`)
	regionPattern  = regexp.MustCompile(`^[a-z0-9-]*$`)
	segmentPattern = regexp.MustCompile(`^[^\x00-\x1f/\\,]+$`)
)

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func invalid(format string, a ...any) error { return errorf(CodeInvalid, format, a...) }

func checkName(what, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 64 || !utf8.ValidString(name) {
		return "", invalid("A %s name is 1 to 64 characters.", what)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", invalid("A %s name cannot contain control characters.", what)
		}
	}
	return name, nil
}

// subPath checks a directory inside a share and returns it clean and
// absolute, "/" for the root.
func subPath(p string) (string, error) {
	if p == "" {
		return "/", nil
	}
	clean := path.Clean("/" + p)
	for _, seg := range strings.Split(strings.Trim(clean, "/"), "/") {
		if seg != "" && (!segmentPattern.MatchString(seg) || seg == "..") {
			return "", invalid("Invalid directory %q inside the share.", p)
		}
	}
	return clean, nil
}

// normalize checks spec and returns the target it describes, with the
// location label still unset. Everything is checked here, so a target is
// refused when it is added rather than at its first run.
func (s *Settings) normalize(spec TargetSpec) (*Target, error) {
	name, err := checkName("target", spec.Name)
	if err != nil {
		return nil, err
	}
	t := &Target{Name: name, Disk: spec.Disk, SMB: spec.SMB, NFS: spec.NFS, S3: spec.S3, WebDAV: spec.WebDAV}
	n := 0
	for _, set := range []bool{t.Disk != nil, t.SMB != nil, t.NFS != nil, t.S3 != nil, t.WebDAV != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return nil, invalid("A target is exactly one of a disk, an SMB share, an NFS export, an S3 bucket or a WebDAV collection.")
	}
	switch {
	case t.Disk != nil:
		d := *t.Disk
		if d.Path, err = checkHostPath(d.Path, s.cfg.Protected); err != nil {
			return nil, err
		}
		t.Disk = &d
	case t.SMB != nil:
		m := *t.SMB
		if !addrPattern.MatchString(m.Addr) {
			return nil, invalid("Invalid SMB server address.")
		}
		if m.Share = strings.Trim(m.Share, "/"); !segmentPattern.MatchString(m.Share) {
			return nil, invalid("Invalid SMB share name.")
		}
		if m.Path, err = subPath(m.Path); err != nil {
			return nil, err
		}
		if m.Username == "" && m.Password != "" {
			return nil, invalid("An SMB password needs a user name.")
		}
		if err := (docker.Share{Type: "cifs", Addr: m.Addr, Path: m.Share, Username: m.Username, Password: m.Password, Domain: m.Domain, Version: m.Version}).Validate(); err != nil {
			return nil, invalid("%s", strings.TrimPrefix(err.Error(), "share: "))
		}
		t.SMB = &m
	case t.NFS != nil:
		m := *t.NFS
		if !addrPattern.MatchString(m.Addr) {
			return nil, invalid("Invalid NFS server address.")
		}
		if !path.IsAbs(m.Export) {
			return nil, invalid("An NFS export is an absolute path.")
		}
		m.Export = path.Clean(m.Export)
		if m.Path, err = subPath(m.Path); err != nil {
			return nil, err
		}
		if err := (docker.Share{Type: "nfs", Addr: m.Addr, Path: m.Export, Version: m.Version}).Validate(); err != nil {
			return nil, invalid("%s", strings.TrimPrefix(err.Error(), "share: "))
		}
		t.NFS = &m
	case t.S3 != nil:
		m := *t.S3
		host := m.Endpoint
		if strings.Contains(host, "://") || strings.ContainsAny(host, "/?#@ ") || host == "" {
			return nil, invalid("An S3 endpoint is a host name with an optional port, such as s3.eu-central-1.amazonaws.com.")
		}
		if !bucketPattern.MatchString(m.Bucket) {
			return nil, invalid("Invalid S3 bucket name.")
		}
		if !regionPattern.MatchString(m.Region) {
			return nil, invalid("Invalid S3 region.")
		}
		if m.Prefix = strings.Trim(m.Prefix, "/"); !prefixPattern.MatchString(m.Prefix) || strings.Contains(m.Prefix, "//") || slices.Contains(strings.Split(m.Prefix, "/"), "..") {
			return nil, invalid("Invalid S3 prefix.")
		}
		if m.AccessKeyID == "" || m.SecretAccessKey == "" {
			return nil, invalid("An S3 target needs an access key id and a secret key.")
		}
		t.S3 = &m
	case t.WebDAV != nil:
		m := *t.WebDAV
		u, err := url.Parse(m.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, invalid("A WebDAV target needs an http(s) URL.")
		}
		// Credentials go in their own fields, which lists leave out; in
		// the URL they would reach every place the target is named.
		if u.User != nil {
			return nil, invalid("Put the WebDAV user name and password in their own fields, not in the URL.")
		}
		u.Path = strings.TrimSuffix(path.Clean("/"+u.Path), "/")
		u.RawPath = ""
		m.URL = u.String()
		t.WebDAV = &m
	}
	return t, nil
}

// place is what makes two targets the same storage. Two records for one
// place would hold two owner counters for one server directory, and each
// would read the other's writes as a clone.
func (t *Target) place() string {
	switch {
	case t.Disk != nil:
		return "disk:" + t.Disk.Path
	case t.SMB != nil:
		return "smb:" + strings.ToLower(t.SMB.Addr) + "/" + strings.ToLower(t.SMB.Share) + t.SMB.Path
	case t.NFS != nil:
		return "nfs:" + strings.ToLower(t.NFS.Addr) + ":" + t.NFS.Export + t.NFS.Path
	case t.S3 != nil:
		return "s3:" + strings.ToLower(t.S3.Endpoint) + "/" + t.S3.Bucket + "/" + t.S3.Prefix
	case t.WebDAV != nil:
		return "webdav:" + strings.ToLower(t.WebDAV.URL)
	}
	return ""
}

// host is the machine a network target lives on.
func (t *Target) host() string {
	switch {
	case t.SMB != nil:
		return t.SMB.Addr
	case t.NFS != nil:
		return t.NFS.Addr
	case t.S3 != nil:
		if h, _, err := net.SplitHostPort(t.S3.Endpoint); err == nil {
			return h
		}
		return t.S3.Endpoint
	case t.WebDAV != nil:
		if u, err := url.Parse(t.WebDAV.URL); err == nil {
			return u.Hostname()
		}
	}
	return ""
}

func (s *Settings) locate(ctx context.Context, t *Target) string {
	if t.Disk != nil || s.cfg.Locate.IsLocal(ctx, strings.Trim(t.host(), "[]")) {
		return Local
	}
	return Remote
}

func checkUnique(m *module, id string, t *Target) error {
	for other, o := range m.Targets {
		if other == id {
			continue
		}
		if o.place() == t.place() {
			return errorf(CodeExists, "Target %s already uses this storage.", o.Name)
		}
		if strings.EqualFold(o.Name, t.Name) {
			return errorf(CodeExists, "A target named %s already exists.", o.Name)
		}
	}
	return nil
}

// Targets lists the targets by name.
func (s *Settings) Targets() ([]TargetView, error) {
	m, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]TargetView, 0, len(m.Targets))
	for id, t := range m.Targets {
		out = append(out, s.view(id, t))
	}
	slices.SortFunc(out, func(a, b TargetView) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.ID, b.ID))
	})
	return out, nil
}

// Target returns one target.
func (s *Settings) Target(id string) (TargetView, error) {
	m, err := s.read()
	if err != nil {
		return TargetView{}, err
	}
	t := m.Targets[id]
	if t == nil {
		return TargetView{}, errorf(CodeNotFound, "Backup target %s not found.", id)
	}
	return s.view(id, t), nil
}

// UpdateTarget renames a target or replaces its credentials. Where the
// target is cannot change, since its backups are there: that is a new
// target. Empty credentials keep the saved ones. New credentials are saved
// only once the target opened with them.
func (s *Settings) UpdateTarget(ctx context.Context, id string, spec TargetSpec) (TargetView, error) {
	m, err := s.read()
	if err != nil {
		return TargetView{}, err
	}
	cur := m.Targets[id]
	if cur == nil {
		return TargetView{}, errorf(CodeNotFound, "Backup target %s not found.", id)
	}
	if spec.Key != "" {
		return TargetView{}, invalid("A target's key cannot be changed.")
	}
	if strings.TrimSpace(spec.Name) == "" {
		spec.Name = cur.Name
	}
	credentialsChanged := mergeSecrets(&spec, cur)
	next, err := s.normalize(spec)
	if err != nil {
		return TargetView{}, err
	}
	if next.place() != cur.place() || next.kind() != cur.kind() {
		return TargetView{}, invalid("Where a target is cannot change, because its backups are there. Add another target instead.")
	}
	next.Key, next.KeySource, next.Location, next.SameDisk, next.Created = cur.Key, cur.KeySource, cur.Location, cur.SameDisk, cur.Created
	if credentialsChanged {
		if _, err := s.initTarget(ctx, id, next, false); err != nil {
			return TargetView{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.update(func(m *module) error {
		if m.Targets[id] == nil {
			return errorf(CodeNotFound, "Backup target %s not found.", id)
		}
		if err := checkUnique(m, id, next); err != nil {
			return err
		}
		m.Targets[id] = next
		return nil
	})
	if err != nil {
		return TargetView{}, err
	}
	s.cfg.Log.Log("Updated backup target %s", next.Name)
	return s.view(id, next), nil
}

// mergeSecrets fills the secrets a change left empty from the saved
// target, and reports whether any credential changed.
func mergeSecrets(spec *TargetSpec, cur *Target) bool {
	changed := false
	keep := func(dst *string, saved string) {
		if *dst == "" {
			*dst = saved
		} else if *dst != saved {
			changed = true
		}
	}
	switch {
	case spec.SMB != nil && cur.SMB != nil:
		c := *spec.SMB
		keep(&c.Password, cur.SMB.Password)
		changed = changed || c.Username != cur.SMB.Username || c.Domain != cur.SMB.Domain || c.Version != cur.SMB.Version
		spec.SMB = &c
	case spec.S3 != nil && cur.S3 != nil:
		c := *spec.S3
		keep(&c.SecretAccessKey, cur.S3.SecretAccessKey)
		changed = changed || c.AccessKeyID != cur.S3.AccessKeyID || c.Region != cur.S3.Region || c.DisableTLS != cur.S3.DisableTLS
		spec.S3 = &c
	case spec.WebDAV != nil && cur.WebDAV != nil:
		c := *spec.WebDAV
		keep(&c.Password, cur.WebDAV.Password)
		changed = changed || c.Username != cur.WebDAV.Username
		spec.WebDAV = &c
	case spec.NFS != nil && cur.NFS != nil:
		changed = spec.NFS.Version != cur.NFS.Version
	}
	return changed
}

// DeleteTarget removes a target. The backups on it stay where they are:
// the target is the operator's storage, and adding it again (with its
// key) brings them back. A target that a job uses, or whose deleted job
// still has backups waiting to be deleted, stays.
func (s *Settings) DeleteTarget(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var name string
	err := s.update(func(m *module) error {
		t := m.Targets[id]
		if t == nil {
			return errorf(CodeNotFound, "Backup target %s not found.", id)
		}
		for _, j := range m.Jobs {
			if j.Target == id {
				return errorf(CodeInUse, "Job %s backs up to this target. Delete the job or move its apps to another job first.", j.Name)
			}
		}
		for _, r := range m.Retired {
			if r.Target == id && r.Purge {
				return errorf(CodeInUse, "The backups of deleted job %s are still being deleted from this target.", r.Name)
			}
		}
		for rid, r := range m.Retired {
			if r.Target == id {
				delete(m.Retired, rid)
			}
		}
		name = t.Name
		delete(m.Targets, id)
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.owners.forget(id); err != nil {
		s.cfg.Log.Error("Failed to remove the owner state of backup target %s: %s", name, err.Error())
	}
	s.cfg.Log.Log("Deleted backup target %s", name)
	return nil
}

// Access is what a run needs to open a target's repository.
type Access struct {
	// Repo has everything but the cache, which the manager sets.
	Repo proto.Repo
	// Target is where a disk, SMB or NFS target is mounted from; nil for
	// S3 and WebDAV, which the helper reaches over the network.
	Target *runner.Target
}

// AccessOptions selects how a target is opened.
type AccessOptions struct {
	// Write claims this server's directory for a run that writes (init,
	// backup, delete, maintain).
	Write bool
	// Server opens another server's directory read-only, for listing and
	// restoring its backups; empty is this server.
	Server string
}

// Access returns what a run needs to open the target's repository. A
// write claims the next owner sequence number, persisted before it is
// returned, so a crash cannot make the server send a number twice. Writes
// are refused while a conflict is recorded: see Adopt.
func (s *Settings) Access(id string, opts AccessOptions) (Access, error) {
	m, err := s.read()
	if err != nil {
		return Access{}, err
	}
	t := m.Targets[id]
	if t == nil {
		return Access{}, errorf(CodeNotFound, "Backup target %s not found.", id)
	}
	server := s.cfg.ServerID
	if opts.Server != "" && opts.Server != server {
		if opts.Write {
			return Access{}, invalid("Another server's backups are opened read-only.")
		}
		if !serverIDPattern.MatchString(opts.Server) {
			return Access{}, invalid("Invalid server id.")
		}
		server = opts.Server
	}
	a := access(t, server, s.cfg.ServerID)
	a.Repo.ReadOnly = server != s.cfg.ServerID
	if opts.Write {
		owner, err := s.claim(id, false)
		if err != nil {
			return Access{}, err
		}
		a.Repo.Owner = owner
	}
	return a, nil
}

var serverIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *Settings) claim(id string, adopt bool) (*proto.Owner, error) {
	if !adopt {
		if c := s.owners.conflict(id); c != nil {
			return nil, errorf(CodeOwnerConflict, "Writes to this target are stopped: %s", c.Message)
		}
	}
	seq, err := s.owners.next(id, adopt)
	if err != nil {
		return nil, fmt.Errorf("settings: owner state: %w", err)
	}
	return &proto.Owner{Name: s.cfg.ServerName(), Seq: seq, Adopt: adopt}, nil
}

// access lays the server directory out on t: <target>/<server id>/
// repository, with the ownership file next to the repository.
func access(t *Target, server, self string) Access {
	dir := server + "/" + proto.RepositoryDir
	a := Access{Repo: proto.Repo{Password: t.Key, ServerID: self}}
	st := &a.Repo.Storage
	switch {
	case t.Disk != nil:
		a.Target = &runner.Target{Root: t.Disk.Path}
		st.Filesystem = &proto.FilesystemStorage{Path: "/" + dir}
	case t.SMB != nil:
		m := t.SMB
		a.Target = &runner.Target{Share: &docker.Share{Type: "cifs", Addr: m.Addr, Path: m.Share, Username: m.Username, Password: m.Password, Domain: m.Domain, Version: m.Version}}
		st.Filesystem = &proto.FilesystemStorage{Path: path.Join(m.Path, dir)}
	case t.NFS != nil:
		m := t.NFS
		a.Target = &runner.Target{Share: &docker.Share{Type: "nfs", Addr: m.Addr, Path: m.Export, Version: m.Version}}
		st.Filesystem = &proto.FilesystemStorage{Path: path.Join(m.Path, dir)}
	case t.S3 != nil:
		m := t.S3
		prefix := dir + "/"
		if m.Prefix != "" {
			prefix = m.Prefix + "/" + prefix
		}
		st.S3 = &proto.S3Storage{Endpoint: m.Endpoint, Region: m.Region, Bucket: m.Bucket, Prefix: prefix, AccessKeyID: m.AccessKeyID, SecretAccessKey: m.SecretAccessKey, DisableTLS: m.DisableTLS}
	case t.WebDAV != nil:
		m := t.WebDAV
		st.WebDAV = &proto.WebDAVStorage{URL: m.URL + "/" + dir, Username: m.Username, Password: m.Password}
	}
	return a
}

// Observe records what a run's failure says about its target. The manager
// passes every failed run here: an owner conflict stops writes to the
// target until the operator adopts it.
func (s *Settings) Observe(id string, err error) {
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeOwnerConflict {
		return
	}
	if serr := s.owners.setConflict(id, pe.Message, s.nowMs()); serr != nil {
		s.cfg.Log.Error("Failed to record the owner conflict on backup target %s: %s", id, serr.Error())
	}
	s.cfg.Log.Warn("Stopped writing to backup target %s: another machine writes under this server's id there (%s)", id, pe.Message)
}

// Adopt takes this server's directory on the target over whatever it
// holds and lets writes go on. It is the operator's answer to a conflict
// once they know where it came from: the target was added again, or this
// server's state was restored from an older copy. A clone of this server
// that still writes there has to be stopped or given a new id first, or
// the conflict comes back on its next write.
func (s *Settings) Adopt(ctx context.Context, id string) (TargetView, error) {
	m, err := s.read()
	if err != nil {
		return TargetView{}, err
	}
	t := m.Targets[id]
	if t == nil {
		return TargetView{}, errorf(CodeNotFound, "Backup target %s not found.", id)
	}
	if _, err := s.initTarget(ctx, id, t, true); err != nil {
		return TargetView{}, err
	}
	s.cfg.Log.Log("Adopted this server's directory on backup target %s", t.Name)
	return s.view(id, t), nil
}

// initTarget runs init on t, claiming the server directory, and records
// the sequence number an adopt wrote. It holds no lock: init can take as
// long as the storage does.
func (s *Settings) initTarget(ctx context.Context, id string, t *Target, adopt bool) (*proto.Result, error) {
	owner, err := s.claim(id, adopt)
	if err != nil {
		return nil, err
	}
	a := access(t, s.cfg.ServerID, s.cfg.ServerID)
	a.Repo.Owner = owner
	res, err := s.cfg.Init.Init(ctx, a)
	if err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) {
			switch pe.Code {
			case proto.CodeOwnerConflict:
				return nil, errorf(CodeOwnerConflict, "This target already holds backups written under this server's id (%s). If this server wrote them, adopt the directory; if a copy of this server writes there, stop it or give it a new id first.", pe.Message)
			case proto.CodeWrongPassword:
				return nil, errorf(CodeUnreachable, "The target holds a repository for this server that the key does not open.")
			}
		}
		return nil, errorf(CodeUnreachable, "The target could not be opened: %s", maskSecrets(err.Error(), t))
	}
	if res == nil {
		res = &proto.Result{}
	}
	if adopt || res.Seq > owner.Seq {
		if err := s.owners.adopted(id, max(res.Seq, owner.Seq)); err != nil {
			return nil, fmt.Errorf("settings: owner state: %w", err)
		}
	}
	return res, nil
}

// maskSecrets keeps a secret out of an error that came back from a run.
// The helper masks its own messages; this covers what the runner adds.
func maskSecrets(msg string, t *Target) string {
	for _, secret := range t.secrets() {
		if len(secret) >= 4 {
			msg = strings.ReplaceAll(msg, secret, "***")
		}
	}
	return msg
}

func (t *Target) secrets() []string {
	out := []string{t.Key}
	switch {
	case t.SMB != nil:
		out = append(out, t.SMB.Password)
	case t.S3 != nil:
		out = append(out, t.S3.SecretAccessKey)
	case t.WebDAV != nil:
		out = append(out, t.WebDAV.Password)
	}
	return out
}
