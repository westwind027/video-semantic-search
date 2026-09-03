package embedding

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"video-semantic-search/internal/httpclient"
)

type Result struct {
	Vectors   [][]float32
	Model     string
	Dimension int
	Modality  string
	Profile   string
}

// ImageProfile controls the image processor used before image embedding.
// Original leaves the model's native processor settings untouched; compressed
// explicitly bounds the image token budget for a faster, lower-memory pass.
type ImageProfile string

const (
	ImageProfileOriginal   ImageProfile = "original"
	ImageProfileCompressed ImageProfile = "compressed"
)

type ImageOptions struct {
	Profile ImageProfile
}

type Health struct {
	Status     string   `json:"status"`
	Model      string   `json:"model"`
	Dimension  int      `json:"dimension"`
	Modalities []string `json:"modalities"`
}

type Client interface {
	EmbedText(context.Context, []string, string) (Result, error)
	EmbedImages(context.Context, []string) (Result, error)
	Health(context.Context) (Health, error)
}

// ProfiledImageClient is optional so existing embedding adapters and tests do
// not need to change. PythonClient implements it; older adapters fall back to
// their existing image embedding behavior.
type ProfiledImageClient interface {
	EmbedImagesWithOptions(context.Context, []string, ImageOptions) (Result, error)
}

type PythonClient struct {
	endpoint   string
	httpClient *http.Client
}

func NewPythonClient(endpoint string, timeout time.Duration) *PythonClient {
	return &PythonClient{endpoint: strings.TrimRight(endpoint, "/"), httpClient: httpclient.NewDirectClient(timeout)}
}

func (c *PythonClient) EmbedText(ctx context.Context, texts []string, role string) (Result, error) {
	return c.embed(ctx, embeddingRequest{Texts: texts, Modality: "text", Role: role})
}

func (c *PythonClient) EmbedImages(ctx context.Context, images []string) (Result, error) {
	return c.EmbedImagesWithOptions(ctx, images, ImageOptions{Profile: ImageProfileOriginal})
}

func (c *PythonClient) EmbedImagesWithOptions(ctx context.Context, images []string, options ImageOptions) (Result, error) {
	profile := options.Profile
	if profile == "" {
		profile = ImageProfileOriginal
	}
	if profile != ImageProfileOriginal && profile != ImageProfileCompressed {
		return Result{}, fmt.Errorf("unsupported image profile %q", profile)
	}
	return c.embed(ctx, embeddingRequest{Images: images, Modality: "image", ImageProfile: string(profile)})
}

func (c *PythonClient) Health(ctx context.Context) (Health, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/healthz", nil)
	if err != nil {
		return Health{}, err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return Health{}, fmt.Errorf("embedding health request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Health{}, fmt.Errorf("embedding health returned HTTP %d", response.StatusCode)
	}
	var health Health
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&health); err != nil {
		return Health{}, fmt.Errorf("decode embedding health: %w", err)
	}
	return health, nil
}

type embeddingRequest struct {
	Texts        []string `json:"texts,omitempty"`
	Images       []string `json:"images,omitempty"`
	Modality     string   `json:"modality"`
	Role         string   `json:"role,omitempty"`
	ImageProfile string   `json:"image_profile,omitempty"`
}

type embeddingResponse struct {
	Embeddings   [][]float32 `json:"embeddings"`
	Model        string      `json:"model"`
	Dimension    int         `json:"dimension"`
	Modality     string      `json:"modality"`
	ImageProfile string      `json:"image_profile"`
}

func (c *PythonClient) embed(ctx context.Context, payload embeddingRequest) (Result, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Result{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/embeddings", strings.NewReader(string(body)))
	if err != nil {
		return Result{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return Result{}, fmt.Errorf("embedding request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return Result{}, fmt.Errorf("embedding returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	var decoded embeddingResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 128<<20)).Decode(&decoded); err != nil {
		return Result{}, fmt.Errorf("decode embedding response: %w", err)
	}
	if len(decoded.Embeddings) == 0 {
		return Result{}, fmt.Errorf("embedding response contains no vectors")
	}
	dimension := decoded.Dimension
	if dimension == 0 {
		dimension = len(decoded.Embeddings[0])
	}
	for index, vector := range decoded.Embeddings {
		if len(vector) != dimension {
			return Result{}, fmt.Errorf("embedding %d has dimension %d, expected %d", index, len(vector), dimension)
		}
	}
	return Result{Vectors: decoded.Embeddings, Model: decoded.Model, Dimension: dimension, Modality: decoded.Modality, Profile: decoded.ImageProfile}, nil
}
