package docker

import (
	"context"
	"regexp"
	"strings"
	"sync"

	"github.com/distribution/reference"
)

// imageIDRe is the shape of an image ID and of a registry digest, the only
// identity values the agent reports.
var imageIDRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// maxCachedImages bounds the image info cache. List drops the IDs no app
// runs any more (RetainImageInfo), and this caps what accumulates between
// two lists: a full cache is dropped whole rather than allowed to grow with
// the server's history, and refilling it costs one inspect per live image.
const maxCachedImages = 512

// imageInfo is what app.list reports about one image, read from a single
// inspect. The version meta is filtered before it is stored, so the cache
// never holds an image's full label set or environment.
type imageInfo struct {
	digests []string
	meta    ImageVersionMeta
}

// imageInfoCache holds imageInfo per image ID. An ID names immutable
// content, so an entry never needs refreshing, and List can ask for every
// app on every run without an image inspect per row.
type imageInfoCache struct {
	mu   sync.Mutex
	byID map[string]imageInfo
}

func (c *imageInfoCache) get(id string) (imageInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	info, ok := c.byID[id]
	return info, ok
}

func (c *imageInfoCache) put(id string, info imageInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil || len(c.byID) >= maxCachedImages {
		c.byID = map[string]imageInfo{}
	}
	info.digests = append([]string(nil), info.digests...)
	c.byID[id] = info
}

// retain drops every entry whose ID is not in inUse.
func (c *imageInfoCache) retain(inUse map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.byID {
		if !inUse[id] {
			delete(c.byID, id)
		}
	}
}

// imageInfo returns the cached info of imageID, inspecting it on a miss.
// A failed inspect is not cached: a missing image or a stalled daemon is no
// answer, and the next list asks again.
func (c *Client) imageInfo(imageID string) (imageInfo, bool) {
	if !c.available || !imageIDRe.MatchString(imageID) {
		return imageInfo{}, false
	}
	if info, ok := c.images.get(imageID); ok {
		return info, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	data, _, err := c.api.ImageInspectWithRaw(ctx, imageID)
	cancel()
	if err != nil {
		return imageInfo{}, false
	}
	var labels map[string]string
	var env []string
	if data.Config != nil {
		labels, env = data.Config.Labels, data.Config.Env
	}
	info := imageInfo{digests: data.RepoDigests, meta: declaredVersionMeta(labels, env)}
	c.images.put(imageID, info)
	return info, true
}

// RetainImageInfo forgets the cached info of every image ID not in inUse.
// app.list calls it with the IDs its containers run, so the images a server
// rebuilt or pulled past do not stay cached for the life of the process.
func (c *Client) RetainImageInfo(inUse map[string]bool) {
	c.images.retain(inUse)
}

// ImageDigest returns the registry digest (sha256:<hex>) under which image
// imageID was pulled from ref's repository, "" when it has none: a locally
// built image, one loaded from an archive, or a ref the image was never
// pulled as. Only the digest of the app's own repository counts, because an
// image pulled under two names carries a digest for each and the other one
// says nothing about where this app came from.
func (c *Client) ImageDigest(imageID, ref string) string {
	if ref == "" {
		return ""
	}
	info, ok := c.imageInfo(imageID)
	if !ok {
		return ""
	}
	return matchRepoDigest(info.digests, ref)
}

// ImageVersionMeta returns what image imageID declares about its own
// version (see ImageVersionMeta), false when the image could not be read.
// It shares ImageDigest's cache entry, so a list asking for both costs one
// inspect per image ID.
func (c *Client) ImageVersionMeta(imageID string) (ImageVersionMeta, bool) {
	info, ok := c.imageInfo(imageID)
	if !ok {
		return ImageVersionMeta{}, false
	}
	return info.meta, true
}

// ParseImageRef parses ref as a normalized image reference with
// case-insensitive registry domain matching. distribution/reference preserves
// uppercase letters in the domain component (and only collapses docker.io and
// index.docker.io when lowercase), so without lowercasing the host first,
// GHCR.IO/org/app and ghcr.io/org/app compare as different repositories.
// Repository path and tag case stay untouched so uppercase repository paths
// remain rejected and case-sensitive tags stay exact.
func ParseImageRef(ref string) (reference.Named, error) {
	if domain, rest, ok := strings.Cut(ref, "/"); ok && (strings.ContainsAny(domain, ".:") || strings.EqualFold(domain, "localhost")) {
		if lower := strings.ToLower(domain); lower != domain {
			ref = lower + "/" + rest
		}
	}
	return reference.ParseNormalizedNamed(ref)
}

// matchRepoDigest picks the digest of ref's repository out of an image's
// RepoDigests. Both sides are normalized, so `postgres`,
// `docker.io/library/postgres` and `index.docker.io/library/postgres` are
// one repository, and registry hostnames match case-insensitively.
func matchRepoDigest(repoDigests []string, ref string) string {
	named, err := ParseImageRef(ref)
	if err != nil {
		return ""
	}
	want := named.Name()
	for _, entry := range repoDigests {
		parsed, err := ParseImageRef(entry)
		if err != nil || parsed.Name() != want {
			continue
		}
		canonical, ok := parsed.(reference.Canonical)
		if !ok {
			continue
		}
		if digest := canonical.Digest().String(); imageIDRe.MatchString(digest) {
			return digest
		}
	}
	return ""
}
