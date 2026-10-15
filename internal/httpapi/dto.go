package httpapi

import (
	"encoding/json"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/domain"
)

// Wire types. They are separate from the domain types on purpose: the JSON contract (snake_case, money as
// numbers, no PII on customers) must not change because a domain struct did.

type customerRequest struct {
	Name           string  `json:"name"`
	TaxID          string  `json:"tax_id"`
	PassportNumber *string `json:"passport_number"`
	Email          string  `json:"email"`
}

func (c customerRequest) input() domain.CustomerInput {
	return domain.CustomerInput{Name: c.Name, TaxID: c.TaxID, PassportNumber: c.PassportNumber, Email: c.Email}
}

// customerResponse exposes only id and name: tax id, passport and e-mail are PII, and no consumer of this
// API needs them back (the caller that created the record already has them).
type customerResponse struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

func toCustomerResponse(c domain.Customer) customerResponse {
	return customerResponse{ID: c.ID, Name: c.Name}
}

type itemRequest struct {
	Description string          `json:"description"`
	UnitPrice   decimal.Decimal `json:"unit_price"`
	Quantity    int             `json:"quantity"`
}

func (i itemRequest) input() domain.ItemInput {
	return domain.ItemInput{Description: i.Description, UnitPrice: i.UnitPrice, Quantity: i.Quantity}
}

type orderRequest struct {
	CustomerID uuid.UUID     `json:"customer_id"`
	Items      []itemRequest `json:"items"`
}

type quantityRequest struct {
	Quantity int `json:"quantity"`
}

type itemResponse struct {
	ID          uuid.UUID   `json:"id"`
	OrderID     uuid.UUID   `json:"order_id"`
	Description string      `json:"description"`
	UnitPrice   json.Number `json:"unit_price"`
	Quantity    int         `json:"quantity"`
	Subtotal    json.Number `json:"subtotal"`
}

type orderResponse struct {
	ID         uuid.UUID      `json:"id"`
	CustomerID uuid.UUID      `json:"customer_id"`
	Status     domain.Status  `json:"status"`
	Version    int64          `json:"version"`
	Items      []itemResponse `json:"items"`
	Total      json.Number    `json:"total"`
}

// money renders an amount as a JSON number with two decimals ("2.50"), never as a float.
func money(d decimal.Decimal) json.Number { return json.Number(d.StringFixed(2)) }

func toOrderResponse(o domain.Order) orderResponse {
	items := make([]itemResponse, 0, len(o.Items))
	for _, item := range o.Items {
		items = append(items, itemResponse{
			ID: item.ID, OrderID: item.OrderID, Description: item.Description,
			UnitPrice: money(item.UnitPrice), Quantity: item.Quantity, Subtotal: money(item.Subtotal()),
		})
	}
	return orderResponse{
		ID: o.ID, CustomerID: o.CustomerID, Status: o.Status, Version: o.Version, Items: items, Total: money(o.Total()),
	}
}

type pageResponse[T any] struct {
	Items []T `json:"items"`
	Total int `json:"total"`
	Page  int `json:"page"`
	Size  int `json:"size"`
}

func toPageResponse[D, T any](page app.Page[D], convert func(D) T) pageResponse[T] {
	items := make([]T, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, convert(item))
	}
	return pageResponse[T]{Items: items, Total: page.Total, Page: page.Page, Size: page.Size}
}
