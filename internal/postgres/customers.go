package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/apperr"
	"github.com/ronybrand/go-order-api/internal/domain"
)

const customerColumns = `id, name, tax_id, passport_number, email, version,
	created_at, updated_at, created_by, updated_by, deleted_at, deleted_by`

type customerRepo struct{ db db }

var _ app.CustomerRepository = (*customerRepo)(nil)

func scanCustomer(row pgx.Row) (*domain.Customer, error) {
	var c domain.Customer
	err := row.Scan(&c.ID, &c.Name, &c.TaxID, &c.PassportNumber, &c.Email, &c.Version,
		&c.CreatedAt, &c.UpdatedAt, &c.CreatedBy, &c.UpdatedBy, &c.DeletedAt, &c.DeletedBy)
	if err != nil {
		return nil, err
	}
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	return &c, nil
}

func customerNotFound(id uuid.UUID) error {
	return apperr.NotFound(apperr.CodeCustomerNotFound, "customer not found", map[string]any{"id": id})
}

func (r *customerRepo) read(ctx context.Context, id uuid.UUID, lock string) (*domain.Customer, error) {
	row := r.db.QueryRow(ctx, "SELECT "+customerColumns+" FROM customers WHERE id = $1 AND deleted_at IS NULL"+lock, id)
	c, err := scanCustomer(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, customerNotFound(id)
	}
	return c, wrap("read customer", err)
}

func (r *customerRepo) Get(ctx context.Context, id uuid.UUID) (*domain.Customer, error) {
	return r.read(ctx, id, "")
}

func (r *customerRepo) LockForUpdate(ctx context.Context, id uuid.UUID) (*domain.Customer, error) {
	return r.read(ctx, id, " FOR UPDATE")
}

func (r *customerRepo) LockForShare(ctx context.Context, id uuid.UUID) (*domain.Customer, error) {
	return r.read(ctx, id, " FOR SHARE")
}

func (r *customerRepo) Create(ctx context.Context, c *domain.Customer) error {
	err := r.db.QueryRow(ctx, `INSERT INTO customers
		(name, tax_id, passport_number, email, version, created_at, updated_at, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		c.Name, c.TaxID, c.PassportNumber, c.Email, c.Version, c.CreatedAt, c.UpdatedAt, c.CreatedBy, c.UpdatedBy,
	).Scan(&c.ID)
	return wrap("insert customer", err)
}

func (r *customerRepo) Update(ctx context.Context, c *domain.Customer) error {
	tag, err := r.db.Exec(ctx, `UPDATE customers
		SET name = $3, tax_id = $4, passport_number = $5, email = $6, updated_at = $7, updated_by = $8, version = version + 1
		WHERE id = $1 AND version = $2 AND deleted_at IS NULL`,
		c.ID, c.Version, c.Name, c.TaxID, c.PassportNumber, c.Email, c.UpdatedAt, c.UpdatedBy)
	if err != nil {
		return wrap("update customer", err)
	}
	if tag.RowsAffected() == 0 {
		return concurrentModification()
	}
	c.Version++
	return nil
}

func (r *customerRepo) SoftDelete(ctx context.Context, id uuid.UUID, actor string, at time.Time) error {
	tag, err := r.db.Exec(ctx, `UPDATE customers
		SET deleted_at = $2, deleted_by = $3, updated_at = $2, updated_by = $3, version = version + 1
		WHERE id = $1 AND deleted_at IS NULL`, id, at, actor)
	if err != nil {
		return wrap("delete customer", err)
	}
	if tag.RowsAffected() == 0 {
		return customerNotFound(id)
	}
	return nil
}

func (r *customerRepo) TaxIDTaken(ctx context.Context, taxID string, except uuid.UUID) (bool, error) {
	return r.taken(ctx, "tax_id", taxID, except)
}

func (r *customerRepo) PassportTaken(ctx context.Context, passport string, except uuid.UUID) (bool, error) {
	return r.taken(ctx, "passport_number", passport, except)
}

// taken is called with a column name from this file only, never with client input.
func (r *customerRepo) taken(ctx context.Context, column, value string, except uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM customers WHERE "+column+
		" = $1 AND id <> $2 AND deleted_at IS NULL)", value, except).Scan(&exists)
	return exists, wrap("check uniqueness", err)
}

func (r *customerRepo) Search(ctx context.Context, req app.SearchRequest) (app.Page[domain.Customer], error) {
	q, err := customerEntity.buildQuery(req)
	if err != nil {
		return app.Page[domain.Customer]{}, err
	}
	page := app.Page[domain.Customer]{Page: req.Page, Size: req.Size, Items: []domain.Customer{}}
	if err := r.db.QueryRow(ctx, "SELECT count(*) FROM customers WHERE "+q.where, q.args...).Scan(&page.Total); err != nil {
		return page, wrap("count customers", err)
	}
	args := append(q.args, req.Size, req.Offset())
	rows, err := r.db.Query(ctx, "SELECT "+customerColumns+" FROM customers WHERE "+q.where+
		" ORDER BY "+q.orderBy+" LIMIT $"+itoa(len(q.args)+1)+" OFFSET $"+itoa(len(q.args)+2), args...)
	if err != nil {
		return page, wrap("search customers", err)
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanCustomer(rows)
		if err != nil {
			return page, wrap("scan customer", err)
		}
		page.Items = append(page.Items, *c)
	}
	return page, wrap("search customers", rows.Err())
}
