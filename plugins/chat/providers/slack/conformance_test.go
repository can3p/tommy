package slack_test

import (
	"testing"

	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/plugins/chat"
	"github.com/can3p/tommy/plugins/chat/providers/slack"
)

// TestConformance proves the provider satisfies every discoverability rule
// every provider must: real descriptions, at least one working snippet per
// surface, and every mounted route declared (and every declared route
// actually mounted).
func TestConformance(t *testing.T) {
	plugintest.Conformance(t, chat.New(slack.New()))
}
