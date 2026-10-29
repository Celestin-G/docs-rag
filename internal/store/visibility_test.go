package store

import (
	"context"
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
// compile-time check proves nothing about it. Skips when TEST_DATABASE_URL is
// unset so the default `go test ./...` stays runnable without infrastructure,
// and never reads DATABASE_URL — see testdb_test.go. This test queries
// jsonb_each directly with no table involved, so it needs a bare connection
// rather than the schema-per-test isolation testStore provides.
//
//	TEST_DATABASE_URL=postgres://... go test ./internal/store/
func TestVisibilityPredicate(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	for name, tc := range map[string]struct {
		// visibility is what the page declares. nil is an ungated page: Insert
		// leaves the column NULL rather than writing a JSON value, and jsonb_each
		// on NULL yields no rows. Passing the JSON scalar `null` here instead would
		// raise "cannot call jsonb_each on a non-object" — a state the writer never
		// produces.
		visibility interface{}
		selected   string // what the caller asked for
		want       bool   // eligible?
	}{
		"ungated page is always eligible":                  {nil, `{"numscript":"legacy"}`, true},
		"empty object gates nothing":                       {`{}`, `{"numscript":"legacy"}`, true},
		"matching value is eligible":                       {`{"numscript":["new"]}`, `{"numscript":"new"}`, true},
		"non-matching value is excluded":                   {`{"numscript":["new"]}`, `{"numscript":"legacy"}`, false},
		"unselected dimension does not gate":               {`{"numscript":["new"]}`, `{}`, true},
		"other dimension selected is ignored":              {`{"numscript":["new"]}`, `{"sdk":"go"}`, true},
		"value within a list is eligible":                  {`{"sdk":["go","python"]}`, `{"sdk":"python"}`, true},
		"value outside a list is excluded":                 {`{"sdk":["go","python"]}`, `{"sdk":"java"}`, false},
		"all constraints must hold":                        {`{"numscript":["new"],"sdk":["go"]}`, `{"numscript":"new","sdk":"java"}`, false},
		"empty allowed list excludes every selected value": {`{"numscript":[]}`, `{"numscript":"legacy"}`, false},
		"empty allowed list does not gate when unselected": {`{"numscript":[]}`, `{}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			var got bool
			if err := pool.QueryRow(ctx, visibilityPredicate, tc.visibility, tc.selected).Scan(&got); err != nil {
				t.Fatalf("query: %v", err)
			}
			if got != tc.want {
				t.Errorf("visibility=%v selected=%s → eligible=%v, want %v",
					tc.visibility, tc.selected, got, tc.want)
			}
		})
	}
}

// TestVisibilityPredicateRejectsJSONNullScalar pins down the assumption the
// "ungated page" doc comment above states as fact: Insert always writes SQL
// NULL for an ungated page, never the JSON scalar `null`, because jsonb_each
// errors on the latter. If a future change to Insert (or a hand-written row)
// ever puts a literal JSON null in the column, this must surface as a loud
// query error rather than silently misclassifying the page as ungated.
func TestVisibilityPredicateRejectsJSONNullScalar(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	var got bool
	err = pool.QueryRow(ctx, visibilityPredicate, "null", `{"numscript":"legacy"}`).Scan(&got)
	if err == nil {
		t.Fatalf("query on JSON null scalar visibility succeeded (eligible=%v), want an error — "+
			"jsonb_each(NULL::jsonb) should raise, not silently pass", got)
	}
}
