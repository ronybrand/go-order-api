package postgres

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/apperr"
)

func filters(field string, conditions ...app.Condition) app.SearchRequest {
	return app.SearchRequest{Filters: map[string][]app.Condition{field: conditions}}
}

func TestBuildQuery_Operators(t *testing.T) {
	id := uuid.New()
	tests := map[string]struct {
		entity    entity
		req       app.SearchRequest
		wantWhere string
		wantArgs  []any
	}{
		"no filter": {customerEntity, app.SearchRequest{}, "deleted_at IS NULL", nil},
		"eq default op": {customerEntity, filters("name", app.Condition{Value: "Ana"}),
			"deleted_at IS NULL AND name = $1", []any{"Ana"}},
		"neq": {orderEntity, filters("status", app.Condition{Op: "neq", Value: "OPEN"}),
			"deleted_at IS NULL AND status <> $1", []any{"OPEN"}},
		"gte int from json.Number": {orderEntity, filters("version", app.Condition{Op: "gte", Value: json.Number("2")}),
			"deleted_at IS NULL AND version >= $1", []any{int64(2)}},
		"in uuid": {orderEntity, filters("customer_id", app.Condition{Op: "in", Value: []any{id.String()}}),
			"deleted_at IS NULL AND customer_id IN ($1)", []any{id}},
		"between int": {customerEntity, filters("version", app.Condition{Op: "between", Value: []any{float64(1), float64(5)}}),
			"deleted_at IS NULL AND version BETWEEN $1 AND $2", []any{int64(1), int64(5)}},
		"lk escapes wildcards": {customerEntity, filters("name", app.Condition{Op: "lk", Value: `50%_\`}),
			`deleted_at IS NULL AND name ILIKE $1 ESCAPE '\'`, []any{`%50\%\_\\%`}},
		"unknown field ignored": {customerEntity, filters("nope", app.Condition{Value: "x"}), "deleted_at IS NULL", nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			q, err := tt.entity.buildQuery(tt.req)

			if err != nil {
				t.Fatal(err)
			}
			if q.where != tt.wantWhere || !reflect.DeepEqual(q.args, tt.wantArgs) {
				t.Errorf("where = %q args = %v", q.where, q.args)
			}
		})
	}
}

func TestBuildQuery_RejectsInvalidFilters(t *testing.T) {
	tests := map[string]app.SearchRequest{
		"string where int expected":     filters("version", app.Condition{Value: "abc"}),
		"fractional int":                filters("version", app.Condition{Value: 1.5}),
		"bad uuid":                      filters("id", app.Condition{Value: "not-a-uuid"}),
		"bad timestamp":                 filters("created_at", app.Condition{Op: "gt", Value: "yesterday"}),
		"unknown enum value":            filters("status", app.Condition{Value: "SHIPPED"}),
		"ordering operator on enum":     filters("status", app.Condition{Op: "gt", Value: "OPEN"}),
		"between on enum":               filters("status", app.Condition{Op: "between", Value: []any{"OPEN", "CANCELED"}}),
		"between with three values":     filters("version", app.Condition{Op: "between", Value: []any{1, 2, 3}}),
		"in with a scalar":              filters("name", app.Condition{Op: "in", Value: "Ana"}),
		"in with no values":             filters("name", app.Condition{Op: "in", Value: []any{}}),
		"lk on a non text field":        filters("version", app.Condition{Op: "lk", Value: "1"}),
		"unknown operator":              filters("name", app.Condition{Op: "regex", Value: ".*"}),
		"sql in the value is only data": filters("version", app.Condition{Value: "1; DROP TABLE customers"}),
	}
	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := customerEntity.buildQuery(req)
			if name == "unknown enum value" || strings.Contains(name, "enum") {
				_, err = orderEntity.buildQuery(req)
			}

			if !apperr.Is(err, apperr.CodeInvalidFilter) {
				t.Errorf("err = %v, want %s", err, apperr.CodeInvalidFilter)
			}
		})
	}
}

func TestBuildQuery_Sort(t *testing.T) {
	asc, err := customerEntity.buildQuery(app.SearchRequest{Sort: "name"})
	desc, err2 := customerEntity.buildQuery(app.SearchRequest{Sort: "-created_at"})
	_, errUnknown := customerEntity.buildQuery(app.SearchRequest{Sort: "name; DROP TABLE customers"})
	_, errSoftDelete := customerEntity.buildQuery(app.SearchRequest{Sort: "deleted_at"})

	if err != nil || err2 != nil {
		t.Fatal(err, err2)
	}
	if asc.orderBy != "name ASC, id" || desc.orderBy != "created_at DESC, id" {
		t.Errorf("orderBy = %q / %q", asc.orderBy, desc.orderBy)
	}
	if !apperr.Is(errUnknown, apperr.CodeUnknownSort) || !apperr.Is(errSoftDelete, apperr.CodeUnknownSort) {
		t.Errorf("errors = %v / %v, want %s", errUnknown, errSoftDelete, apperr.CodeUnknownSort)
	}
}
