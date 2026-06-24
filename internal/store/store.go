package store

import (
	"context"
	"fmt"

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
}

// SearchFilter restricts which chunks are eligible for retrieval.
type SearchFilter struct {
	License    string // "ee" (no filter) or "community" (exclude enterprise pages)
	Deployment string // "cloud", "self-hosted", or "both" (no filter)
	VersionNum int16  // 0=no filter; 30/31/32/33 = max stack version to include
}

// Insert stores a single chunk in the database.
func (s *Store) Insert(ctx context.Context, chunk DocChunk, badge string, versionNum int16, deployment string) error {
	var vn interface{}
	if versionNum > 0 {
		vn = versionNum
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO doc_chunks (source_file, section, content, embedding, badge, version_num, deployment)
		 VALUES ($1, $2, $3, $4::vector, $5, $6, $7)`,
		chunk.SourceFile,
		chunk.Section,
		chunk.Content,
		pgvector.NewVector(chunk.Embedding).String(),
		badge,
		vn,
		deployment,
	)
	if err != nil {
		return fmt.Errorf("insert chunk: %w", err)
	}
	return nil
}

// Search retrieves the top-k most similar chunks to the given embedding using cosine similarity.
// Only chunks with similarity >= minScore are returned (pass 0 to disable filtering).
func (s *Store) Search(ctx context.Context, embedding []float32, topK int, minScore float32, f SearchFilter) ([]DocChunk, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT source_file, section, content, 1 - (embedding <=> $1::vector) AS score
		 FROM doc_chunks
		 WHERE
		   ($2::text = 'both' OR deployment = 'both' OR deployment = $2::text)
		   AND ($3::text = 'ee' OR badge IS NULL OR badge != 'enterprise')
		   AND ($4::smallint = 0 OR version_num IS NULL OR version_num <= $4::smallint)
		 ORDER BY embedding <=> $1::vector
		 LIMIT $5`,
		pgvector.NewVector(embedding).String(),
		f.Deployment,
		f.License,
		f.VersionNum,
		topK,
	)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var results []DocChunk
	for rows.Next() {
		var c DocChunk
		if err := rows.Scan(&c.SourceFile, &c.Section, &c.Content, &c.Score); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		if minScore > 0 && c.Score < minScore {
			continue
		}
		results = append(results, c)
	}
	return results, rows.Err()
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
