package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
)

// Store handles all PostgreSQL interactions for doc chunks.
type Store struct {
	pool *pgxpool.Pool
}

// New creates a Store connected to the given DSN.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// DocChunk is a document chunk with its embedding.
type DocChunk struct {
	SourceFile string
	Section    string
	Content    string
	Embedding  []float32
	Score      float32 // cosine similarity [0,1] — only set by Search
	// Badge is the page's frontmatter badge ("enterprise", "experimental", "")
	// — only set by Search, so callers can warn about what they are quoting.
	Badge string
}

// SearchFilter restricts which chunks are eligible for retrieval.
type SearchFilter struct {
	License    string // "ee" (no filter) or "community" (exclude enterprise pages)
	Deployment string // "cloud", "self-hosted", or "both" (no filter)
	// VersionNum is the stack version being asked about: 0=no filter, else
	// 30/31/32/33. A chunk is eligible when this version falls inside its
	// [version_num, version_below) range.
	VersionNum int16
}

// Insert stores a single chunk in the database. versionNum is the lower version
// bound and versionBelow the exclusive upper bound; pass 0 for either to mean
// unbounded on that side.
func (s *Store) Insert(ctx context.Context, chunk DocChunk, badge string, versionNum, versionBelow int16, versionComponent, deployment string) error {
	// Zero means "no bound" and must reach SQL as NULL, not as 0: the filters
	// treat NULL as unbounded but would compare a literal 0.
	nullIfZero := func(v int16) interface{} {
		if v > 0 {
			return v
		}
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO doc_chunks (source_file, section, content, embedding, content_tsv, badge, version_num, version_below, version_component, deployment)
		 VALUES ($1, $2, $3, $4::vector,
		         to_tsvector('english', coalesce($2, '') || ' ' || $3),
		         $5, $6, $7, $8, $9)`,
		chunk.SourceFile,
		chunk.Section,
		chunk.Content,
		pgvector.NewVector(chunk.Embedding).String(),
		badge,
		nullIfZero(versionNum),
		nullIfZero(versionBelow),
		versionComponent,
		deployment,
	)
	if err != nil {
		return fmt.Errorf("insert chunk: %w", err)
	}
	return nil
}

// Search retrieves the top-k most similar chunks using vector similarity.
// When textQuery is non-empty, combines vector score (70%) with BM25 (30%) for hybrid retrieval.
// Only chunks with score >= minScore are returned (pass 0 to disable filtering).
func (s *Store) Search(ctx context.Context, embedding []float32, textQuery string, topK int, minScore float32, f SearchFilter) ([]DocChunk, error) {
	vec := pgvector.NewVector(embedding).String()

	if textQuery == "" {
		return s.searchVector(ctx, vec, topK, minScore, f)
	}
	return s.searchHybrid(ctx, vec, textQuery, topK, minScore, f)
}

func (s *Store) searchVector(ctx context.Context, vec string, topK int, minScore float32, f SearchFilter) ([]DocChunk, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT source_file, section, content, badge, 1 - (embedding <=> $1::vector) AS score
		 FROM doc_chunks
		 WHERE
		   ($2::text = 'both' OR deployment = 'both' OR deployment = $2::text)
		   AND ($3::text = 'ee' OR badge IS NULL OR badge != 'enterprise')
		   AND ($4::smallint = 0 OR version_component <> 'stack' OR (
		         (version_num IS NULL OR version_num <= $4::smallint)
		         AND (version_below IS NULL OR $4::smallint < version_below)))
		 ORDER BY embedding <=> $1::vector
		 LIMIT $5`,
		vec, f.Deployment, f.License, f.VersionNum, topK,
	)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()
	return scanChunks(rows, minScore)
}

