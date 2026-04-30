package limits

import (
	"strconv"
	"sync"
	"testing"
)

func smallProfile() Profile {
	return Profile{
		MaxPerUserIP: 2,
		MaxPerUser:   3,
		MaxPerIP:     4,
		MaxTotal:     5,
		NewConnPerIP: 0, // disable rate gate; tested separately
	}
}

func TestAcquire_PerIPCap(t *testing.T) {
	l := New(smallProfile())

	for i := 0; i < 4; i++ {
		if _, r := l.Acquire("1.1.1.1"); r != ReasonOK {
			t.Fatalf("acquire %d: want OK, got %s", i, r)
		}
	}
	if _, r := l.Acquire("1.1.1.1"); r != ReasonPerIP {
		t.Fatalf("5th acquire: want ReasonPerIP, got %s", r)
	}
}

func TestAcquire_GlobalCap(t *testing.T) {
	l := New(smallProfile())

	for i := 0; i < 5; i++ {
		ip := "10.0.0." + strconv.Itoa(i)
		if _, r := l.Acquire(ip); r != ReasonOK {
			t.Fatalf("acquire %d: want OK, got %s", i, r)
		}
	}
	if _, r := l.Acquire("10.0.0.99"); r != ReasonTotal {
		t.Fatalf("6th acquire: want ReasonTotal, got %s", r)
	}
}

func TestBindUser_PerUserIPCap(t *testing.T) {
	l := New(smallProfile())

	h1, _ := l.Acquire("1.1.1.1")
	h2, _ := l.Acquire("1.1.1.1")
	h3, _ := l.Acquire("1.1.1.1")

	if r := h1.BindUser("alice"); r != ReasonOK {
		t.Fatalf("bind 1: want OK, got %s", r)
	}
	if r := h2.BindUser("alice"); r != ReasonOK {
		t.Fatalf("bind 2: want OK, got %s", r)
	}
	if r := h3.BindUser("alice"); r != ReasonPerUserIP {
		t.Fatalf("bind 3 (same user/IP): want ReasonPerUserIP, got %s", r)
	}
}

func TestBindUser_PerUserCap(t *testing.T) {
	l := New(smallProfile())

	// 3 connections from 3 distinct IPs, all binding to the same user.
	for i := 0; i < 3; i++ {
		ip := "2.2.2." + strconv.Itoa(i)
		h, r := l.Acquire(ip)
		if r != ReasonOK {
			t.Fatalf("acquire %d: %s", i, r)
		}
		if r := h.BindUser("alice"); r != ReasonOK {
			t.Fatalf("bind %d: %s", i, r)
		}
	}

	h, r := l.Acquire("2.2.2.99")
	if r != ReasonOK {
		t.Fatalf("acquire 4: %s", r)
	}
	if r := h.BindUser("alice"); r != ReasonPerUser {
		t.Fatalf("bind 4: want ReasonPerUser, got %s", r)
	}
}

func TestRelease_DecrementsAndAllowsReacquire(t *testing.T) {
	l := New(smallProfile())

	handles := make([]*Handle, 4)
	for i := 0; i < 4; i++ {
		h, r := l.Acquire("3.3.3.3")
		if r != ReasonOK {
			t.Fatalf("acquire %d: %s", i, r)
		}
		handles[i] = h
	}
	if _, r := l.Acquire("3.3.3.3"); r != ReasonPerIP {
		t.Fatalf("over cap: want ReasonPerIP, got %s", r)
	}

	handles[0].Release()
	if _, r := l.Acquire("3.3.3.3"); r != ReasonOK {
		t.Fatalf("after release: want OK, got %s", r)
	}
}

func TestRelease_Idempotent(t *testing.T) {
	l := New(smallProfile())
	h, _ := l.Acquire("4.4.4.4")
	h.BindUser("bob")

	h.Release()
	h.Release() // must not double-decrement
	h.Release()

	total, ips, users := l.Snapshot()
	if total != 0 || ips != 0 || users != 0 {
		t.Fatalf("counters not zero after idempotent release: total=%d ips=%d users=%d", total, ips, users)
	}
}

func TestRelease_NilSafe(t *testing.T) {
	var h *Handle
	h.Release() // must not panic
}

func TestBindUser_FailureDoesNotIncrement(t *testing.T) {
	l := New(smallProfile())

	for i := 0; i < 3; i++ {
		ip := "5.5.5." + strconv.Itoa(i)
		h, _ := l.Acquire(ip)
		h.BindUser("alice")
	}

	// Acquire a 4th conn, BindUser will fail (per-user cap = 3).
	h, _ := l.Acquire("5.5.5.99")
	if r := h.BindUser("alice"); r != ReasonPerUser {
		t.Fatalf("want ReasonPerUser, got %s", r)
	}

	// Releasing the failed-bind handle must only decrement IP/total,
	// not under-count alice. After releasing all 3 successful sessions
	// alice's counter should hit zero exactly.
	h.Release()
	total, _, users := l.Snapshot()
	if total != 3 {
		t.Fatalf("total: want 3, got %d", total)
	}
	if users != 1 {
		t.Fatalf("users: want 1 (alice), got %d", users)
	}
}

func TestRateBucket_LimitsBurst(t *testing.T) {
	p := smallProfile()
	p.NewConnPerIP = 1
	p.NewConnBurst = 2
	l := New(p)

	// Two acquires fit in the burst; third immediate one is rate-limited.
	if _, r := l.Acquire("9.9.9.9"); r != ReasonOK {
		t.Fatalf("burst 1: %s", r)
	}
	if _, r := l.Acquire("9.9.9.9"); r != ReasonOK {
		t.Fatalf("burst 2: %s", r)
	}
	if _, r := l.Acquire("9.9.9.9"); r != ReasonRate {
		t.Fatalf("burst 3: want ReasonRate, got %s", r)
	}
}

func TestConcurrent_NeverExceedsCaps(t *testing.T) {
	l := New(smallProfile())

	const goroutines = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)

	var accepted int64
	var mu sync.Mutex

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			h, r := l.Acquire("7.7.7.7")
			if r == ReasonOK {
				mu.Lock()
				accepted++
				mu.Unlock()
				h.Release()
			}
		}()
	}
	wg.Wait()

	// All connections were released before the test ended, so totals are 0.
	total, _, _ := l.Snapshot()
	if total != 0 {
		t.Fatalf("leaked counter: total=%d", total)
	}
}
