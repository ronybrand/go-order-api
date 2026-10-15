package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/apperr"
	"github.com/ronybrand/go-order-api/internal/auth"
	"github.com/ronybrand/go-order-api/internal/domain"
)

// CustomerService and OrderService are the use cases the handlers call. They are declared here, where they
// are consumed, so the handlers can be tested with fakes; *app.CustomerService and *app.OrderService satisfy them.
type CustomerService interface {
	Create(ctx context.Context, in domain.CustomerInput) (*domain.Customer, error)
	Get(ctx context.Context, id uuid.UUID) (*domain.Customer, error)
	Update(ctx context.Context, id uuid.UUID, in domain.CustomerInput) (*domain.Customer, error)
	Delete(ctx context.Context, id uuid.UUID) error
	Search(ctx context.Context, req app.SearchRequest) (app.Page[domain.Customer], error)
}

type OrderService interface {
	Create(ctx context.Context, customerID uuid.UUID, items []domain.ItemInput) (*domain.Order, error)
	Get(ctx context.Context, id uuid.UUID) (*domain.Order, error)
	Delete(ctx context.Context, id uuid.UUID) error
	AddItem(ctx context.Context, orderID uuid.UUID, in domain.ItemInput) (*domain.Order, error)
	UpdateItemQuantity(ctx context.Context, orderID, itemID uuid.UUID, quantity int) (*domain.Order, error)
	RemoveItem(ctx context.Context, orderID, itemID uuid.UUID) (*domain.Order, error)
	Confirm(ctx context.Context, orderID uuid.UUID) (*domain.Order, error)
	Cancel(ctx context.Context, orderID uuid.UUID) (*domain.Order, error)
	Search(ctx context.Context, req app.SearchRequest) (app.Page[domain.Order], error)
}

var (
	_ CustomerService = (*app.CustomerService)(nil)
	_ OrderService    = (*app.OrderService)(nil)
)

// Options wires the HTTP layer. Everything it needs is passed in; nothing is read from globals.
type Options struct {
	Customers CustomerService
	Orders    OrderService
	Verifier  TokenVerifier
	Logger    *slog.Logger
	// Ping checks the database for /health.
	Ping func(context.Context) error

	CORSOrigins      []string
	MaxBodyBytes     int64
	RateLimitPerUser int // per minute, any method
	RateLimitWrites  int // per minute, POST/PUT/PATCH/DELETE
	RateLimitPerIP   int // per minute, before authentication
	Now              func() time.Time
}

const (
	healthTimeout = 2 * time.Second
	healthPath    = "/health"
)

type api struct {
	customers CustomerService
	orders    OrderService
	logger    *slog.Logger
	ping      func(context.Context) error
}

