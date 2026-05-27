package embedder

import (
	"context"
	"fmt"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
)

const batchSize = 100

// Embedder wraps the OpenAI embeddings API.
type Embedder struct {
	client openai.Client
}

// New creates an Embedder using the given OpenAI API key.
func New(apiKey string) *Embedder {
	return &Embedder{
		client: openai.NewClient(option.WithAPIKey(apiKey)),
	}
}

// Embed returns the embedding vector for a single text.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	resp, err := e.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{OfString: param.NewOpt(text)},
		Model: openai.EmbeddingModelTextEmbedding3Small,
	})
	if err != nil {
		return nil, fmt.Errorf("openai embed: %w", err)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("openai embed: no data in response")
	}
	return toFloat32(resp.Data[0].Embedding), nil
}

// EmbedBatch returns embeddings for multiple texts, sending up to batchSize texts per API call.
func (e *Embedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	results := make([][]float32, len(texts))

	for start := 0; start < len(texts); start += batchSize {
		end := start + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[start:end]

		resp, err := e.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
			Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: batch},
			Model: openai.EmbeddingModelTextEmbedding3Small,
		})
		if err != nil {
			return nil, fmt.Errorf("openai batch embed [%d:%d]: %w", start, end, err)
		}

		for _, d := range resp.Data {
			results[start+int(d.Index)] = toFloat32(d.Embedding)
		}
	}
	return results, nil
}

func toFloat32(src []float64) []float32 {
	out := make([]float32, len(src))
	for i, v := range src {
		out[i] = float32(v)
	}
	return out
}
