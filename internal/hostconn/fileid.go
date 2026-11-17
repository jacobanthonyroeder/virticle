package hostconn

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// FileID is the identity of a file on a node (internal/nodecmd/verbs_fileid.go): what proves later that a file
// under a name is still the one that was made, and not another that took its name.
type FileID struct {
	Path    string
	Exists  bool
	Regular bool
	// ID is empty when the file system offers no identity: such a file cannot be proved to be the same one.
	ID     string
	Size   uint64 // apparent size
	Blocks uint64 // bytes allocated
}

// FileIDs reports the identity of each path. A symbolic link or a directory exists but is not Regular.
func (h *Host) FileIDs(paths ...string) ([]FileID, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out, err := h.run("file-id", paths...)
	if err != nil {
		return nil, nodeErr(out, err)
	}
	return parseFileIDs(paths, out)
}

// FileIDsCtx is FileIDs that gives up when ctx ends (a share that stopped answering): the node command is hung up,
// and nothing was changed, as file-id only looks.
func (h *Host) FileIDsCtx(ctx context.Context, paths ...string) ([]FileID, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out, err := h.RunNodeCtx(ctx, "file-id", paths...)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("no answer while identifying files: %w", ctx.Err())
	}
	if err != nil {
		return nil, nodeErr(out, err)
	}
	return parseFileIDs(paths, out)
}

func parseFileIDs(paths []string, out string) ([]FileID, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != len(paths) {
		return nil, fmt.Errorf("unexpected answer while identifying files: %s", firstLineOf(out, errors.New("empty")))
	}
	res := make([]FileID, len(paths))
	for i, l := range lines {
		res[i].Path = paths[i]
		switch l = strings.TrimSpace(l); l {
		case "MISSING":
			continue
		case "OTHER":
			res[i].Exists = true
			continue
		}
		f := strings.Split(l, "|")
		if len(f) != 3 || f[0] == "" {
			return nil, fmt.Errorf("unexpected answer while identifying %s: %s", paths[i], l)
		}
		sz, e1 := strconv.ParseUint(f[1], 10, 64)
		bl, e2 := strconv.ParseUint(f[2], 10, 64)
		if e1 != nil || e2 != nil {
			return nil, fmt.Errorf("unexpected answer while identifying %s: %s", paths[i], l)
		}
		id := f[0]
		if id == "-" {
			id = ""
		} else if !validID(id) {
			return nil, fmt.Errorf("unexpected answer while identifying %s: %s", paths[i], l)
		}
		res[i] = FileID{Path: paths[i], Exists: true, Regular: true, ID: id, Size: sz, Blocks: bl}
	}
	return res, nil
}

func validID(s string) bool {
	if len(s) < 4 || len(s) > 300 || !(strings.HasPrefix(s, "fh-") || strings.HasPrefix(s, "ib-")) {
		return false
	}
	return strings.Trim(s[3:], "0123456789abcdef-.") == ""
}

// Answers of RemoveIfID.
const (
	RemovedFile = "removed" // the file was the one named and is gone
	MissingFile = "missing" // nothing was there
	OtherFile   = "differs" // another file (or not a regular file) has the name; it was left as it was
	InUseFile   = "inuse"   // it is the file, but a process (QEMU) holds a lock on it; it was left as it was
)

// RemoveIfID removes the regular file at path only when its identity is id, atomically: the node renames whatever
// has the name to a private name, checks that file, and removes it or puts it back. The answer is RemovedFile,
// MissingFile, OtherFile or InUseFile.
func (h *Host) RemoveIfID(path, id string) (string, error) {
	if !validID(id) {
		return "", errors.New("no identity to compare with")
	}
	out, err := h.run("remove-id", path, id)
	if err != nil {
		return "", nodeErr(out, err)
	}
	switch a := strings.TrimSpace(out); a {
	case RemovedFile, MissingFile, OtherFile, InUseFile:
		return a, nil
	}
	return "", fmt.Errorf("unexpected answer while removing %s: %s", path, firstLineOf(out, errors.New("empty")))
}

// RemoveIfIDCtx is RemoveIfID that stops waiting when ctx ends. The node command is hung up then; if it had moved
// the file aside already it puts it back (or has removed it), so the caller must look again before it decides
// anything: an error that wraps ctx.Err() means the outcome is not known.
func (h *Host) RemoveIfIDCtx(ctx context.Context, path, id string) (string, error) {
	if !validID(id) {
		return "", errors.New("no identity to compare with")
	}
	out, err := h.RunNodeCtx(ctx, "remove-id", path, id)
	if ctx.Err() != nil {
		return "", fmt.Errorf("no answer while removing %s: %w", path, ctx.Err())
	}
	if err != nil {
		return "", nodeErr(out, err)
	}
	switch a := strings.TrimSpace(out); a {
	case RemovedFile, MissingFile, OtherFile, InUseFile:
		return a, nil
	}
	return "", fmt.Errorf("unexpected answer while removing %s: %s", path, firstLineOf(out, errors.New("empty")))
}

// ImageNew creates a new, empty disk image (format raw or qcow2, size bytes; backing and its format for a qcow2
// overlay, or empty) and returns its identity. An existing file is never replaced.
func (h *Host) ImageNew(path, format string, size uint64, backing, backingFormat string) (string, error) {
	args := []string{path, format, strconv.FormatUint(size, 10)}
	if backing != "" {
		args = append(args, backing, backingFormat)
	}
	out, err := h.run("img-new", args...)
	if err != nil {
		return "", nodeErr(out, err)
	}
	return answerID(out)
}

// NewNVRAMID is NewNVRAM that returns the identity of the store it made.
func (h *Host) NewNVRAMID(template, dst string) (string, error) {
	out, err := h.run("nvram-new", template, dst)
	if err != nil {
		return "", nodeErr(out, err)
	}
	return answerID(out)
}

// answerID reads the identity a creating verb prints; "-" (no identity on that file system) is "".
func answerID(out string) (string, error) {
	id := strings.TrimSpace(out)
	if id == "-" {
		return "", nil
	}
	if !validID(id) {
		return "", fmt.Errorf("unexpected answer of the node: %q", firstLineOf(out, errors.New("empty")))
	}
	return id, nil
}

// PeerSendID is PeerSend that returns the identity of the file the peer made ("" when its file system offers none).
func (h *Host) PeerSendID(ctx context.Context, host, port, user, src, dst, mode string, rate uint64, progress func(done, total uint64)) (string, error) {
	return h.peerSend(ctx, host, port, user, src, dst, mode, rate, progress)
}

// doneID reads the last line of peer-send, "@@done SIZE ID".
func doneID(l string) string {
	f := strings.Fields(l)
	if len(f) == 3 && f[2] != "-" && validID(f[2]) {
		return f[2]
	}
	return ""
}
