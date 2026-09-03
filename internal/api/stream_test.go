package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"video-semantic-search/internal/model"
	"video-semantic-search/internal/search"
	"video-semantic-search/internal/source"
	"video-semantic-search/internal/store"
)

type fakeStreamResolver struct {
	calls     atomic.Int32
	responses []source.Video
	err       error
}

func (resolver *fakeStreamResolver) ResolveVideo(context.Context, string, string) (source.Video, error) {
	index := int(resolver.calls.Add(1)) - 1
	if index >= len(resolver.responses) {
		index = len(resolver.responses) - 1
	}
	if resolver.err != nil {
		return source.Video{}, resolver.err
	}
	return resolver.responses[index], nil
}

func newStreamTestServer(t *testing.T, media model.Media, resolver streamResolver) (http.Handler, *Server) {
	t.Helper()
	directory := t.TempDir()
	index, err := store.NewFileStore(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { index.Close() })
	engine := search.NewEngine(index, testEmbedder{}, false)
	server := NewServerWithAcquisition(engine, index, testEmbedder{}, nil, filepath.Join(directory, "frames"))
	server.streams = newStreamProxy(resolver)
	// Point the disk block cache at a per-test directory so tests never
	// create or wipe the real data/stream-cache directory.
	server.cache = newStreamCache(filepath.Join(directory, "stream-cache"), 64<<20)
	if _, err := engine.Index(context.Background(), media); err != nil {
		t.Fatal(err)
	}
	return server.Handler(), server
}

func TestMediaStreamServesLocalFileWithRangeRequests(t *testing.T) {
	directory := t.TempDir()
	videoPath := filepath.Join(directory, "movie.mp4")
	payload := make([]byte, 2048)
	for index := range payload {
		payload[index] = byte(index % 251)
	}
	if err := os.WriteFile(videoPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	media := model.Media{MediaID: "local-media", Title: "local", Metadata: map[string]any{"source": "local_file", "local_path": videoPath}, Scenes: []model.Scene{{Start: 0, End: 1}}}
	handler, _ := newStreamTestServer(t, media, nil)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/media/local-media/stream", nil)
	request.Header.Set("Range", "bytes=100-199")
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent {
		t.Fatalf("local stream status = %d, body=%s", response.Code, response.Body.String())
	}
	if got := response.Body.Len(); got != 100 {
		t.Fatalf("local stream body = %d bytes, want 100", got)
	}
	if contentRange := response.Header().Get("Content-Range"); contentRange != "bytes 100-199/2048" {
		t.Fatalf("Content-Range = %q", contentRange)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "video/mp4" {
		t.Fatalf("Content-Type = %q, want video/mp4", contentType)
	}
	if acceptRanges := response.Header().Get("Accept-Ranges"); acceptRanges != "bytes" {
		t.Fatalf("Accept-Ranges = %q", acceptRanges)
	}

	full := httptest.NewRecorder()
	handler.ServeHTTP(full, httptest.NewRequest(http.MethodGet, "/v1/media/local-media/stream", nil))
	if full.Code != http.StatusOK || full.Body.Len() != len(payload) {
		t.Fatalf("full stream status = %d, body = %d bytes", full.Code, full.Body.Len())
	}
}

func TestMediaStreamProxiesAliyunDriveRanges(t *testing.T) {
	// Two-plus blocks so the parallel prefetch has work beyond block 0.
	payload := make([]byte, 9<<20)
	for index := range payload {
		payload[index] = byte(index % 253)
	}
	var upstreamRequests atomic.Int32
	var rangeCalls sync.Map // "start-end" -> count of upstream fetches
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		upstreamRequests.Add(1)
		spec := strings.TrimPrefix(request.Header.Get("Range"), "bytes=")
		if value, loaded := rangeCalls.LoadOrStore(spec, new(int32)); loaded {
			atomic.AddInt32(value.(*int32), 1)
		} else {
			atomic.AddInt32(value.(*int32), 1)
		}
		parts := strings.SplitN(spec, "-", 2)
		start, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		end := int64(len(payload)) - 1
		if len(parts) == 2 && parts[1] != "" {
			if end, err = strconv.ParseInt(parts[1], 10, 64); err != nil {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		response.Header().Set("Content-Type", "video/mp4")
		response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		response.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		response.WriteHeader(http.StatusPartialContent)
		_, _ = response.Write(payload[start : end+1])
	}))
	t.Cleanup(upstream.Close)

	resolver := &fakeStreamResolver{responses: []source.Video{{URL: upstream.URL + "/movie.mp4", Name: "movie.mp4", Size: int64(len(payload))}}}
	media := model.Media{MediaID: "remote-media", Title: "remote", Metadata: map[string]any{"source": "aliyun_drive", "remote_drive_id": "drive-1", "remote_file_id": "file-1"}, Scenes: []model.Scene{{Start: 0, End: 1}}}
	handler, server := newStreamTestServer(t, media, resolver)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/media/remote-media/stream", nil)
	request.Header.Set("Range", "bytes=100-199")
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent {
		t.Fatalf("remote stream status = %d, body=%s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); body != string(payload[100:200]) {
		t.Fatalf("remote stream body mismatch: %d bytes", response.Body.Len())
	}
	if contentRange := response.Header().Get("Content-Range"); contentRange != "bytes 100-199/9437184" {
		t.Fatalf("Content-Range = %q", contentRange)
	}
	if resolver.calls.Load() != 1 {
		t.Fatalf("resolver calls = %d, want 1 (signed URL should be cached)", resolver.calls.Load())
	}
	// Block 0 is fetched exactly once (streamed into the cache); the
	// browser's narrow range is carved out of it.
	if value, _ := rangeCalls.Load("0-1048575"); atomic.LoadInt32(value.(*int32)) != 1 {
		t.Fatalf("block 0 upstream fetches = %d, want 1", atomic.LoadInt32(value.(*int32)))
	}

	// The prefetch chain fills the following blocks in parallel. Once a block
	// is cached, a range inside it is answered without touching the drive.
	waitUntil(t, func() bool { return upstreamRequests.Load() >= 3 })
	thirdRequest := httptest.NewRequest(http.MethodGet, "/v1/media/remote-media/stream", nil)
	thirdRequest.Header.Set("Range", "bytes=4194304-4194403")
	third := httptest.NewRecorder()
	handler.ServeHTTP(third, thirdRequest)
	if third.Code != http.StatusPartialContent {
		t.Fatalf("cached block stream status = %d", third.Code)
	}
	if third.Body.String() != string(payload[4194304:4194404]) {
		t.Fatalf("cached block body mismatch")
	}
	// Block 4 was fetched exactly once by the prefetch; serving the browser
	// range from it must not add any upstream request for that block.
	if value, _ := rangeCalls.Load("4194304-5242879"); atomic.LoadInt32(value.(*int32)) != 1 {
		t.Fatalf("block 4 upstream fetches = %d, want 1 (served from cache)", atomic.LoadInt32(value.(*int32)))
	}

	// A repeat request reuses the cached signed URL without hitting the drive API.
	again := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodGet, "/v1/media/remote-media/stream", nil)
	secondRequest.Header.Set("Range", "bytes=100-199")
	handler.ServeHTTP(again, secondRequest)
	if again.Code != http.StatusPartialContent {
		t.Fatalf("second remote stream status = %d", again.Code)
	}
	if again.Body.String() != string(payload[100:200]) {
		t.Fatalf("second remote stream body mismatch")
	}
	if resolver.calls.Load() != 1 {
		t.Fatalf("resolver calls after second seek = %d, want 1", resolver.calls.Load())
	}
	// Block 0 was streamed once during the first request; a repeat must be
	// served from cache (background prefetch requests do not count here).
	if value, _ := rangeCalls.Load("0-1048575"); atomic.LoadInt32(value.(*int32)) != 1 {
		t.Fatalf("block 0 upstream fetches after second seek = %d, want 1", atomic.LoadInt32(value.(*int32)))
	}
	// Let the background prefetches finish before TempDir cleanup, or they
	// will write files after RemoveAll and the cleanup errors out. The number
	// of cached blocks is timing dependent (busy prefetches give up without
	// retrying), so wait for in-flight downloads to drain; a second check
	// after a grace period catches ones registered in the meantime.
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(150 * time.Millisecond)
		}
		waitUntil(t, func() bool {
			server.cache.mu.Lock()
			defer server.cache.mu.Unlock()
			return len(server.cache.loading) == 0
		})
	}
}

