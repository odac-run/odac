package swap

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// newController returns the live Linux actuator.
func newController() controller { return linuxController{} }

// linuxController performs the real host mutations: fallocate/mkswap/swapon,
// swapoff/rm, and atomic fstab edits. Requires root (ODAC runs as root on the
// host). Not unit tested off-host — the fstab block logic it delegates to
// (updateFstabBlock) is tested purely; swapon/mkswap are covered by the manual
// Linux integration pass (SWAP_PLAN.md step 7).
type linuxController struct{}

func (linuxController) open(path string, size int64) error {
	// The swap directory may not exist yet (first grow, or a fresh <baseDir>/swap).
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir swap dir for %s: %w", path, err)
	}
	// fallocate is instant; fall back to dd (zero-fill) on filesystems that
	// reject it (e.g. some tmpfs/older setups).
	if err := run("fallocate", "-l", strconv.FormatInt(size, 10), path); err != nil {
		count := strconv.FormatInt(size/mib, 10)
		if derr := run("dd", "if=/dev/zero", "of="+path, "bs=1M", "count="+count); derr != nil {
			os.Remove(path) // don't leave a partial file behind
			return fmt.Errorf("allocate %s: fallocate: %v; dd fallback: %w", path, err, derr)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		os.Remove(path)
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := run("mkswap", path); err != nil {
		os.Remove(path)
		return fmt.Errorf("mkswap %s: %w", path, err)
	}
	if err := run("swapon", path); err != nil {
		os.Remove(path)
		return fmt.Errorf("swapon %s: %w", path, err)
	}
	return nil
}

func (linuxController) remove(path string) error {
	if err := run("swapoff", path); err != nil {
		return fmt.Errorf("swapoff %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rm %s: %w", path, err)
	}
	return nil
}

func (linuxController) syncFstab(paths []string) error {
	const fstab = "/etc/fstab"
	raw, err := os.ReadFile(fstab)
	if os.IsNotExist(err) {
		// No fstab to keep in sync — typical inside a container, where boot-time
		// persistence is neither reachable nor needed (the manager reprovisions
		// on startup). Skip quietly rather than logging a failure every change.
		return nil
	}
	if err != nil {
		return fmt.Errorf("read fstab: %w", err)
	}
	updated := updateFstabBlock(string(raw), paths)
	if updated == string(raw) {
		return nil // already in sync — no write, no backup churn
	}
	// Back up the pre-edit fstab, then write atomically (tmp + rename) so a
	// crash mid-write can never leave /etc/fstab truncated.
	if err := os.WriteFile(fstab+".odac.bak", raw, 0o644); err != nil {
		return fmt.Errorf("backup fstab: %w", err)
	}
	tmp := filepath.Join(filepath.Dir(fstab), ".fstab.odac.tmp")
	if err := os.WriteFile(tmp, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write fstab tmp: %w", err)
	}
	if err := os.Rename(tmp, fstab); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace fstab: %w", err)
	}
	return nil
}

// run executes a host command, surfacing its stderr/stdout in the error so a
// bare "exit status 1" from swapon/mkswap becomes the real kernel message
// (e.g. "swapon failed: Operation not permitted" → caps/seccomp restriction).
func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err == nil {
		return nil
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("%w: %s", err, msg)
	}
	return err
}

// readSnapshot gathers the live host state decide() needs. Any unreadable source
// degrades that field to zero; a missing /proc/meminfo (should never happen on
// Linux) makes the snapshot !ok so decide holds. The parsing is done by the
// pure helpers in swap.go so it stays testable off-host.
func readSnapshot(dir string) snapshot {
	memRaw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return snapshot{}
	}
	memTotal, memAvail, swapTotal, swapFree := parseMeminfoBytes(memRaw)

	psi := 0.0
	if raw, err := os.ReadFile("/proc/pressure/memory"); err == nil {
		psi = parsePressureSomeAvg10(raw)
	}

	var areas []area
	if raw, err := os.ReadFile("/proc/swaps"); err == nil {
		areas = parseSwaps(raw, swapPrefix(dir))
	}

	return snapshot{
		memTotal:     memTotal,
		memAvail:     memAvail,
		swapTotal:    swapTotal,
		swapFree:     swapFree,
		freeDisk:     freeDiskBytes(dir),
		psiSomeAvg10: psi,
		areas:        areas,
		ok:           memTotal > 0,
	}
}

// freeDiskBytes is the available space on the filesystem holding the swapfiles.
// It statfs's the swap dir (not "/") because in a container the root is overlay
// while the swap dir is a bind-mounted host filesystem — the disk budget must be
// measured against the volume the swapfiles actually consume. Falls back to the
// nearest existing ancestor when the dir has not been created yet. Mirrors
// sysinfo.diskBytes' Bavail accounting.
func freeDiskBytes(dir string) int64 {
	for d := dir; ; d = filepath.Dir(d) {
		var st unix.Statfs_t
		if unix.Statfs(d, &st) == nil {
			return int64(st.Bavail) * int64(st.Bsize)
		}
		if d == "/" || d == "." {
			return 0
		}
	}
}
