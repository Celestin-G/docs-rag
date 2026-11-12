package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/celestingeniaut/docs-rag/internal/embedder"
	"github.com/celestingeniaut/docs-rag/internal/llm"
	"github.com/celestingeniaut/docs-rag/internal/store"
)

// fakeReview is an in-memory reviewStore mirroring internal/store/review.go's
// semantics (insert-if-absent, one grade per item and reviewer, fewest grades
// first). The SQL itself is covered by internal/store's DB-gated tests.
type fakeReview struct {
	mu     sync.Mutex
	items  []store.ReviewItem
	grades map[[2]string]store.ReviewGrade
	tick   int
}

func (f *fakeReview) InsertReviewItems(_ context.Context, items []store.ReviewItem) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, it := range items {
		exists := false
		for _, have := range f.items {
			exists = exists || have.ID == it.ID
		}
		if !exists {
			f.items = append(f.items, it)
			n++
		}
	}
	return n, nil
}

func (f *fakeReview) NextReviewItem(_ context.Context, reviewer string) (*store.ReviewItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range f.items {
		if _, graded := f.grades[[2]string{it.ID, reviewer}]; !graded {
			it := it
			return &it, nil
		}
	}
	return nil, nil
}

func (f *fakeReview) GetReviewItem(_ context.Context, id string) (*store.ReviewItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range f.items {
		if it.ID == id {
			return &it, nil
		}
	}
	return nil, nil
}

func (f *fakeReview) ReviewProgressFor(_ context.Context, reviewer string) (store.ReviewProgress, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := store.ReviewProgress{Total: len(f.items)}
	for k := range f.grades {
		if k[1] == reviewer {
			p.Graded++
		}
	}
	return p, nil
}

func (f *fakeReview) UpsertReviewGrade(_ context.Context, itemID, reviewer, grade, note string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for _, it := range f.items {
		found = found || it.ID == itemID
	}
	if !found {
		return store.ErrReviewItemNotFound
	}
	if f.grades == nil {
		f.grades = map[[2]string]store.ReviewGrade{}
	}
	f.tick++
	f.grades[[2]string{itemID, reviewer}] = store.ReviewGrade{
		ItemID: itemID, ReviewerID: reviewer, Grade: grade, Note: note,
		CreatedAt: time.Unix(int64(f.tick), 0),
	}
	return nil
}

func (f *fakeReview) DeleteReviewGrade(_ context.Context, itemID, reviewer string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := [2]string{itemID, reviewer}
	_, ok := f.grades[k]
	delete(f.grades, k)
	return ok, nil
}

func (f *fakeReview) ReviewResults(_ context.Context, batchID string) ([]store.ReviewGrade, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	batchOf := map[string]string{}
	for _, it := range f.items {
		batchOf[it.ID] = it.BatchID
	}
	out := []store.ReviewGrade{}
	for _, g := range f.grades {
		if batchOf[g.ItemID] == batchID {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ItemID != out[j].ItemID {
			return out[i].ItemID < out[j].ItemID
		}
		return out[i].ReviewerID < out[j].ReviewerID
	})
	return out, nil
}

const (
	reviewerA = "reviewer-aaaa-1111"
	reviewerB = "reviewer-bbbb-2222"
	adminTok  = "s3cret-admin-token"
)

// newReviewServer builds the real handler (through New, so the routes and the
// REVIEW_ADMIN_TOKEN wiring are exercised) over a fake review store.
func newReviewServer(t *testing.T, token string) (*Server, *fakeReview) {
	t.Helper()
	t.Setenv("TRUSTED_PROXIES", "")
	t.Setenv("MCP_TOKEN", "")
	t.Setenv("REVIEW_ADMIN_TOKEN", token)
	h, err := New(nil, embedder.New(), llm.New())
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	fake := &fakeReview{}
	h.s.review = fake
	setReviewLimit(h, 100000, 100000)
	return h, fake
}

// setReviewLimit retunes the limiter in place: the routes captured the pointer
// when New registered them, so replacing the field would change nothing.
func setReviewLimit(h *Server, perMinute, burst int) {
	l := newRateLimiter(perMinute, burst)
	h.s.reviewLimiter.every, h.s.reviewLimiter.burst = l.every, l.burst
}

