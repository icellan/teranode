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
	if float64(total)*0.5 < 1_000_000_000 {
		t.Fatalf("capacity too small: total=%d", total)
	}
	if l.slotsPerSeg&(l.slotsPerSeg-1) != 0 {
		t.Fatalf("slotsPerSeg not pow2: %d", l.slotsPerSeg)
	}
}
