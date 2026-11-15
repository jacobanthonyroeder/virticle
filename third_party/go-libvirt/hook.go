package libvirt

import (
	"bytes"

	"github.com/digitalocean/go-libvirt/internal/constants"
	xdr "github.com/digitalocean/go-libvirt/internal/go-xdr/xdr2"
)

// Added for Virticle (see README-virticle.md): a hook that sees the calls made
// through one view of a connection, for a record of what a request did.

// The RPC programs a CallHook sees.
const (
	ProgramRemote = constants.Program
	ProgramQEMU   = constants.QEMUProgram
)

// CallHook is called before a remote procedure call with the program, the
// procedure number and the XDR-encoded arguments. The function it returns, if
// not nil, is called when the call has ended, with its error. A hook must not
// keep or change payload.
type CallHook func(program, proc uint32, payload []byte) func(err error)

// WithCallHook returns a view of l that shares its connection and calls h for
// every call made through the view. Calls made through l itself, or through
// other views, do not reach h. Connect and Disconnect act on the shared
// connection.
func (l *Libvirt) WithCallHook(h CallHook) *Libvirt {
	return &Libvirt{rpcState: l.rpcState, hook: h}
}

// DecodeArgs decodes the arguments of a call, as a CallHook gets them, into v:
// a pointer to the procedure's Args struct (DomainCreateArgs for
// DomainCreate, for example).
func DecodeArgs(payload []byte, v interface{}) error {
	dec := xdr.NewDecoderCustomTypes(bytes.NewReader(payload), 0, map[string]xdr.TypeDecoder{"libvirt.TypedParam": typedParamDecoder{}})
	_, err := dec.Decode(v)
	return err
}

// EncodeArgs encodes the Args struct of a call as the call sends it: the reverse of DecodeArgs, for tests.
func EncodeArgs(v interface{}) ([]byte, error) { return encode(v) }
