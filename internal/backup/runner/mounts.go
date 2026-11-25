package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strconv"

	"odac/internal/backup/proto"
	"odac/internal/docker"
)

// Where a helper sees what a run mounts.
const (
	helperRoot    = "/backup"
	helperCache   = helperRoot + "/cache"
	helperTarget  = helperRoot + "/target"
	helperSources = helperRoot + "/src"
	helperRestore = helperRoot + "/restore"
)

// Capabilities by need; see docker.HelperSpec.Caps.
var (
	// Every run writes a target or cache the operator may own as another
	// user.
	baseCaps = []string{"DAC_OVERRIDE"}
	// A backup reads app data owned by any user.
	backupCaps = []string{"DAC_OVERRIDE", "DAC_READ_SEARCH"}
	// A restore gives files back their owners and modes.
	restoreCaps = []string{"CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "FSETID"}
)

// followable lists the operations worth handing to the next server: the
// long ones whose result the server records. The rest are quick, or
// (a stream restore) carry their output on stdout and must not log it.
var followable = map[string]bool{proto.OpBackup: true, proto.OpMaintain: true, proto.OpVerify: true}

func cloneRequest(req *proto.Request) (*proto.Request, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var out proto.Request
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// inside joins a path taken inside a target to the target's root. A clean
// absolute path cannot climb above "/", so it stays under root.
func inside(root, p string) (string, error) {
	if !path.IsAbs(p) {
		return "", fmt.Errorf("runner: repository path %q must be absolute inside its target", p)
	}
	return path.Join(root, path.Clean(p)), nil
}

func absolute(what, p string) error {
	if !filepath.IsAbs(p) {
		return fmt.Errorf("runner: %s %q is not an absolute path", what, p)
	}
	return nil
}

// helperSpec mounts what the request names and rewrites its paths to
// where the helper sees them. Sources are read-only; only the cache, the
// target and a restore's destination are writable.
func helperSpec(inv Invocation) (*proto.Request, docker.HelperSpec, error) {
	var spec docker.HelperSpec
	req, err := cloneRequest(inv.Request)
	if err != nil {
		return nil, spec, err
	}
	if err := absolute("cache directory", req.Repo.CacheDir); err != nil {
		return nil, spec, err
	}
	spec.Mounts = append(spec.Mounts, docker.HelperMount{Source: req.Repo.CacheDir, Target: helperCache})
	req.Repo.CacheDir = helperCache

	st := req.Repo.Storage
	switch {
	case st.Filesystem != nil:
		p := st.Filesystem.Path
		switch t := inv.Target; {
		case t == nil:
			if err := absolute("repository", p); err != nil {
				return nil, spec, err
			}
			spec.Mounts = append(spec.Mounts, docker.HelperMount{Source: p, Target: helperTarget})
			st.Filesystem.Path = helperTarget
		case t.Share != nil:
			share := *t.Share
			share.Target = helperTarget
			spec.Share = &share
			if st.Filesystem.Path, err = inside(helperTarget, p); err != nil {
				return nil, spec, err
			}
		default:
			if !path.IsAbs(t.Root) || path.Clean(t.Root) == "/" {
				return nil, spec, fmt.Errorf("runner: target root %q", t.Root)
			}
			spec.Mounts = append(spec.Mounts, docker.HelperMount{Source: t.Root, HostSource: true, Target: helperTarget})
			if st.Filesystem.Path, err = inside(helperTarget, p); err != nil {
				return nil, spec, err
			}
		}
	case st.S3 != nil, st.WebDAV != nil:
		if inv.Target != nil {
			return nil, spec, errors.New("runner: a remote repository has no target to mount")
		}
		spec.Network = true
	}

	spec.Caps = baseCaps
	switch {
	case req.Backup != nil:
		spec.Caps = backupCaps
		for i := range req.Backup.Items {
			it := &req.Backup.Items[i]
			if it.Dir == "" {
				continue
			}
			if err := absolute("source", it.Dir); err != nil {
				return nil, spec, err
			}
			target := helperSources + "/" + strconv.Itoa(i)
			spec.Mounts = append(spec.Mounts, docker.HelperMount{Source: it.Dir, Target: target, ReadOnly: true})
			it.Dir = target
		}
	case req.Restore != nil && req.Restore.Dir != "":
		spec.Caps = restoreCaps
		if err := absolute("restore destination", req.Restore.Dir); err != nil {
			return nil, spec, err
		}
		spec.Mounts = append(spec.Mounts, docker.HelperMount{Source: req.Restore.Dir, Target: helperRestore})
		req.Restore.Dir = helperRestore
	}
	spec.Follow = followable[req.Op]
	return req, spec, nil
}

// localRequest is the request for a child process, which sees this
// server's paths as they are. A share needs the daemon to mount it, so it
// has no local form.
func localRequest(inv Invocation) (*proto.Request, error) {
	req, err := cloneRequest(inv.Request)
	if err != nil {
		return nil, err
	}
	if t := inv.Target; t != nil && req.Repo.Storage.Filesystem != nil {
		if t.Share != nil {
			return nil, errors.New("runner: an SMB or NFS target needs ODAC in a container")
		}
		fs := req.Repo.Storage.Filesystem
		if fs.Path, err = inside(t.Root, fs.Path); err != nil {
			return nil, err
		}
	}
	return req, nil
}
