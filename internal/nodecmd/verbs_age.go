package nodecmd

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"filippo.io/age"
	"golang.org/x/sys/unix"
)

// Encryption of backup files with age (https://age-encryption.org/v1), streaming, on the node that holds the file:
//
//	age-encrypt SRC DST RECIPIENT   encrypt SRC to the public key RECIPIENT ("age1..."); SRC and DST are in the same
//	                                work directory, DST is new
//	age-decrypt SRC DST             decrypt SRC into DST, a new file in a work directory; the identity
//	                                ("AGE-SECRET-KEY-1...") is the first line of stdin, never an argument
//
// Both print "@@progress DONE TOTAL" (bytes of SRC read, its size) once a second while they work, then one line:
//
//	encrypt: PLAIN_SHA256 PLAIN_BYTES STORED_SHA256 STORED_BYTES
//	decrypt: STORED_SHA256 STORED_BYTES PLAIN_SHA256 PLAIN_BYTES
//
// so the checksums of both forms of a file are taken in the one pass. DST appears only complete (CreateNew); a
// file that does not decrypt (another key, damaged, cut short) leaves nothing. Runs of zeros in a decrypted file
// are left as holes, so a qcow2 image does not grow on disk.

func init() {
	Register("age-encrypt", verbAgeEncrypt)
	Register("age-decrypt", verbAgeDecrypt)
}

// ageFiles resolves SRC (an existing regular file) and DST (a new name in a work directory).
func (c *Ctx) ageFiles() (src, dst *Ref, size int64, err error) {
	if src, err = c.ResolveRead(c.Args[0]); err != nil {
		return nil, nil, 0, err
	}
	st, exists, _ := src.Lstat()
	if !exists || !isRegMode(st.Mode) {
		src.Close()
		return nil, nil, 0, Fail("%s is not a regular file", c.Args[0])
	}
	if dst, err = c.ResolveNew(c.Args[1]); err != nil {
		src.Close()
		return nil, nil, 0, err
	}
	if !WorkName(dst.Dir) {
		src.Close()
		dst.Close()
		return nil, nil, 0, Die("%s is not in a work directory", c.Args[1])
	}
	return src, dst, st.Size, nil
}

// ageProgress prints the progress line once a second until the returned function is called.
func (c *Ctx) ageProgress(n *atomic.Int64, total int64) func() {
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				fmt.Fprintf(c.Stdout, "@@progress %d %d\n", n.Load(), total)
			}
		}
	}()
	return func() {
		close(stop)
		<-stopped
	}
}

// hashCount hashes and counts what passes through it. The hashing runs in a goroutine of its own, so the two
// checksums and the encryption use separate cores: a copy of each block is queued (at most a few MiB).
type hashCount struct {
	h    hash.Hash
	n    atomic.Int64
	q    chan []byte
	done chan struct{}
	free chan []byte
}

func newHashCount() *hashCount {
	hc := &hashCount{h: sha256.New(), q: make(chan []byte, 4), done: make(chan struct{}), free: make(chan []byte, 6)}
	go func() {
		defer close(hc.done)
		for b := range hc.q {
			hc.h.Write(b)
			select {
			case hc.free <- b[:0]:
			default:
			}
		}
	}()
	return hc
}

func (hc *hashCount) Write(p []byte) (int, error) {
	var b []byte
	select {
	case b = <-hc.free:
	default:
	}
	hc.q <- append(b, p...)
	hc.n.Add(int64(len(p)))
	return len(p), nil
}

// sum ends the hashing and returns the digest; call it once, after the last Write.
func (hc *hashCount) sum() string {
	close(hc.q)
	<-hc.done
	return hex.EncodeToString(hc.h.Sum(nil))
}

// ctxReader stops reading once the verb's context is cancelled (a signal).
type ctxReader struct {
	c *Ctx
	r io.Reader
}

func (r ctxReader) Read(p []byte) (int, error) {
	if r.c.Context != nil && r.c.Context.Err() != nil {
		return 0, r.c.Context.Err()
	}
	return r.r.Read(p)
}

