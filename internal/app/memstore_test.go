package app_test

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/apperr"
	"github.com/ronybrand/go-order-api/internal/domain"
)

// memStore is an in-memory app.Store for the service tests. A transaction works on a copy of the data and
// swaps it in on commit (so a failed transaction leaves nothing behind), and only one transaction runs at a
// time, which is what the row locks of the real database guarantee for the code under test.
type memStore struct {
	mu   sync.Mutex
	data *memData

	// calls is the ordered log of repository calls, used to assert that locks are taken before the writes
	// they protect and that the writes of one use case share one transaction.
	calls []string
	// beforeOrderSave runs once, just before the next Orders.Save, to simulate a concurrent writer.
	beforeOrderSave func(d *memData)
}

type memData struct {
	customers map[uuid.UUID]*domain.Customer
	orders    map[uuid.UUID]*domain.Order
}

func newMemStore() *memStore {
	return &memStore{data: &memData{customers: map[uuid.UUID]*domain.Customer{}, orders: map[uuid.UUID]*domain.Order{}}}
}

func (d *memData) clone() *memData {
	out := &memData{customers: map[uuid.UUID]*domain.Customer{}, orders: map[uuid.UUID]*domain.Order{}}
	for id, c := range d.customers {
		copied := *c
		out.customers[id] = &copied
	}
	for id, o := range d.orders {
		copied := *o
		copied.Items = slices.Clone(o.Items)
		out.orders[id] = &copied
	}
	return out
}

func (s *memStore) log(call string) { s.calls = append(s.calls, call) }

func (s *memStore) Repos() app.Repositories {
	return app.Repositories{
		Customers: memCustomers{store: s},
		Orders:    memOrders{store: s},
	}
}

func (s *memStore) InTx(ctx context.Context, fn func(ctx context.Context, r app.Repositories) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	work := s.data.clone()
	repos := app.Repositories{
		Customers: memCustomers{store: s, work: work},
		Orders:    memOrders{store: s, work: work},
	}
	s.log("begin")
	if err := fn(ctx, repos); err != nil {
		s.log("rollback")
		return err
	}
	s.data = work
	s.log("commit")
	return nil
}

// seedCustomer stores an already-valid customer and returns it.
func (s *memStore) seedCustomer(c domain.Customer) *domain.Customer {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.Version == 0 {
		c.Version = 1
	}
	s.data.customers[c.ID] = &c
	return &c
}

func (s *memStore) seedOrder(o domain.Order) *domain.Order {
	if o.ID == uuid.Nil {
		o.ID = uuid.New()
	}
	if o.Version == 0 {
		o.Version = 1
	}
	for i := range o.Items {
		if o.Items[i].ID == uuid.Nil {
			o.Items[i].ID = uuid.New()
		}
		o.Items[i].OrderID = o.ID
	}
	s.data.orders[o.ID] = &o
	return &o
}

func (s *memStore) lastCall() string { return s.calls[len(s.calls)-1] }

// memCustomers and memOrders use `work` inside a transaction and the committed data otherwise.
type memCustomers struct {
	store *memStore
	work  *memData
}

func (r memCustomers) data() *memData {
	if r.work != nil {
		return r.work
	}
	return r.store.data
}

func (r memCustomers) lockIfAutonomous() func() {
	if r.work != nil {
		return func() {}
	}
	r.store.mu.Lock()
	return r.store.mu.Unlock
}

func customerNotFound(id uuid.UUID) error {
	return apperr.NotFound(apperr.CodeCustomerNotFound, "customer not found", map[string]any{"id": id})
}

func (r memCustomers) find(id uuid.UUID) (*domain.Customer, error) {
	c, ok := r.data().customers[id]
	if !ok || c.DeletedAt != nil {
		return nil, customerNotFound(id)
	}
	copied := *c
	return &copied, nil
}

func (r memCustomers) Get(_ context.Context, id uuid.UUID) (*domain.Customer, error) {
	defer r.lockIfAutonomous()()
	return r.find(id)
}

func (r memCustomers) LockForUpdate(_ context.Context, id uuid.UUID) (*domain.Customer, error) {
	r.store.log("customer.lock-update")
	return r.find(id)
}

func (r memCustomers) LockForShare(_ context.Context, id uuid.UUID) (*domain.Customer, error) {
	r.store.log("customer.lock-share")
	return r.find(id)
}

func (r memCustomers) Create(_ context.Context, c *domain.Customer) error {
	defer r.lockIfAutonomous()()
	r.store.log("customer.create")
	c.ID = uuid.New()
	copied := *c
	r.data().customers[c.ID] = &copied
	return nil
}

func (r memCustomers) Update(_ context.Context, c *domain.Customer) error {
	defer r.lockIfAutonomous()()
	stored, ok := r.data().customers[c.ID]
	if !ok || stored.DeletedAt != nil {
		return customerNotFound(c.ID)
	}
	if stored.Version != c.Version {
		return apperr.Conflict(apperr.CodeConcurrentModification, "record was modified by another request", nil)
	}
	c.Version++
	copied := *c
	r.data().customers[c.ID] = &copied
	return nil
}

