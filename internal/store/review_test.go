package store

import (
	"context"
	"errors"
	"testing"
)

// testReviewStore is testStore plus the review tables. Gated on
// TEST_DATABASE_URL like every DB test here (see testdb_test.go).
func testReviewStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	// Twice: the DDL is shared with `rag migrate`, which must be re-runnable.
	for range 2 {
		if err := s.Exec(context.Background(), ReviewTablesDDL); err != nil {
			t.Fatalf("create review tables: %v", err)
		}
	}
	return s
}

func reviewItem(id, batch string) ReviewItem {
	return ReviewItem{ID: id, BatchID: batch, TicketKey: "SUP-1", Question: "q " + id, Draft: "d " + id}
}

func TestReviewStore(t *testing.T) {
	ctx := context.Background()
	s := testReviewStore(t)

	t.Run("import is idempotent on item_id and keeps existing content", func(t *testing.T) {
		score := 0.7
		first := reviewItem("i1", "b1")
		first.Score = &score
		n, err := s.InsertReviewItems(ctx, []ReviewItem{first, reviewItem("i2", "b1")})
		if err != nil || n != 2 {
			t.Fatalf("first import = %d, %v", n, err)
		}
		changed := reviewItem("i1", "b2")
		changed.Draft = "CHANGED"
		n, err = s.InsertReviewItems(ctx, []ReviewItem{changed, reviewItem("i3", "b1"), reviewItem("i3", "b1")})
		if err != nil || n != 1 {
			t.Fatalf("second import = %d, %v (want 1 inserted)", n, err)
		}
		got, err := s.GetReviewItem(ctx, "i1")
		if err != nil || got == nil {
			t.Fatalf("GetReviewItem: %v, %v", got, err)
		}
		if got.Draft != "d i1" || got.BatchID != "b1" || got.Score == nil || *got.Score != 0.7 || got.CreatedAt.IsZero() {
			t.Errorf("existing item changed or lost data: %+v", got)
		}
		if missing, err := s.GetReviewItem(ctx, "nope"); err != nil || missing != nil {
			t.Errorf("GetReviewItem(unknown) = %v, %v; want nil, nil", missing, err)
		}
	})

	t.Run("next, progress and grades are per reviewer", func(t *testing.T) {
		const a, b = "reviewer-aaaa-1111", "reviewer-bbbb-2222"
		next, err := s.NextReviewItem(ctx, a)
		if err != nil || next == nil || next.ID != "i1" {
			t.Fatalf("next for A = %+v, %v", next, err)
		}
		if err := s.UpsertReviewGrade(ctx, "i1", a, "good", "fine"); err != nil {
			t.Fatal(err)
		}
		if next, _ = s.NextReviewItem(ctx, a); next == nil || next.ID == "i1" {
			t.Errorf("A is offered an item already graded: %+v", next)
		}
		// i1 now has a grade, so B (who has graded nothing) is steered to an
		// ungraded item first: coverage spreads.
		if next, _ = s.NextReviewItem(ctx, b); next == nil || next.ID == "i1" {
			t.Errorf("fewest-grades-first: B was offered %+v", next)
		}
		pa, _ := s.ReviewProgressFor(ctx, a)
		pb, _ := s.ReviewProgressFor(ctx, b)
		if pa != (ReviewProgress{Graded: 1, Total: 3}) || pb != (ReviewProgress{Graded: 0, Total: 3}) {
			t.Errorf("progress A=%+v B=%+v", pa, pb)
		}

		for _, id := range []string{"i2", "i3"} {
			if err := s.UpsertReviewGrade(ctx, id, a, "meh", ""); err != nil {
				t.Fatal(err)
			}
		}
		if next, err = s.NextReviewItem(ctx, a); err != nil || next != nil {
			t.Errorf("A has graded everything, next = %+v, %v", next, err)
		}
	})

	t.Run("regrade replaces, constraints hold, results list grades", func(t *testing.T) {
		const a, b = "reviewer-aaaa-1111", "reviewer-bbbb-2222"
		if err := s.UpsertReviewGrade(ctx, "i1", a, "bad", "changed my mind"); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertReviewGrade(ctx, "i1", b, "good", ""); err != nil {
			t.Fatal(err)
		}
		res, err := s.ReviewResults(ctx, "b1")
		if err != nil {
			t.Fatal(err)
		}
		// i1 (A replaced, B), i2 and i3 (A): 4 rows, one per (item, reviewer).
		if len(res) != 4 {
			t.Fatalf("results = %d rows, want 4: %+v", len(res), res)
		}
		if res[0].ItemID != "i1" || res[0].ReviewerID != a || res[0].Grade != "bad" || res[0].Note != "changed my mind" {
			t.Errorf("regrade not applied: %+v", res[0])
		}
		if other, _ := s.ReviewResults(ctx, "nobody"); other == nil || len(other) != 0 {
			t.Errorf("unknown batch = %+v, want empty non-nil", other)
		}

		if err := s.UpsertReviewGrade(ctx, "nope", a, "good", ""); !errors.Is(err, ErrReviewItemNotFound) {
			t.Errorf("grade on unknown item = %v, want ErrReviewItemNotFound", err)
		}
		if err := s.UpsertReviewGrade(ctx, "i1", a, "great", ""); err == nil {
			t.Error("grade outside the enum was accepted by the CHECK constraint")
		}
	})

	t.Run("undo deletes only the caller's grade", func(t *testing.T) {
		const a, b = "reviewer-aaaa-1111", "reviewer-bbbb-2222"
		if ok, err := s.DeleteReviewGrade(ctx, "i1", a); err != nil || !ok {
			t.Fatalf("undo = %v, %v", ok, err)
		}
		if ok, _ := s.DeleteReviewGrade(ctx, "i1", a); ok {
			t.Error("second undo reported a deletion")
		}
		if p, _ := s.ReviewProgressFor(ctx, b); p.Graded != 1 {
			t.Errorf("B lost a grade: %+v", p)
		}
		if next, _ := s.NextReviewItem(ctx, a); next == nil || next.ID != "i1" {
			t.Errorf("after undo A should see i1 again, got %+v", next)
		}
	})
}
