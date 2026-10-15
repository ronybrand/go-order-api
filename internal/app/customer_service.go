package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/ronybrand/go-order-api/internal/apperr"
	"github.com/ronybrand/go-order-api/internal/domain"
)

// CustomerService implements the customer use cases. Authorization (who may call what) is decided by the
// HTTP layer; the rules here are the ones that hold for every caller.
type CustomerService struct {
	store Store
	opts  options
}

// NewCustomerService builds the service.
func NewCustomerService(store Store, opts ...Option) *CustomerService {
	return &CustomerService{store: store, opts: newOptions(opts)}
}

// Create registers a customer, rejecting a duplicate tax id (CONFLICT-01) or passport (CONFLICT-02).
func (s *CustomerService) Create(ctx context.Context, in domain.CustomerInput) (*domain.Customer, error) {
	actor := ActorFrom(ctx)
	customer, err := domain.NewCustomer(in, actor.ID, s.opts.now())
	if err != nil {
		return nil, err
	}
	repo := s.store.Repos().Customers
	if err := s.ensureUnique(ctx, repo, customer, uuid.Nil); err != nil {
		return nil, err
	}
	// The unique indexes are the real guarantee under concurrency: the check above only gives the common case
	// a readable error without a failed write, and the repository maps a violation to the same codes.
	if err := repo.Create(ctx, customer); err != nil {
		return nil, err
	}
	s.opts.logger.InfoContext(ctx, "customer created", "customer", customer, "actor", actor.ID)
	return customer, nil
}

// Get returns an active customer.
func (s *CustomerService) Get(ctx context.Context, id uuid.UUID) (*domain.Customer, error) {
	return s.store.Repos().Customers.Get(ctx, id)
}

// Update replaces a customer's data, validating uniqueness against the other customers.
func (s *CustomerService) Update(ctx context.Context, id uuid.UUID, in domain.CustomerInput) (*domain.Customer, error) {
	actor := ActorFrom(ctx)
	repo := s.store.Repos().Customers
	customer, err := repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := customer.Replace(in, actor.ID, s.opts.now()); err != nil {
		return nil, err
	}
	if err := s.ensureUnique(ctx, repo, customer, id); err != nil {
		return nil, err
	}
	if err := repo.Update(ctx, customer); err != nil {
		return nil, err
	}
	s.opts.logger.InfoContext(ctx, "customer updated", "customer", customer, "actor", actor.ID)
	return customer, nil
}

// Delete soft-deletes a customer that has no active orders (CONFLICT-03 otherwise).
func (s *CustomerService) Delete(ctx context.Context, id uuid.UUID) error {
	actor := ActorFrom(ctx)
	err := s.store.InTx(ctx, func(ctx context.Context, r Repositories) error {
		// FOR UPDATE first: an order being created for this customer holds the shared lock, so the check below
		// sees it, and one that starts afterwards waits for this transaction and then finds the customer gone.
		if _, err := r.Customers.LockForUpdate(ctx, id); err != nil {
			return err
		}
		hasOrders, err := r.Orders.HasActive(ctx, id)
		if err != nil {
			return err
		}
		if hasOrders {
			return apperr.Conflict(apperr.CodeCustomerHasOrders,
				"customer has orders and cannot be deleted", map[string]any{"id": id})
		}
		return r.Customers.SoftDelete(ctx, id, actor.ID, s.opts.now())
	})
	if err != nil {
		return err
	}
	s.opts.logger.InfoContext(ctx, "customer deleted", "id", id, "actor", actor.ID)
	return nil
}

// Search filters, sorts and pages the active customers.
func (s *CustomerService) Search(ctx context.Context, req SearchRequest) (Page[domain.Customer], error) {
	req = req.WithDefaults()
	if err := req.Validate(); err != nil {
		return Page[domain.Customer]{}, err
	}
	return s.store.Repos().Customers.Search(ctx, req)
}

// ensureUnique returns CONFLICT-01/02 without echoing the value: tax id and passport are PII.
func (s *CustomerService) ensureUnique(ctx context.Context, repo CustomerRepository, c *domain.Customer, except uuid.UUID) error {
	taken, err := repo.TaxIDTaken(ctx, c.TaxID, except)
	if err != nil {
		return err
	}
	if taken {
		return apperr.Conflict(apperr.CodeDuplicateTaxID, "a customer with this tax_id already exists", nil)
	}
	if c.PassportNumber == nil {
		return nil
	}
	taken, err = repo.PassportTaken(ctx, *c.PassportNumber, except)
	if err != nil {
		return err
	}
	if taken {
		return apperr.Conflict(apperr.CodeDuplicatePassport, "a customer with this passport_number already exists", nil)
	}
	return nil
}