func (r memCustomers) SoftDelete(_ context.Context, id uuid.UUID, actor string, at time.Time) error {
	defer r.lockIfAutonomous()()
	r.store.log("customer.soft-delete")
	c, err := r.find(id)
	if err != nil {
		return err
	}
	c.DeletedAt, c.DeletedBy = &at, &actor
	r.data().customers[id] = c
	return nil
}

func (r memCustomers) taken(match func(c *domain.Customer) bool, except uuid.UUID) bool {
	for id, c := range r.data().customers {
		if c.DeletedAt == nil && id != except && match(c) {
			return true
		}
	}
	return false
}

func (r memCustomers) TaxIDTaken(_ context.Context, taxID string, except uuid.UUID) (bool, error) {
	defer r.lockIfAutonomous()()
	return r.taken(func(c *domain.Customer) bool { return c.TaxID == taxID }, except), nil
}

func (r memCustomers) PassportTaken(_ context.Context, passport string, except uuid.UUID) (bool, error) {
	defer r.lockIfAutonomous()()
	return r.taken(func(c *domain.Customer) bool { return c.PassportNumber != nil && *c.PassportNumber == passport }, except), nil
}

func (r memCustomers) Search(_ context.Context, req app.SearchRequest) (app.Page[domain.Customer], error) {
	defer r.lockIfAutonomous()()
	var items []domain.Customer
	for _, c := range r.data().customers {
		if c.DeletedAt == nil {
			items = append(items, *c)
		}
	}
	return app.Page[domain.Customer]{Items: items, Total: len(items), Page: req.Page, Size: req.Size}, nil
}

type memOrders struct {
	store *memStore
	work  *memData
}

func (r memOrders) data() *memData {
	if r.work != nil {
		return r.work
	}
	return r.store.data
}

func (r memOrders) lockIfAutonomous() func() {
	if r.work != nil {
		return func() {}
	}
	r.store.mu.Lock()
	return r.store.mu.Unlock
}

func orderNotFound(id uuid.UUID) error {
	return apperr.NotFound(apperr.CodeOrderNotFound, "order not found", map[string]any{"id": id})
}

func (r memOrders) find(id uuid.UUID) (*domain.Order, error) {
	o, ok := r.data().orders[id]
	if !ok || o.DeletedAt != nil {
		return nil, orderNotFound(id)
	}
	copied := *o
	copied.Items = slices.Clone(o.Items)
	return &copied, nil
}

func (r memOrders) Get(_ context.Context, id uuid.UUID) (*domain.Order, error) {
	defer r.lockIfAutonomous()()
	return r.find(id)
}

func (r memOrders) Create(_ context.Context, o *domain.Order) error {
	defer r.lockIfAutonomous()()
	r.store.log("order.create")
	o.ID = uuid.New()
	for i := range o.Items {
		o.Items[i].ID, o.Items[i].OrderID = uuid.New(), o.ID
	}
	copied := *o
	copied.Items = slices.Clone(o.Items)
	r.data().orders[o.ID] = &copied
	return nil
}

func (r memOrders) Save(_ context.Context, o *domain.Order) error {
	defer r.lockIfAutonomous()()
	r.store.log("order.save")
	if hook := r.store.beforeOrderSave; hook != nil {
		r.store.beforeOrderSave = nil
		hook(r.data())
	}
	stored, ok := r.data().orders[o.ID]
	if !ok || stored.DeletedAt != nil {
		return orderNotFound(o.ID)
	}
	if stored.Version != o.Version {
		return apperr.Conflict(apperr.CodeConcurrentModification, "record was modified by another request", nil)
	}
	o.Version++
	for i := range o.Items {
		if o.Items[i].ID == uuid.Nil {
			o.Items[i].ID = uuid.New()
		}
		o.Items[i].OrderID = o.ID
	}
	copied := *o
	copied.Items = slices.Clone(o.Items)
	r.data().orders[o.ID] = &copied
	return nil
}

func (r memOrders) SoftDelete(_ context.Context, id uuid.UUID, actor string, at time.Time) error {
	defer r.lockIfAutonomous()()
	o, err := r.find(id)
	if err != nil {
		return err
	}
	o.DeletedAt, o.DeletedBy = &at, &actor
	r.data().orders[id] = o
	return nil
}

func (r memOrders) HasActive(_ context.Context, customerID uuid.UUID) (bool, error) {
	defer r.lockIfAutonomous()()
	r.store.log("order.has-active")
	for _, o := range r.data().orders {
		if o.CustomerID == customerID && o.DeletedAt == nil {
			return true, nil
		}
	}
	return false, nil
}

func (r memOrders) Search(_ context.Context, req app.SearchRequest) (app.Page[domain.Order], error) {
	defer r.lockIfAutonomous()()
	var items []domain.Order
	for _, o := range r.data().orders {
		if o.DeletedAt == nil {
			items = append(items, *o)
		}
	}
	return app.Page[domain.Order]{Items: items, Total: len(items), Page: req.Page, Size: req.Size}, nil
}

// recordingPublisher captures the published events; err makes every publish fail.
type recordingPublisher struct {
	mu     sync.Mutex
	events []domain.OrderStatusChanged
	err    error
}

func (p *recordingPublisher) PublishStatusChanged(_ context.Context, event domain.OrderStatusChanged) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, event)
	return p.err
}
