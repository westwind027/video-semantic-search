package search

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"video-semantic-search/internal/embedding"
	"video-semantic-search/internal/model"
	"video-semantic-search/internal/store"
)

type IndexResult struct {
	SceneCount int
	Model      string
	Dimension  int
	Modality   string
	Profile    string
}

type Engine struct {
	store             store.IndexStore
	embedder          embedding.Client
	useImageEmbedding bool
	ImageBatchSize    int
	ImageProfile      embedding.ImageProfile
}

func NewEngine(indexStore store.IndexStore, embedder embedding.Client, useImageEmbedding bool) *Engine {
	return &Engine{store: indexStore, embedder: embedder, useImageEmbedding: useImageEmbedding, ImageBatchSize: 32, ImageProfile: embedding.ImageProfileOriginal}
}

// GetMedia exposes the indexed media record to asynchronous management jobs.
// The store still owns the actual persistence and returns a defensive copy.
func (e *Engine) GetMedia(mediaID string) (model.Media, bool) {
	return e.store.GetMedia(mediaID)
}

// FindMediaByFingerprint returns an indexed media item with the same content
// fingerprint. The path fallback is only for legacy local records created
// before content fingerprints were stored.
func (e *Engine) FindMediaByFingerprint(fingerprint, localPath string) (model.Media, bool) {
	fingerprint = strings.TrimSpace(fingerprint)
	localPath = strings.TrimSpace(localPath)
	mediaList := e.store.ListMedia()
	for _, media := range mediaList {
		for _, key := range []string{"content_sha256", "content_fingerprint", "remote_fingerprint"} {
			if value, ok := media.Metadata[key].(string); ok && strings.EqualFold(strings.TrimSpace(value), fingerprint) && fingerprint != "" {
				return media, true
			}
		}
	}
	for _, media := range mediaList {
		if _, hasSHA256 := media.Metadata["content_sha256"].(string); !hasSHA256 && localPath != "" {
			if _, hasFingerprint := media.Metadata["content_fingerprint"].(string); hasFingerprint {
				continue
			}
			if _, hasRemoteFingerprint := media.Metadata["remote_fingerprint"].(string); hasRemoteFingerprint {
				continue
			}
			if storedPath, ok := media.Metadata["local_path"].(string); ok && strings.TrimSpace(storedPath) == localPath {
				return media, true
			}
		}
	}
	return model.Media{}, false
}

func (e *Engine) Index(ctx context.Context, media model.Media) (IndexResult, error) {
	return e.indexWithProgress(ctx, media, nil)
}

// IndexWithProgress indexes a media item and reports image embedding batches as
// they finish. Keeping the batches in Go avoids one oversized HTTP request and
// gives acquisition tasks an observable final stage.
func (e *Engine) IndexWithProgress(ctx context.Context, media model.Media, report func(done, total int)) (IndexResult, error) {
	return e.indexWithImageOptions(ctx, media, e.useImageEmbedding, embedding.ImageOptions{Profile: e.ImageProfile}, report)
}

func (e *Engine) indexWithProgress(ctx context.Context, media model.Media, report func(done, total int)) (IndexResult, error) {
	return e.indexWithImageOptions(ctx, media, e.useImageEmbedding, embedding.ImageOptions{Profile: e.ImageProfile}, report)
}

// RebuildEmbeddings regenerates vectors for an already indexed media item
// without parsing the source video or extracting frames again. This is the
// intended path for comparing WeMM's native image processing with the bounded
// compressed profile.
func (e *Engine) RebuildEmbeddings(ctx context.Context, mediaID string, profile embedding.ImageProfile, report func(done, total int)) (IndexResult, error) {
	if profile == "" {
		profile = embedding.ImageProfileOriginal
	}
	if profile != embedding.ImageProfileOriginal && profile != embedding.ImageProfileCompressed {
		return IndexResult{}, fmt.Errorf("unsupported image profile %q", profile)
	}
	media, ok := e.store.GetMedia(mediaID)
	if !ok {
		return IndexResult{}, fmt.Errorf("media %q not found", mediaID)
	}
	return e.indexWithImageOptions(ctx, media, true, embedding.ImageOptions{Profile: profile}, report)
}

