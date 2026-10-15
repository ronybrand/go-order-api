// Package app holds the use cases of the order domain. It talks to the outside world only through the
// small interfaces declared here, which the infrastructure packages (postgres, messaging) implement:
// "accept interfaces, return structs", with the interfaces defined where they are consumed.
package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ronybrand/go-order-api/internal/domain"
)

// CustomerRepository persists customers. Every read only sees active (non soft-deleted) rows, and a
// missing row is reported as apperr RESOURCE-NOT-FOUND-01.
type CustomerRepository interface {
	Get(ctx context.Context, id uuid.UUID) (*domain.Customer, error)
	// LockForUpdate reads the customer and takes a row lock that excludes every other lock on it. Deleting a
	// customer uses it so the "has orders?" check and the soft delete cannot interleave with an order creation.
	LockForUpdate(ctx context.Context, id uuid.UUID) (*domain.Customer, error)
	// LockForShare reads the customer and takes a shared row lock: many orders can be created for one
	// customer at once, but none while that customer is being deleted (and vice versa).
	LockForShare(ctx context.Context, id uuid.UUID) (*domain.Customer, error)
	Create(ctx context.Context, c *domain.Customer) error
	// Update writes the customer if its Version still matches the row; otherwise CONFLICT-00.
	Update(ctx context.Context, c *domain.Customer) error
	SoftDelete(ctx context.Context, id uuid.UUID, actor string, at time.Time) error
	// TaxIDTaken and PassportTaken ignore the customer except (uuid.Nil to ignore none).
	TaxIDTaken(ctx context.Context, taxID string, except uuid.UUID) (bool, error)
	PassportTaken(ctx context.Context, passport string, except uuid.UUID) (bool, error)
	Search(ctx context.Context, req SearchRequest) (Page[domain.Customer], error)
}

// OrderRepository persists orders together with their items.
type OrderRepository interface {
	Get(ctx context.Context, id uuid.UUID) (*domain.Order, error)
	Create(ctx context.Context, o *domain.Order) error
	// Save writes the status and the items of an order that was read earlier. It checks and increments the
	// version: a concurrent change fails with CONFLICT-00 instead of being overwritten silently.
	Save(ctx context.Context, o *domain.Order) error
	SoftDelete(ctx context.Context, id uuid.UUID, actor string, at time.Time) error
	// HasActive reports whether the customer has any non soft-deleted order.
	HasActive(ctx context.Context, customerID uuid.UUID) (bool, error)
	Search(ctx context.Context, req SearchRequest) (Page[domain.Order], error)
}

// Repositories is the set of repositories bound to one connection or one transaction.
type Repositories struct {
	Customers CustomerRepository
	Orders    OrderRepository
}

// Store gives the use cases their repositories, either directly or inside a transaction.
type Store interface {
	// Repos returns repositories running each statement on its own.
	Repos() Repositories
	// InTx runs fn in a transaction: it commits when fn returns nil and rolls back otherwise.
	InTx(ctx context.Context, fn func(ctx context.Context, repos Repositories) error) error
}

// EventPublisher announces order events. Publishing happens after the database commit and is best effort:
// see docs/adr/0002 for why this is not a transactional outbox.
type EventPublisher interface {
	PublishStatusChanged(ctx context.Context, event domain.OrderStatusChanged) error
}