func (s *Store) searchHybrid(ctx context.Context, vec string, textQuery string, topK int, minScore float32, f SearchFilter) ([]DocChunk, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT source_file, section, content, badge,
		        (1 - (embedding <=> $1::vector)) * 0.7 +
		        ts_rank(content_tsv, plainto_tsquery('english', $2)) * 0.3 AS score
		 FROM doc_chunks
		 WHERE
		   ($3::text = 'both' OR deployment = 'both' OR deployment = $3::text)
		   AND ($4::text = 'ee' OR badge IS NULL OR badge != 'enterprise')
		   AND ($5::smallint = 0 OR version_component <> 'stack' OR (
		         (version_num IS NULL OR version_num <= $5::smallint)
		         AND (version_below IS NULL OR $5::smallint < version_below)))
		 ORDER BY score DESC
		 LIMIT $6`,
		vec, textQuery, f.Deployment, f.License, f.VersionNum, topK,
	)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()
	return scanChunks(rows, minScore)
}

func scanChunks(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}, minScore float32) ([]DocChunk, error) {
	var results []DocChunk
	for rows.Next() {
		var c DocChunk
		// badge is nullable in the table; scan through a pointer so a NULL page
		// (most of them) leaves the field empty instead of failing the row.
		var badge *string
		if err := rows.Scan(&c.SourceFile, &c.Section, &c.Content, &badge, &c.Score); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		if badge != nil {
			c.Badge = *badge
		}
		if minScore > 0 && c.Score < minScore {
			continue
		}
		results = append(results, c)
	}
	return results, rows.Err()
}

// StackVersions returns the stack versions the index actually carries, as the
// compact form used in filters (30, 31, 32…), ascending.
//
// The picker is built from this rather than from a hardcoded list: the docs
// framework adds and removes stack lines over time — v3.3 was announced, then
// cancelled and folded into v3.2 — and a list written into the HTML drifts
// silently, offering versions that match nothing.
func (s *Store) StackVersions(ctx context.Context) ([]int16, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT version_num FROM doc_chunks
		 WHERE version_num IS NOT NULL AND version_component = 'stack'
		 ORDER BY version_num`)
	if err != nil {
		return nil, fmt.Errorf("stack versions: %w", err)
	}
	defer rows.Close()

	var out []int16
	for rows.Next() {
		var v int16
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan version_num: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// EnterpriseFeatures returns human-readable names of all enterprise-badged
// pages currently in the index, derived from their file names. This keeps the
// EE-feature list in the LLM system prompt in sync with the actual docs.
func (s *Store) EnterpriseFeatures(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT source_file FROM doc_chunks WHERE badge = 'enterprise' ORDER BY source_file`)
	if err != nil {
		return nil, fmt.Errorf("enterprise features: %w", err)
	}
	defer rows.Close()

	seen := map[string]bool{}
	var names []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, fmt.Errorf("scan source_file: %w", err)
		}
		name := featureName(f)
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, rows.Err()
}

// featureName maps an enterprise-badged doc path to a feature name at module
// granularity, so sub-pages ("wallets/adding-funds.mdx") collapse into their
// module ("Wallets module") instead of leaking meaningless leaf names like
// "Send" or "Delay" into the prompt.
func featureName(path string) string {
	p := strings.TrimPrefix(path, "apps/docs/content/pages/")
	segs := strings.Split(p, "/")
	switch {
	case len(segs) >= 4 && segs[0] == "modules" && segs[1] == "connectivity" && segs[2] == "connectors":
		return prettify(stripExt(segs[len(segs)-1])) + " connector"
	case len(segs) >= 2 && segs[0] == "modules":
		return prettify(stripExt(segs[1])) + " module"
	case len(segs) >= 4 && segs[0] == "deploy" && segs[1] == "self-hosted" && segs[2] == "modules":
		return prettify(stripExt(segs[3])) + " module"
	default:
		return prettify(stripExt(segs[len(segs)-1]))
	}
}

func stripExt(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[:i]
	}
	return s
}

// acronyms are doc-page names that must render fully uppercased.
var acronyms = map[string]bool{"sso": true, "rbac": true, "api": true, "sdk": true, "mcp": true, "oidc": true}

// prettify title-cases hyphenated names ("audit-logs" → "Audit Logs"),
// uppercasing known acronyms ("sso" → "SSO").
func prettify(base string) string {
	if base == "" {
		return ""
	}
	words := strings.FieldsFunc(base, func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range words {
		if w == "" {
			continue
		}
		if acronyms[strings.ToLower(w)] {
			words[i] = strings.ToUpper(w)
		} else {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// InsertFeedback stores a user thumbs-up/down on an answer.
func (s *Store) InsertFeedback(ctx context.Context, question, answer string, rating int) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO feedback (question, answer, rating) VALUES ($1, $2, $3)`,
		question, answer, rating)
	return err
}