func doReview(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "100.64.0.1:4000"
	if token != "" {
		r.Header.Set(reviewTokenHeader, token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
}

func batchBody(batch string, ids ...string) string {
	items := make([]map[string]any, len(ids))
	for i, id := range ids {
		items[i] = map[string]any{"item_id": id, "ticket_key": "SUP-" + id, "question": "Question " + id, "draft": "Draft **" + id + "**", "team_reply": "Reply " + id, "decision": "send", "score": 0.5}
	}
	b, _ := json.Marshal(map[string]any{"batch_id": batch, "items": items})
	return string(b)
}

func gradeBody(item, reviewer, grade, note string) string {
	b, _ := json.Marshal(map[string]string{"item_id": item, "reviewer": reviewer, "grade": grade, "note": note})
	return string(b)
}

func TestReviewAdminAuth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    string
		header string
		want   int
	}{
		{"token unset: 503 even with a header", "", "anything", http.StatusServiceUnavailable},
		{"token unset, no header: 503", "", "", http.StatusServiceUnavailable},
		{"missing header: 401", adminTok, "", http.StatusUnauthorized},
		{"wrong token: 401", adminTok, "nope", http.StatusUnauthorized},
		{"prefix of the token: 401", adminTok, adminTok[:5], http.StatusUnauthorized},
		{"correct token: 200", adminTok, adminTok, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newReviewServer(t, tc.env)
			for _, rt := range []struct{ method, path, body string }{
				{http.MethodPost, "/api/review/batches", batchBody("b1", "i1")},
				{http.MethodGet, "/api/review/results?batch_id=b1", ""},
			} {
				if w := doReview(t, h, rt.method, rt.path, tc.header, rt.body); w.Code != tc.want {
					t.Errorf("%s %s = %d, want %d (%s)", rt.method, rt.path, w.Code, tc.want, w.Body.String())
				}
			}
		})
	}
}

func TestReviewBatchImportIsIdempotent(t *testing.T) {
	h, fake := newReviewServer(t, adminTok)
	type resp struct{ Received, Inserted, Skipped int }
	post := func(body string) resp {
		w := doReview(t, h, http.MethodPost, "/api/review/batches", adminTok, body)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var r resp
		decodeBody(t, w, &r)
		return r
	}
	if got := post(batchBody("b1", "i1", "i2")); got != (resp{2, 2, 0}) {
		t.Errorf("first import = %+v", got)
	}
	// Same ids again, one new, one repeated inside the request: only the new
	// id is inserted and existing content is not overwritten.
	again := strings.Replace(batchBody("b2", "i1", "i3", "i3"), "Draft **i1**", "CHANGED", 1)
	if got := post(again); got != (resp{3, 1, 2}) {
		t.Errorf("second import = %+v", got)
	}
	if len(fake.items) != 3 {
		t.Errorf("stored %d items, want 3", len(fake.items))
	}
	for _, it := range fake.items {
		if it.ID == "i1" && (it.Draft != "Draft **i1**" || it.BatchID != "b1") {
			t.Errorf("existing item was modified: %+v", it)
		}
	}
}

