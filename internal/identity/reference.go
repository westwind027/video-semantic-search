package identity

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"video-semantic-search/internal/httpclient"
	"video-semantic-search/internal/model"
)

// ReferenceConfig controls the durable reference-image ingestion pipeline.
// Workers overlap image downloads; the Python service may still serialize GPU
// inference when its provider requires it.
type ReferenceConfig struct {
	DownloadRoot     string
	ProxyURL         string
	Workers          int
	MinDetScore      float32
	MaxDownloadBytes int64
	KeepLocalFiles   bool
}

func DefaultReferenceConfig() ReferenceConfig {
	return ReferenceConfig{
		DownloadRoot:     filepath.Join("data", "identity-faces"),
		Workers:          4,
		MinDetScore:      0.60,
		MaxDownloadBytes: 20 << 20,
	}
}

// ReferenceResult is one idempotent reference-image outcome. Error is kept
// separate from Image.Status so callers can persist partial batch results and
// show a useful failure reason without aborting all other images.
type ReferenceResult struct {
	Image     model.PersonImage
	HasVector bool
	Skipped   bool
	Error     error
}

type ReferenceIngestor struct {
	store      Store
	client     Client
	httpClient *http.Client
	config     ReferenceConfig
}

func NewReferenceIngestor(identityStore Store, client Client, config ReferenceConfig) *ReferenceIngestor {
	defaults := DefaultReferenceConfig()
	if strings.TrimSpace(config.DownloadRoot) == "" {
		config.DownloadRoot = defaults.DownloadRoot
	}
	if config.Workers <= 0 {
		config.Workers = defaults.Workers
	}
	if config.MinDetScore <= 0 {
		config.MinDetScore = defaults.MinDetScore
	}
	if config.MaxDownloadBytes <= 0 {
		config.MaxDownloadBytes = defaults.MaxDownloadBytes
	}
	imageClient := httpclient.NewDirectClient(2 * time.Minute)
	if strings.TrimSpace(config.ProxyURL) != "" {
		if proxyClient, err := httpclient.NewProxyClient(2*time.Minute, config.ProxyURL); err == nil {
			imageClient = proxyClient
		}
	}
	return &ReferenceIngestor{store: identityStore, client: client, httpClient: imageClient, config: config}
}

// Ingest processes all supplied images and preserves input order in the
// returned results. A ready image with a stored vector is skipped, making the
// collector safe to stop and resume after a network or model failure.
func (i *ReferenceIngestor) Ingest(ctx context.Context, images []model.PersonImage) []ReferenceResult {
	results := make([]ReferenceResult, len(images))
	if i == nil || i.store == nil || i.client == nil || len(images) == 0 {
		return results
	}
	if ctx == nil {
		ctx = context.Background()
	}
	workers := i.config.Workers
	if workers > len(images) {
		workers = len(images)
	}
	jobs := make(chan int)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				results[index] = i.ingestOne(ctx, images[index])
			}
		}()
	}
	for index := range images {
		select {
		case jobs <- index:
		case <-ctx.Done():
			results[index] = ReferenceResult{Image: images[index], Error: ctx.Err()}
		}
	}
	close(jobs)
	wait.Wait()
	return results
}

