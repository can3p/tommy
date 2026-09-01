// Package integration proves that the official vendor SDKs - unmodified,
// pinned to real released versions - work against a live tommy.
//
// Every other test in this repository either drives tommy with hand-built
// HTTP requests (the plugin and provider unit tests) or with the raw wire
// format an SDK would produce (the e2e suite). Neither actually imports
// mailjet-apiv3-go, sendgrid-go or twilio-go, so neither can catch a
// response-shape mismatch an SDK's own JSON decoder would choke on - a
// quoted integer where the SDK expects a bare one, a missing field its
// struct requires, a status code its client library treats as an error.
// This module closes that gap.
//
// It is a separate Go module (see go.mod, with a replace directive back to
// the repository root) rather than a package inside tommy's own module,
// because tommy's headline promise is a single dependency-free binary: the
// three vendor SDKs these tests import must never become transitive
// dependencies of github.com/can3p/tommy itself. See ../../clienthelp and
// ../../docs/clients.md for the wiring these tests exercise, and this
// package's README.md for how to run it.
//
// Every test file below carries "//go:build integration" so that this
// module builds (with zero tests) even without the tag, and only pulls in
// the SDKs' compiled code when a caller opts in with
// `go test -tags integration ./...`.
package integration
