package resend

import (
	"testing"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/plugins/mail"
)

func mailAddress(name, email string) mail.Address {
	return mail.Address{Name: name, Email: email}
}

// TestIDRoundTrip is the property GET /emails/{id} depends on: whatever
// emailIDFor mints, eventIDFromEmailID must give back, with no index anywhere.
func TestIDRoundTrip(t *testing.T) {
	t.Parallel()

	for i := 0; i < 200; i++ {
		id := event.NewID()
		email := emailIDFor(id)
		if !looksLikeUUID(email) {
			t.Fatalf("emailIDFor(%q) = %q, which is not UUID-shaped", id, email)
		}
		if email[14] != '4' || email[19] != 'a' {
			t.Fatalf("emailIDFor(%q) = %q is not a syntactically valid v4 UUID", id, email)
		}
		got, ok := eventIDFromEmailID(email)
		if !ok {
			t.Fatalf("eventIDFromEmailID(%q) reported not-ours", email)
		}
		if got != id {
			t.Fatalf("round trip: %q -> %q -> %q", id, email, got)
		}
		// Uppercase is what some clients hand a UUID back as.
		if got, ok := eventIDFromEmailID(upper(email)); !ok || got != id {
			t.Fatalf("uppercase round trip failed for %q", email)
		}
	}
}

func upper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

// TestForeignIDsAreRejected: an id this provider never minted must not decode
// into some other event's id, which is the whole reason for the marker.
func TestForeignIDsAreRejected(t *testing.T) {
	t.Parallel()

	for _, s := range []string{
		"",
		"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794", // a real Resend id from the docs
		"49a3999c-0ce1-4ea6-ab68-afcd6dcfacade",
		"00000000-0000-3000-a000-000001facade", // wrong version nibble
		"00000000-0000-4000-b000-000001facade", // wrong variant nibble
		"not-a-uuid",
		"zzzzzzzz-zzzz-4zzz-azzz-zzzzzzfacade",
	} {
		if _, ok := eventIDFromEmailID(s); ok {
			t.Errorf("eventIDFromEmailID(%q) accepted an id this provider never minted", s)
		}
	}
}

// TestNonHexEventIDPassesThrough is the fallback that keeps an injected id
// generator working: not UUID-shaped, but still reversible, which is all GET
// needs.
func TestNonHexEventIDPassesThrough(t *testing.T) {
	t.Parallel()

	const id = "test-id-001"
	if got := emailIDFor(id); got != id {
		t.Fatalf("emailIDFor(%q) = %q, want it passed through unchanged", id, got)
	}
	if _, ok := eventIDFromEmailID(id); ok {
		t.Fatalf("eventIDFromEmailID(%q) must report not-ours so the handler falls back to the raw form", id)
	}
}

// TestFormatAddress covers the one place this provider deliberately differs
// from mail.Address.String(): a plain display name is not quoted, because the
// vendor does not quote it either.
func TestFormatAddress(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, email, want string
	}{
		{"", "bob@example.com", "bob@example.com"},
		{"Acme", "alice@example.com", "Acme <alice@example.com>"},
		{"Acme Corp", "alice@example.com", "Acme Corp <alice@example.com>"},
		{"Acme, Inc.", "alice@example.com", `"Acme, Inc." <alice@example.com>`},
		{"Ünicode", "alice@example.com", `"Ünicode" <alice@example.com>`},
		{"a@b", "alice@example.com", `"a@b" <alice@example.com>`},
	}
	for _, tc := range cases {
		got := formatAddress(mailAddress(tc.name, tc.email))
		if got != tc.want {
			t.Errorf("formatAddress(%q, %q) = %q, want %q", tc.name, tc.email, got, tc.want)
		}
	}
}