func TestReviewBatchValidation(t *testing.T) {
	item := func(mod func(m map[string]any)) string {
		m := map[string]any{"item_id": "i1", "question": "q", "draft": "d"}
		mod(m)
		b, _ := json.Marshal(map[string]any{"batch_id": "b1", "items": []any{m}})
		return string(b)
	}
	many := make([]map[string]string, maxReviewItems+1)
	for i := range many {
		many[i] = map[string]string{"item_id": strings.Repeat("x", 1) + string(rune('a'+i%26)) + strings.Repeat("0", i), "question": "q", "draft": "d"}
	}
	tooMany, _ := json.Marshal(map[string]any{"batch_id": "b1", "items": many})

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"valid minimal item", item(func(map[string]any) {}), http.StatusOK},
		{"item at the length cap", item(func(m map[string]any) { m["draft"] = strings.Repeat("d", maxReviewDraftLen) }), http.StatusOK},
		{"not JSON", "{nope", http.StatusBadRequest},
		{"missing batch_id", `{"items":[{"item_id":"i","question":"q","draft":"d"}]}`, http.StatusBadRequest},
		{"no items", `{"batch_id":"b","items":[]}`, http.StatusBadRequest},
		{"too many items", string(tooMany), http.StatusBadRequest},
		{"missing item_id", item(func(m map[string]any) { delete(m, "item_id") }), http.StatusBadRequest},
		{"blank question", item(func(m map[string]any) { m["question"] = "  \n" }), http.StatusBadRequest},
		{"missing draft", item(func(m map[string]any) { delete(m, "draft") }), http.StatusBadRequest},
		{"item_id too long", item(func(m map[string]any) { m["item_id"] = strings.Repeat("a", maxReviewIDLen+1) }), http.StatusBadRequest},
		{"draft too long", item(func(m map[string]any) { m["draft"] = strings.Repeat("d", maxReviewDraftLen+1) }), http.StatusBadRequest},
		{"NUL byte in question", item(func(m map[string]any) { m["question"] = "a\u0000b" }), http.StatusBadRequest},
		{"score has the wrong type", item(func(m map[string]any) { m["score"] = "high" }), http.StatusBadRequest},
		{"body over the cap", `{"batch_id":"b","items":[],"pad":"` + strings.Repeat("p", maxReviewBatchBytes) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fake := newReviewServer(t, adminTok)
			w := doReview(t, h, http.MethodPost, "/api/review/batches", adminTok, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d (%.200s)", w.Code, tc.want, w.Body.String())
			}
			if tc.want != http.StatusOK && len(fake.items) != 0 {
				t.Errorf("rejected request still stored %d items", len(fake.items))
			}
		})
	}
}

func TestReviewNextGradeUndoCycle(t *testing.T) {
	h, fake := newReviewServer(t, adminTok)
	doReview(t, h, http.MethodPost, "/api/review/batches", adminTok, batchBody("b1", "i1", "i2"))

	type view struct {
		Done bool `json:"done"`
		Item *struct {
			ItemID    string `json:"item_id"`
			Question  string `json:"question"`
			DraftHTML string `json:"draft_html"`
			TeamReply string `json:"team_reply"`
		} `json:"item"`
		Progress struct{ Graded, Total int } `json:"progress"`
	}
	next := func(reviewer string) view {
		t.Helper()
		w := doReview(t, h, http.MethodGet, "/api/review/next?reviewer="+reviewer, "", "")
		if w.Code != http.StatusOK {
			t.Fatalf("next: %d %s", w.Code, w.Body.String())
		}
		var v view
		decodeBody(t, w, &v)
		return v
	}
	grade := func(item, reviewer, g, note string, want int) {
		t.Helper()
		if w := doReview(t, h, http.MethodPost, "/api/review/grades", "", gradeBody(item, reviewer, g, note)); w.Code != want {
			t.Fatalf("grade %s/%s: %d, want %d (%s)", item, g, w.Code, want, w.Body.String())
		}
	}

	v := next(reviewerA)
	if v.Done || v.Item.ItemID != "i1" || v.Progress.Graded != 0 || v.Progress.Total != 2 {
		t.Fatalf("first next = %+v", v)
	}
	if !strings.Contains(v.Item.DraftHTML, "<strong>i1</strong>") || v.Item.Question != "Question i1" || v.Item.TeamReply != "Reply i1" {
		t.Errorf("item payload wrong: %+v", v.Item)
	}

	grade("i1", reviewerA, "good", "", http.StatusNoContent)
	if v = next(reviewerA); v.Item.ItemID != "i2" || v.Progress.Graded != 1 {
		t.Errorf("after one grade, next = %+v", v)
	}
	// Reviewer B is unaffected by A's grades.
	if v = next(reviewerB); v.Item.ItemID != "i1" || v.Progress.Graded != 0 {
		t.Errorf("reviewer B next = %+v", v)
	}

	// Regrading replaces the grade and the note, it does not add a second one.
	grade("i1", reviewerA, "bad", "wrong version", http.StatusNoContent)
	if g := fake.grades[[2]string{"i1", reviewerA}]; g.Grade != "bad" || g.Note != "wrong version" || len(fake.grades) != 1 {
		t.Errorf("regrade did not replace: %+v (%d grades)", g, len(fake.grades))
	}

	grade("i2", reviewerA, "meh", "", http.StatusNoContent)
	if v = next(reviewerA); !v.Done || v.Item != nil || v.Progress.Graded != 2 || v.Progress.Total != 2 {
		t.Errorf("when finished, next = %+v", v)
	}

	// Undo: only the caller's own grade goes, and the item comes back.
	grade("i1", reviewerB, "good", "", http.StatusNoContent)
	w := doReview(t, h, http.MethodDelete, "/api/review/grades", "", `{"item_id":"i2","reviewer":"`+reviewerA+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("undo: %d %s", w.Code, w.Body.String())
	}
	v = view{}
	decodeBody(t, w, &v)
	if v.Done || v.Item.ItemID != "i2" || v.Progress.Graded != 1 {
		t.Errorf("undo response = %+v", v)
	}
	if _, ok := fake.grades[[2]string{"i1", reviewerB}]; !ok {
		t.Error("undo by A removed B's grade")
	}
	if w := doReview(t, h, http.MethodDelete, "/api/review/grades", "", `{"item_id":"i2","reviewer":"`+reviewerA+`"}`); w.Code != http.StatusNotFound {
		t.Errorf("second undo = %d, want 404", w.Code)
	}
	if w := doReview(t, h, http.MethodDelete, "/api/review/grades", "", `{"item_id":"i2","reviewer":"bad id"}`); w.Code != http.StatusBadRequest {
		t.Errorf("undo with bad reviewer = %d, want 400", w.Code)
	}
}

func TestReviewGradeValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"valid", gradeBody("i1", reviewerA, "meh", "ok"), http.StatusNoContent},
		{"note at the cap", gradeBody("i1", reviewerA, "meh", strings.Repeat("n", maxReviewNoteLen)), http.StatusNoContent},
		{"grade outside the enum", gradeBody("i1", reviewerA, "great", ""), http.StatusBadRequest},
		{"empty grade", gradeBody("i1", reviewerA, "", ""), http.StatusBadRequest},
		{"grade is case-sensitive", gradeBody("i1", reviewerA, "GOOD", ""), http.StatusBadRequest},
		{"reviewer too short", gradeBody("i1", "short", "good", ""), http.StatusBadRequest},
		{"reviewer too long", gradeBody("i1", strings.Repeat("a", 65), "good", ""), http.StatusBadRequest},
		{"reviewer with bad characters", gradeBody("i1", "reviewer<script>", "good", ""), http.StatusBadRequest},
		{"note over the cap", gradeBody("i1", reviewerA, "good", strings.Repeat("n", maxReviewNoteLen+1)), http.StatusBadRequest},
		{"NUL in note", gradeBody("i1", reviewerA, "good", "a\u0000b"), http.StatusBadRequest},
		{"missing item_id", gradeBody("", reviewerA, "good", ""), http.StatusBadRequest},
		{"unknown item", gradeBody("nope", reviewerA, "good", ""), http.StatusNotFound},
		{"not JSON", "grade=good", http.StatusBadRequest},
		{"body over the cap", `{"note":"` + strings.Repeat("n", maxReviewGradeBytes) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newReviewServer(t, adminTok)
			doReview(t, h, http.MethodPost, "/api/review/batches", adminTok, batchBody("b1", "i1"))
			if w := doReview(t, h, http.MethodPost, "/api/review/grades", "", tc.body); w.Code != tc.want {
				t.Errorf("status %d, want %d (%.200s)", w.Code, tc.want, w.Body.String())
			}
		})
	}

	t.Run("next rejects a bad reviewer id", func(t *testing.T) {
		h, _ := newReviewServer(t, adminTok)
		for _, q := range []string{"", "?reviewer=", "?reviewer=short", "?reviewer=has%20space%20in%20it"} {
			if w := doReview(t, h, http.MethodGet, "/api/review/next"+q, "", ""); w.Code != http.StatusBadRequest {
				t.Errorf("next%s = %d, want 400", q, w.Code)
			}
		}
	})
}

