package mail

import (
	"regexp"
	"testing"
	"time"
)

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		when time.Time
		want string
	}{
		{"just now", now.Add(-2 * time.Second), "just now"},
		{"seconds", now.Add(-40 * time.Second), "40s ago"},
		{"a minute", now.Add(-90 * time.Second), "1m ago"},
		{"minutes", now.Add(-10 * time.Minute), "10m ago"},
		{"an hour", now.Add(-90 * time.Minute), "1h ago"},
		{"hours", now.Add(-5 * time.Hour), "5h ago"},
		{"a day", now.Add(-30 * time.Hour), "1d ago"},
		{"days", now.Add(-3 * 24 * time.Hour), "3d ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := relativeTime(tt.when, now); got != tt.want {
				t.Errorf("relativeTime(%v) = %q, want %q", tt.when, got, tt.want)
			}
		})
	}

	// Older than a week falls back to a plain date, not an ever-growing "312d ago".
	old := now.Add(-30 * 24 * time.Hour)
	if got := relativeTime(old, now); regexp.MustCompile(`ago$`).MatchString(got) {
		t.Errorf("relativeTime for a month-old message should not say \"ago\": %q", got)
	}

	// A clock skew in the other direction (event timestamped fractionally in
	// the future) must not panic or print a negative duration.
	future := now.Add(2 * time.Second)
	if got := relativeTime(future, now); regexp.MustCompile(`ago|-`).MatchString(got) {
		t.Errorf("relativeTime for a future timestamp = %q, want a clock time, not a negative duration", got)
	}
}

func TestAttachmentKind(t *testing.T) {
	tests := map[string]string{
		"image/png":                "image",
		"IMAGE/JPEG":               "image",
		"application/pdf":          "pdf",
		"text/csv":                 "sheet",
		"application/zip":          "archive",
		"application/x-tar":        "archive",
		"application/msword":       "doc",
		"text/plain":               "text",
		"application/octet-stream": "file",
		"audio/mpeg":               "audio",
		"video/mp4":                "video",
		"application/vnd.ms-excel": "sheet",
	}
	for ct, want := range tests {
		if got := attachmentKind(ct); got != want {
			t.Errorf("attachmentKind(%q) = %q, want %q", ct, got, want)
		}
	}
}

func TestEmptyMessageDistinguishesNoMailFromNoMatch(t *testing.T) {
	tests := []struct {
		name string
		view inboxView
		want string
	}{
		{"nothing captured", inboxView{}, "No mail captured yet."},
		{"search filter narrowed to nothing", inboxView{Filter: inboxFilter{Search: "x"}}, "No message matches this filter."},
		{"provider filter narrowed to nothing", inboxView{Filter: inboxFilter{Provider: "mailjet"}}, "No message matches this filter."},
		{"attachments filter narrowed to nothing", inboxView{Filter: inboxFilter{Attachments: "1"}}, "No message matches this filter."},
		{"messages present", inboxView{Messages: []messageRow{{ID: "1"}}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.view.emptyMessage(); got != tt.want {
				t.Errorf("emptyMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}
