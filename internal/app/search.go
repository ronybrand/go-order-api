package app

import (
	"math"

	"github.com/ronybrand/go-order-api/internal/apperr"
)

// Limits of a search request: one request must not carry hundreds of predicates.
const (
	DefaultPageSize       = 20
	MaxPageSize           = 100
	MaxFilterFields       = 10
	MaxConditionsPerField = 10
	MaxInValues           = 100
	MaxFilterValueLength  = 200
	maxOffset             = math.MaxInt32
)

// Condition is one predicate on a field, e.g. {"op": "gte", "value": 10}. An empty Op means "eq".
type Condition struct {
	Op    string `json:"op"`
	Value any    `json:"value"`
}

// SearchRequest is the filter, sort and page of a search. Page is 0-based; Sort is a field name with a
// "-" prefix for descending order.
type SearchRequest struct {
	Page    int                    `json:"page"`
	Size    int                    `json:"size"`
	Sort    string                 `json:"sort"`
	Filters map[string][]Condition `json:"filters"`
}

// Offset is the number of rows to skip.
func (r SearchRequest) Offset() int { return r.Page * r.Size }

// WithDefaults fills in the page size when the client left it out.
func (r SearchRequest) WithDefaults() SearchRequest {
	if r.Size == 0 {
		r.Size = DefaultPageSize
	}
	return r
}

// Validate checks the structural limits of the request. Field names and value types are checked later,
// against the columns of the entity being searched (they are known only to the repository).
func (r SearchRequest) Validate() error {
	switch {
	case r.Page < 0:
		return apperr.InvalidField("page", "page must not be negative")
	case r.Size < 1 || r.Size > MaxPageSize:
		return apperr.InvalidField("size", "size must be between 1 and 100")
	case int64(r.Page)*int64(r.Size) > maxOffset:
		return apperr.InvalidField("page", "page is too large")
	case len(r.Filters) > MaxFilterFields:
		return apperr.Validation(apperr.CodeInvalidFilter, "too many filter fields", nil)
	}
	for field, conditions := range r.Filters {
		if len(conditions) > MaxConditionsPerField {
			return apperr.Validation(apperr.CodeInvalidFilter, "too many conditions on one field",
				map[string]any{"field": field})
		}
		for _, condition := range conditions {
			if !withinValueLimits(condition.Value) {
				return apperr.Validation(apperr.CodeInvalidFilter, "filter value exceeds the allowed limits",
					map[string]any{"field": field})
			}
		}
	}
	return nil
}

func withinValueLimits(value any) bool {
	switch v := value.(type) {
	case string:
		return len(v) <= MaxFilterValueLength
	case []any:
		if len(v) > MaxInValues {
			return false
		}
		for _, item := range v {
			if !withinValueLimits(item) {
				return false
			}
		}
	}
	return true
}

// Page is one page of results plus the total number of matches.
type Page[T any] struct {
	Items []T
	Total int
	Page  int
	Size  int
}
