package app

import (
	"context"
	"log/slog"
	"time"
)

// SystemActor is recorded in the audit columns when no user is attached to the context (background work).
const SystemActor = "system"

type actorKey struct{}

// Actor is the authenticated caller. The audit columns (created_by...) always come from here: only the
// application knows who made the request.
type Actor struct {
	ID    string
	Roles []string
}

// WithActor returns a context carrying the actor.
func WithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFrom returns the actor of the context, or the system actor when there is none.
func ActorFrom(ctx context.Context) Actor {
	if actor, ok := ctx.Value(actorKey{}).(Actor); ok && actor.ID != "" {
		return actor
	}
	return Actor{ID: SystemActor}
}

// Option configures a service. Services take their collaborators as constructor arguments and the optional
// behavior as functional options, so a zero-configuration call stays valid.
type Option func(*options)

type options struct {
	now    func() time.Time
	logger *slog.Logger
}

func newOptions(opts []Option) options {
	o := options{now: func() time.Time { return time.Now().UTC() }, logger: slog.Default()}
	for _, apply := range opts {
		apply(&o)
	}
	return o
}

// WithClock replaces the time source (tests use a fixed clock).
func WithClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// WithLogger replaces the logger.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}
