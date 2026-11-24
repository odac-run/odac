package docker

import (
	"context"
	"errors"
	"slices"

	"github.com/docker/docker/client"
)

// errUnavailable answers the image-change calls when the daemon never
// answered the construction-time ping.
var errUnavailable = errors.New("Docker is not available")

// ImageMeta is what an image change needs to know about a local image.
type ImageMeta struct {
	// ID is the image ID (sha256:<hex>) the reference resolves to locally.
	ID string
	// Healthcheck is true when the image defines a HEALTHCHECK, which turns
	// the health gate's "healthy" requirement on.
	Healthcheck bool
}

// ContainerState is the slice of a container's inspect the image change's
// health gate reads.
type ContainerState struct {
	// Exists is false when the daemon answered 404: the container is gone.
	Exists    bool
	Running   bool
	Restarts  int
	OOMKilled bool
	// Health is the engine's health status (starting, healthy, unhealthy),
	// empty when the container has no healthcheck.
	Health  string
	ImageID string
}

// PullImage fetches ref for a deliberate image change. Unlike EnsureImage it
// takes the caller's context, so the change's time budget bounds a pull that
// a slow registry would otherwise hold forever, and force re-pulls a tag that
// is already local: a rolling tag such as postgres:17 only moves when asked.
// Unlike RefreshImage a failed forced pull is an error, because the operator
// asked for the registry's copy, not for whatever happens to be cached.
func (c *Client) PullImage(ctx context.Context, ref string, force bool) error {
	if !c.available {
		return errUnavailable
	}
	if !force {
		if _, _, err := c.api.ImageInspectWithRaw(ctx, ref); err == nil {
			return nil
		}
	}
	c.log.Log("Pulling image %s...", ref)
	if err := c.pull(ctx, ref, nil); err != nil {
		return c.pullFailed(ref, nil, err)
	}
	c.log.Log("Image %s pulled successfully.", ref)
	return nil
}

// ImageMeta inspects a local image.
func (c *Client) ImageMeta(ref string) (ImageMeta, error) {
	if !c.available {
		return ImageMeta{}, errUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	data, _, err := c.api.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		return ImageMeta{}, err
	}
	meta := ImageMeta{}
	if imageIDRe.MatchString(data.ID) {
		meta.ID = data.ID
	}
	if data.Config != nil && data.Config.Healthcheck != nil {
		test := data.Config.Healthcheck.Test
		meta.Healthcheck = len(test) > 0 && test[0] != "NONE"
	}
	return meta, nil
}

// TagImage points target at source (an image ID or a reference), the way
// `docker tag` does. An image change uses it to keep the previous image
// named while it may still be needed, and to point the app's own reference
// back at it on a rollback.
func (c *Client) TagImage(source, target string) error {
	if !c.available {
		return errUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return c.api.ImageTag(ctx, source, target)
}

// InspectState reads what the health gate needs off one inspect. A missing
// container is an answer (Exists false), a failed probe is an error, so a
// stalled daemon is never read as a dead container.
func (c *Client) InspectState(name string) (ContainerState, error) {
	if !c.available {
		return ContainerState{}, errUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	data, err := c.api.ContainerInspect(ctx, name)
	if err != nil {
		if client.IsErrNotFound(err) {
			return ContainerState{}, nil
		}
		return ContainerState{}, err
	}
	st := ContainerState{Exists: true, Restarts: data.RestartCount}
	if imageIDRe.MatchString(data.Image) {
		st.ImageID = data.Image
	}
	if data.State != nil {
		st.Running = data.State.Running
		st.OOMKilled = data.State.OOMKilled
		if data.State.Health != nil && slices.Contains(healthStatuses, data.State.Health.Status) {
			st.Health = data.State.Health.Status
		}
	}
	return st, nil
}

// healthStatuses are the engine's health values the gate acts on; "none"
// and anything unknown read as "no healthcheck".
var healthStatuses = []string{"starting", "healthy", "unhealthy"}
