package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"video-semantic-search/internal/model"
)

func TestFileStorePersistsIdentityData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPerson(model.Person{ID: "person:leo", Name: "Leonardo DiCaprio"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMovie(model.Movie{ID: "movie:inception", Title: "Inception"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceMovieCast("movie:inception", []model.MovieCast{{MovieID: "wrong", PersonID: "person:leo"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertFaceVector(model.FaceVector{ID: "image-1", PersonID: "person:leo", ImageID: "image-1", Vector: []float32{1, 0}, Model: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceScenePeople("media-1", "scene-1", []model.ScenePerson{{SceneID: "scene-1", PersonID: "person:leo", BestScore: .9}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cast := reloaded.GetMovieCast("movie:inception")
	if len(cast) != 1 || cast[0].MovieID != "movie:inception" {
		t.Fatalf("cast = %+v", cast)
	}
	if len(reloaded.ListFaceVectors("person:leo")) != 1 {
		t.Fatal("face vector was not persisted")
	}
	people := reloaded.GetScenePeople("media-1", "scene-1")
	if len(people) != 1 || people[0].PersonID != "person:leo" {
		t.Fatalf("scene people = %+v", people)
	}
}

func TestFileStoreSearchPersonsRanksLocalMatches(t *testing.T) {
	store, err := NewFileStore(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, person := range []model.Person{
		{ID: "person-alice", Name: "Alice Smith", IMDbID: "nm0000001"},
		{ID: "person-alicia", Name: "Alicia Keys"},
		{ID: "person-bob", Name: "Bob Smith"},
	} {
		if err := store.UpsertPerson(person); err != nil {
			t.Fatal(err)
		}
	}
	people := store.SearchPersons("ali", 10)
	if len(people) != 2 || people[0].Name != "Alice Smith" || people[1].Name != "Alicia Keys" {
		t.Fatalf("people = %+v", people)
	}
	people = store.SearchPersons("", 1)
	if len(people) != 1 || people[0].Name != "Alice Smith" {
		t.Fatalf("bounded people = %+v", people)
	}
}

type fakeFaceClient struct{}

func (fakeFaceClient) Embed(_ context.Context, _ string) (EmbedResult, error) {
	return EmbedResult{Model: "test-face", Dimension: 2, Faces: []Face{{DetScore: 0.99, Quality: 0.95, Embedding: []float32{1, 0}}}}, nil
}

func (fakeFaceClient) Health(context.Context) (Health, error) { return Health{Status: "ok"}, nil }

func TestReferenceIngestorIsResumable(t *testing.T) {
	store, err := NewFileStore(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(t.TempDir(), "actor.jpg")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	ingestor := NewReferenceIngestor(store, fakeFaceClient{}, ReferenceConfig{DownloadRoot: filepath.Join(t.TempDir(), "faces"), Workers: 2, MinDetScore: .6})
	input := model.PersonImage{ID: "image-1", PersonID: "person-1", Source: "local", LocalPath: imagePath, Status: "pending"}
	first := ingestor.Ingest(context.Background(), []model.PersonImage{input})
	if len(first) != 1 || first[0].Error != nil || !first[0].HasVector || first[0].Skipped {
		t.Fatalf("first ingest = %+v", first)
	}
	second := ingestor.Ingest(context.Background(), []model.PersonImage{input})
	if len(second) != 1 || second[0].Error != nil || !second[0].HasVector || !second[0].Skipped {
		t.Fatalf("resumed ingest = %+v", second)
	}
	if vectors := store.ListFaceVectors("person-1"); len(vectors) != 1 || vectors[0].ImageID != "image-1" {
		t.Fatalf("vectors = %+v", vectors)
	}
}

func TestReferenceIngestorRemovesRemoteImageAfterEmbedding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte("image"))
	}))
	defer server.Close()

	store, err := NewFileStore(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	facesRoot := filepath.Join(t.TempDir(), "faces")
	ingestor := NewReferenceIngestor(store, fakeFaceClient{}, ReferenceConfig{DownloadRoot: facesRoot, Workers: 1, MinDetScore: .6, KeepLocalFiles: false})
	result := ingestor.Ingest(context.Background(), []model.PersonImage{{ID: "remote-1", PersonID: "person-1", Source: "tmdb", SourceURL: server.URL + "/actor.jpg", Status: "pending"}})
	if len(result) != 1 || result[0].Error != nil || result[0].Image.LocalPath != "" {
		t.Fatalf("remote ingest = %+v", result)
	}
	entries, err := os.ReadDir(filepath.Join(facesRoot, "person-1"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("remote image was retained: %v", entries)
	}
}

func TestClearRemoteLocalPathsKeepsLocalImages(t *testing.T) {
	store, err := NewFileStore(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPersonImage(model.PersonImage{
		ID:        "remote",
		PersonID:  "person-1",
		Source:    "tmdb",
		SourceURL: "https://image.tmdb.org/profile.jpg",
		LocalPath: "/tmp/removed.jpg",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPersonImage(model.PersonImage{
		ID:        "local",
		PersonID:  "person-1",
		Source:    "local",
		LocalPath: "/mnt/media/profile.jpg",
	}); err != nil {
		t.Fatal(err)
	}

	cleared, err := store.ClearRemoteLocalPaths()
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 1 {
		t.Fatalf("cleared = %d, want 1", cleared)
	}
	images := store.ListPersonImages("person-1")
	for _, image := range images {
		switch image.ID {
		case "remote":
			if image.LocalPath != "" {
				t.Fatalf("remote local_path = %q, want empty", image.LocalPath)
			}
		case "local":
			if image.LocalPath != "/mnt/media/profile.jpg" {
				t.Fatalf("local local_path = %q, want original path", image.LocalPath)
			}
		}
	}
}
