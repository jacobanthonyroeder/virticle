// Package optrace records what a request or a task really did on the nodes: the libvirt calls that change
// something (with the virsh command that does the same, see virsh.go) and the programs run on a node. Reads are
// not recorded.
//
// A Trace is made for each request that changes something and travels in its context; a task started by the
// request shares it. internal/hostconn hands it every call made through a host view bound to it
// (Host.Traced), and the API stores each operation when it ends. Secrets are removed here, before anything is
// stored (redact.go).
package optrace

//go:generate sh -c "awk -f procs.awk ../../third_party/go-libvirt/internal/constants/remote_protocol.gen.go ../../third_party/go-libvirt/internal/constants/qemu_protocol.gen.go > procs.gen.go && gofmt -w procs.gen.go"

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"virticle/internal/nodeproto"
)

// Bounds of a trace.
const (
	MaxOps  = 200      // operations kept per trace; one KindLimit entry marks the rest
	MaxFile = 16 << 10 // bytes kept of an XML document
	MaxCmd  = 4 << 10  // bytes kept of a command line
	MaxErr  = 1 << 10  // bytes kept of an error message
)

// Kinds of operation.
const (
	KindLibvirt = "libvirt" // a libvirt API call
	KindExec    = "exec"    // a program run on the node over SSH
	KindLimit   = "limit"   // MaxOps was reached: later operations were not recorded
)

