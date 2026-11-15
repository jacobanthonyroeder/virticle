package hostconn

// Encryption of backup files on the node (the node command's age-encrypt and age-decrypt verbs). The identity for a
// decryption goes to the node on the session's stdin, never on a command line.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// AgeSums are the checksums and sizes of both forms of a file, taken in the one pass that encrypted or decrypted it.
type AgeSums struct {
	PlainSHA    string
	PlainBytes  uint64
	StoredSHA   string // of the encrypted file
	StoredBytes uint64
}

// ErrNoAge means the node command is older than the encryption of backups.
var ErrNoAge = errors.New("prepare the node again (virticle node prepare)")

func validSHA(s string) bool { return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == "" }

// parseAgeSums reads the last line of age-encrypt (plain first) or age-decrypt (stored first).
func parseAgeSums(l string, plainFirst bool) (AgeSums, bool) {
	f := strings.Fields(l)
	if len(f) != 4 || !validSHA(f[0]) || !validSHA(f[2]) {
		return AgeSums{}, false
	}
	a, e1 := strconv.ParseUint(f[1], 10, 64)
	b, e2 := strconv.ParseUint(f[3], 10, 64)
	if e1 != nil || e2 != nil {
		return AgeSums{}, false
	}
	if plainFirst {
		return AgeSums{PlainSHA: f[0], PlainBytes: a, StoredSHA: f[2], StoredBytes: b}, true
	}
	return AgeSums{StoredSHA: f[0], StoredBytes: a, PlainSHA: f[2], PlainBytes: b}, true
}

// ageLines collects the output of an age verb: progress goes to progress, the rest is kept (the last lines).
type ageLines struct {
	progress func(done, total uint64)
	last     string
	tail     []string
	part     []byte
}

func (a *ageLines) line(l string) {
	if d, t, ok := parseProgress(l); ok {
		if a.progress != nil {
			a.progress(d, t)
		}
		return
	}
	if l = strings.TrimSpace(l); l == "" || strings.Contains(l, "AGE-SECRET-KEY") {
		return // a terminal that echoed stdin must not put the identity into a message
	}
	a.last = l
	if a.tail = append(a.tail, l); len(a.tail) > 3 {
		a.tail = a.tail[1:]
	}
}

func (a *ageLines) Write(p []byte) (int, error) {
	a.part = append(a.part, p...)
	for {
		i := bytes.IndexByte(a.part, '\n')
		if i < 0 {
			break
		}
		a.line(strings.TrimRight(string(a.part[:i]), "\r"))
		a.part = a.part[i+1:]
	}
	return len(p), nil
}

func (a *ageLines) result(h *Host, verb, src string, err error, plainFirst bool) (AgeSums, error) {
	if sums, ok := parseAgeSums(a.last, plainFirst); ok && err == nil {
		return sums, nil
	}
	msg := strings.Join(a.tail, "; ")
	if msg == "" && err != nil {
		msg = err.Error()
	}
	if strings.Contains(msg, "unknown subcommand") {
		return AgeSums{}, fmt.Errorf("the node command on %s is too old to %s backups: %w", h.Name, verb, ErrNoAge)
	}
	msg = strings.TrimSpace(strings.ReplaceAll(msg, "virticle-node: ", ""))
	if msg == "" {
		msg = "no answer"
	}
	return AgeSums{}, fmt.Errorf("%s %s on %s: %s", verb, src, h.Name, msg)
}

// CanAge checks that the node command has the age verbs (without them it answers "unknown subcommand").
func (h *Host) CanAge() error {
	out, err := h.RunNode("age-encrypt")
	if err != nil && strings.Contains(out, "unknown subcommand") {
		return fmt.Errorf("the node command on %s is too old to encrypt backups: %w", h.Name, ErrNoAge)
	}
	if errors.Is(err, ErrNoPrivilege) {
		return err
	}
	return nil
}

// AgeEncrypt encrypts src into dst (both in one work directory) to the age recipient. progress receives the bytes
// read so far and the size of src about once a second. Cancelling ctx stops it; dst is then not there.
func (h *Host) AgeEncrypt(ctx context.Context, src, dst, recipient string, progress func(done, total uint64)) (AgeSums, error) {
	cmd, err := h.nodeCmd("age-encrypt", src, dst, recipient)
	if err != nil {
		return AgeSums{}, err
	}
	a := &ageLines{progress: progress}
	err = h.Stream(ctx, cmd, a.line)
	if ctx.Err() != nil {
		return AgeSums{}, ctx.Err()
	}
	return a.result(h, "encrypt", src, err, true)
}

// AgeDecrypt decrypts src into dst, a new file in a work directory, with the identity ("AGE-SECRET-KEY-1..."),
// which the node reads from stdin. The sums say what was read and written; the caller compares them with the
// manifest. Cancelling ctx stops it; dst is then not there.
func (h *Host) AgeDecrypt(ctx context.Context, src, dst string, identity []byte, progress func(done, total uint64)) (AgeSums, error) {
	cmd, err := h.nodeCmd("age-decrypt", src, dst)
	if err != nil {
		return AgeSums{}, err
	}
	a := &ageLines{progress: progress}
	in := make([]byte, 0, len(identity)+1)
	in = append(append(in, identity...), '\n')
	defer clear(in)
	// No terminal: stdin is a plain pipe, so nothing can echo the identity. A cancelled session closes the pipes,
	// and the verb ends at its next progress line (SIGPIPE).
	err = h.Pipe(ctx, cmd, bytes.NewReader(in), a)
	if ctx.Err() != nil {
		return AgeSums{}, ctx.Err()
	}
	if err != nil {
		// Pipe puts the node's stderr behind the status; keep the node's words only.
		if _, msg, ok := strings.Cut(err.Error(), ": "); ok {
			a.line(msg)
		}
		err = errors.New("failed")
	}
	return a.result(h, "decrypt", src, err, false)
}