func TestMediaStreamRefreshesExpiredDriveURL(t *testing.T) {
	payload := []byte("0123456789")
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/expired.mp4") {
			// The first signed URL has been revoked by the drive.
			response.WriteHeader(http.StatusForbidden)
			return
		}
		response.Header().Set("Content-Type", "video/mp4")
		response.Header().Set("Content-Length", "10")
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write(payload)
	}))
	t.Cleanup(upstream.Close)

	resolver := &fakeStreamResolver{responses: []source.Video{
		{URL: upstream.URL + "/expired.mp4", Name: "movie.mp4"},
		{URL: upstream.URL + "/fresh.mp4", Name: "movie.mp4"},
	}}
	media := model.Media{MediaID: "expired-media", Title: "expired", Metadata: map[string]any{"source": "aliyun_drive", "remote_drive_id": "drive-1", "remote_file_id": "file-1"}, Scenes: []model.Scene{{Start: 0, End: 1}}}
	handler, server := newStreamTestServer(t, media, resolver)
	// Pre-populate the cache with the expired URL, then flip the upstream to
	// reject it so the proxy must refresh and retry.
	resolver.calls.Store(1)
	server.streams.urls["expired-media"] = cachedStreamURL{url: upstream.URL + "/expired.mp4", name: "movie.mp4", expireAt: time.Now().Add(10 * time.Minute)}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/media/expired-media/stream", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("refreshed stream status = %d, body=%s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); body != string(payload) {
		t.Fatalf("refreshed stream body = %q", body)
	}
	if resolver.calls.Load() != 2 {
		t.Fatalf("resolver calls = %d, want 2 after refresh", resolver.calls.Load())
	}
	if resolver.responses[1].URL != upstream.URL+"/fresh.mp4" {
		// Sanity: the fake must have served the fresh URL on the second call.
		t.Fatalf("unexpected resolver responses = %+v", resolver.responses)
	}
}

func TestMediaStreamRejectsUnknownMedia(t *testing.T) {
	handler, _ := newStreamTestServer(t, model.Media{MediaID: "known", Title: "known", Scenes: []model.Scene{{Start: 0, End: 1}}}, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/media/unknown/stream", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown media stream status = %d, want 404", response.Code)
	}
}
