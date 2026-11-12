package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/celestingeniaut/docs-rag/internal/store"
)

// Review page: teammates grade the support bot's draft answers. See the
// "Review page" section of README.md for the routes and deployment notes.
//
// Access model: the whole service is only reachable over Tailscale and there
// is no login, so the grading routes (/review, /api/review/next,
// /api/review/grades) carry no token; they validate every field instead. The
// two admin routes (batch import, results) require x-review-token, compared
// in constant time against REVIEW_ADMIN_TOKEN, which has no default: unset,
// both answer 503.

//go:embed review_assets/index.html review_assets/app.js review_assets/app.css
var reviewAssets embed.FS

const (
	reviewTokenHeader = "x-review-token"

	// Admin import limits.
	maxReviewBatchBytes = 8 << 20
	maxReviewItems      = 500

	// Field caps, in bytes.
	maxReviewIDLen       = 200 // item_id, batch_id
	maxReviewTicketLen   = 100
	maxReviewQuestionLen = 20_000
	maxReviewDraftLen    = 100_000
	maxReviewReplyLen    = 100_000
	maxReviewDecisionLen = 100

	// Grading routes: tiny bodies (a note is at most maxReviewNoteLen).
	maxReviewGradeBytes = 16 << 10
	maxReviewNoteLen    = 2000 // characters (runes), like the textarea maxlength

	// Writers get a dedicated bucket: a reviewer clicking through items is
	// far faster than someone asking questions, and must not share /api/ask's
	// 5 per minute.
	reviewRatePerMinute = 120
	reviewRateBurst     = 30
)

// reviewerRE is the reviewer id format: the page generates a UUID, but any
// 8-64 characters of [A-Za-z0-9_-] is accepted.
var reviewerRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

var validGrades = map[string]bool{"bad": true, "meh": true, "good": true}

// reviewStore is the part of *store.Store the review routes use, so tests can
// run the handlers against an in-memory fake.
type reviewStore interface {
	InsertReviewItems(ctx context.Context, items []store.ReviewItem) (int, error)
	NextReviewItem(ctx context.Context, reviewer string) (*store.ReviewItem, error)
	GetReviewItem(ctx context.Context, id string) (*store.ReviewItem, error)
	ReviewProgressFor(ctx context.Context, reviewer string) (store.ReviewProgress, error)
	UpsertReviewGrade(ctx context.Context, itemID, reviewer, grade, note string) error
	DeleteReviewGrade(ctx context.Context, itemID, reviewer string) (bool, error)
	ReviewResults(ctx context.Context, batchID string) ([]store.ReviewGrade, error)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeReviewBody reads one JSON object, capped at limit bytes. Unknown
// fields are ignored, like every other JSON route in this server, so the
// importer can add fields without breaking older servers. It writes the error
// response itself and reports whether decoding succeeded.
func decodeReviewBody(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body too large (max %d bytes)", limit))
			return false
		}
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// requireReviewAdmin gates the admin routes. 503 when REVIEW_ADMIN_TOKEN is
// unset (the feature is off, never open), 401 on a missing or wrong token.
func (s *srv) requireReviewAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.reviewToken == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "review admin routes are disabled: REVIEW_ADMIN_TOKEN is not set")
		return false
	}
	// Compare fixed-length digests so the comparison time does not reveal the
	// token's length.
	got := sha256.Sum256([]byte(r.Header.Get(reviewTokenHeader)))
	want := sha256.Sum256([]byte(s.reviewToken))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

// checkText validates a string field: UTF-8, no NUL (Postgres TEXT rejects
// it), at most max bytes, and non-empty when required.
func checkText(name, v string, max int, required bool) string {
	switch {
	case required && strings.TrimSpace(v) == "":
		return name + " is required"
	case len(v) > max:
		return fmt.Sprintf("%s is too long (max %d bytes)", name, max)
	case !utf8.ValidString(v) || strings.ContainsRune(v, 0):
		return name + " contains invalid characters"
	}
	return ""
}

// checkNote validates the grade note. The cap counts characters, matching the
// textarea's maxlength; the body size cap bounds the bytes.
func checkNote(v string) string {
	switch {
	case !utf8.ValidString(v) || strings.ContainsRune(v, 0):
		return "note contains invalid characters"
	case utf8.RuneCountInString(v) > maxReviewNoteLen:
		return fmt.Sprintf("note is too long (max %d characters)", maxReviewNoteLen)
	}
	return ""
}

// --- Admin: import ----------------------------------------------------------

type reviewBatchItem struct {
	ItemID    string   `json:"item_id"`
	TicketKey string   `json:"ticket_key"`
	Question  string   `json:"question"`
	Draft     string   `json:"draft"`
	TeamReply string   `json:"team_reply"`
	Decision  string   `json:"decision"`
	Score     *float64 `json:"score"`
}

