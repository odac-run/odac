// Package resources is the single source of truth for the resource sizing an
// app may ask for. Today that is one knob, `shmSize`: the size of the
// container's /dev/shm. Tomorrow it is where a memory or CPU limit belongs,
// which is why it is its own vocabulary rather than a third field on the
// kernel request: caps and sysctls are an allowlist question (may this
// container hold that slice of root?), a size is a bounds question.
//
// It is deliberately dependency-free, like gpu, netmode and kernel: appmgr
// validates and persists a request with it, docker translates it into
// container HostConfig. One vocabulary, two consumers, no drift.
//
// The default /dev/shm is 64 MiB, which is generous for a web app and far
// too small for anything that moves frames between processes through shared
// memory. Frigate is the clearest case: its detector and its camera decoders
// hand each other raw frames through /dev/shm, so a handful of cameras
// exhausts the default and the app dies mid-stream with an error that names
// no cause. Sizing it is the whole fix.
//
// The value is bounded rather than allowlisted, and the ceiling matters for
// the same reason the kernel allowlist does: /dev/shm is a tmpfs the host's
// RAM backs, so a container that fills an oversized one takes memory from
// the host, not from a limit of its own.
package resources

import (
	"fmt"
	"strconv"
	"strings"
)

// Shared-memory bounds. The floor is a unit-mistake guard: a bare `512`
// means 512 bytes, and nobody wants a half-kilobyte /dev/shm — they meant
// 512 MiB, and a refusal at create says so where a 512-byte tmpfs would only
// surface as an unexplained crash later. The ceiling is a sanity bound on an
// untrusted payload: no workload needs 64 GiB of shared memory, and the
// pages come out of the host's RAM.
const (
	MinShmSize = int64(1) << 20
	MaxShmSize = int64(64) << 30
)

// DefaultShmSize is the engine's own default, documented here because it is
// the number a request is judged against: asking for exactly this is asking
// for nothing.
const DefaultShmSize = int64(64) << 20

// Spec is a validated resource request attached to an app. A nil Spec means
// "nothing requested", which is what almost every app is: it must translate
// to the exact same container config as before this field existed.
type Spec struct {
	// ShmSize is the /dev/shm size in bytes, 0 when unrequested.
	ShmSize int64
}

// Parse validates the `shmSize` member of an app definition (an app.create
// payload, a recipe, a template sub-app, or a persisted app record). A
// definition declaring none yields (nil, nil): the app is untouched by this
// feature and its container config must stay identical.
//
// Anything present but malformed IS an error. Silently dropping the size
// produces a container that starts, runs, and then dies under load in the
// one way this field exists to prevent.
func Parse(def any) (*Spec, error) {
	raw, ok := def.(map[string]any)
	if !ok || raw == nil {
		return nil, nil
	}
	size, err := parseShmSize(raw["shmSize"])
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	return &Spec{ShmSize: size}, nil
}

// parseShmSize accepts both shapes the value crosses a JSON boundary in: a
// number of bytes (what ODAC persists and the dashboard round-trips) and a
// human string with a unit ("512m", "1.5 GiB"), which is what an operator
// writes and what `docker run --shm-size` takes.
func parseShmSize(v any) (int64, error) {
	switch value := v.(type) {
	case nil:
		return 0, nil
	case float64:
		return validShmSize(value, value == float64(int64(value)))
	case int:
		return validShmSize(float64(value), true)
	case string:
		text := strings.TrimSpace(value)
		if text == "" {
			return 0, nil
		}
		bytes, ok := parseSize(text)
		return validShmSize(bytes, ok)
	}
	return 0, fmt.Errorf("invalid shmSize: expected a byte count or a size like \"512m\"")
}

// parseSize reads a decimal size with an optional unit suffix. Both the
// decimal and the binary spellings map to powers of 1024, the way container
// tooling has always read them: `--shm-size=1g` is 1 GiB, not 10^9 bytes.
func parseSize(text string) (float64, bool) {
	lower := strings.ToLower(text)
	unit := int64(1)
	for _, suffix := range []struct {
		name  string
		scale int64
	}{
		{"kib", 1 << 10}, {"kb", 1 << 10}, {"k", 1 << 10},
		{"mib", 1 << 20}, {"mb", 1 << 20}, {"m", 1 << 20},
		{"gib", 1 << 30}, {"gb", 1 << 30}, {"g", 1 << 30},
		{"b", 1},
	} {
		if strings.HasSuffix(lower, suffix.name) {
			unit = suffix.scale
			lower = strings.TrimSuffix(lower, suffix.name)
			break
		}
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(lower), 64)
	if err != nil {
		return 0, false
	}
	return number * float64(unit), true
}

// validShmSize bounds a parsed size. It rejects a fractional byte count for
// the same reason it rejects a bad unit: the caller meant something, and
// guessing which way to round is not this package's job.
func validShmSize(bytes float64, ok bool) (int64, error) {
	if !ok || bytes != float64(int64(bytes)) {
		return 0, fmt.Errorf("invalid shmSize: expected a byte count or a size like \"512m\"")
	}
	size := int64(bytes)
	switch {
	case size == 0:
		return 0, nil
	case size < MinShmSize:
		return 0, fmt.Errorf("shmSize %s is too small, the minimum is %s (a bare number is bytes: write \"512m\" for 512 MiB)", FormatSize(size), FormatSize(MinShmSize))
	case size > MaxShmSize:
		return 0, fmt.Errorf("shmSize %s is too large, the maximum is %s", FormatSize(size), FormatSize(MaxShmSize))
	}
	return size, nil
}

// Apply stamps the spec onto an app record, and clears the field when there
// is nothing to stamp. Absent means "no request": an app that never asked
// for shared memory must not grow a `shmSize: 0`, because the Cloud compares
// app.list rows by serialization and a cosmetic field rewrites the column
// for every app on every server.
func (s *Spec) Apply(app map[string]any) {
	if s == nil || s.ShmSize <= 0 {
		delete(app, "shmSize")
		return
	}
	app["shmSize"] = float64(s.ShmSize)
}

// Shm returns the requested /dev/shm size, 0 for a nil spec so callers can
// ask without a nil check.
func (s *Spec) Shm() int64 {
	if s == nil {
		return 0
	}
	return s.ShmSize
}

// FormatSize renders a byte count the way an operator wrote it, for log
// lines and error messages.
func FormatSize(bytes int64) string {
	switch {
	case bytes >= 1<<30 && bytes%(1<<30) == 0:
		return strconv.FormatInt(bytes/(1<<30), 10) + "GiB"
	case bytes >= 1<<20 && bytes%(1<<20) == 0:
		return strconv.FormatInt(bytes/(1<<20), 10) + "MiB"
	case bytes >= 1<<10 && bytes%(1<<10) == 0:
		return strconv.FormatInt(bytes/(1<<10), 10) + "KiB"
	}
	return strconv.FormatInt(bytes, 10) + "B"
}
