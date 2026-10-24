// Package memory: embeddings.go ports the memory embedding adapter
// (lisa/sdk/src/sdk/memory/embeddings.py + mcp_server/memory/embeddings.py):
// one vector per text through an OpenAI-compatible /embeddings endpoint over
// net/http (no SDK dependency — Go has no openai client in this repo).
// Provider failures propagate to the store's logged, best-effort embedding
// boundary; configuration resolution accepts CLI values or the
// LISA_EMBEDDING_* environment the Python server honors.
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Embedding defaults (parity: resolve_embedding_config).
const (
	// DefaultEmbeddingDimension is LISA_EMBEDDING_DIMENSION's default.
	DefaultEmbeddingDimension = 1536
	// DefaultEmbeddingTimeoutSecs is LISA_EMBEDDING_TIMEOUT's default.
	DefaultEmbeddingTimeoutSecs = 30
)

// OpenAIEmbeddingProvider resolves one vector per text through an
// OpenAI-compatible embedding gateway. Accepts a base endpoint or its full
// /embeddings URL (parity: removesuffix("/embeddings")). max_retries=0 in
// the Python client — here one attempt per embed call, no retry.
type OpenAIEmbeddingProvider struct {
	modelID  string
	endpoint string
	apiKey   string
	timeout  time.Duration
	client   *http.Client
}

// NewOpenAIEmbeddingProvider builds the provider. The endpoint is normalized
// to the /embeddings base URL.
func NewOpenAIEmbeddingProvider(modelID, endpoint, apiKey string, timeout time.Duration) *OpenAIEmbeddingProvider {
	if timeout <= 0 {
		timeout = DefaultEmbeddingTimeoutSecs * time.Second
	}
	return &OpenAIEmbeddingProvider{
		modelID:  modelID,
		endpoint: normalizeEmbeddingsEndpoint(endpoint),
		apiKey:   orDefault(apiKey, "not-required"),
		timeout:  timeout,
		client:   &http.Client{Timeout: timeout},
	}
}

// normalizeEmbeddingsEndpoint mirrors endpoint.rstrip("/").removesuffix(
// "/embeddings") + "/": a base URL or a full /embeddings URL both work.
func normalizeEmbeddingsEndpoint(endpoint string) string {
	trimmed := endpoint
	for strings.HasSuffix(trimmed, "/") {
		trimmed = strings.TrimSuffix(trimmed, "/")
	}
	trimmed = strings.TrimSuffix(trimmed, "/embeddings")
	return trimmed + "/embeddings"
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// embedRequest is the OpenAI /embeddings request body.
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embedResponse is the OpenAI /embeddings response shape.
type embedResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Embed implements EmbeddingProvider. Errors are raised to the caller (the
// store logs them and continues); the response must return exactly one
// vector per input with the configured dimension (parity: the Python
// provider's two ValueError checks).
func (p *OpenAIEmbeddingProvider) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{Model: p.modelID, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("embed request marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed request build: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("embed response read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed endpoint %s: status %d: %s", p.endpoint, resp.StatusCode, truncateForLog(string(payload), 512))
	}
	var parsed embedResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, fmt.Errorf("embed response parse: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("embed endpoint error: %s", parsed.Error.Message)
	}
	vectors := parsed.Data
	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("embedding response returned %d vectors for %d texts", len(vectors), len(texts))
	}
	out := make([][]float64, len(vectors))
	for i, d := range vectors {
		out[i] = d.Embedding
	}
	return out, nil
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// EmbeddingConfig is the resolved embedding configuration (parity: the
// (provider, dimension) tuple resolve_embedding_config returns).
type EmbeddingConfig struct {
	Provider  EmbeddingProvider // nil when disabled (lexical-only store)
	Dimension int
}

// ResolveEmbeddingConfig resolves CLI values and environment; provider
// calls belong to the store. When either the model id or endpoint is
// absent, no provider is wired and the store stays lexical-only (the
// previous default behavior, mirroring the Python server).
func ResolveEmbeddingConfig(modelIDFlag, endpointFlag string) (EmbeddingConfig, error) {
	modelID := firstNonEmpty(modelIDFlag, os.Getenv("LISA_EMBEDDING_MODEL"))
	endpoint := firstNonEmpty(endpointFlag, os.Getenv("LISA_EMBEDDING_ENDPOINT"))
	if modelID == "" || endpoint == "" {
		return EmbeddingConfig{Dimension: DefaultEmbeddingDimension}, nil
	}
	apiKey := os.Getenv("LISA_EMBEDDING_API_KEY")
	dimension := DefaultEmbeddingDimension
	if raw := firstNonEmpty(os.Getenv("LISA_EMBEDDING_DIMENSION")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return EmbeddingConfig{}, fmt.Errorf("invalid LISA_EMBEDDING_DIMENSION %q: %w", raw, err)
		}
		dimension = n
	}
	timeout := time.Duration(DefaultEmbeddingTimeoutSecs) * time.Second
	if raw := firstNonEmpty(os.Getenv("LISA_EMBEDDING_TIMEOUT")); raw != "" {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return EmbeddingConfig{}, fmt.Errorf("invalid LISA_EMBEDDING_TIMEOUT %q: %w", raw, err)
		}
		timeout = time.Duration(f * float64(time.Second))
	}
	return EmbeddingConfig{
		Provider:  NewOpenAIEmbeddingProvider(modelID, endpoint, apiKey, timeout),
		Dimension: dimension,
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
