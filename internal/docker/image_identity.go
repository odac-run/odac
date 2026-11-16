package docker

import (
	"context"
	"regexp"
	"sync"

	"github.com/distribution/reference"
)

// imageIDRe is the shape of an image ID and of a registry digest, the only
// identity values the agent reports.
var imageIDRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// maxCachedImages bounds the RepoDigests cache. Every rebuild and pull adds
// an ID, so the cache is dropped whole rather than allowed to grow with the
// server's history; refilling it costs one inspect per live image.
const maxCachedImages = 512

// imageDigestCache holds RepoDigests per image ID. An ID names immutable
// content, so an entry never needs refreshing, and List can ask for every
// app on every run without an image inspect per row.
type imageDigestCache struct {
	mu   sync.Mutex
	byID map[string][]string
}

func (c *imageDigestCache) get(id string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	digests, ok := c.byID[id]
	return digests, ok
}

func (c *imageDigestCache) put(id string, digests []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil || len(c.byID) >= maxCachedImages {
		c.byID = map[string][]string{}
	}
	c.byID[id] = append([]string(nil), digests...)
}

// ImageDigest returns the registry digest (sha256:<hex>) under which image
// imageID was pulled from ref's repository, "" when it has none: a locally
// built image, one loaded from an archive, or a ref the image was never
// pulled as. Only the digest of the app's own repository counts, because an
// image pulled under two names carries a digest for each and the other one
// says nothing about where this app came from.
func (c *Client) ImageDigest(imageID, ref string) string {
	if !c.available || !imageIDRe.MatchString(imageID) || ref == "" {
		return ""
	}
	digests, ok := c.digests.get(imageID)
	if !ok {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		data, _, err := c.api.ImageInspectWithRaw(ctx, imageID)
		cancel()
		if err != nil {
			// Not cached: a missing image or a stalled daemon is no answer.
			return ""
		}
		digests = data.RepoDigests
		c.digests.put(imageID, digests)
	}
	return matchRepoDigest(digests, ref)
}

// matchRepoDigest picks the digest of ref's repository out of an image's
// RepoDigests. Both sides are normalized, so `postgres`,
// `docker.io/library/postgres` and `index.docker.io/library/postgres` are
// one repository.
func matchRepoDigest(repoDigests []string, ref string) string {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ""
	}
	want := named.Name()
	for _, entry := range repoDigests {
		parsed, err := reference.ParseNormalizedNamed(entry)
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
