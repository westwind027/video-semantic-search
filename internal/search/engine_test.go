package search

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"video-semantic-search/internal/embedding"
	"video-semantic-search/internal/model"
	"video-semantic-search/internal/store"
)

type fakeEmbedder struct{}

type fixedQueryEmbedder struct{}

type batchingEmbedder struct {
	calls []int
}

func (e *batchingEmbedder) EmbedText(context.Context, []string, string) (embedding.Result, error) {
	return embedding.Result{Vectors: [][]float32{{1, 0}}, Model: "batching", Dimension: 2, Modality: "text"}, nil
}

func (e *batchingEmbedder) EmbedImages(_ context.Context, values []string) (embedding.Result, error) {
	e.calls = append(e.calls, len(values))
	vectors := make([][]float32, len(values))
	for index := range vectors {
		vectors[index] = []float32{1, 0}
	}
	return embedding.Result{Vectors: vectors, Model: "batching", Dimension: 2, Modality: "image"}, nil
}

func (e *batchingEmbedder) Health(context.Context) (embedding.Health, error) {
	return embedding.Health{Status: "ok", Model: "batching", Dimension: 2}, nil
}

func (fixedQueryEmbedder) EmbedText(context.Context, []string, string) (embedding.Result, error) {
	return embedding.Result{Vectors: [][]float32{{1, 1}}, Model: "fixed", Dimension: 2, Modality: "text"}, nil
}

func (fixedQueryEmbedder) EmbedImages(context.Context, []string) (embedding.Result, error) {
	return embedding.Result{Vectors: [][]float32{{1, 1}}, Model: "fixed", Dimension: 2, Modality: "image"}, nil
}

func (fixedQueryEmbedder) Health(context.Context) (embedding.Health, error) {
	return embedding.Health{Status: "ok", Model: "fixed", Dimension: 2}, nil
}

func (fakeEmbedder) EmbedText(_ context.Context, values []string, _ string) (embedding.Result, error) {
	result := make([][]float32, len(values))
	for index, value := range values {
		if strings.Contains(value, "雨中") {
			result[index] = []float32{1, 0}
		} else if strings.Contains(value, "夕阳") {
			result[index] = []float32{0, 1}
		} else {
			result[index] = []float32{0, 0}
		}
	}
	return embedding.Result{Vectors: result, Model: "fake", Dimension: 2, Modality: "text"}, nil
}

func (fakeEmbedder) EmbedImages(context.Context, []string) (embedding.Result, error) {
	return embedding.Result{Vectors: [][]float32{{1, 0}}, Model: "fake", Dimension: 2, Modality: "image"}, nil
}

func (fakeEmbedder) Health(context.Context) (embedding.Health, error) {
	return embedding.Health{Status: "ok", Model: "fake", Dimension: 2}, nil
}