func (i *ReferenceIngestor) ingestOne(ctx context.Context, image model.PersonImage) ReferenceResult {
	if strings.TrimSpace(image.ID) == "" {
		id, err := model.NewMediaID()
		if err != nil {
			return ReferenceResult{Image: image, Error: err}
		}
		image.ID = id
	}
	if strings.TrimSpace(image.PersonID) == "" {
		return ReferenceResult{Image: image, Error: fmt.Errorf("person id is required")}
	}
	for _, vector := range i.store.ListFaceVectors(image.PersonID) {
		if vector.ImageID == image.ID || vector.ID == image.ID {
			image.Status = "ready"
			return ReferenceResult{Image: image, HasVector: true, Skipped: true}
		}
	}

	remoteImage := strings.TrimSpace(image.LocalPath) == "" && strings.TrimSpace(image.SourceURL) != ""
	path, err := i.resolveImagePath(ctx, image)
	if err != nil {
		return i.persistFailure(image, err)
	}
	if remoteImage && !i.config.KeepLocalFiles {
		defer func() { _ = os.Remove(path) }()
	}
	image.LocalPath = path
	faceResult, err := i.client.Embed(ctx, path)
	if err != nil {
		if remoteImage && !i.config.KeepLocalFiles {
			image.LocalPath = ""
		}
		return i.persistFailure(image, err)
	}
	image.FaceCount = len(faceResult.Faces)
	if len(faceResult.Faces) != 1 || faceResult.Faces[0].DetScore < i.config.MinDetScore {
		_ = i.store.DeleteFaceVector(image.ID)
		image.Status = "rejected"
		if remoteImage && !i.config.KeepLocalFiles {
			image.LocalPath = ""
		}
		err := fmt.Errorf("reference must contain exactly one face with detection score >= %.2f", i.config.MinDetScore)
		_ = i.store.UpsertPersonImage(image)
		return ReferenceResult{Image: image, Error: err}
	}
	face := faceResult.Faces[0]
	image.QualityScore = face.Quality
	image.Status = "ready"
	if remoteImage && !i.config.KeepLocalFiles {
		image.LocalPath = ""
	}
	if err := i.store.UpsertPersonImage(image); err != nil {
		return ReferenceResult{Image: image, Error: err}
	}
	if err := i.store.UpsertFaceVector(model.FaceVector{ID: image.ID, PersonID: image.PersonID, ImageID: image.ID, Quality: face.Quality, Model: faceResult.Model, Vector: face.Embedding}); err != nil {
		return ReferenceResult{Image: image, Error: err}
	}
	return ReferenceResult{Image: image, HasVector: true}
}

func (i *ReferenceIngestor) persistFailure(image model.PersonImage, err error) ReferenceResult {
	image.Status = "failed"
	_ = i.store.UpsertPersonImage(image)
	return ReferenceResult{Image: image, Error: err}
}

func (i *ReferenceIngestor) resolveImagePath(ctx context.Context, image model.PersonImage) (string, error) {
	if strings.TrimSpace(image.LocalPath) != "" {
		path, err := filepath.Abs(filepath.Clean(image.LocalPath))
		if err != nil {
			return "", err
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return "", fmt.Errorf("reference image is not a non-empty file")
		}
		return path, nil
	}
	if strings.TrimSpace(image.SourceURL) == "" {
		return "", fmt.Errorf("local_path or source_url is required")
	}
	parsed, err := url.Parse(image.SourceURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("source_url must be an http or https URL")
	}
	directory := filepath.Join(i.config.DownloadRoot, image.PersonID)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", err
	}
	extension := strings.ToLower(filepath.Ext(parsed.Path))
	if extension != ".jpg" && extension != ".jpeg" && extension != ".png" && extension != ".webp" {
		extension = ".jpg"
	}
	path := filepath.Join(directory, safeFilename(image.ID)+extension)
	if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return filepath.Abs(path)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, image.SourceURL, nil)
	if err != nil {
		return "", err
	}
	response, err := i.httpClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("reference image returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > i.config.MaxDownloadBytes {
		return "", fmt.Errorf("reference image exceeds %d bytes", i.config.MaxDownloadBytes)
	}
	temporary, err := os.CreateTemp(directory, ".reference-*")
	if err != nil {
		return "", err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	bytes, copyErr := io.Copy(temporary, io.LimitReader(response.Body, i.config.MaxDownloadBytes+1))
	if copyErr != nil {
		_ = temporary.Close()
		return "", copyErr
	}
	if bytes > i.config.MaxDownloadBytes {
		_ = temporary.Close()
		return "", fmt.Errorf("reference image exceeds %d bytes", i.config.MaxDownloadBytes)
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

func safeFilename(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "reference"
	}
	cleaned := strings.Map(func(character rune) rune {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '-' || character == '_' || character == '.' {
			return character
		}
		return '_'
	}, value)
	cleaned = strings.Trim(cleaned, "._")
	if cleaned == "" {
		return "reference"
	}
	return cleaned
}
