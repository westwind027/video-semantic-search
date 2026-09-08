package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTMDBSyncMovieBuildsCastAndReferenceImages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/movie/12" {
			t.Fatalf("unexpected TMDB path %s", request.URL.Path)
		}
		if got := request.URL.Query().Get("language"); got != "en-US" {
			t.Fatalf("TMDB language = %q, want en-US", got)
		}
		_, _ = response.Write([]byte(`{"id":12,"imdb_id":"tt0012","title":"Test Movie","original_title":"Test Movie Original","release_date":"2024-01-01","overview":"overview","poster_path":"/poster.jpg","credits":{"cast":[{"id":34,"name":"Actor A","character":"Hero","profile_path":"/actor.jpg"}]}}`))
	}))
	defer server.Close()

	client := NewTMDBClient("secret", server.URL, time.Second)
	result, err := client.SyncMovie(context.Background(), "ignored", nil, 12)
	if err != nil {
		t.Fatal(err)
	}
	if result.Movie.ID != "movie:tmdb:12" || result.Movie.Year == nil || *result.Movie.Year != 2024 {
		t.Fatalf("movie = %+v", result.Movie)
	}
	if len(result.Persons) != 1 || len(result.Cast) != 1 || len(result.ProfileImage) != 1 {
		t.Fatalf("sync result = %+v", result)
	}
	if result.ProfileImage[0].SourceURL != "https://image.tmdb.org/t/p/w500/actor.jpg" {
		t.Fatalf("profile image = %+v", result.ProfileImage[0])
	}
}

func TestTMDBSyncMovieByIMDbIDLoadsFullCastAndAllProfileImages(t *testing.T) {
	requests := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.URL.Path)
		if got := request.URL.Query().Get("language"); got != "en-US" {
			t.Fatalf("TMDB language = %q, want en-US", got)
		}
		switch {
		case request.URL.Path == "/find/tt0042":
			_, _ = response.Write([]byte(`{"movie_results":[{"id":42}]}`))
		case request.URL.Path == "/movie/42":
			_, _ = response.Write([]byte(`{"id":42,"imdb_id":"tt0042","title":"Full Cast","original_title":"Full Cast","release_date":"2020-03-04","credits":{"cast":[{"id":7,"name":"Actor One","character":"A"},{"id":8,"name":"Actor Two","character":"B"}]}}`))
		case request.URL.Path == "/person/7/images":
			_, _ = response.Write([]byte(`{"profiles":[{"file_path":"/actor-7-a.jpg","width":1000,"height":1500,"vote_average":5.5},{"file_path":"/actor-7-b.jpg"}]}`))
		case request.URL.Path == "/person/8/images":
			_, _ = response.Write([]byte(`{"profiles":[{"file_path":"/actor-8-a.jpg"}]}`))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewTMDBClient("secret", server.URL, time.Second)
	year := 2020
	result, err := client.SyncMovieByIMDbID(context.Background(), "tt0042", "fallback", &year)
	if err != nil {
		t.Fatal(err)
	}
	if result.Movie.TMDBID != 42 || result.Movie.IMDbID != "tt0042" {
		t.Fatalf("movie = %+v", result.Movie)
	}
	if len(result.Cast) != 2 || len(result.Persons) != 2 || len(result.ProfileImage) != 3 {
		t.Fatalf("full result = %+v", result)
	}
	if result.ProfileImage[0].ID == result.ProfileImage[1].ID {
		t.Fatalf("profile image IDs must be unique: %+v", result.ProfileImage)
	}
	if result.ProfileImage[0].SourceURL != "https://image.tmdb.org/t/p/w500/actor-7-a.jpg" || result.ProfileImage[0].Width != 1000 || result.ProfileImage[0].VoteAverage != 5.5 {
		t.Fatalf("profile image = %+v", result.ProfileImage[0])
	}
	requestCount := len(requests)
	if _, err := client.GetPersonImages(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if len(requests) != requestCount {
		t.Fatalf("cached profile images triggered another request: %v", requests)
	}
	for _, path := range []string{"/find/tt0042", "/movie/42", "/person/7/images", "/person/8/images"} {
		found := false
		for _, requestPath := range requests {
			if requestPath == path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("request %s was not made; requests=%v", path, requests)
		}
	}
}

func TestTMDBGetRetriesRateLimit(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		attempts++
		if attempts < 2 {
			response.Header().Set("Retry-After", "0")
			response.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = fmt.Fprint(response, `{"movie_results":[{"id":12}]}`)
	}))
	defer server.Close()
	client := NewTMDBClient("secret", server.URL, time.Second)
	if _, err := client.FindMovieByIMDbID(context.Background(), "tt0012"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d", attempts)
	}
}

