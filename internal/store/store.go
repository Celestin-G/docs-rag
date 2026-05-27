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
}

// Insert stores a single chunk in the database.
func (s *Store) Insert(ctx context.Context, chunk DocChunk) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO doc_chunks (source_file, section, content, embedding)
		 VALUES ($1, $2, $3, $4::vector)`,
		chunk.SourceFile,
		chunk.Section,
		chunk.Content,
		pgvector.NewVector(chunk.Embedding).String(),
	)
	if err != nil {
		return fmt.Errorf("insert chunk: %w", err)
	}
	return nil
}

// Search retrieves the top-k most similar chunks to the given embedding using cosine distance.
func (s *Store) Search(ctx context.Context, embedding []float32, topK int) ([]DocChunk, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT source_file, section, content
		 FROM doc_chunks
		 ORDER BY embedding <=> $1::vector
		 LIMIT $2`,
		pgvector.NewVector(embedding).String(),
		topK,
	)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var results []DocChunk
	for rows.Next() {
		var c DocChunk
		if err := rows.Scan(&c.SourceFile, &c.Section, &c.Content); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
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
