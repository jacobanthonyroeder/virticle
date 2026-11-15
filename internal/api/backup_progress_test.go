package api

import "testing"

func TestBackupPhasesInOrder(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, hashing := range []bool{false, true} {
			p := bkPhasesFor(remote, hashing, false)
			seq := []float64{0, p.copyLo, p.copyHi, p.hashHi, p.sendHi, p.storedAt, p.checkpointsAt}
			for i := 1; i < len(seq); i++ {
				if seq[i] < seq[i-1] {
					t.Errorf("remote %v hashing %v: phase %d ends before the one before it: %v", remote, hashing, i, seq)
				}
			}
			if p.checkpointsAt >= 1 {
				t.Errorf("remote %v hashing %v: the bar is full before the task ends: %v", remote, hashing, seq)
			}
			if p.copyHi <= p.copyLo {
				t.Errorf("the copy has no room: %v", seq)
			}
			if hashing != (p.hashHi > p.copyHi) {
				t.Errorf("remote %v hashing %v: checksums have room %v", remote, hashing, p.hashHi-p.copyHi)
			}
			if remote != (p.sendHi > p.hashHi) {
				t.Errorf("remote %v hashing %v: the copy to the other node has room %v", remote, hashing, p.sendHi-p.hashHi)
			}
		}
	}
}

// Encryption reads and writes every byte again: the bar gives it room after the copy, also when no checksum
// pass was asked for (it takes the checksums anyway), and keeps room for the copy to another node.
func TestBackupPhasesWithEncryption(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, hashing := range []bool{false, true} {
			p := bkPhasesFor(remote, hashing, true)
			if p.hashHi-p.copyHi < 0.3 {
				t.Errorf("remote %v: encryption has too little room: %v", remote, p)
			}
			if p.copyHi <= p.copyLo || p.hashHi > p.sendHi || p.sendHi > p.storedAt || remote != (p.sendHi > p.hashHi) {
				t.Errorf("remote %v: phases out of order: %+v", remote, p)
			}
		}
	}
}

func TestForwardOnly(t *testing.T) {
	var got []float64
	f := forwardOnly(func(p float64) { got = append(got, p) })
	for _, p := range []float64{0.01, 0.3, 0.2, 0.3, 0.5, 1, 0.995, 0.4} {
		f(p)
	}
	want := []float64{0.01, 0.3, 0.5, 0.99}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%v, want %v", got, want)
		}
	}
}

func TestSpreadBytes(t *testing.T) {
	if spreadBytes(0.2, 0.6, 50, 100) != 0.4 || spreadBytes(0.2, 0.6, 200, 100) != 0.6 || spreadBytes(0.2, 0.6, 0, 0) != 0.6 {
		t.Fatal(spreadBytes(0.2, 0.6, 50, 100))
	}
}
