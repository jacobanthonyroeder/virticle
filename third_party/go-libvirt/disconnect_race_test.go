package libvirt

import (
	"net"
	"testing"
	"time"

	"github.com/digitalocean/go-libvirt/internal/constants"
	"github.com/digitalocean/go-libvirt/socket"
)

// pipeDialer hands out the client end of a pipe whose server end answers the
// connect handshake and then hangs up at once: the peer went away just as the
// connection was made.
type pipeDialer struct{}

func (pipeDialer) Dial() (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		srv := &consoleTestServer{conn: server}
		defer server.Close()
		for {
			hdr, _, err := srv.readPacket()
			if err != nil {
				return
			}
			switch hdr.Procedure {
			case constants.ProcAuthList:
				_ = srv.writePacket(hdr.Serial, hdr.Procedure, socket.Reply, socket.StatusOK, []byte{0, 0, 0, 0})
			case constants.ProcConnectOpen:
				_ = srv.writePacket(hdr.Serial, hdr.Procedure, socket.Reply, socket.StatusOK, nil)
				return
			}
		}
	}()
	return client, nil
}

// TestDisconnectedFiresWhenPeerLeavesDuringConnect: a connection that is lost
// right after the handshake must still be reported by Disconnected(). The base
// version made the channel only after the goroutine that closes it on a lost
// connection had started; when the loss came first, that goroutine closed the
// old channel and Disconnected() returned a new one that nothing ever closed.
func TestDisconnectedFiresWhenPeerLeavesDuringConnect(t *testing.T) {
	for i := 0; i < 300; i++ {
		l := NewWithDialer(pipeDialer{})
		if err := l.Connect(); err != nil {
			continue // the loss came before the reply: an error is a fine answer too
		}
		select {
		case <-l.Disconnected():
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d: the connection is gone but Disconnected() never fired", i)
		}
	}
}