func verbAgeEncrypt(c *Ctx) error {
	if err := c.Need(3, 3, "age-encrypt SRC DST RECIPIENT"); err != nil {
		return err
	}
	rec, err := age.ParseX25519Recipient(c.Args[2])
	if err != nil {
		return Die("unsupported recipient")
	}
	src, dst, size, err := c.ageFiles()
	if err != nil {
		return err
	}
	defer src.Close()
	defer dst.Close()
	if src.Dir != dst.Dir {
		return Die("SRC and DST must be in the same work directory")
	}
	in, err := src.Open(unix.O_RDONLY, 0)
	if err != nil {
		return Fail("cannot read %s", c.Args[0])
	}
	defer in.Close()
	plain, stored := newHashCount(), newHashCount()
	err = c.CreateNew(dst, 0o600, func(f *os.File) error {
		bw := bufio.NewWriterSize(io.MultiWriter(f, stored), 1<<20)
		w, err := age.Encrypt(bw, rec)
		if err != nil {
			return Fail("cannot encrypt: %v", err)
		}
		done := c.ageProgress(&plain.n, size)
		_, err = io.CopyBuffer(w, io.TeeReader(ctxReader{c, in}, plain), make([]byte, 1<<20))
		done()
		if err != nil {
			return Fail("cannot encrypt %s: %v", c.Args[0], err)
		}
		if err := w.Close(); err != nil {
			return Fail("cannot write %s: %v", c.Args[1], err)
		}
		if err := bw.Flush(); err != nil {
			return Fail("cannot write %s: %v", c.Args[1], err)
		}
		if err := f.Sync(); err != nil {
			return Fail("cannot write %s: %v", c.Args[1], err)
		}
		if plain.n.Load() != size {
			return Fail("%s changed size while it was encrypted", c.Args[0])
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Stdout, "%s %d %s %d\n", plain.sum(), plain.n.Load(), stored.sum(), stored.n.Load())
	return nil
}

// readIdentity reads the identity from the first line of stdin.
func (c *Ctx) readIdentity() (*age.X25519Identity, error) {
	line, err := bufio.NewReader(io.LimitReader(c.Stdin, 4096)).ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return nil, Die("the identity is expected on stdin")
	}
	id, err := age.ParseX25519Identity(strings.TrimSpace(line))
	if err != nil {
		return nil, Die("the identity on stdin is not an age X25519 identity")
	}
	return id, nil
}

// holeWriter writes p, leaving blocks that are all zero as holes; Finish sets the length.
type holeWriter struct {
	f   *os.File
	off int64
}

var zeroBlock = make([]byte, 64<<10)

func (w *holeWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		k := min(len(p), len(zeroBlock))
		b := p[:k]
		if !isZero(b) {
			if _, err := w.f.WriteAt(b, w.off); err != nil {
				return 0, err
			}
		}
		w.off += int64(k)
		p = p[k:]
	}
	return n, nil
}

func isZero(b []byte) bool {
	for len(b) >= 8 {
		if b[0]|b[1]|b[2]|b[3]|b[4]|b[5]|b[6]|b[7] != 0 {
			return false
		}
		b = b[8:]
	}
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func (w *holeWriter) Finish() error { return w.f.Truncate(w.off) }

func verbAgeDecrypt(c *Ctx) error {
	if err := c.Need(2, 2, "age-decrypt SRC DST"); err != nil {
		return err
	}
	id, err := c.readIdentity()
	if err != nil {
		return err
	}
	src, dst, size, err := c.ageFiles()
	if err != nil {
		return err
	}
	defer src.Close()
	defer dst.Close()
	in, err := src.Open(unix.O_RDONLY, 0)
	if err != nil {
		return Fail("cannot read %s", c.Args[0])
	}
	defer in.Close()
	plain, stored := newHashCount(), newHashCount()
	err = c.CreateNew(dst, 0o600, func(f *os.File) error {
		done := c.ageProgress(&stored.n, size)
		defer done()
		r, err := age.Decrypt(bufio.NewReaderSize(io.TeeReader(ctxReader{c, in}, stored), 1<<20), id)
		if err != nil {
			var nm *age.NoIdentityMatchError
			if errors.As(err, &nm) {
				return Fail("%s is not encrypted to this backup key", c.Args[0])
			}
			return Fail("%s is not a readable age file: %v", c.Args[0], err)
		}
		hw := &holeWriter{f: f}
		if _, err := io.CopyBuffer(io.MultiWriter(hw, plain), r, make([]byte, 1<<20)); err != nil {
			return Fail("%s does not decrypt: it is damaged or incomplete (%v)", c.Args[0], err)
		}
		if err := hw.Finish(); err != nil {
			return Fail("cannot write %s: %v", c.Args[1], err)
		}
		// age reads only as far as the last chunk: anything after it is not part of the file.
		if rest, _ := io.Copy(io.Discard, io.TeeReader(in, stored)); rest > 0 {
			return Fail("%s has data after its end: it is damaged", c.Args[0])
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Stdout, "%s %d %s %d\n", stored.sum(), stored.n.Load(), plain.sum(), plain.n.Load())
	return nil
}
