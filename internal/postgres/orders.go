package postgres

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/ronybrand/go-order-api/internal/app"
	"github.com/ronybrand/go-order-api/internal/apperr"
	"github.com/ronybrand/go-order-api/internal/domain"
)

const orderColumns = `id, customer_id, status, version,
	created_at, updated_at, created_by, updated_by, deleted_at, deleted_by`

func itoa(n int) string { return strconv.Itoa(n) }

type orderRepo struct{ db db }

var _ app.OrderRepository = (*orderRepo)(nil)

func scanOrder(row pgx.Row) (*domain.Order, error) {
	var o domain.Order
	err := row.Scan(&o.ID, &o.CustomerID, &o.Status, &o.Version,
		&o.CreatedAt, &o.UpdatedAt, &o.CreatedBy, &o.UpdatedBy, &o.DeletedAt, &o.DeletedBy)
	if err != nil {
		return nil, err
	}
	o.CreatedAt, o.UpdatedAt = o.CreatedAt.UTC(), o.UpdatedAt.UTC()
	return &o, nil
}

func orderNotFound(id uuid.UUID) error {
	return apperr.NotFound(apperr.CodeOrderNotFound, "order not found", map[string]any{"id": id})
}

func (r *orderRepo) Get(ctx context.Context, id uuid.UUID) (*domain.Order, error) {
	order, err := scanOrder(r.db.QueryRow(ctx, "SELECT "+orderColumns+" FROM orders WHERE id = $1 AND deleted_at IS NULL", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, orderNotFound(id)
	}
	if err != nil {
		return nil, wrap("read order", err)
	}
	items, err := r.itemsOf(ctx, order.ID)
	if err != nil {
		return nil, err
	}
	order.Items = items[order.ID]
	return order, nil
}

// itemsOf loads the items of the given orders in insertion order (seq), grouped by order.
func (r *orderRepo) itemsOf(ctx context.Context, orderIDs ...uuid.UUID) (map[uuid.UUID][]domain.Item, error) {
	rows, err := r.db.Query(ctx, `SELECT id, order_id, description, unit_price::text, quantity
		FROM order_items WHERE order_id = ANY($1) ORDER BY seq`, orderIDs)
	if err != nil {
		return nil, wrap("read items", err)
	}
	defer rows.Close()
	grouped := make(map[uuid.UUID][]domain.Item, len(orderIDs))
	for rows.Next() {
		var item domain.Item
		var price string
		if err := rows.Scan(&item.ID, &item.OrderID, &item.Description, &price, &item.Quantity); err != nil {
			return nil, wrap("scan item", err)
		}
		if item.UnitPrice, err = decimal.NewFromString(price); err != nil {
			return nil, wrap("parse unit price", err)
		}
		grouped[item.OrderID] = append(grouped[item.OrderID], item)
	}
	return grouped, wrap("read items", rows.Err())
}

func (r *orderRepo) Create(ctx context.Context, o *domain.Order) error {
	return wrap("insert order", pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO orders
			(customer_id, status, version, created_at, updated_at, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			o.CustomerID, o.Status, o.Version, o.CreatedAt, o.UpdatedAt, o.CreatedBy, o.UpdatedBy).Scan(&o.ID)
		if err != nil {
			return err
		}
		for i := range o.Items {
			o.Items[i].OrderID = o.ID
			if err := insertItem(ctx, tx, &o.Items[i]); err != nil {
				return err
			}
		}
		return nil
	}))
}

func insertItem(ctx context.Context, conn db, item *domain.Item) error {
	// The price travels as text and is cast by the server: no float ever touches money.
	return conn.QueryRow(ctx, `INSERT INTO order_items (order_id, description, unit_price, quantity)
		VALUES ($1, $2, $3::text::numeric, $4) RETURNING id`,
		item.OrderID, item.Description, item.UnitPrice.StringFixed(2), item.Quantity).Scan(&item.ID)
}

