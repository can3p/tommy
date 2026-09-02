package trap

import (
	"context"
	"net"

	"github.com/gosnmp/gosnmp"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/snmp"
)

// handleDatagram decodes one UDP datagram, records it, and - for an inform,
// the only case SNMP actually requires a reply for - writes a GetResponse
// back to the sender. It runs in its own goroutine per datagram so a slow
// store or a stalled write never holds up the listener's read loop.
func handleDatagram(ctx context.Context, conn net.PacketConn, decoder *gosnmp.GoSNMP, datagram []byte, peer net.Addr, d plugin.Deps) {
	pkt, err := decoder.UnmarshalTrap(datagram, false)
	if err != nil {
		record(ctx, d, &snmp.Trap{DecodeError: err.Error()}, datagram, peer)
		return
	}

	t := fromPacket(pkt)
	record(ctx, d, t, datagram, peer)

	if !t.Inform {
		// A trap - v1 or v2c - is unconfirmed by design (RFC 3416 §4.2.6):
		// no reply, ever.
		return
	}
	if err := reply(conn, pkt, peer); err != nil {
		d.Logger.Warn("send inform response", "peer", peer.String(), "err", err)
	}
}

func record(ctx context.Context, d plugin.Deps, t *snmp.Trap, datagram []byte, peer net.Addr) {
	// The datagram bytes are shared with the event once appended (Event.Raw
	// is treated as immutable, see core/event) but ReadFrom's buffer is
	// fresh per read already - see provider.go's Listen loop - so no copy is
	// needed here.
	ev := snmp.NewEvent(ProviderName, t, datagram, peer.String())
	if err := d.Append(ctx, ev); err != nil {
		d.Logger.Warn("append snmp trap event", "peer", peer.String(), "err", err)
	}
}
