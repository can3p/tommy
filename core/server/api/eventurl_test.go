package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/server/ingress"
)

// urlOf is the one field this file is about, read off whatever the API
// returned without caring about the rest of the shape.
type urlOf struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// Everything the API says about an event carries the link to its page, so a
// caller that just sent something can open what it sent.
func TestEventsCarryTheirPageURL(t *testing.T) {
	in := start(t)
	id := send(t, in, "a@example.com", "b@example.com", "Look at me")

	want := in.UI("/events/" + id)

	var listed []urlOf
	if status := in.GetJSON(in.API("/events"), &listed); status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	if len(listed) != 1 || listed[0].URL != want {
		t.Errorf("listed url = %+v, want %q", listed, want)
	}

	var single urlOf
	if status := in.GetJSON(in.API("/events/"+id), &single); status != http.StatusOK {
		t.Fatalf("get status = %d", status)
	}
	if single.URL != want {
		t.Errorf("single url = %q, want %q", single.URL, want)
	}

	// The link is absolute on purpose: the caller is usually talking to the
	// ingress, on a port with no UI on it.
	if !strings.HasPrefix(single.URL, "http://") {
		t.Errorf("url = %q, want an absolute link", single.URL)
	}
}

// The stream carries the same field, so a script can follow events live and
// print a link for each one.
func TestEventStreamCarriesThePageURL(t *testing.T) {
	in := start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.API("/events/stream"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := in.Do(req)
	defer func() { _ = resp.Body.Close() }()

	id := send(t, in, "a@example.com", "b@example.com", "streamed")

	buf := make([]byte, 8192)
	deadline := time.Now().Add(4 * time.Second)
	var seen string
	for time.Now().Before(deadline) && seen == "" {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			for _, line := range strings.Split(string(buf[:n]), "\n") {
				data, ok := strings.CutPrefix(line, "data: ")
				if !ok {
					continue
				}
				var frame urlOf
				if json.Unmarshal([]byte(data), &frame) == nil && frame.ID == id {
					seen = frame.URL
				}
			}
		}
		if err != nil {
			break
		}
	}
	if want := in.UI("/events/" + id); seen != want {
		t.Errorf("streamed url = %q, want %q", seen, want)
	}
}

// The response to the send itself names what it captured, so the link reaches
// an application's own log without it calling the API back.
func TestIngressAnswersWithTheEventLink(t *testing.T) {
	in := start(t)

	req, err := http.NewRequest(http.MethodPost, in.Ingress("/fake/v1/send"),
		strings.NewReader(`{"from":"a@example.com","to":"b@example.com","text":"link me"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp := in.Do(req)
	defer func() { _ = resp.Body.Close() }()

	links := resp.Header.Values(ingress.LinkHeader)
	if len(links) != 1 {
		t.Fatalf("%s = %v, want exactly one link", ingress.LinkHeader, links)
	}
	if !strings.HasPrefix(links[0], in.UI("/events/")) {
		t.Errorf("%s = %q, want an absolute link to the captured event", ingress.LinkHeader, links[0])
	}
	// And it points at the event that request produced.
	if status, _ := in.GetBody(links[0]); status != http.StatusOK {
		t.Errorf("the link is broken: status %d", status)
	}
}