type reviewBatchReq struct {
	BatchID string            `json:"batch_id"`
	Items   []reviewBatchItem `json:"items"`
}

// handleReviewBatch imports a batch of drafts. Idempotent on item_id: an item
// whose id already exists is skipped, not updated (see store.InsertReviewItems).
// The whole request is validated before anything is written.
func (s *srv) handleReviewBatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireReviewAdmin(w, r) {
		return
	}
	var req reviewBatchReq
	if !decodeReviewBody(w, r, maxReviewBatchBytes, &req) {
		return
	}
	if msg := checkText("batch_id", req.BatchID, maxReviewIDLen, true); msg != "" {
		writeJSONError(w, http.StatusBadRequest, msg)
		return
	}
	if len(req.Items) == 0 {
		writeJSONError(w, http.StatusBadRequest, "items is required")
		return
	}
	if len(req.Items) > maxReviewItems {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("too many items (max %d)", maxReviewItems))
		return
	}
	items := make([]store.ReviewItem, len(req.Items))
	for i, it := range req.Items {
		for _, c := range []struct {
			name     string
			v        string
			max      int
			required bool
		}{
			{"item_id", it.ItemID, maxReviewIDLen, true},
			{"ticket_key", it.TicketKey, maxReviewTicketLen, false},
			{"question", it.Question, maxReviewQuestionLen, true},
			{"draft", it.Draft, maxReviewDraftLen, true},
			{"team_reply", it.TeamReply, maxReviewReplyLen, false},
			{"decision", it.Decision, maxReviewDecisionLen, false},
		} {
			if msg := checkText(c.name, c.v, c.max, c.required); msg != "" {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("items[%d]: %s", i, msg))
				return
			}
		}
		items[i] = store.ReviewItem{
			ID: it.ItemID, BatchID: req.BatchID, TicketKey: it.TicketKey,
			Question: it.Question, Draft: it.Draft, TeamReply: it.TeamReply,
			Decision: it.Decision, Score: it.Score,
		}
	}
	inserted, err := s.review.InsertReviewItems(r.Context(), items)
	if err != nil {
		log.Printf("review batch insert: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"batch_id": req.BatchID,
		"received": len(items),
		"inserted": inserted,
		"skipped":  len(items) - inserted,
	})
}

// --- Admin: results ---------------------------------------------------------

type reviewGradeOut struct {
	ItemID     string `json:"item_id"`
	ReviewerID string `json:"reviewer_id"`
	Grade      string `json:"grade"`
	Note       string `json:"note"`
	CreatedAt  string `json:"created_at"`
}

