package thin

import (
	"container/heap"
	"math"
	"time"
)

// Squeeze orders the backups of the given jobs for deletion when space
// runs short, the least harmful first. It is handed what Thin left, and
// each job's newest backup never appears: the next backup deduplicates
// against it, so deleting it gives back the least.
//
// Harm is measured against age, the way the rule itself measures density.
// The most recent hours are always the densest in absolute terms, and
// emptying them first would leave them sparser than last year. Removing a
// backup leaves its neighbours a gap; the next pick is the one whose gap,
// divided by the age of its newer end, comes out smallest, which is how
// lowering k would thin. A job's oldest backup comes after every other
// candidate: the plan keeps the far past reachable, only sparser, so depth
// is given up last.
//
// The caller deletes the shortest prefix that frees Space.Excess and leaves
// the rest for the next check, after the space has been measured again.
func Squeeze(now time.Time, backups []Backup, jobs map[string]bool) []string {
	var series [][]Backup
	for job, s := range byJob(backups) {
		if jobs[job] {
			series = append(series, s)
		}
	}
	return squeeze(now, series)
}

type node struct {
	t          int64
	id         string
	prev, next *node
	version    int
}

type candidate struct {
	n       *node
	cost    float64
	version int
}

type candidates []candidate

func (c candidates) Len() int { return len(c) }
func (c candidates) Less(i, j int) bool {
	if c[i].cost != c[j].cost {
		return c[i].cost < c[j].cost
	}
	if c[i].n.t != c[j].n.t {
		return c[i].n.t < c[j].n.t
	}
	return c[i].n.id < c[j].n.id
}
func (c candidates) Swap(i, j int) { c[i], c[j] = c[j], c[i] }
func (c *candidates) Push(x any)   { *c = append(*c, x.(candidate)) }
func (c *candidates) Pop() any {
	old := *c
	x := old[len(old)-1]
	*c = old[:len(old)-1]
	return x
}

// squeeze merges every series into one order. A series' newest node is
// its anchor and never becomes a candidate.
func squeeze(now time.Time, series [][]Backup) []string {
	nowSec := now.Unix()
	h := &candidates{}
	for _, s := range series {
		var prev *node
		for _, b := range s {
			n := &node{t: b.Time.Unix(), id: b.ID, prev: prev}
			if prev != nil {
				prev.next = n
			}
			prev = n
		}
		for n := prev; n != nil; n = n.prev {
			if n.next != nil {
				heap.Push(h, candidate{n: n, cost: cost(nowSec, n)})
			}
		}
	}
	var order []string
	for h.Len() > 0 {
		c := heap.Pop(h).(candidate)
		if c.version != c.n.version {
			continue
		}
		n := c.n
		order = append(order, n.id)
		p, nx := n.prev, n.next
		nx.prev = p
		if p != nil {
			p.next = nx
		}
		for _, m := range []*node{p, nx} {
			if m == nil || m.next == nil {
				continue
			}
			m.version++
			heap.Push(h, candidate{n: m, cost: cost(nowSec, m), version: m.version})
		}
	}
	return order
}

func cost(now int64, n *node) float64 {
	if n.prev == nil {
		return math.Inf(1)
	}
	age := max(now-n.next.t, 1)
	return float64(n.next.t-n.prev.t) / float64(age)
}
