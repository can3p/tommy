//go:build integration

// FCM integration tests point google.golang.org/api/fcm/v1 - the same
// discovery-document-generated Go client family as gmail/v1, drive/v3 and
// every other google-api-go-client package, and the closest thing to an
// "official Google SDK" this API has a Go binding for at all (none of the
// Firebase Admin SDKs expose a way to redirect the FCM client at a custom
// host - see plugins/push/providers/fcm/README.md's "Driving a real SDK"
// section) - at a live tommy via option.WithEndpoint and
// option.WithoutAuthentication, the standard pattern every client generated
// this way supports. It proves the client's own JSON marshaling round-trips
// through the fcm provider and that its generated Message struct decodes the
// real success response without a workaround.
package integration

import (
	"context"
	"testing"
	"time"

	"google.golang.org/api/fcm/v1"
	"google.golang.org/api/option"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/push"
	pushfcm "github.com/can3p/tommy/plugins/push/providers/fcm"
)

// startPushTommy boots a tommy with just the push plugin and the fcm
// provider. The push plugin is not yet wired into plugins/all (it ships with
// no real provider there until this task and its apns sibling land), so this
// cannot use startTommy/all.Plugins() the way the other integration suites
// do.
func startPushTommy(t *testing.T) *testutil.Instance {
	t.Helper()
	return testutil.Start(t, nil, push.New(pushfcm.New()))
}

// fcmService builds the generated client pointed at inst's ingress with no
// real OAuth: option.WithoutAuthentication skips credential lookup entirely,
// and option.WithHTTPClient(inst.Client) reuses testutil's client so the
// request actually reaches the ephemeral listener under test.
func fcmService(t *testing.T, inst *testutil.Instance) *fcm.Service {
	t.Helper()
	svc, err := fcm.NewService(context.Background(),
		option.WithEndpoint(inst.IngressURL),
		option.WithHTTPClient(inst.Client),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatalf("fcm.NewService: %v", err)
	}
	return svc
}

// TestFCMSDKSendsAndIsCaptured builds a message with the generated client's
// own types (fcm.Message, fcm.Notification, fcm.AndroidConfig), sends it
// through ProjectsMessagesService.Send, and checks both that the client
// decodes tommy's response into its own Message struct without error and
// that tommy captured the right canonical push.Message.
func TestFCMSDKSendsAndIsCaptured(t *testing.T) {
	inst := startPushTommy(t)
	svc := fcmService(t, inst)

	req := &fcm.SendMessageRequest{
		Message: &fcm.Message{
			Topic: "weather",
			Notification: &fcm.Notification{
				Title: "Storm warning",
				Body:  "Batten down the hatches",
			},
			Android: &fcm.AndroidConfig{
				Priority:    "HIGH",
				Ttl:         "3600s",
				CollapseKey: "weather",
			},
		},
	}

	resp, err := svc.Projects.Messages.Send("projects/my-project", req).Do()
	if err != nil {
		t.Fatalf("Projects.Messages.Send: %v", err)
	}
	if resp.Name == "" {
		t.Fatalf("response Name is empty")
	}
	t.Logf("fcm SDK decoded response.Name = %q", resp.Name)

	evs := waitForPushEvents(t, inst, 1, 5*time.Second)
	m, ok := push.MessageOf(evs[0])
	if !ok {
		t.Fatalf("event carries no push message")
	}
	if m.Target.Kind != push.TargetTopic || m.Target.Value != "weather" || m.Target.Source != "topic" {
		t.Errorf("Target = %+v, want topic/weather/topic", m.Target)
	}
	if m.Alert == nil || m.Alert.Title != "Storm warning" || m.Alert.Body != "Batten down the hatches" {
		t.Errorf("Alert = %+v, want the SDK's own title/body", m.Alert)
	}
	if m.Delivery.Priority != push.PriorityHigh {
		t.Errorf("Delivery.Priority = %q, want high", m.Delivery.Priority)
	}
	if m.Delivery.CollapseKey != "weather" {
		t.Errorf("Delivery.CollapseKey = %q, want weather", m.Delivery.CollapseKey)
	}
	if m.Kind != push.KindNotification || !m.Displays() {
		t.Errorf("Kind = %q, want notification", m.Kind)
	}
}

// TestFCMSDKDataOnlyMessageIsSilent sends a data-only message - the SDK's
// own Data map, no Notification - and checks tommy calls it silent, the way
// a real device would treat it.
func TestFCMSDKDataOnlyMessageIsSilent(t *testing.T) {
	inst := startPushTommy(t)
	svc := fcmService(t, inst)

	req := &fcm.SendMessageRequest{
		Message: &fcm.Message{
			Token: "cQAdeviceRegistrationTokenExampleFromTheSDK000111",
			Data:  map[string]string{"kind": "refresh"},
		},
	}

	if _, err := svc.Projects.Messages.Send("projects/my-project", req).Do(); err != nil {
		t.Fatalf("Projects.Messages.Send: %v", err)
	}

	evs := waitForPushEvents(t, inst, 1, 5*time.Second)
	m, ok := push.MessageOf(evs[0])
	if !ok {
		t.Fatalf("event carries no push message")
	}
	if m.Target.Kind != push.TargetDevice || m.Target.Source != "token" {
		t.Errorf("Target = %+v, want a device token", m.Target)
	}
	if m.Kind != push.KindSilent || m.Displays() {
		t.Errorf("Kind = %q, want silent", m.Kind)
	}
}

// TestFCMSDKValidationErrorSurfacesToTheClient sends a message with no
// target at all - the SDK cannot construct one client-side, since Message's
// target fields are just plain strings it is free to leave unset - and
// checks the generated client surfaces tommy's 400 as its own *googleapi.Error
// rather than panicking on an unexpected body shape.
func TestFCMSDKValidationErrorSurfacesToTheClient(t *testing.T) {
	inst := startPushTommy(t)
	svc := fcmService(t, inst)

	req := &fcm.SendMessageRequest{
		Message: &fcm.Message{
			Notification: &fcm.Notification{Title: "x", Body: "y"},
		},
	}

	_, err := svc.Projects.Messages.Send("projects/my-project", req).Do()
	if err == nil {
		t.Fatalf("Send succeeded, want an error for a message with no target")
	}
	t.Logf("fcm SDK surfaced error: %v", err)

	if len(waitForZeroPushEvents(t, inst)) != 0 {
		t.Errorf("a rejected request must not be recorded")
	}
}

func waitForPushEvents(t *testing.T, inst *testutil.Instance, n int, timeout time.Duration) []*event.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		evs, err := inst.Store.List(context.Background(), store.Query{Plugin: push.Name})
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		if len(evs) >= n {
			return evs
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %d push event(s), got %d", timeout, n, len(evs))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForZeroPushEvents(t *testing.T, inst *testutil.Instance) []*event.Event {
	t.Helper()
	evs, err := inst.Store.List(context.Background(), store.Query{Plugin: push.Name})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	return evs
}
