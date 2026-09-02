package mllp

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/hl7"
)

// handleConn serves one accepted connection until it closes, ctx is
// canceled, or a write fails. Each frame is captured as its own event and
// answered with its own acknowledgement, so a pipelined connection produces
// one event and one ACK per message, in order.
func handleConn(ctx context.Context, conn net.Conn, cfg Config, d plugin.Deps) {
	defer func() { _ = conn.Close() }()

	// A read that is already blocked when ctx is canceled needs its own
	// nudge: Listen only closes the listener, not connections already
	// accepted, so nothing here would otherwise notice the server is
	// shutting down until the read or write timeout does it instead.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	fr := newFrameReader(conn, cfg.MaxMessageBytes)
	peer := conn.RemoteAddr().String()

	for {
		if cfg.ReadTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(cfg.ReadTimeout))
		}
		payload, err := fr.next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				d.Logger.Debug("mllp: connection ended", "peer", peer, "err", err)
			}
			return
		}

		msg, parseErr := hl7.Parse(payload)
		if parseErr != nil {
			// hl7.Parse fails only on hl7.ErrEmpty: a frame whose start and
			// end bytes had nothing usable between them. There is no
			// message to build an event or a correlated ACK from, so the
			// frame is dropped and the connection carries on to the next
			// one - this is "nothing was sent", not "a message failed".
			d.Logger.Debug("mllp: empty frame, no message to capture", "peer", peer)
			continue
		}

		ev := hl7.NewEvent(ProviderName, msg, payload)
		ev.Raw.PeerAddr = peer
		// hl7.NewEvent already fills Meta from the message itself; add the
		// transport details on top rather than replacing them (framing.go's
		// header comment, and CLAUDE.md's rule for provider Meta).
		ev.Meta["peer_addr"] = peer
		ev.Meta["local_addr"] = conn.LocalAddr().String()
		ev.Meta["framing"] = "mllp"

		if err := d.Append(ctx, ev); err != nil {
			d.Logger.Warn("mllp: append failed", "peer", peer, "err", err)
			return
		}

		code := classify(msg)
		ack := frame(buildACK(msg, code, d.Now(), d.NewID()))
		if cfg.WriteTimeout > 0 {
			_ = conn.SetWriteDeadline(time.Now().Add(cfg.WriteTimeout))
		}
		if _, err := conn.Write(ack); err != nil {
			d.Logger.Debug("mllp: writing ack failed", "peer", peer, "err", err)
			return
		}
	}
}