// New builds the whole HTTP handler: routes plus the middleware stack.
func New(opts Options) http.Handler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	a := &api{customers: opts.Customers, orders: opts.Orders, logger: opts.Logger, ping: opts.Ping}

	userLimit := newKeyedLimiter(opts.RateLimitPerUser, opts.Now)
	writeLimit := newKeyedLimiter(opts.RateLimitWrites, opts.Now)
	ipLimit := newKeyedLimiter(opts.RateLimitPerIP, opts.Now)
	actorKey := func(r *http.Request) string { return app.ActorFrom(r.Context()).ID }

	// protected wraps a handler for authenticated callers. Every route is registered through it or through
	// public(): a route is never open by omission.
	protected := func(fn func(http.ResponseWriter, *http.Request) error, extra ...Middleware) http.Handler {
		stack := append([]Middleware{
			authenticate(opts.Verifier, opts.Logger),
			rateLimit(userLimit, actorKey, opts.Logger),
			onlyUnsafeMethods(rateLimit(writeLimit, actorKey, opts.Logger)),
		}, extra...)
		return chain(a.handle(fn), stack...)
	}
	admin := requireRole(auth.RoleAdmin, opts.Logger)

	mux := http.NewServeMux()
	mux.Handle("GET "+healthPath, http.HandlerFunc(a.health))

	mux.Handle("POST /customers", protected(a.createCustomer, admin))
	mux.Handle("GET /customers/search", protected(a.searchCustomersGET))
	mux.Handle("POST /customers/search", protected(a.searchCustomersPOST))
	mux.Handle("GET /customers/{id}", protected(a.getCustomer))
	mux.Handle("PUT /customers/{id}", protected(a.updateCustomer, admin))
	mux.Handle("DELETE /customers/{id}", protected(a.deleteCustomer, admin))

	mux.Handle("POST /orders", protected(a.createOrder))
	mux.Handle("GET /orders/search", protected(a.searchOrdersGET))
	mux.Handle("POST /orders/search", protected(a.searchOrdersPOST))
	mux.Handle("GET /orders/{id}", protected(a.getOrder))
	mux.Handle("DELETE /orders/{id}", protected(a.deleteOrder))
	mux.Handle("POST /orders/{id}/items", protected(a.addItem))
	mux.Handle("PATCH /orders/{id}/items/{item_id}", protected(a.updateItem))
	mux.Handle("DELETE /orders/{id}/items/{item_id}", protected(a.removeItem))
	mux.Handle("POST /orders/{id}/confirm", protected(a.confirmOrder))
	mux.Handle("POST /orders/{id}/cancel", protected(a.cancelOrder))

	return chain(a.routeErrors(mux),
		requestID,
		securityHeaders,
		accessLog(opts.Logger), // outside recoverer: a request that panicked is still logged, with its 500
		recoverer(opts.Logger),
		cors(opts.CORSOrigins),
		exceptPath(healthPath, rateLimit(ipLimit, clientIP, opts.Logger)),
		bodyLimit(opts.MaxBodyBytes),
	)
}

// handle adapts a handler that returns an error to http.Handler: every failure goes through writeError.
func (a *api) handle(fn func(http.ResponseWriter, *http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			writeError(w, r, a.logger, err)
		}
	})
}

// routeErrors renders the mux's own 404 and 405 in the API error envelope instead of plain text.
func (a *api) routeErrors(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		probe := &statusProbe{header: http.Header{}}
		mux.ServeHTTP(probe, r)
		body := errorResponse{Params: map[string]any{}, RequestID: requestIDFrom(r.Context())}
		if probe.code == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", probe.header.Get("Allow"))
			body.Message, body.Code = "Method not allowed", apperr.CodeMethodNotAllowed
		} else {
			probe.code = http.StatusNotFound
			body.Message, body.Code = "Route not found", apperr.CodeRouteNotFound
		}
		writeJSON(w, probe.code, body)
	})
}

// statusProbe swallows the response of the mux so only its verdict (status and Allow header) is used.
type statusProbe struct {
	header http.Header
	code   int
}

func (p *statusProbe) Header() http.Header         { return p.header }
func (p *statusProbe) Write(b []byte) (int, error) { return len(b), nil }
func (p *statusProbe) WriteHeader(code int)        { p.code = code }

func exceptPath(path string, m Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		limited := m(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == path {
				next.ServeHTTP(w, r)
				return
			}
			limited.ServeHTTP(w, r)
		})
	}
}

func onlyUnsafeMethods(m Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		limited := m(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
			default:
				limited.ServeHTTP(w, r)
			}
		})
	}
}

func (a *api) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()
	if err := a.ping(ctx); err != nil {
		a.logger.ErrorContext(ctx, "health check failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- helpers ---

func pathID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, apperr.InvalidField(name, name+" must be a valid UUID")
	}
	return id, nil
}

// searchFromQuery reads the GET form of a search: page, size and sort only. Filters need the POST form.
func searchFromQuery(r *http.Request) (app.SearchRequest, error) {
	query := r.URL.Query()
	var req app.SearchRequest
	for name, target := range map[string]*int{"page": &req.Page, "size": &req.Size} {
		raw := query.Get(name)
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return req, apperr.InvalidField(name, name+" must be an integer")
		}
		*target = n
	}
	req.Sort = query.Get("sort")
	return req, nil
}

func searchFromBody(r *http.Request) (app.SearchRequest, error) {
	var req app.SearchRequest
	err := decodeJSON(r, &req)
	return req, err
}

func inputs(items []itemRequest) []domain.ItemInput {
	out := make([]domain.ItemInput, len(items))
	for i, item := range items {
		out[i] = item.input()
	}
	return out
}

