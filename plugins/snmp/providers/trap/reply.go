package trap

import (
	"fmt"
	"net"

	"github.com/gosnmp/gosnmp"
)

// reply answers an InformRequest with a GetResponse.
//
// This is the one reply this plugin ever sends, and it is mechanical in
// exactly the sense the project scopes itself to (CLAUDE.md, "the reply is
// derivable from the request"): RFC 3416 §4.2.7 requires the response to
// carry the inform's own request id and echo its varbind list back
// unchanged, with error-status/error-index cleared. There is no content
// decision here - not even the acknowledgement classification hl7's MLLP
// provider makes - just the shape the protocol itself demands.
func reply(conn net.PacketConn, request *gosnmp.SnmpPacket, peer net.Addr) error {
	resp := *request
	resp.PDUType = gosnmp.GetResponse
	resp.Error = gosnmp.NoError
	resp.ErrorIndex = 0

	data, err := resp.MarshalMsg()
	if err != nil {
		return fmt.Errorf("marshal GetResponse: %w", err)
	}
	if _, err := conn.WriteTo(data, peer); err != nil {
		return fmt.Errorf("write GetResponse to %s: %w", peer, err)
	}
	return nil
}
