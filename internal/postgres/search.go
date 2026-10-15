package postgres

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/apperr"
	"github.com/ronybrand/go-order-api/internal/domain"
)

type fieldType int

const (
	typeString fieldType = iota
	typeUUID
	typeInt
	typeTime
	typeEnum
)

// field describes one searchable column. Values are coerced to the column's type before they reach SQL, so
// a wrong type is a VALIDATION-05 for the client instead of a driver error.
type field struct {
	column  string
	kind    fieldType
	allowed []string // for typeEnum
}

// entity is the whitelist of what a search can filter and sort on. Only columns are listed (never the
// soft-delete markers): the SQL text only ever contains names from here, values are always parameters.
type entity struct {
	table  string
	fields map[string]field
}

func (e entity) names() []string {
	out := make([]string, 0, len(e.fields))
	for name := range e.fields {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

var (
	customerEntity = entity{table: "customers", fields: map[string]field{
		"id":              {column: "id", kind: typeUUID},
		"name":            {column: "name", kind: typeString},
		"tax_id":          {column: "tax_id", kind: typeString},
		"passport_number": {column: "passport_number", kind: typeString},
		"email":           {column: "email", kind: typeString},
		"version":         {column: "version", kind: typeInt},
		"created_at":      {column: "created_at", kind: typeTime},
		"updated_at":      {column: "updated_at", kind: typeTime},
		"created_by":      {column: "created_by", kind: typeString},
		"updated_by":      {column: "updated_by", kind: typeString},
	}}
	orderEntity = entity{table: "orders", fields: map[string]field{
		"id":          {column: "id", kind: typeUUID},
		"customer_id": {column: "customer_id", kind: typeUUID},
		"status": {column: "status", kind: typeEnum, allowed: []string{
			string(domain.StatusOpen), string(domain.StatusConfirmed), string(domain.StatusCanceled)}},
		"version":    {column: "version", kind: typeInt},
		"created_at": {column: "created_at", kind: typeTime},
		"updated_at": {column: "updated_at", kind: typeTime},
		"created_by": {column: "created_by", kind: typeString},
		"updated_by": {column: "updated_by", kind: typeString},
	}}
)

// query is a compiled search: the WHERE and ORDER BY fragments plus their positional arguments.
type query struct {
	where   string
	orderBy string
	args    []any
}

type builder struct {
	args  []any
	where []string
}

func (b *builder) param(value any) string {
	b.args = append(b.args, value)
	return "$" + strconv.Itoa(len(b.args))
}

// buildQuery turns a validated SearchRequest into SQL fragments. Unknown filter fields are ignored (a client
// may send a superset of what an entity knows); an unknown sort field is VALIDATION-06.
func (e entity) buildQuery(req app.SearchRequest) (query, error) {
	b := &builder{where: []string{"deleted_at IS NULL"}}

	names := make([]string, 0, len(req.Filters))
	for name := range req.Filters {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic SQL, easier to test and to read in pg_stat_statements
	for _, name := range names {
		f, known := e.fields[name]
		if !known {
			continue
		}
		for _, condition := range req.Filters[name] {
			clause, err := b.condition(name, f, condition)
			if err != nil {
				return query{}, err
			}
			b.where = append(b.where, clause)
		}
	}

	orderBy, err := e.orderBy(req.Sort)
	if err != nil {
		return query{}, err
	}
	return query{where: strings.Join(b.where, " AND "), orderBy: orderBy, args: b.args}, nil
}

func (e entity) orderBy(sortSpec string) (string, error) {
	if sortSpec == "" {
		return "created_at DESC, id", nil
	}
	name, direction := strings.TrimPrefix(sortSpec, "-"), "ASC"
	if strings.HasPrefix(sortSpec, "-") {
		direction = "DESC"
	}
	f, known := e.fields[name]
	if !known {
		return "", apperr.Validation(apperr.CodeUnknownSort, "cannot sort by this field",
			map[string]any{"field": name, "allowed": e.names()})
	}
	return f.column + " " + direction + ", id", nil // id breaks ties so pages never overlap
}

func (b *builder) condition(name string, f field, c app.Condition) (string, error) {
	op := strings.ToLower(c.Op)
	if op == "" {
		op = "eq"
	}
	bad := func(reason string) error {
		return apperr.Validation(apperr.CodeInvalidFilter, reason, map[string]any{"field": name, "op": op})
	}

	switch op {
	case "eq", "neq":
		v, err := coerce(f, c.Value)
		if err != nil {
			return "", bad(err.Error())
		}
		sqlOp := map[string]string{"eq": "=", "neq": "<>"}[op]
		return fmt.Sprintf("%s %s %s", f.column, sqlOp, b.param(v)), nil
	case "gt", "gte", "lt", "lte":
		if f.kind == typeEnum {
			return "", bad("ordering operators are not supported on this field")
		}
		v, err := coerce(f, c.Value)
		if err != nil {
			return "", bad(err.Error())
		}
		sqlOp := map[string]string{"gt": ">", "gte": ">=", "lt": "<", "lte": "<="}[op]
		return fmt.Sprintf("%s %s %s", f.column, sqlOp, b.param(v)), nil
	case "in":
		values, err := coerceList(f, c.Value, 0)
		if err != nil {
			return "", bad(err.Error())
		}
		if len(values) == 0 || len(values) > app.MaxInValues {
			return "", bad("in expects between 1 and 100 values")
		}
		placeholders := make([]string, len(values))
		for i, v := range values {
			placeholders[i] = b.param(v)
		}
		return fmt.Sprintf("%s IN (%s)", f.column, strings.Join(placeholders, ", ")), nil
	case "between":
		if f.kind == typeEnum {
			return "", bad("ordering operators are not supported on this field")
		}
		values, err := coerceList(f, c.Value, 2)
		if err != nil {
			return "", bad(err.Error())
		}
		return fmt.Sprintf("%s BETWEEN %s AND %s", f.column, b.param(values[0]), b.param(values[1])), nil
	case "lk":
		if f.kind != typeString {
			return "", bad("lk is only supported on text fields")
		}
		text, ok := c.Value.(string)
		if !ok {
			return "", bad("lk expects a string")
		}
		return fmt.Sprintf(`%s ILIKE %s ESCAPE '\'`, f.column, b.param("%"+escapeLike(text)+"%")), nil
	default:
		return "", bad("unknown operator")
	}
}

// escapeLike neutralizes the LIKE wildcards: "50%" must match the text "50%", not everything starting with 50.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func coerceList(f field, value any, exactly int) ([]any, error) {
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("expected a list of values")
	}
	if exactly > 0 && len(list) != exactly {
		return nil, fmt.Errorf("expected exactly %d values", exactly)
	}
	out := make([]any, len(list))
	for i, item := range list {
		v, err := coerce(f, item)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// coerce converts a decoded JSON value to the Go type the driver expects for the column.
func coerce(f field, value any) (any, error) {
	switch f.kind {
	case typeString:
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("expected a string")
		}
		return s, nil
	case typeUUID:
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("expected a UUID")
		}
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("expected a UUID")
		}
		return id, nil
	case typeInt:
		return coerceInt(value)
	case typeTime:
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("expected an RFC 3339 timestamp")
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC(), nil
			}
		}
		return nil, fmt.Errorf("expected an RFC 3339 timestamp or a YYYY-MM-DD date")
	case typeEnum:
		s, ok := value.(string)
		if ok {
			for _, allowed := range f.allowed {
				if s == allowed {
					return s, nil
				}
			}
		}
		return nil, fmt.Errorf("expected one of %s", strings.Join(f.allowed, ", "))
	}
	return nil, fmt.Errorf("unsupported field type")
}

func coerceInt(value any) (any, error) {
	errNotInt := fmt.Errorf("expected an integer")
	switch v := value.(type) {
	case json.Number:
		n, err := strconv.ParseInt(v.String(), 10, 64)
		if err != nil {
			return nil, errNotInt
		}
		return n, nil
	case float64:
		if v != math.Trunc(v) || math.Abs(v) > 1<<53 {
			return nil, errNotInt
		}
		return int64(v), nil
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, errNotInt
		}
		return n, nil
	}
	return nil, errNotInt
}
