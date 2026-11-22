// Package diskbudget is the rule by which a store ODAC grows on the host's
// disk (swap files, the snapshot repository) shares that disk with the
// apps. A store may hold at most a percentage of the free space plus its
// own size: its own bytes count as space it could give back, so the share
// does not shrink as the store grows into it, and it shrinks by itself when
// the apps fill the disk. Two stores under the same rule at 25% each leave
// more than half of the disk to everything else.
package diskbudget

// Limit is the most a store may hold: pct percent of free plus own, where
// free is the disk's free space (which excludes the store) and own is what
// the store holds now.
func Limit(free, own, pct int64) int64 {
	if free < 0 {
		free = 0
	}
	if own < 0 {
		own = 0
	}
	return (free + own) * pct / 100
}

// Room is how much more the store may take before it reaches its limit;
// negative when it is over.
func Room(free, own, pct int64) int64 {
	return Limit(free, own, pct) - max(own, 0)
}
