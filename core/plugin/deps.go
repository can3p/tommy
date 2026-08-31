package plugin

import (
	"context"
	"log/slog"
	"time"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/store"
)

// Deps is everything a plugin or provider is given at registration time.
// Now and NewID are injectable so provider tests are deterministic.
type Deps struct {
	Store  store.Store
	Blobs  blob.BlobStore
	Config ProviderConfig
	Logger *slog.Logger
	Now    func() time.Time
	NewID  func() string
}

// WithConfig returns a copy of d carrying a different provider config section.
func (d Deps) WithConfig(pc ProviderConfig) Deps {
	d.Config = pc
	return d
}

// WithLogger returns a copy of d with extra log attributes.
func (d Deps) WithLogger(args ...any) Deps {
	d.Logger = d.Normalize().Logger.With(args...)
	return d
}

// Normalize fills in the optional fields, so plugin code can call d.Now() and
// d.Logger without nil checks. Every core entry point normalizes before
// handing Deps to a plugin.
func (d Deps) Normalize() Deps {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.NewID == nil {
		d.NewID = event.NewID
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return d
}

// Append is the one-liner every provider uses: stamp the event with the
// injected clock and id generator, then record it.
func (d Deps) Append(ctx context.Context, e *event.Event) error {
	n := d.Normalize()
	if e.ID == "" {
		e.ID = event.ID(n.NewID())
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = n.Now()
	}
	return n.Store.Append(ctx, e)
}