// --- customers ---

func (a *api) createCustomer(w http.ResponseWriter, r *http.Request) error {
	var req customerRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	customer, err := a.customers.Create(r.Context(), req.input())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, toCustomerResponse(*customer))
	return nil
}

func (a *api) getCustomer(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	customer, err := a.customers.Get(r.Context(), id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, toCustomerResponse(*customer))
	return nil
}

func (a *api) updateCustomer(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req customerRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	customer, err := a.customers.Update(r.Context(), id, req.input())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, toCustomerResponse(*customer))
	return nil
}

func (a *api) deleteCustomer(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := a.customers.Delete(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *api) searchCustomers(w http.ResponseWriter, r *http.Request, req app.SearchRequest) error {
	page, err := a.customers.Search(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, toPageResponse(page, toCustomerResponse))
	return nil
}

func (a *api) searchCustomersGET(w http.ResponseWriter, r *http.Request) error {
	req, err := searchFromQuery(r)
	if err != nil {
		return err
	}
	return a.searchCustomers(w, r, req)
}

func (a *api) searchCustomersPOST(w http.ResponseWriter, r *http.Request) error {
	req, err := searchFromBody(r)
	if err != nil {
		return err
	}
	return a.searchCustomers(w, r, req)
}

// --- orders ---

func (a *api) writeOrder(w http.ResponseWriter, status int, order *domain.Order, err error) error {
	if err != nil {
		return err
	}
	writeJSON(w, status, toOrderResponse(*order))
	return nil
}

func (a *api) createOrder(w http.ResponseWriter, r *http.Request) error {
	var req orderRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	order, err := a.orders.Create(r.Context(), req.CustomerID, inputs(req.Items))
	return a.writeOrder(w, http.StatusCreated, order, err)
}

func (a *api) getOrder(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	order, err := a.orders.Get(r.Context(), id)
	return a.writeOrder(w, http.StatusOK, order, err)
}

func (a *api) deleteOrder(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := a.orders.Delete(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *api) addItem(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req itemRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	order, err := a.orders.AddItem(r.Context(), id, req.input())
	return a.writeOrder(w, http.StatusOK, order, err)
}

func (a *api) updateItem(w http.ResponseWriter, r *http.Request) error {
	orderID, err := pathID(r, "id")
	if err != nil {
		return err
	}
	itemID, err := pathID(r, "item_id")
	if err != nil {
		return err
	}
	var req quantityRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	order, err := a.orders.UpdateItemQuantity(r.Context(), orderID, itemID, req.Quantity)
	return a.writeOrder(w, http.StatusOK, order, err)
}

func (a *api) removeItem(w http.ResponseWriter, r *http.Request) error {
	orderID, err := pathID(r, "id")
	if err != nil {
		return err
	}
	itemID, err := pathID(r, "item_id")
	if err != nil {
		return err
	}
	order, err := a.orders.RemoveItem(r.Context(), orderID, itemID)
	return a.writeOrder(w, http.StatusOK, order, err)
}

func (a *api) transition(w http.ResponseWriter, r *http.Request, do func(context.Context, uuid.UUID) (*domain.Order, error)) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	order, err := do(r.Context(), id)
	return a.writeOrder(w, http.StatusOK, order, err)
}

func (a *api) confirmOrder(w http.ResponseWriter, r *http.Request) error {
	return a.transition(w, r, a.orders.Confirm)
}

func (a *api) cancelOrder(w http.ResponseWriter, r *http.Request) error {
	return a.transition(w, r, a.orders.Cancel)
}

func (a *api) searchOrders(w http.ResponseWriter, r *http.Request, req app.SearchRequest) error {
	page, err := a.orders.Search(r.Context(), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, toPageResponse(page, toOrderResponse))
	return nil
}

func (a *api) searchOrdersGET(w http.ResponseWriter, r *http.Request) error {
	req, err := searchFromQuery(r)
	if err != nil {
		return err
	}
	return a.searchOrders(w, r, req)
}

func (a *api) searchOrdersPOST(w http.ResponseWriter, r *http.Request) error {
	req, err := searchFromBody(r)
	if err != nil {
		return err
	}
	return a.searchOrders(w, r, req)
}
