package thin

import "math"

// Floor is the free space a filesystem target keeps whatever the backups
// want: the larger of 10% of the disk and 5 GB. A percentage alone is too
// little on a small disk and a fixed size too little on a large one. It is
// also the room maintenance itself needs, since once a disk is full not
// even a delete can be written.
func Floor(total int64) int64 {
	return max(total/10, floorBytes)
}

const floorBytes = 5 << 30

// Space is what a repository's target looks like after maintenance.
type Space struct {
	// Used is what the repository holds on the target, as measured.
	Used int64
	// Garbage is the part of Used no snapshot references any more. Under
	// SafetyFull a deleted snapshot's space comes back only after at least
	// three full maintenance runs more than five hours apart, so without
	// this the next check would see the same shortage and delete again for
	// space that is already on its way back.
	Garbage int64
	// Quota caps what the snapshots may reference; zero means none.
	Quota int64
	// Free and Total describe the target's filesystem. Free is negative
	// for a target with no notion of free space (S3, WebDAV), where only
	// the quota and the density rule apply.
	Free, Total int64
	// Peers are the other servers actively writing to the same
	// filesystem, as their usage files report them. With any, the floor
	// is defended by fair share: see Excess.
	Peers []Peer
}

// Peer is another active server's repository on a shared filesystem
// target. A server whose usage file went stale is not a peer: its space
// counts as taken, and nobody deletes its backups.
type Peer struct {
	Used, Garbage int64
}

// Excess is how many bytes the snapshots must give up: what they
// reference above the quota, or what the floor lacks once the garbage is
// back, whichever is larger. The floor applies under a quota too, because
// a quota is not a promise to keep backups on a disk that is full.
func (s Space) Excess() int64 {
	garbage := clampGarbage(s.Garbage, s.Used)
	var over int64
	if s.Quota > 0 {
		over = s.Used - garbage - s.Quota
	}
	if s.Free >= 0 && s.Total > 0 {
		over = max(over, s.floorShare(garbage))
	}
	return max(over, 0)
}

// floorShare is this server's part of what the floor lacks once every
// active server's garbage is back. Alone on the disk that is all of it.
// Shared, the usable space (what the active servers hold plus what is
// free, less the floor) splits evenly among them, and only those holding
// more live data than their share give anything up, each in proportion
// to how far above it is. The parts add up to the lack, since the live
// data above the share sums to at least that much, and a server at or
// below its share never deletes for another's growth; space it leaves
// unused is still the others' to fill until the floor is reached.
func (s Space) floorShare(garbage int64) int64 {
	floor := Floor(s.Total)
	returning := garbage
	for _, p := range s.Peers {
		returning += clampGarbage(p.Garbage, p.Used)
	}
	lack := floor - (s.Free + returning)
	if lack <= 0 || len(s.Peers) == 0 {
		return lack
	}
	usable := float64(s.Free) + float64(max(s.Used, 0)) - float64(floor)
	for _, p := range s.Peers {
		usable += float64(max(p.Used, 0))
	}
	share := usable / float64(len(s.Peers)+1)
	above := func(used, garbage int64) float64 {
		return math.Max(float64(max(used, 0)-garbage)-share, 0)
	}
	own := above(s.Used, garbage)
	total := own
	for _, p := range s.Peers {
		total += above(p.Used, clampGarbage(p.Garbage, p.Used))
	}
	if own <= 0 || total <= 0 {
		return 0
	}
	part := math.Ceil(float64(lack) * own / total)
	return int64(math.Min(part, math.Ceil(own)))
}

// Share is each active server's fair share of a shared filesystem
// target: the space they hold together with what is free, less the
// floor, split evenly. ok is false alone on a target or without a notion
// of free space, where there is no share to keep.
func (s Space) Share() (share int64, ok bool) {
	if len(s.Peers) == 0 || s.Free < 0 || s.Total <= 0 {
		return 0, false
	}
	usable := float64(s.Free) + float64(max(s.Used, 0)) - float64(Floor(s.Total))
	for _, p := range s.Peers {
		usable += float64(max(p.Used, 0))
	}
	return int64(usable / float64(len(s.Peers)+1)), true
}

func clampGarbage(garbage, used int64) int64 {
	return min(max(garbage, 0), max(used, 0))
}