func TestTMDBLazyLookupUsesIMDbIDsWithoutCredits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/find/tt0042":
			_, _ = response.Write([]byte(`{"movie_results":[{"id":42}],"person_results":[]}`))
		case "/movie/42":
			if request.URL.Query().Get("append_to_response") != "" {
				t.Fatalf("lazy metadata lookup must not request credits")
			}
			_, _ = response.Write([]byte(`{"id":42,"imdb_id":"tt0042","title":"Lazy Movie","original_title":"Lazy Movie","release_date":"2025-01-02","overview":"overview","poster_path":"/poster.jpg"}`))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewTMDBClient("secret", server.URL, time.Second)
	movie, err := client.GetMovieMetadataByIMDbID(context.Background(), "tt0042")
	if err != nil {
		t.Fatal(err)
	}
	if movie.TMDBID != 42 || movie.IMDbID != "tt0042" || movie.Title != "Lazy Movie" || movie.Metadata["tmdb_metadata_loaded"] != true {
		t.Fatalf("movie = %+v", movie)
	}
}

func TestTMDBSyncMovieByIMDbIDDeduplicatesAndParallelizesProfiles(t *testing.T) {
	var inFlight int32
	var maxInFlight int32
	var profileRequests int32
	var requestMu sync.Mutex
	requests := make(map[string]int)

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestMu.Lock()
		requests[request.URL.Path]++
		requestMu.Unlock()
		switch {
		case request.URL.Path == "/find/tt-parallel":
			_, _ = fmt.Fprint(response, `{"movie_results":[{"id":42}]}`)
		case request.URL.Path == "/movie/42":
			_, _ = fmt.Fprint(response, `{"id":42,"imdb_id":"tt-parallel","title":"Parallel Movie","release_date":"2020-01-01","credits":{"cast":[{"id":7,"name":"Actor 7","character":"Role A"},{"id":7,"name":"Actor 7","character":"Role B"},{"id":8,"name":"Actor 8"},{"id":9,"name":"Actor 9"},{"id":10,"name":"Actor 10"}]}}`)
		case strings.HasPrefix(request.URL.Path, "/person/") && strings.HasSuffix(request.URL.Path, "/images"):
			atomic.AddInt32(&profileRequests, 1)
			current := atomic.AddInt32(&inFlight, 1)
			for {
				previous := atomic.LoadInt32(&maxInFlight)
				if current <= previous || atomic.CompareAndSwapInt32(&maxInFlight, previous, current) {
					break
				}
			}
			time.Sleep(40 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			_, _ = fmt.Fprintf(response, `{"profiles":[{"file_path":"/profile-%s.jpg"}]}`, strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/person/"), "/images"))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewTMDBClient("secret", server.URL, time.Second)
	result, err := client.SyncMovieByIMDbID(context.Background(), "tt-parallel", "fallback", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Persons) != 4 || len(result.Cast) != 5 || len(result.ProfileImage) != 4 {
		t.Fatalf("deduplicated result persons=%d cast=%d images=%d: %+v", len(result.Persons), len(result.Cast), len(result.ProfileImage), result)
	}
	if got := atomic.LoadInt32(&profileRequests); got != 4 {
		t.Fatalf("profile requests = %d, want one per unique TMDB person", got)
	}
	if got := atomic.LoadInt32(&maxInFlight); got < 2 {
		t.Fatalf("profile requests were not parallelized; max in flight = %d", got)
	}
	requestMu.Lock()
	defer requestMu.Unlock()
	if requests["/person/7/images"] != 1 {
		t.Fatalf("duplicate person profile requests = %d", requests["/person/7/images"])
	}
	for _, image := range result.ProfileImage {
		if !strings.HasPrefix(image.SourceURL, "https://image.tmdb.org/t/p/w500/profile-") {
			t.Fatalf("unexpected profile URL %q", image.SourceURL)
		}
	}
}

func TestTMDBSyncMovieByIMDbIDForNamesSkipsNonIMDbCastProfiles(t *testing.T) {
	var profileRequestsMu sync.Mutex
	profileRequests := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/find/tt-filtered":
			_, _ = fmt.Fprint(response, `{"movie_results":[{"id":42}]}`)
		case "/movie/42":
			_, _ = fmt.Fprint(response, `{"id":42,"imdb_id":"tt-filtered","title":"Filtered Movie","credits":{"cast":[{"id":7,"name":"IMDb Actor"},{"id":8,"name":"TMDB Extra"}]}}`)
		case "/person/7/images", "/person/8/images":
			profileRequestsMu.Lock()
			profileRequests[request.URL.Path]++
			profileRequestsMu.Unlock()
			_, _ = fmt.Fprintf(response, `{"profiles":[{"file_path":"%s.jpg"}]}`, request.URL.Path)
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewTMDBClient("secret", server.URL, time.Second)
	result, err := client.SyncMovieByIMDbIDForNames(context.Background(), "tt-filtered", "fallback", nil, []string{"IMDb Actor"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Persons) != 1 || len(result.Cast) != 1 || len(result.ProfileImage) != 1 {
		t.Fatalf("filtered result persons=%d cast=%d images=%d: %+v", len(result.Persons), len(result.Cast), len(result.ProfileImage), result)
	}
	profileRequestsMu.Lock()
	defer profileRequestsMu.Unlock()
	if profileRequests["/person/7/images"] != 1 || profileRequests["/person/8/images"] != 0 {
		t.Fatalf("profile requests = %v, want only IMDb actor", profileRequests)
	}
}
