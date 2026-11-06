package ebpf

import "testing"

func hist(slots map[int]uint64) [Slots]uint64 {
	var h [Slots]uint64
	for i, n := range slots {
		h[i] = n
	}
	return h
}

func TestP99IsTheUpperBoundOfItsBucket(t *testing.T) {
	// Slot i holds latencies of 2^i to 2^(i+1) microseconds.
	got, ok := P99(hist(map[int]uint64{3: 100}))
	if !ok || got != 16 {
		t.Fatalf("got %v, %v, want 16 us", got, ok)
	}
}

func TestP99FindsTheTail(t *testing.T) {
	// 990 fast, 10 slow: the 990th event is the last fast one.
	got, ok := P99(hist(map[int]uint64{2: 990, 12: 10}))
	if !ok || got != 8 {
		t.Fatalf("got %v, %v, want 8 us", got, ok)
	}
	// One more slow event pushes rank 991 of 1001 into the tail.
	got, ok = P99(hist(map[int]uint64{2: 990, 12: 11}))
	if !ok || got != 8192 {
		t.Fatalf("got %v, %v, want 8192 us", got, ok)
	}
}

func TestP99WithTooFewEventsIsNoData(t *testing.T) {
	if _, ok := P99(hist(map[int]uint64{3: MinEvents - 1})); ok {
		t.Fatal("a p99 of a handful of events is just the slowest one")
	}
	if _, ok := P99(hist(nil)); ok {
		t.Fatal("an empty histogram has no p99")
	}
	if _, ok := P99(hist(map[int]uint64{3: MinEvents})); !ok {
		t.Fatal("exactly the minimum should count")
	}
}

func TestDeltaIsWhatHappenedSinceTheLastRead(t *testing.T) {
	got := Delta(hist(map[int]uint64{1: 10, 2: 5}), hist(map[int]uint64{1: 4}))
	if got != hist(map[int]uint64{1: 6, 2: 5}) {
		t.Fatalf("got %v", got)
	}
}

// A cgroup id is never reused while its histogram exists, but a recreated map starts
// over, and an unsigned subtraction would wrap to an enormous count.
func TestDeltaOfACounterThatWentBackwardsIsZero(t *testing.T) {
	got := Delta(hist(map[int]uint64{1: 3}), hist(map[int]uint64{1: 10}))
	if got != hist(nil) {
		t.Fatalf("got %v", got)
	}
}