// File is an XML document a libvirt call carried, under the name its virsh line uses.
type File struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// Op is one operation on a node.
type Op struct {
	Seq  int    `json:"seq"`
	Time int64  `json:"time"` // start, unix milliseconds
	Node string `json:"node"`
	Kind string `json:"kind"`
	// API is the libvirt function (virDomainSnapshotCreateXML) or the program that ran (virticle-node
	// clone-disk, sh).
	API string `json:"api"`
	// Cmd is, for a libvirt call, the virsh command that does the same (empty when there is none); for a
	// program, the command line as it ran.
	Cmd string `json:"cmd,omitempty"`
	// Args are the main arguments of a libvirt call that has no virsh equivalent.
	Args  string `json:"args,omitempty"`
	Files []File `json:"files,omitempty"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Ms    int64  `json:"ms"`
}

// Trace collects the operations of one request (and the tasks it started).
type Trace struct {
	sink func(id int64, op Op)

	mu sync.Mutex
	id int64
	n  int
}

// New makes a trace; sink stores an operation when it has ended. sink may be called from several goroutines.
func New(sink func(id int64, op Op)) *Trace { return &Trace{sink: sink} }

// ID returns the trace's id, giving it one on first use. A request that never asks has no id and costs nothing
// to store.
func (t *Trace) ID() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.id == 0 {
		t.id = newID()
	}
	return t.id
}

// Assigned is the id when the trace has one, else 0.
func (t *Trace) Assigned() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.id
}

var lastID atomic.Int64

// newID is unique within the process and grows with time (microseconds), so ids stay unique across restarts.
func newID() int64 {
	for {
		old := lastID.Load()
		id := max(time.Now().UnixMicro(), old+1)
		if lastID.CompareAndSwap(old, id) {
			return id
		}
	}
}

// begin numbers an operation; ok is false when the trace is full.
func (t *Trace) begin() (seq int, ok bool) {
	t.mu.Lock()
	if t.id == 0 {
		t.id = newID()
	}
	t.n++
	n, id := t.n, t.id
	t.mu.Unlock()
	if n == MaxOps+1 && t.sink != nil {
		t.sink(id, Op{Seq: n, Time: time.Now().UnixMilli(), Kind: KindLimit, OK: true})
	}
	return n, n <= MaxOps
}

func (t *Trace) end(op Op, start time.Time) {
	op.Ms = time.Since(start).Milliseconds()
	op.Cmd = cut(RedactText(op.Cmd), MaxCmd)
	op.Args = cut(RedactText(op.Args), MaxCmd)
	op.Error = cut(RedactText(op.Error), MaxErr)
	for i := range op.Files {
		op.Files[i].Text = cutXML(RedactXML(op.Files[i].Text))
	}
	if t.sink != nil {
		t.sink(t.Assigned(), op)
	}
}

// Libvirt is called before a libvirt call made through a traced host (see go-libvirt's CallHook). It returns
// what to call when the call has ended, or nil for a read, which is not recorded.
func (t *Trace) Libvirt(node string, program, proc uint32, payload []byte) func(error) {
	if t == nil {
		return nil
	}
	op, rec := Describe(program, proc, payload)
	if !rec {
		return nil
	}
	seq, ok := t.begin()
	if !ok {
		return nil
	}
	start := time.Now()
	op.Seq, op.Time, op.Node, op.Kind = seq, start.UnixMilli(), node, KindLibvirt
	return func(err error) {
		op.OK = err == nil
		if err != nil {
			op.Error = err.Error()
		}
		t.end(op, start)
	}
}

// Exec is called before a command runs on a node over SSH. The function it returns gets the exit status (-1
// when the command did not run to an end) and the error. It returns nil for a command that only reads (see
// ReadOnlyCommand), which is not recorded.
func (t *Trace) Exec(node, cmd string) func(exit int, err error) {
	if t == nil {
		return nil
	}
	if ReadOnlyCommand(cmd) {
		return nil
	}
	seq, ok := t.begin()
	if !ok {
		return nil
	}
	start := time.Now()
	op := Op{Seq: seq, Time: start.UnixMilli(), Node: node, Kind: KindExec, API: program(cmd), Cmd: RedactCommand(cmd)}
	return func(exit int, err error) {
		op.OK = exit == 0 && err == nil
		switch {
		case exit > 0:
			op.Error = "exit status " + strconv.Itoa(exit)
		case err != nil && !errors.Is(err, context.Canceled):
			op.Error = err.Error()
		case err != nil:
			op.Error = "stopped"
		}
		t.end(op, start)
	}
}

// NodeCmdPath is where the node command lives. internal/nodeproto is a leaf package (no controller packages).
const NodeCmdPath = nodeproto.Path

// bare drops what only wraps a command: sudo, timeout, exec and the like.
func bare(f []string) []string {
	for len(f) > 0 {
		switch f[0] {
		case "sudo", "-n", "exec", "nice", "ionice", "nohup", "env":
			f = f[1:]
			continue
		case "timeout":
			f = f[1:]
			if len(f) > 0 && strings.Trim(f[0], "0123456789smh.") == "" {
				f = f[1:]
			}
			continue
		}
		break
	}
	return f
}

// program names what a command line runs: "virticle-node VERB" for the node command, else its first program
// after sudo, timeout, exec and the like.
func program(cmd string) string {
	f := bare(strings.Fields(cmd))
	if len(f) == 0 {
		return "sh"
	}
	if f[0] == NodeCmdPath {
		if len(f) > 1 {
			return "virticle-node " + strings.Trim(f[1], "'")
		}
		return "virticle-node"
	}
	p := f[0]
	if i := strings.LastIndexByte(p, '/'); i >= 0 && !strings.ContainsAny(p, "'\"$;|&") {
		p = p[i+1:]
	}
	if strings.ContainsAny(p, "'\"$;|&()=") || p == "" {
		return "sh"
	}
	return p
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	n = runeStart(s, n)
	return s[:n] + " [cut: " + strconv.Itoa(len(s)-n) + " more bytes]"
}

func cutXML(s string) string {
	if len(s) <= MaxFile {
		return s
	}
	n := runeStart(s, MaxFile)
	return s[:n] + "\n<!-- cut: " + strconv.Itoa(len(s)-n) + " more bytes -->"
}

// runeStart moves n back to the start of a UTF-8 character.
func runeStart(s string, n int) int {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

type ctxKey struct{}

// With returns ctx carrying t.
func With(ctx context.Context, t *Trace) context.Context { return context.WithValue(ctx, ctxKey{}, t) }

// From returns the trace in ctx, or nil.
func From(ctx context.Context) *Trace {
	t, _ := ctx.Value(ctxKey{}).(*Trace)
	return t
}
