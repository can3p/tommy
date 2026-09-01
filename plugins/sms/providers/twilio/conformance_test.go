package twilio_test

import (
	"testing"

	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/plugins/sms"
	"github.com/can3p/tommy/plugins/sms/providers/twilio"
)

// TestConformance proves the provider satisfies every discoverability rule
// every provider must: real descriptions, at least one working snippet, and
// every mounted route declared (and every declared route actually mounted).
func TestConformance(t *testing.T) {
	plugintest.Conformance(t, sms.New(sms.WithProviders(twilio.New())))
}
