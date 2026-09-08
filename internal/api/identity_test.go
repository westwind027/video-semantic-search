package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"video-semantic-search/internal/identity"
	"video-semantic-search/internal/model"
	"video-semantic-search/internal/search"
	"video-semantic-search/internal/store"
)

type apiFakeFaceClient struct{}

func (apiFakeFaceClient) Embed(context.Context, string) (identity.EmbedResult, error) {
	return identity.EmbedResult{Model: "test-face", Dimension: 2, Faces: []identity.Face{{DetScore: .99, Quality: .9, Embedding: []float32{1, 0}}}}, nil
}

func (apiFakeFaceClient) Health(context.Context) (identity.Health, error) {
	return identity.Health{Status: "ok", Model: "test-face", Dimension: 2}, nil
}

func TestIdentityMetadataAndPersonSearch(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	identityStore, err := identity.NewFileStore(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := identityStore.UpsertPerson(model.Person{ID: "person-a", IMDbID: "nm0000001", Name: "Actor A"}); err != nil {
		t.Fatal(err)
	}
	if err := identityStore.UpsertPerson(model.Person{ID: "person-b", IMDbID: "nm0000002", Name: "Actor B"}); err != nil {
		t.Fatal(err)
	}
	if err := identityStore.UpsertFaceVector(model.FaceVector{ID: "face-b", PersonID: "person-b", ImageID: "face-b", Vector: []float32{0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := identityStore.UpsertMovie(model.Movie{ID: "movie-a", IMDbID: "tt0000001", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := identityStore.UpsertMovie(model.Movie{ID: "movie-b", IMDbID: "tt0000002", Title: "B"}); err != nil {
		t.Fatal(err)
	}
	if err := identityStore.ReplaceMovieCast("movie-a", []model.MovieCast{{MovieID: "movie-a", PersonID: "person-a", IMDbNameID: "nm0000001", Source: "imdb_principals"}}); err != nil {
		t.Fatal(err)
	}
	if err := identityStore.ReplaceMovieCast("movie-b", []model.MovieCast{{MovieID: "movie-b", PersonID: "person-b", IMDbNameID: "nm0000002", Source: "imdb_principals"}}); err != nil {
		t.Fatal(err)
	}
	if err := identityStore.UpsertFaceVector(model.FaceVector{ID: "face-a", PersonID: "person-a", ImageID: "face-a", Vector: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	media := model.Media{MediaID: "media-a", MovieID: "movie-a", Title: "A", Scenes: []model.Scene{{SceneID: "scene-a", Start: 0, End: 1, PersonIDs: []string{"person-a"}}}}
	if err := index.UpsertMedia(media, [][]float32{{1, 0}}, "test"); err != nil {
		t.Fatal(err)
	}
	server := NewServer(search.NewEngine(index, testEmbedder{}, false), index, testEmbedder{})
	server.ConfigureIdentity(identityStore, nil)
	handler := server.Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/metadata/persons?q=actor&limit=10", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("person lookup status=%d body=%s", response.Code, response.Body.String())
	}
	var people []model.Person
	if err := json.Unmarshal(response.Body.Bytes(), &people); err != nil {
		t.Fatal(err)
	}
	if len(people) != 1 || people[0].ID != "person-a" {
		t.Fatalf("person lookup should exclude unprocessed movie people = %+v", people)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"query":"anything","person":"Actor A","mode":"scene"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("person search status=%d body=%s", response.Code, response.Body.String())
	}
	var result model.SearchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Scene.PersonIDs[0] != "person-a" {
		t.Fatalf("person search result=%+v", result.Results)
	}
	if err := index.UpdateScenePeople("media-a", "scene-a", []string{"person-b"}); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"query":"anything","person_ids":["person-a","person-b"],"mode":"scene"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("multi-person search status=%d body=%s", response.Code, response.Body.String())
	}
	result = model.SearchResponse{}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Scene.PersonIDs[0] != "person-b" {
		t.Fatalf("multi-person search result=%+v", result.Results)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/metadata/movies/movie-a", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("movie detail status=%d body=%s", response.Code, response.Body.String())
	}
	var movieDetail struct {
		Cast   []model.MovieCast `json:"cast"`
		People []model.Person    `json:"people"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &movieDetail); err != nil {
		t.Fatal(err)
	}
	if len(movieDetail.Cast) != 1 || len(movieDetail.People) != 1 || movieDetail.People[0].ID != "person-a" {
		t.Fatalf("movie detail=%+v", movieDetail)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/metadata/movies", strings.NewReader(`{"title":"A Movie","tmdb_id":12}`)))
	if response.Code != http.StatusCreated {
		t.Fatalf("movie create status=%d body=%s", response.Code, response.Body.String())
	}
	var movie model.Movie
	if err := json.Unmarshal(response.Body.Bytes(), &movie); err != nil {
		t.Fatal(err)
	}
	if movie.ID != "movie:tmdb:12" {
		t.Fatalf("movie=%+v", movie)
	}
}

func TestMovieResolveEndpointUsesLegacyReleaseFilename(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	identityStore, err := identity.NewFileStore(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	year := 2006
	want := model.Movie{ID: "movie-pursuit", IMDbID: "tt0454921", Title: "The Pursuit of Happyness", Year: &year}
	if err := identityStore.UpsertMovie(want); err != nil {
		t.Fatal(err)
	}
	server := NewServer(search.NewEngine(index, testEmbedder{}, false), index, testEmbedder{})
	server.ConfigureIdentity(identityStore, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/metadata/movies/resolve", strings.NewReader(`{"file_name":"[当幸福来敲门(国英双语)].The.Pursuit.0f.Happyness.2006.BluRay.mkv"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("resolve status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Found bool        `json:"found"`
		Movie model.Movie `json:"movie"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Found || result.Movie.ID != want.ID {
		t.Fatalf("resolve result=%+v", result)
	}
}

func TestReferenceFaceIngestionPersistsOneFaceVector(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	identityStore, err := identity.NewFileStore(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := identityStore.UpsertPerson(model.Person{ID: "person-a", Name: "Actor A"}); err != nil {
		t.Fatal(err)
	}
	frame := filepath.Join(t.TempDir(), "reference.jpg")
	if err := os.WriteFile(frame, []byte("not really a jpeg; fake client owns decoding"), 0o600); err != nil {
		t.Fatal(err)
	}
	tagger := identity.NewTagger(identityStore, apiFakeFaceClient{}, identity.Config{MatchThreshold: .45, MarginThreshold: .05, MinDetScore: .6, ReferenceMax: 10})
	server := NewServer(search.NewEngine(index, testEmbedder{}, false), index, testEmbedder{})
	server.ConfigureIdentity(identityStore, tagger)
	handler := server.Handler()
	payload := `{"images":[{"id":"image-a","local_path":"` + frame + `","source":"test"}]}`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/persons/person-a/faces", strings.NewReader(payload)))
	if response.Code != http.StatusOK {
		t.Fatalf("face ingestion status=%d body=%s", response.Code, response.Body.String())
	}
	if got := identityStore.ListFaceVectors("person-a"); len(got) != 1 || got[0].ImageID != "image-a" {
		t.Fatalf("face vectors=%+v", got)
	}
}
