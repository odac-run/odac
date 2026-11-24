package backup

import (
	"strconv"
	"strings"

	"github.com/distribution/reference"
)

// BlockedImageMismatch marks an app of an in-place restore whose restore
// point was taken on another repository or another major version of its
// image than the app runs now. Its data may be in a format the running
// version cannot read (postgres 17 data under postgres 18), so the restore
// is refused until the operator has changed the version back; the data and
// the image never come back in one step.
const BlockedImageMismatch = "image_mismatch"

// RestoreImage is an app's image at the restore point (from the definition
// stored with it) and now (its record), as the records name them.
type RestoreImage struct {
	Point   string `json:"point"`
	Current string `json:"current"`
}

// maxRestoreImageRef bounds a reported reference, the same bound an image
// change accepts.
const maxRestoreImageRef = 255

// compareImages decides whether data from an image may be restored into an
// app running another. It reports both references when each parses as an
// image reference, and blocks when the repositories differ (normalized, so
// postgres is docker.io/library/postgres) or when both tags have a major
// version and the majors differ. A patch or minor difference is not
// blocked: the data format holds within a major. A tag without a leading
// integer (latest, alpine, a digest only) has no major and blocks nothing;
// records carry no image ID or digest to compare instead, so such a pair is
// reported and left to the operator.
func compareImages(point, current string) (*RestoreImage, bool) {
	p, okP := parseImage(point)
	c, okC := parseImage(current)
	if !okP || !okC {
		return nil, false
	}
	img := &RestoreImage{Point: point, Current: current}
	if p.Name() != c.Name() {
		return img, true
	}
	pm, okP := imageMajor(p)
	cm, okC := imageMajor(c)
	return img, okP && okC && pm != cm
}

func parseImage(ref string) (reference.Named, bool) {
	if ref == "" || len(ref) > maxRestoreImageRef {
		return nil, false
	}
	n, err := reference.ParseNormalizedNamed(ref)
	return n, err == nil
}

// imageMajor is the leading integer of a reference's tag, after an optional
// "v": 17.6-alpine is 17, 18 is 18, v3.1 is 3. An untagged reference is
// latest, which has none.
func imageMajor(n reference.Named) (int, bool) {
	t, ok := n.(reference.Tagged)
	if !ok {
		return 0, false
	}
	tag := strings.TrimPrefix(strings.TrimPrefix(t.Tag(), "v"), "V")
	end := 0
	for end < len(tag) && end < 9 && tag[end] >= '0' && tag[end] <= '9' {
		end++
	}
	if end == 0 || end < len(tag) && tag[end] >= '0' && tag[end] <= '9' {
		return 0, false
	}
	v, err := strconv.Atoi(tag[:end])
	return v, err == nil
}

// definitionImage is the image a stored or live app record names.
func definitionImage(def map[string]any) string {
	img, _ := def["image"].(string)
	return img
}
