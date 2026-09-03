package api

import (
	"context"
	"fmt"
	"io"
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
	"video-semantic-search/internal/source"
)

func newTestCache(t *testing.T, maxBytes int64) *streamCache {
	t.Helper()
	cache := newStreamCache(t.TempDir(), maxBytes)
	if !cache.enabled() {
		t.Fatalf("stream cache should be enabled")
	}
	return cache
}

// fakeUpstream emulates a drive file of the given size and counts the range
// requests it received.
type fakeUpstream struct {
	data    []byte
	offsets sync.Map // "start-end" -> *atomic.Int32
}

func newFakeUpstream(size int64) *fakeUpstream {
	return &fakeUpstream{data: make([]byte, size)}
}

func (f *fakeUpstream) fetcher() blockFetcher {
	return func(ctx context.Context, offset, length int64, nonBlocking bool) (io.ReadCloser, error) {
		key := fmt.Sprintf("%d-%d", offset, length)
		value, _ := f.offsets.LoadOrStore(key, &atomic.Int32{})
		value.(*atomic.Int32).Add(1)
		end := offset + length
		if end > int64(len(f.data)) {
			end = int64(len(f.data))
		}
		return io.NopCloser(strings.NewReader(string(f.data[offset:end]))), nil
	}
}

func (f *fakeUpstream) calls(offset, length int64) int {
	value, ok := f.offsets.Load(fmt.Sprintf("%d-%d", offset, length))
	if !ok {
		return 0
	}
	return int(value.(*atomic.Int32).Load())
}

func (f *fakeUpstream) totalCalls() int {
	total := 0
	f.offsets.Range(func(_, value any) bool {
		total += int(value.(*atomic.Int32).Load())
		return true
	})
	return total
}

func waitUntil(t *testing.T, condition func() bool) {
	t.Helper()
	for attempt := 0; attempt < 500; attempt++ {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition never became true")
}

func waitForBlocks(t *testing.T, cache *streamCache, want int) {
	t.Helper()
	waitUntil(t, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return len(cache.blocks) == want
	})
}

func TestStreamCacheServesRangesAndReusesBlocks(t *testing.T) {
	cache := newTestCache(t, 64<<20)
	const size = int64(10) << 20 // 10 blocks at 1 MiB each
	upstream := newFakeUpstream(size)
	for index := range upstream.data {
		upstream.data[index] = byte(index % 251)
	}
	fetch := upstream.fetcher()

	readAll := func(start, end int64) []byte {
		reader := cache.reader(context.Background(), "media", size, start, end, fetch)
		defer reader.Close()
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return data
	}

	first := readAll(0, size-1)
	if int64(len(first)) != size {
		t.Fatalf("read %d bytes, want %d", len(first), size)
	}
	if string(first) != string(upstream.data) {
		t.Fatalf("cached content mismatch")
	}
	// The prefetch chain may still be filling blocks; wait for all of them.
	waitForBlocks(t, cache, 10)

	second := readAll(1<<20, 2<<20)
	if string(second) != string(upstream.data[1<<20:2<<20+1]) {
		t.Fatalf("re-read content mismatch")
	}
	if calls := upstream.totalCalls(); calls != 10 {
		t.Fatalf("upstream was called %d times, want 10 (one per block)", calls)
	}
}

func TestStreamCacheSkipsCachedBlocksOnPartialRanges(t *testing.T) {
	cache := newTestCache(t, 64<<20)
	const size = int64(8) << 20
	upstream := newFakeUpstream(size)
	fetch := upstream.fetcher()

	// Only block 5 covers [5 MiB, 6 MiB].
	reader := cache.reader(context.Background(), "media", size, 5<<20, 6<<20, fetch)
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("read: %v", err)
	}
	reader.Close()
	waitForBlocks(t, cache, 3) // block 5 plus sequential prefetch of 6 and 7
	if calls := upstream.calls(5<<20, 1<<20); calls != 1 {
		t.Fatalf("block 5 fetched %d times, want 1", calls)
	}

	// Re-reading the same interval must not re-fetch block 5.
	reader = cache.reader(context.Background(), "media", size, 5<<20, 6<<20, fetch)
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	reader.Close()
	waitUntil(t, func() bool { return upstream.calls(5<<20, 1<<20) >= 1 })
	if calls := upstream.calls(5<<20, 1<<20); calls != 1 {
		t.Fatalf("block 5 fetched %d times after re-read, want 1 (re-read served from cache)", calls)
	}
}

func TestStreamCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cache := newTestCache(t, int64(1536)*1024) // fits one 1 MiB block plus a bit
	// Register three 1 MiB blocks directly (background prefetches would
	// otherwise race the small budget during reads).
	for index := 0; index < 3; index++ {
		path := filepath.Join(cache.directory, fmt.Sprintf("block-%d", index))
		if err := os.WriteFile(path, make([]byte, 1<<20), 0o600); err != nil {
			t.Fatal(err)
		}
		cache.register(fmt.Sprintf("media/%d", index), path, 1<<20)
	}
	cache.mu.Lock()
	blocks := len(cache.blocks)
	total := cache.totalBytes
	cache.mu.Unlock()
	if total > cache.maxBytes {
		t.Fatalf("cache holds %d bytes over budget %d", total, cache.maxBytes)
	}
	if blocks != 1 {
		t.Fatalf("cache keeps %d blocks, want 1 after eviction", blocks)
	}
}

func TestParseStreamRange(t *testing.T) {
	const size = 1000
	cases := []struct {
		header     string
		start, end int64
		status     int
		ok         bool
	}{
		{header: "", start: 0, end: 999, status: http.StatusOK, ok: true},
		{header: "bytes=100-", start: 100, end: 999, status: http.StatusPartialContent, ok: true},
		{header: "bytes=100-199", start: 100, end: 199, status: http.StatusPartialContent, ok: true},
		{header: "bytes=100-5000", start: 100, end: 999, status: http.StatusPartialContent, ok: true},
		{header: "bytes=-200", start: 800, end: 999, status: http.StatusPartialContent, ok: true},
		{header: "bytes=-0", ok: false},
		{header: "bytes=1000-", ok: false},
		{header: "bytes=abc-", ok: false},
		{header: "bytes=0-1,5-9", ok: false},
		{header: "chunks=0-1", start: 0, end: 999, status: http.StatusOK, ok: true},
	}
	for _, testCase := range cases {
		start, end, status, ok := parseStreamRange(testCase.header, size)
		if ok != testCase.ok || start != testCase.start || end != testCase.end || status != testCase.status {
			t.Fatalf("parseStreamRange(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
				testCase.header, start, end, status, ok, testCase.start, testCase.end, testCase.status, testCase.ok)
		}
	}
}

func TestMediaStreamCachesRemoteBlocks(t *testing.T) {
	const size = int64(9) << 20
	upstreamData := make([]byte, size)
	for index := range upstreamData {
		upstreamData[index] = byte(index % 249)
	}
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		upstreamCalls.Add(1)
		rangeHeader := request.Header.Get("Range")
		if !strings.HasPrefix(rangeHeader, "bytes=") {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		spec := strings.TrimPrefix(rangeHeader, "bytes=")
		parts := strings.SplitN(spec, "-", 2)
		start, _ := strconv.ParseInt(parts[0], 10, 64)
		end := size - 1
		if len(parts) == 2 && parts[1] != "" {
			end, _ = strconv.ParseInt(parts[1], 10, 64)
		}
		response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		response.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		response.WriteHeader(http.StatusPartialContent)
		_, _ = response.Write(upstreamData[start : end+1])
	}))
	defer upstream.Close()

	media := model.Media{
		MediaID: "cached-remote",
		Title:   "Cached Remote",
		Scenes:  []model.Scene{{Start: 0, End: 1}},
		Metadata: map[string]any{
			"source":          "aliyun_drive",
			"remote_drive_id": "drive-1",
			"remote_file_id":  "file-1",
			"remote_name":     "movie.mp4",
			"remote_size":     float64(size),
		},
	}
	handler, server := newStreamTestServer(t, media, &fakeStreamResolver{
		responses: []source.Video{{URL: upstream.URL + "/download", Name: "movie.mp4", Size: size}},
	})
	server.cache = newTestCache(t, 64<<20)

	doRange := func(rangeHeader string) *http.Response {
		request := httptest.NewRequest(http.MethodGet, "/v1/media/cached-remote/stream", nil)
		request.Header.Set("Range", rangeHeader)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Result()
	}

	first := doRange("bytes=0-2097151")
	body, err := io.ReadAll(first.Body)
	if err != nil {
		t.Fatalf("read first response: %v", err)
	}
	if first.StatusCode != http.StatusPartialContent {
		t.Fatalf("first response status %d, want 206", first.StatusCode)
	}
	if first.Header.Get("Content-Range") != "bytes 0-2097151/9437184" {
		t.Fatalf("unexpected Content-Range %q", first.Header.Get("Content-Range"))
	}
	if string(body) != string(upstreamData[:2097152]) {
		t.Fatalf("first response body mismatch")
	}

	waitForBlocks(t, server.cache, 9)
	before := upstreamCalls.Load()
	second := doRange("bytes=0-2097151")
	secondBody, _ := io.ReadAll(second.Body)
	if second.StatusCode != http.StatusPartialContent || string(secondBody) != string(upstreamData[:2097152]) {
		t.Fatalf("second response mismatch: status %d", second.StatusCode)
	}
	if after := upstreamCalls.Load(); after != before {
		t.Fatalf("repeat request hit upstream %d extra times", after-before)
	}
	if calls := upstreamCalls.Load(); calls < 1 {
		t.Fatalf("upstream was never called")
	}
}
