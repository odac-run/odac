package thin

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
}

// Excess is how many bytes the snapshots must give up: what they
// reference above the quota, or what the floor lacks once the garbage is
// back, whichever is larger. The floor applies under a quota too, because
// a quota is not a promise to keep backups on a disk that is full.
func (s Space) Excess() int64 {
	garbage := min(max(s.Garbage, 0), max(s.Used, 0))
	var over int64
	if s.Quota > 0 {
		over = s.Used - garbage - s.Quota
	}
	if s.Free >= 0 && s.Total > 0 {
		over = max(over, Floor(s.Total)-(s.Free+garbage))
	}
	return max(over, 0)
}
