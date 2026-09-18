package identity

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"video-semantic-search/internal/metadata"
	"video-semantic-search/internal/model"
)

func TestApplyTMDBResultDeduplicatesPersonAcrossMovies(t *testing.T) {
	sqliteStore, err := NewSQLiteStore(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	store := &countingBatchStore{SQLiteStore: sqliteStore}
	defer store.Close()

	movies := []model.Movie{
		{ID: "movie-1", IMDbID: "tt0001", Title: "Movie One"},
		{ID: "movie-2", IMDbID: "tt0002", Title: "Movie Two"},
	}
	for _, movie := range movies {
		if err := store.UpsertMovie(movie); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertPerson(model.Person{ID: "person-imdb-nm1", IMDbID: "nm0001", Name: "Actor One"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceMovieCast("movie-1", []model.MovieCast{{MovieID: "movie-1", PersonID: "person-imdb-nm1", IMDbNameID: "nm0001", CharacterName: "First Role", Source: "imdb_principals"}}); err != nil {
		t.Fatal(err)
	}

	service := NewMoviePreparationService(store, nil, nil, 1, 2, false)
	image := func(movieID, path string) model.PersonImage {
		return model.PersonImage{ID: path, PersonID: "person:tmdb:7", Source: "tmdb", SourceURL: "https://image.tmdb.org/t/p/original/" + path + ".jpg", SourceIndex: 0}
	}
	first := metadata.TMDBSyncResult{
		Movie:   model.Movie{TMDBID: 101, Title: "Movie One"},
		Persons: []model.Person{{ID: "person:tmdb:7", TMDBID: 7, Name: "Actor One"}},
		Cast:    []model.MovieCast{{PersonID: "person:tmdb:7", CharacterName: "First Role", BillingOrder: 0}},
		ProfileImage: []model.PersonImage{
			image("movie-1", "profile-one"),
		},
	}
	movieOne := movies[0]
	if err := service.ApplyTMDBResult(&movieOne, first); err != nil {
		t.Fatal(err)
	}
	if store.personBatchCalls != 1 || store.imageBatchCalls != 1 {
		t.Fatalf("first TMDB apply batches persons=%d images=%d", store.personBatchCalls, store.imageBatchCalls)
	}
	if err := store.UpsertMovie(movieOne); err != nil {
		t.Fatal(err)
	}

	second := metadata.TMDBSyncResult{
		Movie:   model.Movie{TMDBID: 102, Title: "Movie Two"},
		Persons: []model.Person{{ID: "person:tmdb:7", TMDBID: 7, Name: "Actor One"}},
		Cast:    []model.MovieCast{{PersonID: "person:tmdb:7", CharacterName: "Second Role", BillingOrder: 0}},
		ProfileImage: []model.PersonImage{
			image("movie-2", "profile-two"),
		},
	}
	movieTwo := movies[1]
	if err := store.ReplaceMovieCast("movie-2", []model.MovieCast{{MovieID: "movie-2", PersonID: "person-imdb-nm1", IMDbNameID: "nm0001", CharacterName: "Second Role", Source: "imdb_principals"}}); err != nil {
		t.Fatal(err)
	}
	if err := service.ApplyTMDBResult(&movieTwo, second); err != nil {
		t.Fatal(err)
	}
	if store.personBatchCalls != 2 || store.imageBatchCalls != 2 {
		t.Fatalf("second TMDB apply batches persons=%d images=%d", store.personBatchCalls, store.imageBatchCalls)
	}
	if err := store.UpsertMovie(movieTwo); err != nil {
		t.Fatal(err)
	}

	people := store.ListPersons()
	if len(people) != 1 || people[0].ID != "person-imdb-nm1" || people[0].TMDBID != 7 {
		t.Fatalf("deduplicated people = %+v", people)
	}
	for _, movieID := range []string{"movie-1", "movie-2"} {
		cast := store.GetMovieCast(movieID)
		if len(cast) != 1 || cast[0].PersonID != "person-imdb-nm1" {
			t.Fatalf("cast for %s = %+v", movieID, cast)
		}
	}
	if images := store.ListPersonImages("person-imdb-nm1"); len(images) != 2 {
		t.Fatalf("deduplicated person images = %+v", images)
	}
}

func TestApplyTMDBResultDoesNotCreateTMDBOnlyPeopleOrCast(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	movie := model.Movie{ID: "movie-1", IMDbID: "tt0001", Title: "Movie One"}
	if err := store.UpsertMovie(movie); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPerson(model.Person{ID: "person-imdb-nm1", IMDbID: "nm0001", Name: "Actor One"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceMovieCast(movie.ID, []model.MovieCast{{MovieID: movie.ID, PersonID: "person-imdb-nm1", IMDbNameID: "nm0001", CharacterName: "IMDb Role", Source: "imdb_principals"}}); err != nil {
		t.Fatal(err)
	}
	service := NewMoviePreparationService(store, nil, nil, 1, 2, false)
	result := metadata.TMDBSyncResult{
		Movie: model.Movie{TMDBID: 42, Title: "Movie One"},
		Persons: []model.Person{
			{ID: "person:tmdb:7", TMDBID: 7, Name: "Actor One"},
			{ID: "person:tmdb:8", TMDBID: 8, Name: "TMDB Extra"},
		},
		Cast: []model.MovieCast{
			{PersonID: "person:tmdb:7", CharacterName: "TMDB Role", BillingOrder: 0},
			{PersonID: "person:tmdb:8", CharacterName: "Extra Role", BillingOrder: 1},
		},
		ProfileImage: []model.PersonImage{
			{ID: "image-actor", PersonID: "person:tmdb:7", Source: "tmdb", SourceURL: "https://image/actor.jpg"},
			{ID: "image-extra", PersonID: "person:tmdb:8", Source: "tmdb", SourceURL: "https://image/extra.jpg"},
		},
	}
	if err := service.ApplyTMDBResult(&movie, result); err != nil {
		t.Fatal(err)
	}
	people := store.ListPersons()
	if len(people) != 1 || people[0].ID != "person-imdb-nm1" || people[0].TMDBID != 7 {
		t.Fatalf("people after TMDB merge = %+v", people)
	}
	cast := store.GetMovieCast(movie.ID)
	if len(cast) != 1 || cast[0].PersonID != "person-imdb-nm1" || cast[0].Source != "imdb_principals" || cast[0].CharacterName != "IMDb Role" {
		t.Fatalf("cast after TMDB merge = %+v", cast)
	}
	if images := store.ListPersonImages("person-imdb-nm1"); len(images) != 1 || images[0].ID != "image-actor" {
		t.Fatalf("images after TMDB merge = %+v", images)
	}
	if _, ok := store.GetPerson("person:tmdb:8"); ok {
		t.Fatal("TMDB-only person was created")
	}
}

func TestMoviePreparationCompletesFaceBankAndIsIdempotent(t *testing.T) {
	directory := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(directory, "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	movie := model.Movie{ID: "movie-1", IMDbID: "tt0001", Title: "Movie One", TMDBStatus: TMDBStatusComplete, FaceBankStatus: FaceBankStatusPending, Metadata: map[string]any{"tmdb_full_cast": true}}
	if err := store.UpsertMovie(movie); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPerson(model.Person{ID: "person-1", IMDbID: "nm0001", Name: "Actor One"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceMovieCast(movie.ID, []model.MovieCast{{MovieID: movie.ID, PersonID: "person-1", CharacterName: "Role", Source: "imdb_principals"}}); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		path := filepath.Join(directory, "reference-"+string(rune('a'+index))+".jpg")
		if err := os.WriteFile(path, []byte("test image"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertPersonImage(model.PersonImage{ID: "image-" + string(rune('a'+index)), PersonID: "person-1", SourceIndex: index, LocalPath: path, Status: "pending"}); err != nil {
			t.Fatal(err)
		}
	}

	references := NewReferenceIngestor(store, testFaceClient{}, ReferenceConfig{Workers: 1, MinDetScore: .6})
	service := NewMoviePreparationService(store, nil, references, 2, 2, true)
	result, err := service.Prepare(context.Background(), MoviePreparationRequest{MovieID: movie.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready || result.FaceBankStatus != FaceBankStatusComplete || result.ReadyPersonCount != 1 || len(store.ListFaceVectors("person-1")) != 2 {
		t.Fatalf("first preparation = %+v, vectors=%d", result, len(store.ListFaceVectors("person-1")))
	}

	result, err = service.Prepare(context.Background(), MoviePreparationRequest{MovieID: movie.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready || len(store.ListFaceVectors("person-1")) != 2 {
		t.Fatalf("second preparation = %+v, vectors=%d", result, len(store.ListFaceVectors("person-1")))
	}
}

func TestMoviePreparationAllowsPartialFaceBank(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	movie := model.Movie{
		ID:             "movie-partial-face-bank",
		IMDbID:         "tt0002",
		Title:          "Partial Face Bank",
		TMDBStatus:     TMDBStatusComplete,
		FaceBankStatus: FaceBankStatusPending,
		Metadata:       map[string]any{"tmdb_full_cast": true},
	}
	if err := store.UpsertMovie(movie); err != nil {
		t.Fatal(err)
	}
	people := []model.Person{
		{ID: "person-complete", IMDbID: "nm0002", Name: "Complete Actor"},
		{ID: "person-partial", IMDbID: "nm0003", Name: "Partial Actor"},
	}
	for _, person := range people {
		if err := store.UpsertPerson(person); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReplaceMovieCast(movie.ID, []model.MovieCast{
		{MovieID: movie.ID, PersonID: "person-complete", Source: "imdb_principals"},
		{MovieID: movie.ID, PersonID: "person-partial", Source: "imdb_principals"},
	}); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		if err := store.UpsertFaceVector(model.FaceVector{
			ID:       fmt.Sprintf("complete-vector-%d", index),
			PersonID: "person-complete",
			ImageID:  fmt.Sprintf("complete-image-%d", index),
			Vector:   []float32{1, 0},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertFaceVector(model.FaceVector{ID: "partial-vector", PersonID: "person-partial", ImageID: "partial-image", Vector: []float32{0, 1}}); err != nil {
		t.Fatal(err)
	}

	references := NewReferenceIngestor(store, testFaceClient{}, ReferenceConfig{Workers: 1, MinDetScore: .6})
	service := NewMoviePreparationService(store, nil, references, 2, 2, true)
	result, err := service.Prepare(context.Background(), MoviePreparationRequest{MovieID: movie.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready || result.FaceBankStatus != FaceBankStatusPartial {
		t.Fatalf("partial face bank must not block preparation: %+v", result)
	}
	if result.ReadyPersonCount != 1 || len(result.MissingPersonNames) != 1 || result.MissingPersonNames[0] != "Partial Actor" {
		t.Fatalf("partial face bank details = %+v", result)
	}
}

func TestLazyReferenceLoaderCachesCompletedProfileListing(t *testing.T) {
	var profileRequests int32
	tmdbServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/person/7/images" {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&profileRequests, 1)
		_, _ = response.Write([]byte(`{"profiles":[{"file_path":"/profile-a.jpg"},{"file_path":"/profile-b.jpg"}]}`))
	}))
	defer tmdbServer.Close()

	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	person := model.Person{ID: "person-1", IMDbID: "nm0001", TMDBID: 7, Name: "Actor One"}
	if err := store.UpsertPerson(person); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceMovieCast("movie-1", []model.MovieCast{{MovieID: "movie-1", PersonID: person.ID, Source: "imdb_principals"}}); err != nil {
		t.Fatal(err)
	}

	client := metadata.NewTMDBClient("secret", tmdbServer.URL, time.Second)
	references := NewReferenceIngestor(store, testFaceClient{}, ReferenceConfig{DownloadRoot: filepath.Join(t.TempDir(), "faces"), Workers: 1, MinDetScore: .6})
	references.httpClient = &http.Client{Transport: identityRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("image")), Header: make(http.Header), Request: request}, nil
	})}
	loader := NewLazyReferenceLoader(store, client, references, 3)
	cast := []model.MovieCast{{MovieID: "movie-1", PersonID: person.ID, Source: "imdb_principals"}}
	if err := loader.EnsureMovieReferences(context.Background(), model.Movie{ID: "movie-1", TMDBID: 42}, cast); err != nil {
		t.Fatal(err)
	}
	if got := len(store.ListFaceVectors(person.ID)); got != 2 {
		t.Fatalf("first lazy load vectors = %d, want 2", got)
	}

	// A new client models a process restart: the in-memory TMDB cache is gone,
	// so the durable profile-loaded marker must prevent another remote request.
	secondClient := metadata.NewTMDBClient("secret", tmdbServer.URL, time.Second)
	secondLoader := NewLazyReferenceLoader(store, secondClient, references, 3)
	if err := secondLoader.EnsureMovieReferences(context.Background(), model.Movie{ID: "movie-1", TMDBID: 42}, cast); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&profileRequests); got != 1 {
		t.Fatalf("profile requests after restart = %d, want 1", got)
	}
	if stored, ok := store.GetPerson(person.ID); !ok || !hasMetadataFlag(stored.Metadata, "tmdb_profile_images_loaded") {
		t.Fatalf("profile-loaded marker was not persisted: %+v", stored)
	}
}

type identityRoundTripFunc func(*http.Request) (*http.Response, error)

func (f identityRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type testFaceClient struct{}

type countingBatchStore struct {
	*SQLiteStore
	personBatchCalls int
	imageBatchCalls  int
}

func (s *countingBatchStore) UpsertPersons(persons []model.Person) error {
	s.personBatchCalls++
	return s.SQLiteStore.UpsertPersons(persons)
}

func (s *countingBatchStore) UpsertPersonImages(images []model.PersonImage) error {
	s.imageBatchCalls++
	return s.SQLiteStore.UpsertPersonImages(images)
}

func (testFaceClient) Embed(context.Context, string) (EmbedResult, error) {
	return EmbedResult{Model: "test", Dimension: 2, Faces: []Face{{DetScore: .99, Quality: .9, Embedding: []float32{1, 0}}}}, nil
}

func (testFaceClient) Health(context.Context) (Health, error) {
	return Health{Status: "ok"}, nil
}