func (e *Engine) indexWithImageOptions(ctx context.Context, media model.Media, useImages bool, options embedding.ImageOptions, report func(done, total int)) (IndexResult, error) {
	if err := media.Validate(); err != nil {
		return IndexResult{}, err
	}
	if options.Profile == "" {
		options.Profile = embedding.ImageProfileOriginal
	}
	texts := make([]string, len(media.Scenes))
	if !useImages {
		for index, scene := range media.Scenes {
			texts[index] = sceneText(media, scene)
		}
	}
	var vectors [][]float32
	var result embedding.Result
	var err error
	if useImages && allScenesHavePreview(media) {
		images := make([]string, len(media.Scenes))
		for index, scene := range media.Scenes {
			images[index] = previewReference(scene)
		}
		result, err = e.embedImagesInBatchesWithOptions(ctx, images, options, report)
		if err != nil {
			return IndexResult{}, err
		}
		vectors = result.Vectors
	} else {
		if useImages {
			for index, scene := range media.Scenes {
				texts[index] = sceneText(media, scene)
			}
		}
		textResult, err := e.embedder.EmbedText(ctx, texts, "document")
		if err != nil {
			return IndexResult{}, err
		}
		vectors = textResult.Vectors
		result = textResult
	}
	if useImages && !allScenesHavePreview(media) {
		imagePositions := make([]int, 0, len(media.Scenes))
		images := make([]string, 0, len(media.Scenes))
		for index, scene := range media.Scenes {
			if previewReference(scene) != "" {
				imagePositions = append(imagePositions, index)
				images = append(images, previewReference(scene))
			}
		}
		if len(images) > 0 {
			imageResult, imageErr := e.embedImagesInBatchesWithOptions(ctx, images, options, report)
			if imageErr != nil {
				return IndexResult{}, imageErr
			}
			if len(imageResult.Vectors) != len(imagePositions) {
				return IndexResult{}, fmt.Errorf("image embedding count %d does not match preview count %d", len(imageResult.Vectors), len(imagePositions))
			}
			for index, position := range imagePositions {
				vectors[position] = imageResult.Vectors[index]
			}
			if imageResult.Dimension != result.Dimension {
				return IndexResult{}, fmt.Errorf("text and image embedding dimensions differ: %d vs %d", result.Dimension, imageResult.Dimension)
			}
			result.Modality = "mixed"
			result.Profile = string(options.Profile)
		}
	}
	if len(vectors) != len(media.Scenes) {
		return IndexResult{}, fmt.Errorf("embedding count %d does not match scene count %d", len(vectors), len(media.Scenes))
	}
	media = withEmbeddingMetadata(media, result)
	if err := e.store.UpsertMedia(media, vectors, result.Model); err != nil {
		return IndexResult{}, err
	}
	return IndexResult{SceneCount: len(media.Scenes), Model: result.Model, Dimension: result.Dimension, Modality: result.Modality, Profile: result.Profile}, nil
}

func (e *Engine) embedImagesInBatches(ctx context.Context, images []string, report func(done, total int)) (embedding.Result, error) {
	return e.embedImagesInBatchesWithOptions(ctx, images, embedding.ImageOptions{Profile: embedding.ImageProfileOriginal}, report)
}

