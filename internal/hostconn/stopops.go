package hostconn

// Operations that can be stopped from the controller: a task the user stops ends the work on the node too.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// RunNodeCtx is RunNode for a verb that can run long. Cancelling ctx hangs the session up: it has a terminal,
// so the node command and what it started get SIGHUP and end. The error is ctx.Err() then.
func (h *Host) RunNodeCtx(ctx context.Context, verb string, args ...string) (string, error) {
	cmd, err := h.nodeCmd(verb, args...)
	if err != nil {
		return err.Error(), err
	}
	var lines []string
	err = h.Stream(ctx, cmd, func(l string) { lines = append(lines, l) })
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), err
}

// SHA256Ctx is SHA256 that ctx can interrupt.
func (h *Host) SHA256Ctx(ctx context.Context, path string) (string, error) {
	out, err := h.RunNodeCtx(ctx, "sha256", path)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	f := strings.Fields(out)
	if err != nil || len(f) < 1 || len(f[0]) != 64 || strings.Trim(f[0], "0123456789abcdef") != "" {
		return "", fmt.Errorf("sha256 of %s: %s", path, firstLineOf(out, errors.New("no checksum")))
	}
	return f[0], nil
}

// TarExtractCtx is TarExtract that ctx can interrupt; the members written so far stay in the work directory.
func (h *Host) TarExtractCtx(ctx context.Context, file, workDir string, names ...string) (string, error) {
	return h.RunNodeCtx(ctx, "tar-extract", append([]string{file, workDir}, names...)...)
}

// CopyKill stops the qemu-img process that is writing the pool file dest (the copy libvirt makes for a volume
// created from another volume). It returns an error when none is running, or when the node command is too old
// to know the verb (ErrNoCopyKill).
func (h *Host) CopyKill(dest string) error {
	out, err := h.RunNode("copy-kill", dest)
	if err == nil {
		return nil
	}
	if strings.Contains(out, "unknown subcommand") {
		return ErrNoCopyKill
	}
	return errors.New(nodeCmdMessage(out, err))
}

// ErrNoCopyKill means the node cannot stop a volume copy: its node command has no copy-kill verb yet.
var ErrNoCopyKill = errors.New("this node's command cannot stop a volume copy (update it with: virticle node prepare)")
