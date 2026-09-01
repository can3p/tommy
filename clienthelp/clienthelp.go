// Package clienthelp gives official vendor SDKs a way to talk to tommy even
// when the SDK itself refuses to.
//
// Some SDKs make this trivial: mailjet-apiv3-go takes a base URL right in its
// constructor, and sendgrid-go's rest.Request has a BaseURL field. Others do
// not. twilio-go's RequestHandler.BuildUrl reparses and rebuilds the hostname
// as "product[.edge][.region].twilio.com" on every call — there is no field,
// env var, or option that lets that hostname become anything but a
// twilio.com subdomain. The only supported extension point is a custom
// *http.Client, injected via twilio.NewRestClientWithParams(ClientParams{
// Client: ...}).
//
// That is the case this package exists for: an SDK that will accept a custom
// *http.Client but will not accept a custom base URL. Transport wraps any
// RoundTripper and rewrites the scheme and host of every outbound request to
// point at tommy instead, leaving the path, query, headers and body exactly
// as the SDK built them — which is what lets tommy's fakes, which imitate
// the real path namespaces exactly, receive the request as if it had gone to
// the real vendor host.
//
// This package is pure standard library and always will be: it is meant to
// be imported by application and test code that also imports a vendor SDK,
// and tommy's own go.mod must never gain a transitive dependency on
// mailjet-apiv3-go, sendgrid-go or twilio-go just because clienthelp exists.
//
// Safety note: the rewrite is unconditional. A RoundTripper built by this
// package sends every request that passes through it to baseURL, regardless
// of what host the caller addressed. Use it only for an *http.Client (or
// sub-client) that is dedicated to talking to a tommy instance — never share
// it with code that must also reach the real vendor API.
package clienthelp

import (
	"fmt"
	"net/http"
	"net/url"
)

// transport rewrites the scheme and host of every request it is given before
// handing it to next.
type transport struct {
	next     http.RoundTripper
	base     *url.URL
	parseErr error
}

// Transport returns an http.RoundTripper that rewrites the scheme and host of
// every outbound request to baseURL (e.g. "http://127.0.0.1:8822"), leaving
// the request's path, query string, headers and body untouched. It is the
// answer for any SDK that accepts a custom *http.Client but not a custom base
// URL — point the client's Transport at this and every request the SDK
// builds for its real hardcoded host lands on tommy instead.
//
// The request passed to RoundTrip is never mutated: a clone carries the
// rewritten URL, per the http.RoundTripper contract that a RoundTripper must
// not modify the original request.
//
// baseURL is parsed once, eagerly. An invalid baseURL is not an error here —
// constructors that return a single value have nowhere to put one — instead
// every RoundTrip call fails with that parse error, which surfaces the
// mistake the first time the client is actually used.
func Transport(baseURL string) http.RoundTripper {
	return TransportWith(baseURL, nil)
}

// TransportWith is Transport, but lets the caller supply the underlying
// RoundTripper to send the rewritten request to. A nil next means
// http.DefaultTransport. This is the hook for a caller who also wants to
// layer on logging, retries, or a custom TLS config alongside the rewrite.
func TransportWith(baseURL string, next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	base, err := url.Parse(baseURL)
	return &transport{next: next, base: base, parseErr: err}
}

// RoundTrip implements http.RoundTripper.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.parseErr != nil {
		return nil, fmt.Errorf("clienthelp: invalid base URL: %w", t.parseErr)
	}

	// Clone rather than mutate: RoundTrip must leave the caller's Request
	// alone. Clone deep-copies the URL and headers; the body is left as-is,
	// which matches the RoundTripper contract that a body may be consumed but
	// the request otherwise must not be changed.
	out := req.Clone(req.Context())
	out.URL.Scheme = t.base.Scheme
	out.URL.Host = t.base.Host // carries the port, e.g. "127.0.0.1:8822"
	out.Host = t.base.Host     // overrides any Host the SDK set explicitly

	return t.next.RoundTrip(out)
}

// HTTPClient returns an *http.Client whose every request is redirected to
// baseURL by Transport. This is the one-liner most SDK wiring needs: pass the
// result wherever the SDK accepts a custom *http.Client (sendgrid's
// rest.Request, twilio's ClientParams.Client, or any similar hook).
func HTTPClient(baseURL string) *http.Client {
	return &http.Client{Transport: Transport(baseURL)}
}
