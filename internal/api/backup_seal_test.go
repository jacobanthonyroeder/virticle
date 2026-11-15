package api

import "testing"

func TestSealSpace(t *testing.T) {
	const gib = uint64(1) << 30
	disks := []bkDisk{{Actual: 20 * gib}, {Actual: 2 * gib}}
	// A full backup: the largest disk's data a second time, and a little for age and the qcow2 metadata.
	if got := sealSpace(disks, 0, false); got != 20*gib+20*gib/64 {
		t.Errorf("full: %d", got)
	}
	// An incremental one: only what changed, not the whole disk.
	if got := sealSpace(disks, 800<<20, true); got != 900<<20+(900<<20)/64 {
		t.Errorf("incremental: %d", got)
	}
	// Nothing changed: nothing extra (the general margin holds the UEFI variables and the TPM state).
	if got := sealSpace(disks, 0, true); got != 0 {
		t.Errorf("empty incremental: %d", got)
	}
	// More changed than the largest disk holds (several disks): the largest file is still at most that disk.
	if got := sealSpace(disks, 30*gib, true); got != 20*gib+20*gib/64 {
		t.Errorf("large incremental: %d", got)
	}
}