func (s *srv) handleReviewResults(w http.ResponseWriter, r *http.Request) {
	if !s.requireReviewAdmin(w, r) {
		return
	}
	batchID := r.URL.Query().Get("batch_id")
	if msg := checkText("batch_id", batchID, maxReviewIDLen, true); msg != "" {
		writeJSONError(w, http.StatusBadRequest, msg)
		return
	}
	grades, err := s.review.ReviewResults(r.Context(), batchID)
	if err != nil {
		log.Printf("review results: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]reviewGradeOut, len(grades))
	for i, g := range grades {
		out[i] = reviewGradeOut{g.ItemID, g.ReviewerID, g.Grade, g.Note, g.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")}
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch_id": batchID, "grades": out})
}

// --- Grading routes ---------------------------------------------------------

type reviewItemOut struct {
	ItemID    string `json:"item_id"`
	TicketKey string `json:"ticket_key"`
	Question  string `json:"question"`   // plain text: insert with textContent
	DraftHTML string `json:"draft_html"` // sanitised HTML rendered from markdown
	TeamReply string `json:"team_reply"` // plain text: insert with textContent
}

type reviewProgressOut struct {
	Graded int `json:"graded"`
	Total  int `json:"total"`
}

// reviewView is the response of GET /api/review/next and DELETE
// /api/review/grades. Item is null when done. Decision and score are not
// exposed: they would bias the grade.
type reviewView struct {
	Done     bool              `json:"done"`
	Item     *reviewItemOut    `json:"item"`
	Progress reviewProgressOut `json:"progress"`
}

func (s *srv) reviewView(r *http.Request, reviewer string, it *store.ReviewItem) (reviewView, error) {
	p, err := s.review.ReviewProgressFor(r.Context(), reviewer)
	if err != nil {
		return reviewView{}, err
	}
	v := reviewView{Done: it == nil, Progress: reviewProgressOut{p.Graded, p.Total}}
	if it != nil {
		html, err := renderReviewMarkdown(it.Draft)
		if err != nil {
			return reviewView{}, err
		}
		v.Item = &reviewItemOut{
			ItemID: it.ID, TicketKey: it.TicketKey, Question: it.Question,
			DraftHTML: html, TeamReply: it.TeamReply,
		}
	}
	return v, nil
}

func (s *srv) handleReviewNext(w http.ResponseWriter, r *http.Request) {
	reviewer := r.URL.Query().Get("reviewer")
	if !reviewerRE.MatchString(reviewer) {
		writeJSONError(w, http.StatusBadRequest, "invalid reviewer id (8-64 characters of A-Z a-z 0-9 _ -)")
		return
	}
	it, err := s.review.NextReviewItem(r.Context(), reviewer)
	if err != nil {
		log.Printf("review next: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	v, err := s.reviewView(r, reviewer, it)
	if err != nil {
		log.Printf("review next: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type reviewGradeReq struct {
	ItemID   string `json:"item_id"`
	Reviewer string `json:"reviewer"`
	Grade    string `json:"grade"`
	Note     string `json:"note"`
}

// handleReviewGrade upserts the reviewer's grade: grading an item again
// replaces the earlier grade and note.
func (s *srv) handleReviewGrade(w http.ResponseWriter, r *http.Request) {
	var req reviewGradeReq
	if !decodeReviewBody(w, r, maxReviewGradeBytes, &req) {
		return
	}
	if !reviewerRE.MatchString(req.Reviewer) {
		writeJSONError(w, http.StatusBadRequest, "invalid reviewer id (8-64 characters of A-Z a-z 0-9 _ -)")
		return
	}
	if !validGrades[req.Grade] {
		writeJSONError(w, http.StatusBadRequest, "grade must be one of: bad, meh, good")
		return
	}
	if msg := checkText("item_id", req.ItemID, maxReviewIDLen, true); msg != "" {
		writeJSONError(w, http.StatusBadRequest, msg)
		return
	}
	if msg := checkNote(req.Note); msg != "" {
		writeJSONError(w, http.StatusBadRequest, msg)
		return
	}
	err := s.review.UpsertReviewGrade(r.Context(), req.ItemID, req.Reviewer, req.Grade, req.Note)
	if errors.Is(err, store.ErrReviewItemNotFound) {
		writeJSONError(w, http.StatusNotFound, "item not found")
		return
	}
	if err != nil {
		log.Printf("review grade: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type reviewUndoReq struct {
	ItemID   string `json:"item_id"`
	Reviewer string `json:"reviewer"`
}

// handleReviewUndo deletes the caller's own grade of an item (the "annuler"
// link) and answers with that item in the same shape as /api/review/next, so
// the page can show it again. 404 when this reviewer had no grade on it.
func (s *srv) handleReviewUndo(w http.ResponseWriter, r *http.Request) {
	var req reviewUndoReq
	if !decodeReviewBody(w, r, maxReviewGradeBytes, &req) {
		return
	}
	if !reviewerRE.MatchString(req.Reviewer) {
		writeJSONError(w, http.StatusBadRequest, "invalid reviewer id (8-64 characters of A-Z a-z 0-9 _ -)")
		return
	}
	if msg := checkText("item_id", req.ItemID, maxReviewIDLen, true); msg != "" {
		writeJSONError(w, http.StatusBadRequest, msg)
		return
	}
	deleted, err := s.review.DeleteReviewGrade(r.Context(), req.ItemID, req.Reviewer)
	if err != nil {
		log.Printf("review undo: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !deleted {
		writeJSONError(w, http.StatusNotFound, "no grade to undo")
		return
	}
	it, err := s.review.GetReviewItem(r.Context(), req.ItemID)
	if err == nil && it == nil {
		err = errors.New("item vanished after undo")
	}
	var v reviewView
	if err == nil {
		v, err = s.reviewView(r, req.Reviewer, it)
	}
	if err != nil {
		log.Printf("review undo: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// --- Page and assets --------------------------------------------------------

// reviewCSP: the page ships no inline script or style (JS and CSS are served
// as separate same-origin files), so nothing but our own files may run, and
// nothing may load images or other origins. The only HTML built from database
// text is the sanitised draft, which cannot add script by itself either.
const reviewCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"img-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func (s *srv) serveReviewAsset(file, contentType string, csp bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		b, err := reviewAssets.ReadFile("review_assets/" + file)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Content-Type-Options", "nosniff")
		if csp {
			h.Set("Content-Security-Policy", reviewCSP)
			h.Set("Referrer-Policy", "no-referrer")
		}
		_, _ = w.Write(b)
	}
}
