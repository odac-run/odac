package swap

import "strings"

// controller is the actuator seam: everything that actually mutates host state
// (allocate/swapon/swapoff/rm, fstab). It is an interface so the Manager and
// decide loop test against a fake, and so non-Linux builds get a no-op. The
// Linux implementation lives in swap_linux.go; the pure fstab block logic below
// is platform-independent and unit tested directly.
type controller interface {
	// open allocates a swapfile of size bytes at path, then makes it live
	// (mkswap + swapon). Must be idempotent-safe: only called for paths not
	// already active.
	open(path string, size int64) error
	// remove takes a swapfile offline (swapoff) and deletes it.
	remove(path string) error
	// syncFstab rewrites the odac-managed block of /etc/fstab to exactly paths
	// (empty clears it), leaving the user's own lines untouched.
	syncFstab(paths []string) error
}

const (
	fstabBegin = "# >>> odac-managed swap (do not edit) >>>"
	fstabEnd   = "# <<< odac-managed swap <<<"
	// fstabOpts: nofail is the critical safety net — if a swapfile vanishes,
	// the box must still boot rather than drop to emergency mode.
	fstabOpts = "none swap sw,nofail 0 0"
)

// updateFstabBlock returns fstab content with the odac-managed block set to
// exactly paths. Any prior managed block is removed first, so the function is
// idempotent: applying it twice with the same paths yields the same result.
// The user's lines outside the markers are never touched.
func updateFstabBlock(existing string, paths []string) string {
	stripped := removeFstabBlock(existing)
	if len(paths) == 0 {
		return stripped
	}
	var b strings.Builder
	if trimmed := strings.TrimRight(stripped, "\n"); trimmed != "" {
		b.WriteString(trimmed)
		b.WriteByte('\n')
	}
	b.WriteString(fstabBegin)
	b.WriteByte('\n')
	for _, p := range paths {
		b.WriteString(p)
		b.WriteByte(' ')
		b.WriteString(fstabOpts)
		b.WriteByte('\n')
	}
	b.WriteString(fstabEnd)
	b.WriteByte('\n')
	return b.String()
}

// removeFstabBlock deletes the odac-managed block (markers inclusive) from s,
// leaving everything else intact. A block with a start marker but no end marker
// (hand-corrupted) is cut from the start marker to end-of-file.
func removeFstabBlock(s string) string {
	begin := strings.Index(s, fstabBegin)
	if begin == -1 {
		return s
	}
	after := ""
	if endIdx := strings.Index(s[begin:], fstabEnd); endIdx != -1 {
		end := begin + endIdx + len(fstabEnd)
		if end < len(s) && s[end] == '\n' {
			end++
		}
		after = s[end:]
	}
	before := s[:begin]
	// Drop the blank line the block used to sit on so repeated edits do not
	// accumulate whitespace.
	before = strings.TrimRight(before, "\n")
	if before != "" && after != "" {
		return before + "\n" + after
	}
	return before + after
}

// noopController is the non-Linux (and default-nil) actuator: it does nothing
// successfully, so decide's output is simply never enacted.
type noopController struct{}

func (noopController) open(string, int64) error { return nil }
func (noopController) remove(string) error      { return nil }
func (noopController) syncFstab([]string) error { return nil }