func (r *orderRepo) Save(ctx context.Context, o *domain.Order) error {
	err := pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE orders
			SET status = $3, updated_at = $4, updated_by = $5, version = version + 1
			WHERE id = $1 AND version = $2 AND deleted_at IS NULL`,
			o.ID, o.Version, o.Status, o.UpdatedAt, o.UpdatedBy)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return concurrentModification()
		}
		return syncItems(ctx, tx, o)
	})
	if err != nil {
		return wrap("save order", err)
	}
	o.Version++
	return nil
}

// syncItems makes the stored items equal to o.Items: rows no longer in the aggregate are deleted, new ones
// (zero id) are inserted, the rest are updated. The order row was just updated, so it is locked for us.
func syncItems(ctx context.Context, conn db, o *domain.Order) error {
	keep := make([]uuid.UUID, 0, len(o.Items))
	for _, item := range o.Items {
		if item.ID != uuid.Nil {
			keep = append(keep, item.ID)
		}
	}
	if _, err := conn.Exec(ctx, "DELETE FROM order_items WHERE order_id = $1 AND NOT (id = ANY($2))", o.ID, keep); err != nil {
		return err
	}
	if err := updateItems(ctx, conn, o.ID, o.Items); err != nil {
		return err
	}
	for i := range o.Items {
		if item := &o.Items[i]; item.ID == uuid.Nil {
			item.OrderID = o.ID
			if err := insertItem(ctx, conn, item); err != nil {
				return err
			}
		}
	}
	return nil
}

// updateItems writes the already stored items in one statement, however many there are, and only touches the
// rows that actually differ. Prices travel as text and are cast by the server, like on insert.
func updateItems(ctx context.Context, conn db, orderID uuid.UUID, items []domain.Item) error {
	var ids []uuid.UUID
	var descriptions, prices []string
	var quantities []int32
	for _, item := range items {
		if item.ID == uuid.Nil {
			continue
		}
		ids = append(ids, item.ID)
		descriptions = append(descriptions, item.Description)
		prices = append(prices, item.UnitPrice.StringFixed(2))
		quantities = append(quantities, int32(item.Quantity)) //nolint:gosec // bounded by domain.MaxQuantity
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := conn.Exec(ctx, `UPDATE order_items AS i
		SET description = v.description, unit_price = v.unit_price::numeric, quantity = v.quantity
		FROM unnest($2::uuid[], $3::text[], $4::text[], $5::int[]) AS v(id, description, unit_price, quantity)
		WHERE i.order_id = $1 AND i.id = v.id
		  AND (i.description, i.unit_price, i.quantity) IS DISTINCT FROM (v.description, v.unit_price::numeric, v.quantity)`,
		orderID, ids, descriptions, prices, quantities)
	return err
}

func (r *orderRepo) SoftDelete(ctx context.Context, id uuid.UUID, actor string, at time.Time) error {
	tag, err := r.db.Exec(ctx, `UPDATE orders
		SET deleted_at = $2, deleted_by = $3, updated_at = $2, updated_by = $3, version = version + 1
		WHERE id = $1 AND deleted_at IS NULL`, id, at, actor)
	if err != nil {
		return wrap("delete order", err)
	}
	if tag.RowsAffected() == 0 {
		return orderNotFound(id)
	}
	return nil
}

func (r *orderRepo) HasActive(ctx context.Context, customerID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM orders WHERE customer_id = $1 AND deleted_at IS NULL)", customerID).Scan(&exists)
	return exists, wrap("check active orders", err)
}

func (r *orderRepo) Search(ctx context.Context, req app.SearchRequest) (app.Page[domain.Order], error) {
	q, err := orderEntity.buildQuery(req)
	if err != nil {
		return app.Page[domain.Order]{}, err
	}
	page := app.Page[domain.Order]{Page: req.Page, Size: req.Size, Items: []domain.Order{}}
	if err := r.db.QueryRow(ctx, "SELECT count(*) FROM orders WHERE "+q.where, q.args...).Scan(&page.Total); err != nil {
		return page, wrap("count orders", err)
	}
	args := append(q.args, req.Size, req.Offset())
	rows, err := r.db.Query(ctx, "SELECT "+orderColumns+" FROM orders WHERE "+q.where+
		" ORDER BY "+q.orderBy+" LIMIT $"+itoa(len(q.args)+1)+" OFFSET $"+itoa(len(q.args)+2), args...)
	if err != nil {
		return page, wrap("search orders", err)
	}
	ids := []uuid.UUID{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			rows.Close()
			return page, wrap("scan order", err)
		}
		page.Items = append(page.Items, *o)
		ids = append(ids, o.ID)
	}
	rows.Close() // the connection must be free before the items query
	if err := rows.Err(); err != nil {
		return page, wrap("search orders", err)
	}
	if len(ids) == 0 {
		return page, nil
	}
	items, err := r.itemsOf(ctx, ids...)
	if err != nil {
		return page, err
	}
	for i := range page.Items {
		page.Items[i].Items = items[page.Items[i].ID]
	}
	return page, nil
}
