package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ronybrand/go-order-api/internal/apperr"
	"github.com/ronybrand/go-order-api/internal/domain"
)

// announceTimeout bounds the lookup and the publish that follow a committed transition.
const announceTimeout = 10 * time.Second

// OrderService implements the order use cases.
type OrderService struct {
	store     Store
	publisher EventPublisher
	opts      options
}

// NewOrderService builds the service. The publisher may be nil when no notification is wanted.
func NewOrderService(store Store, publisher EventPublisher, opts ...Option) *OrderService {
	return &OrderService{store: store, publisher: publisher, opts: newOptions(opts)}
}

// Create opens an order for an existing customer (VALIDATION-07 if the customer does not exist).
func (s *OrderService) Create(ctx context.Context, customerID uuid.UUID, items []domain.ItemInput) (*domain.Order, error) {
	actor := ActorFrom(ctx)
	var order *domain.Order
	err := s.store.InTx(ctx, func(ctx context.Context, r Repositories) error {
		// FOR SHARE on the customer, in the same transaction that saves the order: a concurrent delete of this
		// customer waits for us (or we wait for it and then find nothing), so an order is never created under
		// a customer that was deleted in between.
		if _, err := r.Customers.LockForShare(ctx, customerID); err != nil {
			if apperr.Is(err, apperr.CodeCustomerNotFound) {
				return apperr.Validation(apperr.CodeCustomerNotFoundBody, "customer does not exist",
					map[string]any{"customer_id": customerID})
			}
			return err
		}
		built, err := domain.NewOrder(customerID, items, actor.ID, s.opts.now())
		if err != nil {
			return err
		}
		if err := r.Orders.Create(ctx, built); err != nil {
			return err
		}
		order = built
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.opts.logger.InfoContext(ctx, "order created", "order_id", order.ID, "actor", actor.ID)
	return order, nil
}

// Get returns an active order.
func (s *OrderService) Get(ctx context.Context, id uuid.UUID) (*domain.Order, error) {
	return s.store.Repos().Orders.Get(ctx, id)
}

// Delete soft-deletes an order.
func (s *OrderService) Delete(ctx context.Context, id uuid.UUID) error {
	actor := ActorFrom(ctx)
	orders := s.store.Repos().Orders
	if _, err := orders.Get(ctx, id); err != nil {
		return err
	}
	if err := orders.SoftDelete(ctx, id, actor.ID, s.opts.now()); err != nil {
		return err
	}
	s.opts.logger.InfoContext(ctx, "order deleted", "order_id", id, "actor", actor.ID)
	return nil
}

// AddItem adds an item to an OPEN order and returns the updated order.
func (s *OrderService) AddItem(ctx context.Context, orderID uuid.UUID, in domain.ItemInput) (*domain.Order, error) {
	return s.mutate(ctx, orderID, func(o *domain.Order, actor string) error {
		return o.AddItem(in, actor, s.opts.now())
	})
}

// UpdateItemQuantity changes the quantity of an item of an OPEN order.
func (s *OrderService) UpdateItemQuantity(ctx context.Context, orderID, itemID uuid.UUID, quantity int) (*domain.Order, error) {
	return s.mutate(ctx, orderID, func(o *domain.Order, actor string) error {
		return o.UpdateItemQuantity(itemID, quantity, actor, s.opts.now())
	})
}

// RemoveItem removes an item from an OPEN order.
func (s *OrderService) RemoveItem(ctx context.Context, orderID, itemID uuid.UUID) (*domain.Order, error) {
	return s.mutate(ctx, orderID, func(o *domain.Order, actor string) error {
		return o.RemoveItem(itemID, actor, s.opts.now())
	})
}

// Confirm moves an OPEN order to CONFIRMED and announces the change.
func (s *OrderService) Confirm(ctx context.Context, orderID uuid.UUID) (*domain.Order, error) {
	return s.transition(ctx, orderID, func(o *domain.Order, actor string) (domain.Status, error) {
		return o.Confirm(actor, s.opts.now())
	})
}

// Cancel moves an OPEN or CONFIRMED order to CANCELED and announces the change.
func (s *OrderService) Cancel(ctx context.Context, orderID uuid.UUID) (*domain.Order, error) {
	return s.transition(ctx, orderID, func(o *domain.Order, actor string) (domain.Status, error) {
		return o.Cancel(actor, s.opts.now())
	})
}

// Search filters, sorts and pages the active orders.
func (s *OrderService) Search(ctx context.Context, req SearchRequest) (Page[domain.Order], error) {
	req = req.WithDefaults()
	if err := req.Validate(); err != nil {
		return Page[domain.Order]{}, err
	}
	return s.store.Repos().Orders.Search(ctx, req)
}

// mutate loads the order, applies the change and saves it with the version it read.
func (s *OrderService) mutate(ctx context.Context, orderID uuid.UUID, change func(o *domain.Order, actor string) error) (*domain.Order, error) {
	actor := ActorFrom(ctx)
	var saved *domain.Order
	err := s.store.InTx(ctx, func(ctx context.Context, r Repositories) error {
		order, err := r.Orders.Get(ctx, orderID)
		if err != nil {
			return err
		}
		if err := change(order, actor.ID); err != nil {
			return err
		}
		if err := r.Orders.Save(ctx, order); err != nil {
			return err
		}
		saved = order
		return nil
	})
	return saved, err
}

func (s *OrderService) transition(ctx context.Context, orderID uuid.UUID, change func(o *domain.Order, actor string) (domain.Status, error)) (*domain.Order, error) {
	var old domain.Status
	order, err := s.mutate(ctx, orderID, func(o *domain.Order, actor string) (err error) {
		old, err = change(o, actor)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.opts.logger.InfoContext(ctx, "order status changed", "order_id", order.ID, "from", old, "to", order.Status)
	s.announce(ctx, order, old)
	return order, nil
}

// announce publishes the status change after the commit. A broker failure is logged and never reaches the
// caller: the transition already happened and must not fail because a notification could not be queued.
func (s *OrderService) announce(ctx context.Context, order *domain.Order, old domain.Status) {
	if s.publisher == nil {
		return
	}
	// The transition is already committed: a client that hangs up now must not cancel its notification, and a
	// slow broker must not hold the request open without bound.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), announceTimeout)
	defer cancel()
	customer, err := s.store.Repos().Customers.Get(ctx, order.CustomerID)
	if err != nil {
		s.opts.logger.ErrorContext(ctx, "could not load the customer to announce an order status change",
			"order_id", order.ID, "error", err)
		return
	}
	if customer.Email == "" {
		return
	}
	event := domain.OrderStatusChanged{
		OrderID: order.ID, CustomerEmail: customer.Email, CustomerName: customer.Name,
		OldStatus: old, NewStatus: order.Status, TotalAmount: order.Total(), ChangedAt: s.opts.now(),
	}
	if err := s.publisher.PublishStatusChanged(ctx, event); err != nil {
		s.opts.logger.ErrorContext(ctx, "failed to publish order status change", "event", event, "error", err)
	}
}
