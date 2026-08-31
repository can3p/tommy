package memory_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/store/memory"
)

func ev(plugin, provider, typ, title string) *event.Event {
	return &event.Event{
		Plugin:   plugin,
		Provider: provider,
		Type:     typ,
		Summary:  event.Summary{Title: title},
	}
}

func appendN(t *testing.T, s *memory.Store, plugin string, n int) []*event.Event {
	t.Helper()
	out := make([]*event.Event, 0, n)
	for i := range n {
		e := ev(plugin, "p", plugin+".thing", fmt.Sprintf("msg-%02d", i))
		if err := s.Append(context.Background(), e); err != nil {
			t.Fatalf("append: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func titles(events []*event.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Summary.Title
	}
	return out
}

func TestAppendAssignsIDAndTime(t *testing.T) {
	clock := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	s := memory.New(10,
		memory.WithClock(func() time.Time { return clock }),
		memory.WithIDFunc(func() string { return "fixed" }),
	)

	e := ev("mail", "mailjet", "mail.message", "hi")
	if err := s.Append(context.Background(), e); err != nil {
		t.Fatalf("append: %v", err)
	}
	if e.ID != "fixed" {
		t.Errorf("ID = %q, want the injected id written back onto the caller's event", e.ID)
	}
	if !e.ReceivedAt.Equal(clock) {
		t.Errorf("ReceivedAt = %v, want %v", e.ReceivedAt, clock)
	}

	// A second append with the same generated id must be rejected rather than
	// silently shadowing the first.
	if err := s.Append(context.Background(), ev("mail", "mailjet", "mail.message", "again")); err == nil {
		t.Error("expected a duplicate id to be rejected")
	}
}

func TestAppendPreservesExplicitValues(t *testing.T) {
	s := memory.New(10)
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	e := ev("sms", "twilio", "sms.message", "x")
	e.ID = "chosen"
	e.ReceivedAt = when
	if err := s.Append(context.Background(), e); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := s.Get(context.Background(), "chosen")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.ReceivedAt.Equal(when) {
		t.Errorf("ReceivedAt = %v, want the value the provider set", got.ReceivedAt)
	}
}

func TestStoreReturnsCopies(t *testing.T) {
	s := memory.New(10)
	e := ev("mail", "p", "mail.message", "original")
	e.Summary.To = []string{"a@example.com"}
	if err := s.Append(context.Background(), e); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Mutating the caller's event after Append must not change the store.
	e.Summary.Title = "mutated"
	e.Summary.To[0] = "mutated@example.com"

	got, err := s.Get(context.Background(), e.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Summary.Title != "original" {
		t.Errorf("Title = %q, want the value at append time", got.Summary.Title)
	}
	if got.Summary.To[0] != "a@example.com" {
		t.Errorf("To[0] = %q, want the value at append time", got.Summary.To[0])
	}

	// And mutating a returned event must not affect the next reader.
	got.Summary.Title = "scribbled"
	again, err := s.Get(context.Background(), e.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if again.Summary.Title != "original" {
		t.Errorf("Title = %q after a reader mutated its copy", again.Summary.Title)
	}
}

func TestRingEvictionIsPerPlugin(t *testing.T) {
	s := memory.New(3)
	appendN(t, s, "mail", 5)
	appendN(t, s, "sms", 2)

	mail, err := s.List(context.Background(), store.Query{Plugin: "mail"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got, want := titles(mail), []string{"msg-04", "msg-03", "msg-02"}; !equal(got, want) {
		t.Errorf("mail = %v, want the newest %d", got, len(want))
	}

	sms, err := s.List(context.Background(), store.Query{Plugin: "sms"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sms) != 2 {
		t.Errorf("sms has %d events; a chatty plugin must not evict a quiet one", len(sms))
	}
	if s.Len() != 5 {
		t.Errorf("total retained = %d, want 5", s.Len())
	}
}

func TestPerPluginCapacityOverride(t *testing.T) {
	s := memory.New(2, memory.WithPluginCapacity("files", 4))
	appendN(t, s, "files", 6)
	appendN(t, s, "mail", 6)

	files, _ := s.List(context.Background(), store.Query{Plugin: "files"})
	mail, _ := s.List(context.Background(), store.Query{Plugin: "mail"})
	if len(files) != 4 {
		t.Errorf("files retained %d, want its override of 4", len(files))
	}
	if len(mail) != 2 {
		t.Errorf("mail retained %d, want the default of 2", len(mail))
	}
}

func TestEvictedEventsAreUnreachable(t *testing.T) {
	s := memory.New(2)
	events := appendN(t, s, "mail", 3)

	if _, err := s.Get(context.Background(), events[0].ID); err == nil {
		t.Error("an evicted event must not be reachable through Get")
	}
	if _, err := s.Get(context.Background(), events[2].ID); err != nil {
		t.Errorf("the newest event must still be reachable: %v", err)
	}
}

func TestListOrderingIsNewestFirst(t *testing.T) {
	s := memory.New(10)
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, offset := range []time.Duration{2 * time.Second, 0, time.Second} {
		e := ev("mail", "p", "mail.message", fmt.Sprintf("t%d", i))
		e.ReceivedAt = base.Add(offset)
		if err := s.Append(context.Background(), e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	got, _ := s.List(context.Background(), store.Query{})
	if want := []string{"t0", "t2", "t1"}; !equal(titles(got), want) {
		t.Errorf("order = %v, want %v (newest ReceivedAt first)", titles(got), want)
	}
}

func TestListSameTimestampFallsBackToArrivalOrder(t *testing.T) {
	when := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	s := memory.New(10, memory.WithClock(func() time.Time { return when }))
	appendN(t, s, "mail", 3)

	got, _ := s.List(context.Background(), store.Query{})
	if want := []string{"msg-02", "msg-01", "msg-00"}; !equal(titles(got), want) {
		t.Errorf("order = %v, want %v (arrival order breaks ties)", titles(got), want)
	}
}

func TestListFiltering(t *testing.T) {
	s := memory.New(50)
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	seed := []*event.Event{
		{Plugin: "mail", Provider: "mailjet", Type: "mail.message", ReceivedAt: base,
			Summary: event.Summary{From: "alice@example.com", To: []string{"bob@example.com"}, Title: "Invoice 42", Snippet: "please pay"}},
		{Plugin: "mail", Provider: "sendgrid", Type: "mail.message", ReceivedAt: base.Add(time.Minute),
			Summary: event.Summary{From: "carol@example.com", To: []string{"dave@example.com"}, Title: "Welcome", Snippet: "hello there"}},
		{Plugin: "sms", Provider: "twilio", Type: "sms.message", ReceivedAt: base.Add(2 * time.Minute),
			Summary: event.Summary{From: "+15550001111", To: []string{"+15550002222"}, Title: "Your code", Snippet: "123456"}},
		{Plugin: "sms", Provider: "twilio", Type: "sms.status", ReceivedAt: base.Add(3 * time.Minute),
			Summary: event.Summary{Title: "delivered"}},
	}
	for _, e := range seed {
		if err := s.Append(context.Background(), e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	tests := []struct {
		name  string
		query store.Query
		want  []string
	}{
		{"no filter returns everything", store.Query{}, []string{"delivered", "Your code", "Welcome", "Invoice 42"}},
		{"by plugin", store.Query{Plugin: "mail"}, []string{"Welcome", "Invoice 42"}},
		{"by provider", store.Query{Provider: "twilio"}, []string{"delivered", "Your code"}},
		{"by type", store.Query{Type: "sms.status"}, []string{"delivered"}},
		{"search matches title", store.Query{Search: "invoice"}, []string{"Invoice 42"}},
		{"search matches snippet", store.Query{Search: "hello"}, []string{"Welcome"}},
		{"search matches from", store.Query{Search: "alice@"}, []string{"Invoice 42"}},
		{"search matches recipient", store.Query{Search: "dave@"}, []string{"Welcome"}},
		{"search matches type", store.Query{Search: "sms."}, []string{"delivered", "Your code"}},
		{"search is case insensitive", store.Query{Search: "WELCOME"}, []string{"Welcome"}},
		{"search matches nothing", store.Query{Search: "nope"}, nil},
		{"since is exclusive", store.Query{Since: base.Add(time.Minute)}, []string{"delivered", "Your code"}},
		{"limit", store.Query{Limit: 2}, []string{"delivered", "Your code"}},
		{"offset", store.Query{Offset: 2}, []string{"Welcome", "Invoice 42"}},
		{"offset and limit", store.Query{Offset: 1, Limit: 2}, []string{"Your code", "Welcome"}},
		{"offset past the end", store.Query{Offset: 99}, nil},
		{"combined filters", store.Query{Plugin: "mail", Search: "invoice"}, []string{"Invoice 42"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.List(context.Background(), tc.query)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if !equal(titles(got), tc.want) {
				t.Errorf("got %v, want %v", titles(got), tc.want)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	s := memory.New(10)
	events := appendN(t, s, "mail", 3)

	if err := s.Delete(context.Background(), events[1].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ := s.List(context.Background(), store.Query{})
	if want := []string{"msg-02", "msg-00"}; !equal(titles(got), want) {
		t.Errorf("after delete = %v, want %v", titles(got), want)
	}
	if s.Len() != 2 {
		t.Errorf("Len = %d, want 2", s.Len())
	}
	if err := s.Delete(context.Background(), events[1].ID); err == nil {
		t.Error("deleting twice must report ErrNotFound")
	}

	// The ring must still work after a mid-buffer removal.
	appendN(t, s, "mail", 2)
	got, _ = s.List(context.Background(), store.Query{})
	if len(got) != 4 {
		t.Errorf("after refilling = %d events, want 4", len(got))
	}
}

func TestClear(t *testing.T) {
	s := memory.New(10)
	appendN(t, s, "mail", 2)
	appendN(t, s, "sms", 2)

	if err := s.Clear(context.Background(), "mail"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ := s.List(context.Background(), store.Query{Plugin: "mail"}); len(got) != 0 {
		t.Errorf("mail still has %d events", len(got))
	}
	if got, _ := s.List(context.Background(), store.Query{Plugin: "sms"}); len(got) != 2 {
		t.Errorf("clearing one plugin must leave the others alone, sms has %d", len(got))
	}
	if err := s.Clear(context.Background(), "nosuchplugin"); err != nil {
		t.Errorf("clearing an unknown plugin should be a no-op, got %v", err)
	}

	if err := s.Clear(context.Background(), ""); err != nil {
		t.Fatalf("clear all: %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d after clearing everything", s.Len())
	}
}

func TestSubscribeFanOut(t *testing.T) {
	s := memory.New(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := s.Subscribe(ctx)
	b := s.Subscribe(ctx)
	if s.Subscribers() != 2 {
		t.Fatalf("Subscribers = %d, want 2", s.Subscribers())
	}

	e := ev("mail", "p", "mail.message", "fan")
	if err := s.Append(ctx, e); err != nil {
		t.Fatalf("append: %v", err)
	}
	for i, ch := range []<-chan *event.Event{a, b} {
		select {
		case got := <-ch:
			if got.Summary.Title != "fan" {
				t.Errorf("subscriber %d got %q", i, got.Summary.Title)
			}
			got.Summary.Title = "scribbled"
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d never received the event", i)
		}
	}

	// Subscribers get copies, so one cannot corrupt another's view.
	stored, err := s.Get(ctx, e.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Summary.Title != "fan" {
		t.Errorf("stored title = %q after a subscriber mutated its copy", stored.Summary.Title)
	}
}

func TestSubscribeUnsubscribesOnContextCancel(t *testing.T) {
	s := memory.New(10)
	ctx, cancel := context.WithCancel(context.Background())
	ch := s.Subscribe(ctx)

	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel should be closed after the context is done")
		}
	case <-time.After(time.Second):
		t.Fatal("channel was never closed after the context was canceled")
	}

	deadline := time.Now().Add(time.Second)
	for s.Subscribers() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("subscriber was never removed, %d left", s.Subscribers())
		}
		time.Sleep(time.Millisecond)
	}

	// Appending with no subscribers, and after a canceled one, must not panic
	// or block.
	if err := s.Append(context.Background(), ev("mail", "p", "mail.message", "after")); err != nil {
		t.Fatalf("append after unsubscribe: %v", err)
	}
}

func TestSlowConsumerDropsRatherThanBlocking(t *testing.T) {
	const buffer = 4
	s := memory.New(1000, memory.WithSubscriberBuffer(buffer))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A subscriber that never reads.
	_ = s.Subscribe(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 100 {
			if err := s.Append(ctx, ev("mail", "p", "mail.message", fmt.Sprintf("m%d", i))); err != nil {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Append blocked on a subscriber that is not reading")
	}

	if got := s.Dropped(); got == 0 {
		t.Error("expected drops to be counted for the stalled subscriber")
	}
	if got, _ := s.List(ctx, store.Query{}); len(got) != 100 {
		t.Errorf("the store kept %d events; dropping a delivery must not drop the event", len(got))
	}
}

func TestConcurrentUse(t *testing.T) {
	s := memory.New(200, memory.WithSubscriberBuffer(8))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const writers, perWriter = 8, 60

	// Subscribers, readers and writers all run at once; -race is the point.
	var background sync.WaitGroup
	for range 4 {
		sub := s.Subscribe(ctx)
		background.Add(1)
		go func() {
			defer background.Done()
			for range sub {
			}
		}()
	}

	stop := make(chan struct{})
	for range 3 {
		background.Add(1)
		go func() {
			defer background.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				events, err := s.List(context.Background(), store.Query{Limit: 10})
				if err != nil {
					t.Errorf("list: %v", err)
					return
				}
				for _, e := range events {
					_, _ = s.Get(context.Background(), e.ID)
				}
			}
		}()
	}

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			plugin := fmt.Sprintf("p%d", w%3)
			for i := range perWriter {
				if err := s.Append(context.Background(), ev(plugin, "prov", plugin+".thing", fmt.Sprintf("w%d-%d", w, i))); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	close(stop)
	cancel()
	background.Wait()

	if got := s.Len(); got == 0 {
		t.Error("nothing was retained after 480 concurrent appends")
	}
}

func TestContextCancellationIsReported(t *testing.T) {
	s := memory.New(10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.Append(ctx, ev("mail", "p", "mail.message", "x")); err == nil {
		t.Error("Append with a canceled context should fail")
	}
	if _, err := s.List(ctx, store.Query{}); err == nil {
		t.Error("List with a canceled context should fail")
	}
	if _, err := s.Get(ctx, "x"); err == nil {
		t.Error("Get with a canceled context should fail")
	}
	if err := s.Clear(ctx, ""); err == nil {
		t.Error("Clear with a canceled context should fail")
	}
}

func TestInterfaceCompliance(t *testing.T) {
	var _ store.Store = memory.New(1)
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