func (e *Engine) embedImagesInBatchesWithOptions(ctx context.Context, images []string, options embedding.ImageOptions, report func(done, total int)) (embedding.Result, error) {
	if len(images) == 0 {
		return embedding.Result{}, fmt.Errorf("image embedding requires at least one image")
	}
	batchSize := e.ImageBatchSize
	if batchSize <= 0 {
		batchSize = 128
	}
	vectors := make([][]float32, 0, len(images))
	var modelName string
	dimension := 0
	for start := 0; start < len(images); start += batchSize {
		end := start + batchSize
		if end > len(images) {
			end = len(images)
		}
		result, err := e.embedImageBatch(ctx, images[start:end], options)
		if err != nil {
			return embedding.Result{}, err
		}
		if len(result.Vectors) != end-start {
			return embedding.Result{}, fmt.Errorf("image embedding count %d does not match batch size %d", len(result.Vectors), end-start)
		}
		if len(vectors) == 0 {
			modelName = result.Model
			dimension = result.Dimension
		} else if result.Model != modelName || result.Dimension != dimension {
			return embedding.Result{}, fmt.Errorf("image embedding batches differ: %s/%d vs %s/%d", modelName, dimension, result.Model, result.Dimension)
		}
		vectors = append(vectors, result.Vectors...)
		if report != nil {
			report(end, len(images))
		}
	}
	return embedding.Result{Vectors: vectors, Model: modelName, Dimension: dimension, Modality: "image", Profile: string(options.Profile)}, nil
}

func (e *Engine) embedImageBatch(ctx context.Context, images []string, options embedding.ImageOptions) (embedding.Result, error) {
	if profiled, ok := e.embedder.(embedding.ProfiledImageClient); ok {
		return profiled.EmbedImagesWithOptions(ctx, images, options)
	}
	return e.embedder.EmbedImages(ctx, images)
}

func withEmbeddingMetadata(media model.Media, result embedding.Result) model.Media {
	copyMedia := media
	copyMedia.Metadata = make(map[string]any, len(media.Metadata)+2)
	for key, value := range media.Metadata {
		copyMedia.Metadata[key] = value
	}
	modality := result.Modality
	if modality == "" {
		modality = "text"
	}
	copyMedia.Metadata["embedding_modality"] = modality
	if result.Profile != "" {
		copyMedia.Metadata["embedding_profile"] = result.Profile
	} else {
		delete(copyMedia.Metadata, "embedding_profile")
	}
	return copyMedia
}

func allScenesHavePreview(media model.Media) bool {
	if len(media.Scenes) == 0 {
		return false
	}
	for _, scene := range media.Scenes {
		if previewReference(scene) == "" {
			return false
		}
	}
	return true
}

func previewReference(scene model.Scene) string {
	if scene.PreviewPath != "" {
		return scene.PreviewPath
	}
	return scene.Preview
}

