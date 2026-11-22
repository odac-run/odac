package diskbudget

import "testing"

const gib = int64(1) << 30

func TestLimitCountsTheStoreAsReclaimable(t *testing.T) {
	cases := []struct {
		name            string
		free, own, pct  int64
		limit, wantRoom int64
	}{
		{"empty store", 100 * gib, 0, 25, 25 * gib, 25 * gib},
		{"store grown into its share keeps the same limit", 80 * gib, 20 * gib, 25, 25 * gib, 5 * gib},
		{"apps filling the disk shrink it", 20 * gib, 20 * gib, 25, 10 * gib, -10 * gib},
		{"negative inputs read as zero", -1, -1, 25, 0, 0},
	}
	for _, c := range cases {
		if got := Limit(c.free, c.own, c.pct); got != c.limit {
			t.Errorf("%s: Limit = %d, want %d", c.name, got, c.limit)
		}
		if got := Room(c.free, c.own, c.pct); got != c.wantRoom {
			t.Errorf("%s: Room = %d, want %d", c.name, got, c.wantRoom)
		}
	}
}

// Two stores sharing one disk under the same rule at 25% each leave more
// than half of it free, whatever order they grow in.
func TestTwoStoresLeaveMoreThanHalfFree(t *testing.T) {
	disk := 1000 * gib
	var a, b int64
	for range 50 {
		a += max(Room(disk-a-b, a, 25), 0)
		b += max(Room(disk-a-b, b, 25), 0)
	}
	if free := disk - a - b; free*100/disk < 55 {
		t.Fatalf("the two stores leave %d%% of the disk free", free*100/disk)
	}
}
