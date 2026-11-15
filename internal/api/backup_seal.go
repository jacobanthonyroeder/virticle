package api

// Encrypted VM backups: the files of a new backup are encrypted with age to the backup key on the node that made
// them, in the run's work directory, before anything is sent to another node; each plain work file is deleted as
// soon as its encrypted copy is there. A restore, or an export that puts a chain together, decrypts on the node
// that holds the backup, again into a work directory, with the identity handed over for that one operation.
//
// The backup job and qemu-img write qcow2, which needs a seekable file, so encrypting while they write (through a
// pipe) is not possible: encryption is a second pass over the files. It replaces the checksum pass (the node takes
// the checksums of both forms while it encrypts), so the extra cost is writing the encrypted copy once, plus the
// space of the largest file for a moment (sealSpace): for a full backup, a second copy of the largest disk's data.

import (
	"errors"
	"fmt"

	"virticle/internal/hostconn"
)

// sealSpace is the extra space encryption needs where the files are made. The work directory holds the plain
// files of the backup; each is encrypted into a copy next to it and deleted once the copy is checked, so the
// largest one exists twice for a moment. For a full backup that file holds the data of the largest disk (Actual,
// what its image chain occupies); for an incremental one only the changed blocks, of which libvirt's estimate for
// all disks together (changed, with incremental true) is an upper bound. age adds 16 bytes to every 64 KiB; the
// 1/64 covers that and the qcow2 metadata of the work file.
func sealSpace(disks []bkDisk, changed uint64, incremental bool) uint64 {
	var big uint64
	for _, d := range disks {
		big = max(big, d.Actual)
	}
	if incremental {
		big = min(big, changed+changed/8)
	}
	return big + big/64
}

// encryptWork encrypts the work file name into name.age on the backup's source node, checks the copy and removes
// the plain file. progress receives the bytes of name read so far.
func (m *bkRun) encryptWork(wd *bkWork, name string, progress func(uint64)) (hostconn.AgeSums, error) {
	if err := m.checkCancel(); err != nil {
		return hostconn.AgeSums{}, err
	}
	src, dst := wd.file(name), wd.file(name+ageSuffix)
	sums, err := m.src.AgeEncrypt(m.ctx, src, dst, m.enc, func(n, _ uint64) {
		if progress != nil {
			progress(n)
		}
	})
	if err != nil {
		if m.cancelled.Load() {
			return sums, errBackupCancelled
		}
		return sums, fmt.Errorf("encrypting %s: %w", name, err)
	}
	if err := checkSealed(m.src, src, dst, sums); err != nil {
		return sums, err
	}
	// The plaintext goes as soon as its encrypted copy is there.
	if err := m.src.Remove(src); err != nil {
		return sums, fmt.Errorf("removing the plain work file %s: %w", src, err)
	}
	return sums, nil
}

// checkSealed verifies an encrypted copy before its plain file is deleted: the node finished, and the stored file
// has the size the node says it wrote.
func checkSealed(h *hostconn.Host, src, dst string, sums hostconn.AgeSums) error {
	st, err := h.Stat(src, dst)
	switch {
	case err != nil:
		return fmt.Errorf("checking the encrypted copy %s: %w", dst, err)
	case !st[1].Exists || st[1].Size != sums.StoredBytes || sums.StoredBytes < sums.PlainBytes:
		return fmt.Errorf("the encrypted copy %s is incomplete", dst)
	case !st[0].Exists || st[0].Size != sums.PlainBytes:
		return fmt.Errorf("%s changed while it was encrypted", src)
	}
	return nil
}

// seal encrypts every data file of the backup in the work directory: the disks, the UEFI variables and the TPM
// state. The manifest records both checksums of each and the key they are encrypted to. The progress bar moves
// from lo to hi by the bytes of the disks.
func (m *bkRun) seal(wd *bkWork, man *backupManifest, lo, hi float64) error {
	var total, done uint64
	for _, d := range m.disks {
		total += d.Size
	}
	if man.NVRAM != "" {
		sums, err := m.encryptWork(wd, man.NVRAM, nil)
		if err != nil {
			return err
		}
		man.NVRAM += ageSuffix
		man.NVRAMEncrypted = true
		man.NVRAMBytes, man.NVRAMSHA256, man.NVRAMPlainBytes, man.NVRAMPlainSHA256 = sums.StoredBytes, sums.StoredSHA, sums.PlainBytes, sums.PlainSHA
	}
	if man.TPMState != "" {
		sums, err := m.encryptWork(wd, man.TPMState, nil)
		if err != nil {
			return err
		}
		if sums.PlainSHA != man.TPMSHA256 || sums.PlainBytes != man.TPMBytes {
			return errors.New("the saved TPM state changed while it was encrypted")
		}
		man.TPMState += ageSuffix
		man.TPMEncrypted = true
		man.TPMPlainBytes, man.TPMPlainSHA256 = man.TPMBytes, man.TPMSHA256
		man.TPMBytes, man.TPMSHA256 = sums.StoredBytes, sums.StoredSHA
	}
	for i := range m.disks {
		d := &m.disks[i]
		base := done
		sums, err := m.encryptWork(wd, d.File, func(n uint64) { m.progress(spreadBytes(lo, hi, base+n, total)) })
		if err != nil {
			return err
		}
		if sums.PlainBytes != d.Size {
			return fmt.Errorf("the backup image of %s changed while it was encrypted", d.Target)
		}
		d.Encrypted, d.PlainSize, d.PlainSHA = true, d.Size, sums.PlainSHA
		d.File += ageSuffix
		d.Size, d.SHA = sums.StoredBytes, sums.StoredSHA
		done += d.PlainSize
		m.progress(spreadBytes(lo, hi, done, total))
	}
	man.Recipient = m.enc
	return nil
}

// --- decrypting ---

// fileSums is what a manifest records of one stored file.
type fileSums struct {
	Bytes, PlainBytes uint64
	SHA, PlainSHA     string
}

func diskSums(d backupDiskRec) fileSums {
	return fileSums{Bytes: d.Bytes, SHA: d.SHA256, PlainBytes: d.PlainBytes, PlainSHA: d.PlainSHA256}
}

// decryptFile decrypts the stored file src into dst (a new file in a work directory on h) with the identity, and
// checks both forms against the manifest: a changed stored file, or one that decrypts to something else, is an
// error, and dst is removed.
func decryptFile(st *stopper, h *hostconn.Host, src, dst string, want fileSums, id []byte, progress func(uint64)) error {
	sums, err := h.AgeDecrypt(st.Ctx(), src, dst, id, func(n, _ uint64) {
		if progress != nil {
			progress(n)
		}
	})
	if err != nil {
		return err
	}
	switch {
	case sums.StoredSHA != want.SHA || sums.StoredBytes != want.Bytes:
		err = fmt.Errorf("%s is damaged: its SHA-256 is %s, the manifest says %s", src, sums.StoredSHA, want.SHA)
	case sums.PlainSHA != want.PlainSHA || sums.PlainBytes != want.PlainBytes:
		err = fmt.Errorf("%s does not decrypt to what was backed up: the SHA-256 is %s, the manifest says %s", src, sums.PlainSHA, want.PlainSHA)
	}
	if err != nil {
		_ = h.Remove(dst)
	}
	return err
}

// hasEncryptedDisk says whether the backup stores the disk target encrypted.
func (l chainLink) hasEncryptedDisk(target string) bool {
	d, ok := l.diskOf(target)
	return ok && d.Encrypted
}
