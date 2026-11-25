package docker

import (
	"context"
	"path/filepath"
	"time"

	"github.com/distribution/reference"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// imageListTimeout bounds one image list. It is longer than probeTimeout
// because the reply grows with the host's image count, and a list that times
// out is reported as unknown, never as "no images".
const imageListTimeout = 10 * time.Second

// ImageTag is one repository:tag reference of a local image.
type ImageTag struct {
	// Repository is the familiar repository name (odac-app-web).
	Repository string
	Tag        string
	// ImageID is the image (sha256:<hex>) the reference resolves to.
	ImageID string
}

// ImageTags lists the local references matching pattern, the engine's
// `reference` filter (a familiar-name glob such as odac-app-*), one entry
// per repository tag, in one round-trip. It exists so a caller that needs
// the tags of many repositories asks once rather than once per repository.
// A failed list is an error, never an empty slice, because "no tags" is an
// answer callers report and act on.
func (c *Client) ImageTags(pattern string) ([]ImageTag, error) {
	if !c.available {
		return nil, errUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), imageListTimeout)
	defer cancel()
	list, err := c.api.ImageList(ctx, image.ListOptions{Filters: filters.NewArgs(filters.Arg("reference", pattern))})
	if err != nil {
		return nil, err
	}
	out := []ImageTag{}
	for _, img := range list {
		if !imageIDRe.MatchString(img.ID) {
			continue
		}
		for _, ref := range img.RepoTags {
			named, err := reference.ParseNormalizedNamed(ref)
			if err != nil {
				continue
			}
			tagged, ok := named.(reference.Tagged)
			if !ok {
				continue
			}
			out = append(out, ImageTag{Repository: reference.FamiliarName(named), Tag: tagged.Tag(), ImageID: img.ID})
		}
	}
	return out, nil
}

// UntagImage removes one reference without force: a tag that is the last
// name of an image a container still uses is refused by the engine and
// stays, so retention can never take the image a container runs. When the
// reference was the image's last name and nothing uses it, the image goes
// with it. A missing reference is not an error.
func (c *Client) UntagImage(ref string) error {
	if !c.available {
		return errUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if _, err := c.api.ImageRemove(ctx, ref, image.RemoveOptions{PruneChildren: true}); err != nil && !client.IsErrNotFound(err) {
		return err
	}
	return nil
}

// dataRootViews are the places the engine's data root may be visible from
// ODAC: through the host's root when ODAC runs with `pid: host` (the shipped
// compose profile), through an explicit /host bind mount, and as is when
// ODAC runs on the host itself. A test may replace it.
var dataRootViews = []string{"/proc/1/root", "/host", ""}

// DataRootSpace reports the available and total bytes of the filesystem
// holding the engine's data root (`docker info` DockerRootDir), which is
// where every image layer lands. ok is false when that directory cannot be
// reached from here: the path is never walked up to an ancestor, because an
// ancestor on another filesystem would report another disk's space.
func (c *Client) DataRootSpace() (free, total uint64, ok bool) {
	root := c.dataRoot()
	if root == "" {
		return 0, 0, false
	}
	for _, view := range dataRootViews {
		if free, total, ok := statfsBytes(filepath.Join(view, root)); ok && total > 0 {
			return free, total, true
		}
	}
	return 0, 0, false
}

// dataRoot reads DockerRootDir once; a failed or odd answer is retried on
// the next call rather than cached.
func (c *Client) dataRoot() string {
	if !c.available {
		return ""
	}
	c.mu.Lock()
	root := c.dockerRoot
	c.mu.Unlock()
	if root != "" {
		return root
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	info, err := c.api.Info(ctx)
	if err != nil || !filepath.IsAbs(info.DockerRootDir) {
		return ""
	}
	root = filepath.Clean(info.DockerRootDir)
	c.mu.Lock()
	c.dockerRoot = root
	c.mu.Unlock()
	return root
}