func (e *Engine) Search(ctx context.Context, request model.SearchRequest) (model.SearchResponse, error) {
	if request.Limit <= 0 || request.Limit > 100 {
		request.Limit = 16
	}
	minScore := float32(0.3)
	if request.MinScore != nil {
		minScore = *request.MinScore
	}
	if minScore < 0 || minScore > 1 {
		return model.SearchResponse{}, fmt.Errorf("min_score must be between 0 and 1")
	}
	mode := strings.ToLower(strings.TrimSpace(request.Mode))
	if mode == "" {
		mode = "media"
	}
	if mode != "media" && mode != "scene" {
		return model.SearchResponse{}, fmt.Errorf("mode must be media or scene")
	}
	documents := e.store.SearchDocuments(store.Filter{Type: request.Type, Year: request.Year, Language: request.Language})
	response := model.SearchResponse{Query: strings.TrimSpace(request.Query), Results: []model.SearchResult{}}
	if len(documents) == 0 {
		return response, nil
	}
	queryResult, err := e.embedder.EmbedText(ctx, []string{request.Query}, "query")
	if err != nil {
		return model.SearchResponse{}, err
	}
	if len(queryResult.Vectors) != 1 {
		return model.SearchResponse{}, fmt.Errorf("query embedding returned %d vectors", len(queryResult.Vectors))
	}
	queryVector := queryResult.Vectors[0]

	type scoredScene struct {
		document store.SceneDocument
		score    float32
	}
	ranked := make([]scoredScene, 0, len(documents))
	for _, document := range documents {
		if document.EmbeddingModel != "" && queryResult.Model != "" && document.EmbeddingModel != queryResult.Model {
			continue
		}
		if queryResult.Dimension > 0 && len(document.Embedding) != queryResult.Dimension {
			continue
		}
		// WeMM returns L2-normalized vectors and defines cosine similarity as
		// the retrieval score. Keep that score intact: do not apply quality,
		// RRF, max-score normalization, or metadata lexical scores here.
		score := cosine(queryVector, document.Embedding)
		if score <= minScore {
			continue
		}
		ranked = append(ranked, scoredScene{document: document, score: score})
	}
	sort.SliceStable(ranked, func(left, right int) bool {
		if ranked[left].score == ranked[right].score {
			return ranked[left].document.Scene.SceneID < ranked[right].document.Scene.SceneID
		}
		return ranked[left].score > ranked[right].score
	})

	if mode == "scene" {
		if len(ranked) > request.Limit {
			ranked = ranked[:request.Limit]
		}
		for _, hit := range ranked {
			response.Results = append(response.Results, model.SearchResult{
				MediaID:   hit.document.MediaID,
				Title:     hit.document.Title,
				Type:      hit.document.Type,
				Year:      hit.document.Year,
				SourceURL: hit.document.SourceURL,
				Score:     roundScore(hit.score),
				Scene:     sceneResult(hit.document.Scene, hit.score),
			})
		}
		return response, nil
	}

	type mediaHits struct{ hits []scoredScene }
	grouped := map[string]*mediaHits{}
	for _, item := range ranked {
		hits := grouped[item.document.MediaID]
		if hits == nil {
			hits = &mediaHits{}
			grouped[item.document.MediaID] = hits
		}
		hits.hits = append(hits.hits, item)
	}
	mediaGroups := make([]*mediaHits, 0, len(grouped))
	for _, hits := range grouped {
		mediaGroups = append(mediaGroups, hits)
	}
	sort.SliceStable(mediaGroups, func(left, right int) bool {
		if mediaGroups[left].hits[0].score == mediaGroups[right].hits[0].score {
			return mediaGroups[left].hits[0].document.MediaID < mediaGroups[right].hits[0].document.MediaID
		}
		return mediaGroups[left].hits[0].score > mediaGroups[right].hits[0].score
	})
	if len(mediaGroups) > request.Limit {
		mediaGroups = mediaGroups[:request.Limit]
	}
	for _, group := range mediaGroups {
		best := group.hits[0]
		result := model.SearchResult{MediaID: best.document.MediaID, Title: best.document.Title, Type: best.document.Type, Year: best.document.Year, SourceURL: best.document.SourceURL, Score: roundScore(best.score), Scene: sceneResult(best.document.Scene, best.score)}
		for index, hit := range group.hits {
			if index == 5 {
				break
			}
			result.MatchedScenes = append(result.MatchedScenes, sceneResult(hit.document.Scene, hit.score))
		}
		response.Results = append(response.Results, result)
	}
	return response, nil
}

func sceneText(media model.Media, scene model.Scene) string {
	return strings.Join(nonEmpty(media.Title, media.OriginalTitle, media.Description, strings.Join(media.Tags, " "), scene.Caption, scene.Subtitle), " ")
}

func nonEmpty(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, value)
		}
	}
	return result
}

func cosine(left, right []float32) float32 {
	if len(left) == 0 || len(left) != len(right) {
		return 0
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		dot += float64(left[index] * right[index])
		leftNorm += float64(left[index] * left[index])
		rightNorm += float64(right[index] * right[index])
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return float32(math.Max(-1, math.Min(1, dot/math.Sqrt(leftNorm*rightNorm))))
}

func sceneResult(scene model.Scene, score float32) model.SceneResult {
	return model.SceneResult{SceneID: scene.SceneID, Start: scene.Start, End: scene.End, Score: roundScore(score), Preview: scene.Preview, Caption: scene.Caption, Subtitle: scene.Subtitle}
}

func roundScore(value float32) float32 {
	return float32(math.Round(float64(value)*1_000_000) / 1_000_000)
}
