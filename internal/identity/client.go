package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"video-semantic-search/internal/httpclient"
)

type Face struct {
	BBox      []float32 `json:"bbox"`
	DetScore  float32   `json:"det_score"`
	Quality   float32   `json:"quality"`
	Embedding []float32 `json:"embedding"`
}

type EmbedResult struct {
	Faces     []Face `json:"faces"`
	Model     string `json:"model,omitempty"`
	Dimension int    `json:"dimension,omitempty"`
}

type Health struct {
	Status    string   `json:"status"`
	Model     string   `json:"model,omitempty"`
	Dimension int      `json:"dimension,omitempty"`
	Backend   string   `json:"backend,omitempty"`
	Providers []string `json:"providers,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// Client is the narrow boundary between Go orchestration and the Python face
// model. The Go service never imports InsightFace or ONNX Runtime.
type Client interface {
	Embed(context.Context, string) (EmbedResult, error)
	Health(context.Context) (Health, error)
}

// BatchClient is an optional extension for scene workloads. Keeping it
// separate from Client lets older adapters continue to use the one-image
// endpoint while the built-in Python client avoids one HTTP round trip per
// scene.
type BatchClient interface {
	Client
	EmbedBatch(context.Context, []string) ([]EmbedResult, error)
}

type PythonClient struct {
	endpoint   string
	httpClient *http.Client
}

func NewPythonClient(endpoint string, timeout time.Duration) *PythonClient {
	return &PythonClient{endpoint: strings.TrimRight(endpoint, "/"), httpClient: httpclient.NewDirectClient(timeout)}
}

func (c *PythonClient) Embed(ctx context.Context, image string) (EmbedResult, error) {
	payload, err := json.Marshal(struct {
		Image string `json:"image"`
	}{Image: image})
	if err != nil {
		return EmbedResult{}, fmt.Errorf("encode face request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/faces/embed", bytes.NewReader(payload))
	if err != nil {
		return EmbedResult{}, fmt.Errorf("create face request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return EmbedResult{}, fmt.Errorf("face embedding request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return EmbedResult{}, fmt.Errorf("face embedding returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	var result EmbedResult
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<20)).Decode(&result); err != nil {
		return EmbedResult{}, fmt.Errorf("decode face embedding: %w", err)
	}

	return validateEmbedResult(result)
}

// EmbedBatch submits several preview paths in one request. The Python
// service still protects model inference with its provider-safe lock, but
// batching removes per-scene HTTP and JSON framing overhead.
func (c *PythonClient) EmbedBatch(ctx context.Context, images []string) ([]EmbedResult, error) {
	if len(images) == 0 {
		return nil, fmt.Errorf("face embedding batch requires at least one image")
	}
	payload, err := json.Marshal(struct {
		Images []string `json:"images"`
	}{Images: images})
	if err != nil {
		return nil, fmt.Errorf("encode face batch request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/faces/embed-batch", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create face batch request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("face batch embedding request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		// Keep rolling upgrades safe: an older identity process may not know
		// the batch route yet, but it still supports the original endpoint.
		_ = response.Body.Close()
		results := make([]EmbedResult, len(images))
		for index, image := range images {
			result, err := c.Embed(ctx, image)
			if err != nil {
				return nil, err
			}
			results[index] = result
		}
		return results, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return nil, fmt.Errorf("face batch embedding returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	var decoded struct {
		Results []EmbedResult `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 128<<20)).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode face batch embedding: %w", err)
	}
	if len(decoded.Results) != len(images) {
		return nil, fmt.Errorf("face batch embedding count %d does not match image count %d", len(decoded.Results), len(images))
	}
	for index := range decoded.Results {
		result, err := validateEmbedResult(decoded.Results[index])
		if err != nil {
			return nil, fmt.Errorf("validate face batch result %d: %w", index, err)
		}
		decoded.Results[index] = result
	}
	return decoded.Results, nil
}

func validateEmbedResult(result EmbedResult) (EmbedResult, error) {
	for index, face := range result.Faces {
		if len(face.Embedding) == 0 {
			return EmbedResult{}, fmt.Errorf("face %d has an empty embedding", index)
		}
		if result.Dimension == 0 {
			result.Dimension = len(face.Embedding)
		}
		if len(face.Embedding) != result.Dimension {
			return EmbedResult{}, fmt.Errorf("face %d has dimension %d, expected %d", index, len(face.Embedding), result.Dimension)
		}
	}
	return result, nil
}

func (c *PythonClient) Health(ctx context.Context) (Health, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/healthz", nil)
	if err != nil {
		return Health{}, err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return Health{}, fmt.Errorf("identity health request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return Health{}, fmt.Errorf("identity health returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	var health Health
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&health); err != nil {
		return Health{}, fmt.Errorf("decode identity health: %w", err)
	}
	return health, nil
}
