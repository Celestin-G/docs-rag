package store

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// visibilityPredicate is the eligibility rule used by both search queries,
// lifted out so it can be exercised on its own. Keep it in sync with the WHERE
// clauses in searchVector and searchHybrid.
//
// Reads as: a chunk is eligible unless it constrains a dimension the caller
// selected and the selected value is not among the ones it allows.
const visibilityPredicate = `
	SELECT NOT EXISTS (
	  SELECT 1 FROM jsonb_each($1::jsonb) AS v(dim, allowed)
	  WHERE $2::jsonb ? v.dim AND NOT (allowed ? ($2::jsonb ->> v.dim)))`

// TestVisibilityPredicate needs a live PostgreSQL: the rule is pure SQL, so a
// compile-time check proves nothing about it. Skips when DATABASE_URL is unset
// so the default `go test ./...` stays runnable without infrastructure.
//
//	DATABASE_URL=postgres://... go test ./internal/store/
func TestVisibilityPredicate(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping the SQL-level check")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	for name, tc := range map[string]struct {
		visibility string // what the page declares; "null" for an ungated page
		selected   string // what the caller asked for
		want       bool   // eligible?
	}{
		"ungated page is always eligible":     {`null`, `{"numscript":"legacy"}`, true},
		"matching value is eligible":          {`{"numscript":["new"]}`, `{"numscript":"new"}`, true},
		"non-matching value is excluded":      {`{"numscript":["new"]}`, `{"numscript":"legacy"}`, false},
		"unselected dimension does not gate":  {`{"numscript":["new"]}`, `{}`, true},
		"other dimension selected is ignored": {`{"numscript":["new"]}`, `{"sdk":"go"}`, true},
		"value within a list is eligible":     {`{"sdk":["go","python"]}`, `{"sdk":"python"}`, true},
		"value outside a list is excluded":    {`{"sdk":["go","python"]}`, `{"sdk":"java"}`, false},
		"all constraints must hold":           {`{"numscript":["new"],"sdk":["go"]}`, `{"numscript":"new","sdk":"java"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			var got bool
			if err := pool.QueryRow(ctx, visibilityPredicate, tc.visibility, tc.selected).Scan(&got); err != nil {
				t.Fatalf("query: %v", err)
			}
			if got != tc.want {
				t.Errorf("visibility=%s selected=%s → eligible=%v, want %v",
					tc.visibility, tc.selected, got, tc.want)
			}
		})
	}
}
