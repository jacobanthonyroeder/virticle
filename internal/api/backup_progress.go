package api

// How the progress bar of a backup moves. A run has phases of very different length: the copy of the disks, the
// SHA-256 of what was written (it reads every byte again), the copy to another node, and the short steps at the
// end (manifest, moving the directory into place, old checkpoints, retention). Each phase gets a share of the bar
// that matches the time it takes on a node (see bkPhasesFor), so the bar keeps moving through all of them and
// reaches its end only when the task does.

import "sync"

// bkPhases are the points of the progress bar (0..1) where the phases of a backup run end.
type bkPhases struct {
	copyLo, copyHi float64 // the disks are copied into the work directory
	hashHi         float64 // checksums of the copies (from copyHi)
	sendHi         float64 // the copy to another node (from hashHi; the same as hashHi on one node)
	storedAt       float64 // the backup is in place; older checkpoints are deleted next
	checkpointsAt  float64 // then retention, until the task ends
}

// bkPhasesFor shares out the bar. Measured on the lab (nested KVM, a Debian guest with 3 GB of data):
// QEMU's backup job writes into the page cache and is done first; the SHA-256 reads every byte back from the
// disk and takes two to three times as long; a copy to another node about as long as the checksums. The
// manifest, the move into place, the checkpoints and retention take a second together. hashing is false when no
// checksums are taken (a scheduled backup that starts no chain): the copy then has that room.
func bkPhasesFor(remote, hashing bool) bkPhases {
	p := bkPhases{copyLo: 0.02, storedAt: 0.95, checkpointsAt: 0.97}
	switch {
	case remote && hashing:
		p.copyHi, p.hashHi, p.sendHi = 0.18, 0.55, 0.93
	case remote:
		p.copyHi, p.hashHi, p.sendHi = 0.30, 0.30, 0.93
	case hashing:
		p.copyHi, p.hashHi, p.sendHi = 0.30, 0.93, 0.93
	default:
		p.copyHi, p.hashHi, p.sendHi = 0.93, 0.93, 0.93
	}
	return p
}

// forwardOnly wraps a task's progress function so that the bar never moves back (a phase that reports less than
// an earlier one, a retried step) and never shows 100% before the task has ended: the task sets that itself.
func forwardOnly(progress func(float64)) func(float64) {
	var mu sync.Mutex
	last := 0.0
	return func(f float64) {
		if f > 0.99 {
			f = 0.99
		}
		mu.Lock()
		defer mu.Unlock()
		if f <= last {
			return
		}
		last = f
		progress(f)
	}
}

// spreadBytes is the part f of the range lo..hi for done of total bytes.
func spreadBytes(lo, hi float64, done, total uint64) float64 {
	if total == 0 {
		return hi
	}
	f := float64(done) / float64(total)
	if f > 1 {
		f = 1
	}
	return lo + (hi-lo)*f
}
