package api

// A volume created from another volume (a full clone) is one libvirt call that returns when the copy is done,
// and libvirt cannot interrupt it. libvirt runs qemu-img for the copy, so a stop request has the node command
// end that process (copy-kill); libvirt then fails the call and removes the partial file.

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

// errCopyStopped is what copyVolume returns for a copy that a stop request ended. Nothing is left behind.
var errCopyStopped = errors.New("volume copy stopped")

// copyVolume is StorageVolCreateXMLFrom that st can stop. It returns errCopyStopped when the task was
// stopped before the call or while it ran; the new volume is then gone. A copy that finished although the
// task was stopped is returned as a success: the caller decides what to do with it.
func copyVolume(h *hostconn.Host, pool libvirt.StoragePool, xmlDesc, name string, src libvirt.StorageVol, st *stopper) (libvirt.StorageVol, error) {
	if st.Stopped() {
		return libvirt.StorageVol{}, errCopyStopped
	}
	_, existed := h.StorageVolLookupByName(pool, name)
	existedBefore := existed == nil // then the call fails by itself and nothing of ours may be deleted

	finished := make(chan struct{})
	if dir, err := poolDirectory(h, pool); err == nil && safePathRE.MatchString(dir+"/"+name) {
		dest := dir + "/" + name
		undo := st.onStop(func() { killCopy(h, dest, finished) })
		defer undo()
	}
	vol, err := h.StorageVolCreateXMLFrom(pool, xmlDesc, src, 0)
	close(finished)
	if err == nil {
		return vol, nil
	}
	if !st.Stopped() {
		return vol, err
	}
	if !existedBefore {
		// libvirt removes what it wrote when the copy fails; make sure.
		h.StoragePoolRefresh(pool, 0)
		if v, lerr := h.StorageVolLookupByName(pool, name); lerr == nil {
			if derr := h.StorageVolDelete(v, 0); derr != nil {
				log.Printf("stopped copy of %s: cannot delete the partial volume: %v", name, derr)
				return vol, fmt.Errorf("%w: the partial volume %s could not be removed: %v", errCopyStopped, name, derr)
			}
		}
	}
	return vol, errCopyStopped
}

// copyStopped is the task's error for a copy that was stopped: "<what> cancelled", with the note of copyVolume
// when the partial volume could not be removed.
func copyStopped(what string, err error) error {
	msg := what + " cancelled"
	if rest := strings.TrimPrefix(err.Error(), errCopyStopped.Error()+": "); rest != err.Error() {
		msg += "; " + rest
	}
	return stoppedError(msg)
}

// killCopy asks the node to end the qemu-img that writes dest, until that worked, the copy has ended or a
// few seconds have passed (libvirt may not have started qemu-img yet).
func killCopy(h *hostconn.Host, dest string, finished <-chan struct{}) {
	for i := 0; i < 40; i++ {
		select {
		case <-finished:
			return
		default:
		}
		err := h.CopyKill(dest)
		if err == nil {
			return
		}
		if errors.Is(err, hostconn.ErrNoCopyKill) {
			log.Printf("%s: %v; the copy of %s runs to its end", h.Name, err, dest)
			return
		}
		select {
		case <-finished:
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}
