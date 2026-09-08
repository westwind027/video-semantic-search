package identity

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"video-semantic-search/internal/model"
)

type batchFaceClient struct {
	batchCalls  int
	singleCalls int
}

func (c *batchFaceClient) Embed(_ context.Context, _ string) (EmbedResult, error) {
	c.singleCalls++
	return EmbedResult{Model: "test-face", Dimension: 2, Faces: []Face{{DetScore: .99, Quality: .95, Embedding: []float32{1, 0}}}}, nil
}

func (c *batchFaceClient) EmbedBatch(_ context.Context, images []string) ([]EmbedResult, error) {
	c.batchCalls++
	results := make([]EmbedResult, len(images))
	for index := range results {
		results[index] = EmbedResult{Model: "test-face", Dimension: 2, Faces: []Face{{DetScore: .99, Quality: .95, Embedding: []float32{1, 0}}}}
	}
	return results, nil
}

func (c *batchFaceClient) Health(context.Context) (Health, error) {
	return Health{Status: "ok"}, nil
}

func TestTaggerBatchesSceneFaceEmbedding(t *testing.T) {
	identityStore, err := NewFileStore(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := identityStore.UpsertMovie(model.Movie{ID: "movie-batch", Title: "Batch Movie"}); err != nil {
		t.Fatal(err)
	}
	if err := identityStore.UpsertPerson(model.Person{ID: "person-batch", Name: "Batch Actor"}); err != nil {
		t.Fatal(err)
	}
	if err := identityStore.ReplaceMovieCast("movie-batch", []model.MovieCast{{PersonID: "person-batch"}}); err != nil {
		t.Fatal(err)
	}
	if err := identityStore.UpsertFaceVector(model.FaceVector{ID: "reference-batch", PersonID: "person-batch", ImageID: "reference-batch", Vector: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}

	frames := make([]model.Scene, 5)
	for index := range frames {
		path := filepath.Join(t.TempDir(), "frame-"+string(rune('0'+index))+".jpg")
		if err := os.WriteFile(path, []byte("frame"), 0o600); err != nil {
			t.Fatal(err)
		}
		frames[index] = model.Scene{SceneID: "scene-batch-" + string(rune('0'+index)), Start: float64(index), End: float64(index + 1), PreviewPath: path}
	}

	client := &batchFaceClient{}
	tagger := NewTagger(identityStore, client, Config{MatchThreshold: .8, MarginThreshold: .05, MinDetScore: .6, ReferenceMax: 10, SceneBatchSize: 3})
	media, err := tagger.TagMedia(context.Background(), model.Media{MediaID: "media-batch", MovieID: "movie-batch", Title: "Batch Movie", Scenes: frames})
	if err != nil {
		t.Fatal(err)
	}
	if client.batchCalls != 2 || client.singleCalls != 0 {
		t.Fatalf("face embedding calls = batch %d, single %d; want two batches and no single calls", client.batchCalls, client.singleCalls)
	}
	for index, scene := range media.Scenes {
		if len(scene.PersonIDs) != 1 || scene.PersonIDs[0] != "person-batch" {
			t.Fatalf("scene %d people = %+v", index, scene.PersonIDs)
		}
	}
}

func TestTaggerUsesCastAndTopReferenceAggregation(t *testing.T) {
	identityStore, err := NewFileStore(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := identityStore.UpsertMovie(model.Movie{ID: "movie-1", Title: "Test Movie"}); err != nil {
		t.Fatal(err)
	}
	for _, person := range []model.Person{{ID: "person-a", Name: "Actor A"}, {ID: "person-b", Name: "Actor B"}} {
		if err := identityStore.UpsertPerson(person); err != nil {
			t.Fatal(err)
		}
	}
	if err := identityStore.ReplaceMovieCast("movie-1", []model.MovieCast{{PersonID: "person-a"}, {PersonID: "person-b"}}); err != nil {
		t.Fatal(err)
	}
	if err := identityStore.UpsertFaceVector(model.FaceVector{ID: "a-1", PersonID: "person-a", ImageID: "a-1", Vector: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := identityStore.UpsertFaceVector(model.FaceVector{ID: "b-1", PersonID: "person-b", ImageID: "b-1", Vector: []float32{0, 1}}); err != nil {
		t.Fatal(err)
	}
	frame := filepath.Join(t.TempDir(), "frame.jpg")
	if err := os.WriteFile(frame, []byte("frame"), 0o600); err != nil {
		t.Fatal(err)
	}

	tagger := NewTagger(identityStore, fakeFaceClient{}, Config{MatchThreshold: .8, MarginThreshold: .05, MinDetScore: .6, ReferenceMax: 10})
	year := 2024
	media, err := tagger.TagMedia(context.Background(), model.Media{MediaID: "media-1", Title: "Test Movie", Year: &year, Scenes: []model.Scene{{SceneID: "scene-1", Start: 0, End: 1, PreviewPath: frame}}})
	if err != nil {
		t.Fatal(err)
	}
	if media.MovieID != "movie-1" || len(media.Scenes[0].PersonIDs) != 1 || media.Scenes[0].PersonIDs[0] != "person-a" {
		t.Fatalf("tagged media = %+v", media)
	}
	people := identityStore.GetScenePeople("media-1", "scene-1")
	if len(people) != 1 || people[0].BestScore < .99 {
		t.Fatalf("scene people = %+v", people)
	}
}

func TestTaggerLeavesMediaUntouchedWithoutMovieCast(t *testing.T) {
	identityStore, err := NewFileStore(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	media := model.Media{MediaID: "media-1", Title: "Unknown", Scenes: []model.Scene{{Start: 0, End: 1}}}
	tagger := NewTagger(identityStore, fakeFaceClient{}, ConfigFromEnv())
	got, err := tagger.TagMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	if got.MovieID != "" || len(got.Scenes[0].PersonIDs) != 0 {
		t.Fatalf("media changed without cast: %+v", got)
	}
}
