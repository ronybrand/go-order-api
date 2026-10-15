package postgres

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Guard against drift between the schema and the search whitelist: a new column that nobody added to the
// entity would silently be impossible to filter or sort on. Needs TEST_DATABASE_URL.
func TestSearchableFields_CoverEveryColumnOfTheTable(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	// The soft-delete markers are deliberately not searchable: deleted rows are never visible.
	notSearchable := []string{"deleted_at", "deleted_by"}
	for _, e := range []entity{customerEntity, orderEntity} {
		rows, err := pool.Query(ctx,
			"SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1", e.table)
		if err != nil {
			t.Fatal(err)
		}
		var columns []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			columns = append(columns, name)
		}
		rows.Close()

		searchable := map[string]bool{}
		for _, f := range e.fields {
			searchable[f.column] = true
		}
		for _, column := range columns {
			if !slices.Contains(notSearchable, column) && !searchable[column] {
				t.Errorf("%s.%s exists in the schema but is not searchable: add it to the entity in search.go (or to notSearchable here, with a reason)", e.table, column)
			}
		}
		for column := range searchable {
			if !slices.Contains(columns, column) {
				t.Errorf("search entity %s lists %q, which is not a column of the table", e.table, column)
			}
		}
	}
}
