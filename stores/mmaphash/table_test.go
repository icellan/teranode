package mmaphash

import "testing"

func TestNextPow2(t *testing.T) {
	cases := []struct{ in, want uint64 }{
		{0, 1}, {1, 1}, {2, 2}, {3, 4}, {5, 8}, {1 << 20, 1 << 20}, {(1 << 20) + 1, 1 << 21},
	}
	for _, c := range cases {
		if got := nextPow2(c.in); got != c.want {
			t.Fatalf("nextPow2(%d)=%d want %d", c.in, got, c.want)
		}
	}
}

func TestComputeLayout(t *testing.T) {
	// expected 0 -> minimal table, K=1, slotsPerSeg=minSegSlots
	l := computeLayout(0, 0.5)
	if l.numSeg != 1 || l.slotsPerSeg != minSegSlots {
		t.Fatalf("empty: got numSeg=%d slotsPerSeg=%d", l.numSeg, l.slotsPerSeg)
	}
	// large expected -> K clamped to maxSeg, slotsPerSeg pow2, capacity >= expected/LF
	l = computeLayout(1_000_000_000, 0.5)
	if l.numSeg != maxSeg {
		t.Fatalf("large: numSeg=%d want %d", l.numSeg, maxSeg)
	}
	total := l.numSeg * l.slotsPerSeg
	if float64(total)*defaultLoadFactor < 1_000_000_000 {
		t.Fatalf("capacity too small: total=%d", total)
	}
	if l.slotsPerSeg&(l.slotsPerSeg-1) != 0 {
		t.Fatalf("slotsPerSeg not pow2: %d", l.slotsPerSeg)
	}

	// ceiling division must guarantee capacity >= expected at defaultLoadFactor
	for _, exp := range []uint64{1, 10, 262145, 262143, 524291, 1_000_000, 3_000_000} {
		ll := computeLayout(exp, defaultLoadFactor)
		total := ll.numSeg * ll.slotsPerSeg
		if uint64(float64(total)*defaultLoadFactor) < exp {
			t.Fatalf("undersized for expected=%d: total=%d capacity=%d", exp, total, uint64(float64(total)*defaultLoadFactor))
		}
	}
}

func TestComputeLayoutDefaultLoadFactor(t *testing.T) {
	// loadFactor <= 0 must behave identically to defaultLoadFactor
	a := computeLayout(100000, 0)
	b := computeLayout(100000, defaultLoadFactor)
	if a != b {
		t.Fatalf("loadFactor<=0 default mismatch: got %+v want %+v", a, b)
	}
	c := computeLayout(100000, -1)
	if c != b {
		t.Fatalf("negative loadFactor default mismatch: got %+v want %+v", c, b)
	}
}

func TestComputeLayoutMinSegSlotsFloor(t *testing.T) {
	// small non-zero expected -> perSeg computed below minSegSlots -> floored
	l := computeLayout(10, 0.5)
	if l.numSeg != 1 {
		t.Fatalf("numSeg=%d want 1", l.numSeg)
	}
	if l.slotsPerSeg != minSegSlots {
		t.Fatalf("slotsPerSeg=%d want minSegSlots=%d (floor not applied)", l.slotsPerSeg, minSegSlots)
	}
}
