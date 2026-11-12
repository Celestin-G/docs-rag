package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ReviewTablesDDL creates the two tables behind the internal review page,
// where teammates grade the support bot's draft answers. Idempotent, and
// shared by cmd/migrate.go and the store tests so the two cannot drift.
//
// review_items.id is the external item_id chosen by the importer (text, not a
// serial), so re-importing the same batch is a no-op rather than a duplicate.
// review_grades holds one row per (item, reviewer): grading again replaces it.
// Deleting an item cascades to its grades.
const ReviewTablesDDL = `
CREATE TABLE IF NOT EXISTS review_items (
  id         TEXT PRIMARY KEY,
  batch_id   TEXT NOT NULL,
  ticket_key TEXT NOT NULL DEFAULT '',
  question   TEXT NOT NULL,
  draft      TEXT NOT NULL,
  team_reply TEXT NOT NULL DEFAULT '',
  decision   TEXT NOT NULL DEFAULT '',
  score      DOUBLE PRECISION,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS review_items_batch_idx ON review_items (batch_id);

CREATE TABLE IF NOT EXISTS review_grades (
  id          BIGSERIAL PRIMARY KEY,
  item_id     TEXT NOT NULL REFERENCES review_items (id) ON DELETE CASCADE,
  reviewer_id TEXT NOT NULL,
  grade       TEXT NOT NULL CHECK (grade IN ('bad', 'meh', 'good')),
  note        TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (item_id, reviewer_id)
);

CREATE INDEX IF NOT EXISTS review_grades_reviewer_idx ON review_grades (reviewer_id);
`

// ErrReviewItemNotFound is returned when a grade targets an item that does
// not exist.
var ErrReviewItemNotFound = errors.New("review item not found")

// ReviewItem is one bot draft to grade.
type ReviewItem struct {
	ID        string // external item_id
	BatchID   string
	TicketKey string
	Question  string
	Draft     string // markdown
	TeamReply string
	Decision  string
	Score     *float64
	CreatedAt time.Time
}

// ReviewProgress is how far one reviewer has got: Graded of Total items.
type ReviewProgress struct {
	Graded int
	Total  int
}

// ReviewGrade is one reviewer's grade of one item.
type ReviewGrade struct {
	ItemID     string
	ReviewerID string
	Grade      string
	Note       string
	CreatedAt  time.Time
}

// InsertReviewItems imports items in one transaction and reports how many were
// inserted; the rest already existed. An existing item_id is left untouched
// (ON CONFLICT DO NOTHING), even if the new payload differs or belongs to
// another batch: grades stay attached to the exact content that was graded.
func (s *Store) InsertReviewItems(ctx context.Context, items []ReviewItem) (inserted int, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, it := range items {
		tag, err := tx.Exec(ctx,
			`INSERT INTO review_items (id, batch_id, ticket_key, question, draft, team_reply, decision, score)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 ON CONFLICT (id) DO NOTHING`,
			it.ID, it.BatchID, it.TicketKey, it.Question, it.Draft, it.TeamReply, it.Decision, it.Score)
		if err != nil {
			return 0, fmt.Errorf("insert review item %q: %w", it.ID, err)
		}
		inserted += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return inserted, nil
}

const reviewItemCols = `i.id, i.batch_id, i.ticket_key, i.question, i.draft, i.team_reply, i.decision, i.score, i.created_at`

func scanReviewItem(row pgx.Row) (*ReviewItem, error) {
	var it ReviewItem
	err := row.Scan(&it.ID, &it.BatchID, &it.TicketKey, &it.Question, &it.Draft, &it.TeamReply, &it.Decision, &it.Score, &it.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// NextReviewItem returns an item the reviewer has not graded yet, or nil when
// none is left. Items with the fewest grades come first so coverage spreads
// across items; ties break on import order, then id, so the order is stable.
func (s *Store) NextReviewItem(ctx context.Context, reviewer string) (*ReviewItem, error) {
	return scanReviewItem(s.pool.QueryRow(ctx,
		`SELECT `+reviewItemCols+`
		 FROM review_items i
		 WHERE NOT EXISTS (SELECT 1 FROM review_grades g WHERE g.item_id = i.id AND g.reviewer_id = $1)
		 ORDER BY (SELECT count(*) FROM review_grades c WHERE c.item_id = i.id), i.created_at, i.id
		 LIMIT 1`, reviewer))
}

// GetReviewItem returns one item by id, or nil if it does not exist.
func (s *Store) GetReviewItem(ctx context.Context, id string) (*ReviewItem, error) {
	return scanReviewItem(s.pool.QueryRow(ctx,
		`SELECT `+reviewItemCols+` FROM review_items i WHERE i.id = $1`, id))
}

// ReviewProgressFor counts the items this reviewer has graded and all items.
func (s *Store) ReviewProgressFor(ctx context.Context, reviewer string) (ReviewProgress, error) {
	var p ReviewProgress
	err := s.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM review_grades WHERE reviewer_id = $1),
		        (SELECT count(*) FROM review_items)`, reviewer).Scan(&p.Graded, &p.Total)
	return p, err
}

// UpsertReviewGrade records the reviewer's grade of an item, replacing any
// earlier one (grade, note and created_at all take the new values). Returns
// ErrReviewItemNotFound when the item does not exist. grade must be one of
// bad, meh, good; the table's CHECK constraint enforces it as a last resort.
func (s *Store) UpsertReviewGrade(ctx context.Context, itemID, reviewer, grade, note string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO review_grades (item_id, reviewer_id, grade, note) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (item_id, reviewer_id)
		 DO UPDATE SET grade = EXCLUDED.grade, note = EXCLUDED.note, created_at = NOW()`,
		itemID, reviewer, grade, note)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
		return ErrReviewItemNotFound
	}
	return err
}

// DeleteReviewGrade removes the reviewer's own grade of an item (undo) and
// reports whether there was one. Other reviewers' grades are never touched.
func (s *Store) DeleteReviewGrade(ctx context.Context, itemID, reviewer string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM review_grades WHERE item_id = $1 AND reviewer_id = $2`, itemID, reviewer)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ReviewResults lists every grade of the items imported under batchID.
func (s *Store) ReviewResults(ctx context.Context, batchID string) ([]ReviewGrade, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT g.item_id, g.reviewer_id, g.grade, g.note, g.created_at
		 FROM review_grades g JOIN review_items i ON i.id = g.item_id
		 WHERE i.batch_id = $1
		 ORDER BY g.item_id, g.created_at, g.id`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReviewGrade{}
	for rows.Next() {
		var g ReviewGrade
		if err := rows.Scan(&g.ItemID, &g.ReviewerID, &g.Grade, &g.Note, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