func TestReviewResults(t *testing.T) {
	h, _ := newReviewServer(t, adminTok)
	doReview(t, h, http.MethodPost, "/api/review/batches", adminTok, batchBody("b1", "i1", "i2"))
	doReview(t, h, http.MethodPost, "/api/review/batches", adminTok, batchBody("b2", "i9"))
	for _, g := range [][4]string{{"i1", reviewerA, "good", "nice"}, {"i1", reviewerB, "bad", ""}, {"i9", reviewerA, "meh", ""}} {
		doReview(t, h, http.MethodPost, "/api/review/grades", "", gradeBody(g[0], g[1], g[2], g[3]))
	}

	type out struct {
		BatchID string `json:"batch_id"`
		Grades  []struct {
			ItemID     string `json:"item_id"`
			ReviewerID string `json:"reviewer_id"`
			Grade      string `json:"grade"`
			Note       string `json:"note"`
			CreatedAt  string `json:"created_at"`
		} `json:"grades"`
	}
	get := func(q string) (int, out) {
		w := doReview(t, h, http.MethodGet, "/api/review/results"+q, adminTok, "")
		var o out
		if w.Code == http.StatusOK {
			decodeBody(t, w, &o)
		}
		return w.Code, o
	}

	code, o := get("?batch_id=b1")
	if code != http.StatusOK || o.BatchID != "b1" || len(o.Grades) != 2 {
		t.Fatalf("b1 results = %d %+v", code, o)
	}
	if g := o.Grades[0]; g.ItemID != "i1" || g.ReviewerID != reviewerA || g.Grade != "good" || g.Note != "nice" || g.CreatedAt == "" {
		t.Errorf("first grade = %+v", g)
	}
	if g := o.Grades[1]; g.ReviewerID != reviewerB || g.Grade != "bad" {
		t.Errorf("second grade = %+v", g)
	}
	if _, o = get("?batch_id=nobody"); o.Grades == nil || len(o.Grades) != 0 {
		t.Errorf("unknown batch should give an empty list, got %+v", o.Grades)
	}
	if code, _ = get(""); code != http.StatusBadRequest {
		t.Errorf("missing batch_id = %d, want 400", code)
	}
}

func TestReviewPageAndAssets(t *testing.T) {
	h, _ := newReviewServer(t, adminTok)
	page := doReview(t, h, http.MethodGet, "/review", "", "")
	if page.Code != http.StatusOK || !strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("/review = %d %q", page.Code, page.Header().Get("Content-Type"))
	}
	csp := page.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "img-src 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("CSP must not allow inline or eval: %q", csp)
	}
	// The page is static: no inline script or style, nothing from the database.
	html := page.Body.String()
	for _, bad := range []string{"<style", " style=", " onclick=", "javascript:"} {
		if strings.Contains(html, bad) {
			t.Errorf("index.html contains %q", bad)
		}
	}
	if strings.Contains(strings.ReplaceAll(html, `<script src="/review/app.js" defer></script>`, ""), "<script") {
		t.Error("index.html has an inline script")
	}
	for _, want := range []string{"Merci, tout est noté", "Réponse de l’équipe (pour comparer)", "Ajouter une note, si vous voulez", "annuler"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}

	js := doReview(t, h, http.MethodGet, "/review/app.js", "", "")
	css := doReview(t, h, http.MethodGet, "/review/app.css", "", "")
	if js.Code != http.StatusOK || !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") ||
		css.Code != http.StatusOK || !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") {
		t.Errorf("assets: js %d %q, css %d %q", js.Code, js.Header().Get("Content-Type"), css.Code, css.Header().Get("Content-Type"))
	}
	// innerHTML is only ever fed the server-sanitised draft.
	if n := strings.Count(js.Body.String(), ".innerHTML"); n != 1 || !strings.Contains(js.Body.String(), ".innerHTML = view.item.draft_html") {
		t.Errorf("app.js must use innerHTML only for draft_html (found %d uses)", n)
	}
}