// InsertRequestLog records timing and outcome for a single /api/ask call.
func (s *Store) InsertRequestLog(ctx context.Context, question string, durationMs, chunksFound int, status string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO request_logs (question, duration_ms, chunks_found, status) VALUES ($1, $2, $3, $4)`,
		question, durationMs, chunksFound, status)
	return err
}

// Stats holds aggregated analytics data.
type Stats struct {
	TotalRequests int     `json:"total_requests"`
	RequestsToday int     `json:"requests_today"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
	ErrorRate     float64 `json:"error_rate"`
	TotalFeedback int     `json:"total_feedback"`
	PositiveRate  float64 `json:"positive_rate"`

	// IndexedChunks is how many chunks the index currently holds. Zero means the
	// service answers nothing at all, and it is the only signal here that does
	// not need traffic to show a problem.
	IndexedChunks int `json:"indexed_chunks"`
	// NoResultsRate counts answers where retrieval came back empty. It is not an
	// error — the service worked, it just had nothing to say — so it never
	// reached ErrorRate, which is how an empty index went unnoticed for three
	// days behind a 0% error rate. Today's figure is the one to watch: a broken
	// re-ingest sends it straight to 100% while the all-time rate barely moves.
	NoResultsRate      float64 `json:"no_results_rate"`
	NoResultsRateToday float64 `json:"no_results_rate_today"`
}

// GetStats returns aggregated request and feedback statistics.
func (s *Store) GetStats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.pool.QueryRow(ctx, `
		SELECT
		  COUNT(*)                                                          AS total_requests,
		  COUNT(*) FILTER (WHERE created_at >= CURRENT_DATE)               AS requests_today,
		  COALESCE(AVG(duration_ms), 0)                                    AS avg_duration_ms,
		  COALESCE(COUNT(*) FILTER (WHERE status = 'error')::float /
		           NULLIF(COUNT(*), 0), 0)                                 AS error_rate,
		  COALESCE(COUNT(*) FILTER (WHERE status = 'no_results')::float /
		           NULLIF(COUNT(*), 0), 0)                                 AS no_results_rate,
		  COALESCE(COUNT(*) FILTER (WHERE status = 'no_results'
		                              AND created_at >= CURRENT_DATE)::float /
		           NULLIF(COUNT(*) FILTER (WHERE created_at >= CURRENT_DATE), 0), 0)
		                                                                   AS no_results_rate_today
		FROM request_logs
	`).Scan(&st.TotalRequests, &st.RequestsToday, &st.AvgDurationMs, &st.ErrorRate,
		&st.NoResultsRate, &st.NoResultsRateToday)
	if err != nil {
		return st, fmt.Errorf("stats requests: %w", err)
	}

	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM doc_chunks`).Scan(&st.IndexedChunks); err != nil {
		return st, fmt.Errorf("stats indexed chunks: %w", err)
	}
	err = s.pool.QueryRow(ctx, `
		SELECT
		  COUNT(*)                                                        AS total_feedback,
		  COALESCE(COUNT(*) FILTER (WHERE rating = 1)::float /
		           NULLIF(COUNT(*), 0), 0)                               AS positive_rate
		FROM feedback
	`).Scan(&st.TotalFeedback, &st.PositiveRate)
	if err != nil {
		return st, fmt.Errorf("stats feedback: %w", err)
	}
	return st, nil
}

// Truncate removes all chunks from the table.
func (s *Store) Truncate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, "TRUNCATE doc_chunks")
	return err
}

// Exec runs an arbitrary SQL statement (used for migrations).
func (s *Store) Exec(ctx context.Context, sql string) error {
	_, err := s.pool.Exec(ctx, sql)
	return err
}