func TestEngineSearchReturnsBestSceneAndAggregatesMedia(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	engine := NewEngine(index, fakeEmbedder{}, false)
	if _, err := engine.Index(context.Background(), model.Media{MediaID: "rain", Title: "雨夜追车", Scenes: []model.Scene{{Start: 1, End: 5, Caption: "雨中有人奔跑"}, {Start: 8, End: 12, Caption: "红色跑车停下"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Index(context.Background(), model.Media{MediaID: "sun", Title: "夕阳公路", Scenes: []model.Scene{{Start: 0, End: 5, Caption: "夕阳下骑车"}}}); err != nil {
		t.Fatal(err)
	}
	response, err := engine.Search(context.Background(), model.SearchRequest{Query: "雨中男人", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].MediaID != "rain" {
		t.Fatalf("results = %+v", response.Results)
	}
	if response.Results[0].Scene.Start != 1 || response.Results[0].Score <= 0 || response.Results[0].Score > 1 {
		t.Fatalf("best result = %+v", response.Results[0])
	}
	if len(response.Results[0].MatchedScenes) != 1 || response.Results[0].MatchedScenes[0].Score != 1 {
		t.Fatalf("scene scores = %+v", response.Results[0].MatchedScenes)
	}
	noMatch, err := engine.Search(context.Background(), model.SearchRequest{Query: "火车站和沙漠", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(noMatch.Results) != 0 {
		t.Fatalf("top results for unrelated query = %+v", noMatch.Results)
	}
}

func TestEngineSearchAppliesFilters(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	engine := NewEngine(index, fakeEmbedder{}, false)
	year := 2024
	if _, err := engine.Index(context.Background(), model.Media{MediaID: "movie", Type: "movie", Year: &year, Language: []string{"zh"}, Title: "雨夜", Scenes: []model.Scene{{Start: 0, End: 1, Caption: "雨中"}}}); err != nil {
		t.Fatal(err)
	}
	otherYear := 2023
	if _, err := engine.Index(context.Background(), model.Media{MediaID: "series", Type: "series", Year: &otherYear, Language: []string{"en"}, Title: "Rain", Scenes: []model.Scene{{Start: 0, End: 1, Caption: "rain"}}}); err != nil {
		t.Fatal(err)
	}
	response, err := engine.Search(context.Background(), model.SearchRequest{Query: "雨中", Type: "movie", Year: &year, Language: "zh"})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].MediaID != "movie" {
		t.Fatalf("filtered results = %+v", response.Results)
	}
}

func TestEngineSearchUsesCosineSimilarityInsteadOfDotProduct(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	media := model.Media{MediaID: "cosine", Title: "Cosine", Scenes: []model.Scene{
		{SceneID: "large-axis", Start: 0, End: 1},
		{SceneID: "same-direction", Start: 1, End: 2},
	}}
	if err := index.UpsertMedia(media, [][]float32{{10, 0}, {1, 1}}, "fixed"); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(index, fixedQueryEmbedder{}, false)
	response, err := engine.Search(context.Background(), model.SearchRequest{Query: "query", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].Scene.SceneID != "same-direction" {
		t.Fatalf("cosine ranking = %+v", response.Results)
	}
	if response.Results[0].Score != 1 {
		t.Fatalf("cosine score = %v", response.Results[0].Score)
	}
}

func TestEngineSearchDefaultsToSixteenMedia(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	for mediaIndex := 0; mediaIndex < 12; mediaIndex++ {
		media := model.Media{MediaID: fmt.Sprintf("media-%02d", mediaIndex), Title: "Media", Scenes: []model.Scene{{Start: 0, End: 1}}}
		if err := index.UpsertMedia(media, [][]float32{{1, 1}}, "fixed"); err != nil {
			t.Fatal(err)
		}
	}

	engine := NewEngine(index, fixedQueryEmbedder{}, false)
	response, err := engine.Search(context.Background(), model.SearchRequest{Query: "query"})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 12 {
		t.Fatalf("default result count = %d, want 12", len(response.Results))
	}
}

func TestEngineSearchFlattensScenesAndAppliesScore(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	media := model.Media{MediaID: "flat", Title: "Flat", Scenes: []model.Scene{
		{SceneID: "hit", Start: 0, End: 1},
		{SceneID: "miss", Start: 1, End: 2},
	}}
	if err := index.UpsertMedia(media, [][]float32{{1, 0}, {0, -1}}, "fixed"); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(index, fixedQueryEmbedder{}, false)
	threshold := float32(0.6)
	response, err := engine.Search(context.Background(), model.SearchRequest{Query: "query", Mode: "scene", Limit: 16, MinScore: &threshold})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].Scene.SceneID != "hit" || len(response.Results[0].MatchedScenes) != 0 {
		t.Fatalf("flat results = %+v", response.Results)
	}
}

func TestEngineIndexesImagesInBatchesAndReportsProgress(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	embedder := &batchingEmbedder{}
	engine := NewEngine(index, embedder, true)
	engine.ImageBatchSize = 2
	media := model.Media{MediaID: "batched", Title: "Batched", Scenes: make([]model.Scene, 5)}
	for index := range media.Scenes {
		media.Scenes[index].Start = float64(index)
		media.Scenes[index].End = float64(index + 1)
		media.Scenes[index].PreviewPath = fmt.Sprintf("/tmp/frame-%d.jpg", index)
	}
	var progress []int
	if _, err := engine.IndexWithProgress(context.Background(), media, func(done, total int) {
		if total != 5 {
			t.Fatalf("progress total = %d, want 5", total)
		}
		progress = append(progress, done)
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(embedder.calls) != "[2 2 1]" {
		t.Fatalf("embedding batch sizes = %v", embedder.calls)
	}
	if fmt.Sprint(progress) != "[2 4 5]" {
		t.Fatalf("embedding progress = %v", progress)
	}
}
