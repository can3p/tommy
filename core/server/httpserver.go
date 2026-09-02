package server

import (
	"net"
	"net/http"
	"time"
)

// listenerOptions are the transport-level settings of one core listener - what
// is decided per connection rather than per route.
//
// It exists as a seam. Which protocols a listener speaks cannot be decided by
// the router, because both h2c and (later) TLS are settled from the first bytes
// of a connection, long before there is a request to route. Every core listener
// is therefore built through newHTTPServer, and the Wave 9 --tls mode adds a
// field here rather than a second way of constructing a server.
type listenerOptions struct {
	// H2C serves cleartext HTTP/2 alongside HTTP/1.1 on the same port.
	H2C bool

	// ConnState observes every connection's lifecycle. Shutdown uses it to
	// find connections that never carried a request; see httpListener.trackConn.
	ConnState func(net.Conn, http.ConnState)
}

// newHTTPServer builds one core listener's *http.Server.
//
// h2c is the net/http implementation (Server.Protocols with
// SetUnencryptedHTTP2), not golang.org/x/net/http2/h2c, which has been
// deprecated in favor of it. It accepts a connection that opens with the
// HTTP/2 client preface - "prior knowledge", the only cleartext HTTP/2
// handshake RFC 9113 still defines - and serves everything else as HTTP/1.1 on
// the very same port, so enabling it takes nothing away from an HTTP/1.1
// client. The older Upgrade: h2c handshake is not supported by net/http and is
// deliberately not reimplemented: it is deprecated, and a client that tries it
// simply gets its request answered over HTTP/1.1.
func newHTTPServer(h http.Handler, opts listenerOptions) *http.Server {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(opts.H2C)

	return &http.Server{
		Handler:           h,
		Protocols:         protocols,
		ConnState:         opts.ConnState,
		ReadHeaderTimeout: 15 * time.Second,
		// No WriteTimeout: SSE responses are long-lived by design.
	}
}
