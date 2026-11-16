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

// copyVolume is StorageVolCreateXMLFrom that st can stop, for a new volume name of the given format and capacity
// in pool. It returns errCopyStopped when the task was stopped before the call or while it ran. A copy that
// finished although the task was stopped is returned as a success: the caller decides what to do with it.
//
// libvirt writes the copy over a file that has the name but that it does not list (one made on another node of a
// shared pool, or outside libvirt), so the copy is written under a new private name, its identity taken, and only
// then given its name by the node, which never replaces a file. The identity returned proves later (removeOwnFile)
// that a file under the name is still this copy.
func copyVolume(h *hostconn.Host, pool libvirt.StoragePool, name, format string, capacity uint64, src libvirt.StorageVol, st *stopper) (libvirt.StorageVol, hostconn.FileID, error) {
	var none hostconn.FileID
	if st.Stopped() {
		return libvirt.StorageVol{}, none, errCopyStopped
	}
	dir, err := poolDirectory(h, pool)
	if err != nil {
		return libvirt.StorageVol{}, none, err
	}
	tmpName := moveTempName(name)
	final, tmp := dir+"/"+name, dir+"/"+tmpName
	if !safePathRE.MatchString(final) || !safePathRE.MatchString(tmp) {
		return libvirt.StorageVol{}, none, fmt.Errorf("unsupported volume path %s", final)
	}
	if ids, err := h.FileIDs(final); err != nil {
		return libvirt.StorageVol{}, none, err
	} else if len(ids) == 1 && ids[0].Exists {
		return libvirt.StorageVol{}, none, fmt.Errorf("%s exists already; nothing was overwritten", final)
	}

	finished := make(chan struct{})
	undo := st.onStop(func() { killCopy(h, tmp, finished) })
	_, err = h.StorageVolCreateXMLFrom(pool, volumeXML(tmpName, format, capacity), src, 0)
	close(finished)
	undo()
	if err != nil {
		// libvirt removes what it wrote when the copy fails. Anything left under the private name is not proven
		// to be this copy: it stays, and the error names it.
		h.StoragePoolRefresh(pool, 0)
		if st2, serr := h.Stat(tmp); serr != nil || len(st2) != 1 || st2[0].Exists {
			log.Printf("copy of %s: %s may be left behind", name, tmp)
			err = fmt.Errorf("%w (a partial copy may be left at %s; it was not removed)", err, tmp)
			if st.Stopped() {
				return libvirt.StorageVol{}, none, fmt.Errorf("%w: a partial copy may be left at %s; it was not removed", errCopyStopped, tmp)
			}
		}
		if st.Stopped() {
			return libvirt.StorageVol{}, none, errCopyStopped
		}
		return libvirt.StorageVol{}, none, err
	}
	ids, err := h.FileIDs(tmp)
	if err != nil || len(ids) != 1 || !ids[0].Regular {
		return libvirt.StorageVol{}, none, fmt.Errorf("the copy %s cannot be identified (%v); it was left in place", tmp, err)
	}
	id := ids[0]
	if err := h.Move(tmp, final); err != nil {
		why := removeOwnFile(h, id)
		h.StoragePoolRefresh(pool, 0)
		err = fmt.Errorf("giving the copy the name %s: %w", final, err)
		if why != "" {
			err = fmt.Errorf("%w (the copy %s was kept: %s)", err, tmp, why)
		}
		return libvirt.StorageVol{}, none, err
	}
	id.Path = final
	h.StoragePoolRefresh(pool, 0)
	vol, err := h.StorageVolLookupByName(pool, name)
	if err != nil {
		return libvirt.StorageVol{}, id, fmt.Errorf("the copy %s is not listed in its pool: %w", final, err)
	}
	return vol, id, nil
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