// TestReviewItemTextIsNeverRenderedAsHTML: question and team_reply travel as
// plain text (the client uses textContent); only the draft is rendered, and
// sanitised.
func TestReviewItemTextIsNeverRenderedAsHTML(t *testing.T) {
	h, _ := newReviewServer(t, adminTok)
	evil := `<img src=x onerror=alert(1)><script>alert(1)</script>`
	body, _ := json.Marshal(map[string]any{"batch_id": "b", "items": []map[string]string{{
		"item_id": "i1", "question": evil, "team_reply": evil, "draft": evil + "\n\n[x](javascript:alert(1))",
	}}})
	if w := doReview(t, h, http.MethodPost, "/api/review/batches", adminTok, string(body)); w.Code != http.StatusOK {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	w := doReview(t, h, http.MethodGet, "/api/review/next?reviewer="+reviewerA, "", "")
	var v struct {
		Item struct {
			Question  string `json:"question"`
			TeamReply string `json:"team_reply"`
			DraftHTML string `json:"draft_html"`
		} `json:"item"`
	}
	decodeBody(t, w, &v)
	// Raw JSON would carry < as \u003c; decoding gives back the exact text.
	if v.Item.Question != evil || v.Item.TeamReply != evil {
		t.Errorf("question/team_reply were altered: %q / %q", v.Item.Question, v.Item.TeamReply)
	}
	for _, bad := range []string{"<script", "<img", "onerror=", "javascript:"} {
		if strings.Contains(strings.ToLower(v.Item.DraftHTML), bad) {
			t.Errorf("draft_html contains %q: %s", bad, v.Item.DraftHTML)
		}
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestReviewWriteRoutesAreRateLimited(t *testing.T) {
	h, _ := newReviewServer(t, adminTok)
	setReviewLimit(h, 1, 1)
	doReview(t, h, http.MethodPost, "/api/review/batches", adminTok, batchBody("b1", "i1")) // burns the burst
	for _, rt := range []struct{ method, path string }{
		{http.MethodPost, "/api/review/batches"},
		{http.MethodPost, "/api/review/grades"},
		{http.MethodDelete, "/api/review/grades"},
		{http.MethodGet, "/api/review/results?batch_id=b1"},
	} {
		if w := doReview(t, h, rt.method, rt.path, adminTok, "{}"); w.Code != http.StatusTooManyRequests {
			t.Errorf("%s %s = %d, want 429", rt.method, rt.path, w.Code)
		}
	}
}

func TestRenderReviewMarkdown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		src     string
		want    []string
		notWant []string
	}{
		{"basic markdown", "Hello **world**\n\n- a\n- b", []string{"<strong>world</strong>", "<li>a</li>"}, nil},
		{"code block", "```go\nfmt.Println(\"<hi>\")\n```", []string{"&lt;hi&gt;"}, []string{"<hi>"}},
		{"raw script is dropped", "before <script>alert(1)</script> after", []string{"before", "after"}, []string{"<script", "alert(1)</"}},
		{"raw html block is dropped", "<div onclick=\"x()\">hi</div>\n\ntext", []string{"text"}, []string{"<div", "onclick"}},
		{"img onerror is dropped", `<img src=x onerror=alert(1)>`, nil, []string{"<img", "onerror=alert"}},
		{"markdown image becomes a safe link", "![logo](https://example.com/a.png)", []string{`<a href="https://example.com/a.png"`, "logo"}, []string{"<img"}},
		{"javascript link neutralised", "[x](javascript:alert(1))", []string{`href="#"`}, []string{"javascript:"}},
		{"mixed-case javascript link", "[x](JaVaScRiPt:alert(1))", []string{`href="#"`}, []string{"alert"}},
		{"tab inside scheme", "[x](java&#9;script:alert(1))", nil, []string{"javascript:", "java\tscript"}},
		{"data link neutralised", "[x](data:text/html;base64,PHNjcmlwdD4=)", []string{`href="#"`}, []string{"data:"}},
		{"data image link neutralised", "[x](data:image/png;base64,AAAA)", []string{`href="#"`}, []string{"data:"}},
		{"data image becomes inert", "![x](data:image/png;base64,AAAA)", []string{`href="#"`}, []string{"data:", "<img"}},
		{"vbscript autolink is inert text", "<vbscript:msgbox(1)>", nil, []string{"<a ", "href"}},
		{"http link opens in a new tab", "[docs](https://docs.formance.com/x?a=1&b=2)", []string{`href="https://docs.formance.com/x?a=1&amp;b=2"`, `target="_blank"`, `rel="noopener noreferrer"`}, nil},
		{"autolink opens in a new tab", "<https://example.com>", []string{`target="_blank"`, `rel="noopener noreferrer"`}, nil},
		{"mailto allowed", "[me](mailto:a@b.co)", []string{`href="mailto:a@b.co"`, `target="_blank"`}, nil},
		{"link title cannot break out of the attribute", `[x](https://e.com "a\" onmouseover=\"alert(1)")`, nil, []string{`" onmouseover=`}},
		{"table", "| a | b |\n|---|---|\n| 1 | 2 |", []string{"<table>", "<td>1</td>"}, nil},
		{"entity-encoded colon in scheme", "[x](javascript&colon;alert(1))", nil, []string{"javascript:", "javascript&"}},
		{"numeric entity scheme", "[x](&#106;avascript:alert(1))", nil, []string{"avascript:alert", "href=\"j"}},
		{"leading whitespace entity", "[x](&#32;javascript:alert(1))", nil, []string{`href="javascript`, `href=" javascript`}},
		{"reference link javascript", "[x][r]\n\n[r]: javascript:alert(1)", []string{`href="#"`}, []string{"javascript:"}},
		{"reference link https", "[x][r]\n\n[r]: https://example.com", []string{`href="https://example.com"`}, nil},
		{"angle destination javascript", "[x](<javascript:alert(1)>)", nil, []string{"javascript:"}},
		{"image reference data", "![x][r]\n\n[r]: data:image/svg+xml;base64,AAAA", nil, []string{"data:", "<img"}},
		{"html entities in text stay escaped", "&lt;script&gt;alert(1)&lt;/script&gt;", nil, []string{"<script"}},
		{"raw anchor html dropped", `<a href="javascript:alert(1)">x</a>`, nil, []string{"<a href=\"javascript"}},
		{"raw svg block dropped", "<svg onload=alert(1)>\n\ntext", []string{"text"}, []string{"<svg", "onload"}},
		{"html comment trick", "<!-- --><script>alert(1)</script>", nil, []string{"<script"}},
		{"inline code with html", "`<img src=x onerror=alert(1)>`", []string{"&lt;img"}, []string{"<img"}},
		{"autolink javascript text", "<javascript:alert(1)>", nil, []string{"href=\"javascript"}},
		{"bare url is not linkified", "see https://example.com now", []string{"https://example.com"}, []string{"<a "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := renderReviewMarkdown(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("output lacks %q:\n%s", w, got)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("output contains %q:\n%s", nw, got)
				}
			}
			// Every anchor, whatever the input, opens in a new tab safely.
			if n := strings.Count(got, "<a "); n != strings.Count(got, `target="_blank"`) || n != strings.Count(got, `rel="noopener noreferrer"`) {
				t.Errorf("%d links but not all have target/rel:\n%s", n, got)
			}
		})
	}
}

func TestAllowedURL(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://example.com", true},
		{"HTTP://example.com", true},
		{"mailto:a@b.co", true},
		{"/docs/page", true},
		{"#anchor", true},
		{"page?x=a:b", true},
		{"", true},
		{"javascript:alert(1)", false},
		{" javascript:alert(1)", false},
		{"java\tscript:alert(1)", false},
		{"data:text/html,x", false},
		{"vbscript:x", false},
		{"file:///etc/passwd", false},
		{"ftp://example.com", false},
		{"JAVASCRIPT:alert(1)", false},
		{"\x00javascript:alert(1)", false},
		{" javascript:alert(1)", false},
		{"//evil.example/x", true},
		{"jav\nascript:x", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			if got := allowedURL(tc.url); got != tc.want {
				t.Errorf("allowedURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}
