package source

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// requestLog records the intervals the server was actually asked for, so tests
// can assert on download volume and not only on the bytes that came back.
type requestLog struct {
	mu        sync.Mutex
	intervals []string
	bytes     int64
}

func (l *requestLog) add(start, end int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.intervals = append(l.intervals, fmt.Sprintf("%d-%d", start, end))
	l.bytes += end - start + 1
}

func (l *requestLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.intervals)
}

func (l *requestLog) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.intervals, ",")
}

func (l *requestLog) downloaded() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bytes
}

func newRangeTestServer(t *testing.T, payload []byte, delay time.Duration) (*httptest.Server, *requestLog) {
	t.Helper()
	recorded := &requestLog{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		value := request.Header.Get("Range")
		if value == "" {
			t.Error("range header is missing")
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(value, "bytes=%d-%d", &start, &end); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		if start < 0 || start > end || start >= int64(len(payload)) {
			response.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= int64(len(payload)) {
			end = int64(len(payload)) - 1
		}
		recorded.add(start, end)
		if delay > 0 {
			time.Sleep(delay)
		}
		response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		response.WriteHeader(http.StatusPartialContent)
		_, _ = response.Write(payload[start : end+1])
	}))
	t.Cleanup(server.Close)
	return server, recorded
}

func TestRangeReaderSharesCacheAcrossOverlappingReads(t *testing.T) {
	payload := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	server, recorded := newRangeTestServer(t, payload, 0)

	reader := NewHTTPRangeReader(server.URL, int64(len(payload)), server.Client())
	reader.ChunkSize = 8
	buffer := make([]byte, 12)
	n, err := reader.ReadAt(buffer, 5)
	if err != nil || n != len(buffer) || string(buffer) != string(payload[5:17]) {
		t.Fatalf("first read = %d bytes, %v, %q", n, err, buffer)
	}
	// A read that spans the aligned grid is served by one block instead of one
	// request per chunk: [5,16] with chunk size 8 becomes the interval 0-23.
	if got := recorded.joined(); got != "0-23" {
		t.Fatalf("requested intervals = %q, want %q", got, "0-23")
	}
	n, err = reader.ReadAt(buffer[:4], 7)
	if err != nil || n != 4 || string(buffer[:4]) != string(payload[7:11]) {
		t.Fatalf("cached read = %d bytes, %v, %q", n, err, buffer[:4])
	}
	if got := recorded.count(); got != 1 {
		t.Fatalf("range requests = %d, want 1", got)
	}
	stats := reader.Stats()
	if stats.BytesDownloaded != 24 || stats.CachedBlocks != 1 || stats.CachedBytes != 24 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestFetchRangeDownloadsOnlyTheRequestedBytes(t *testing.T) {
	payload := make([]byte, 4096)
	for index := range payload {
		payload[index] = byte(index)
	}
	server, recorded := newRangeTestServer(t, payload, 0)

	reader := NewHTTPRangeReader(server.URL, int64(len(payload)), server.Client())
	ctx := context.Background()
	data, err := reader.FetchRange(ctx, 100, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, payload[100:116]) {
		t.Fatalf("FetchRange returned %v", data)
	}
	// Random access must never be widened to the default 4 MiB chunk grid: this
	// is what made a single keyframe sample cost megabytes of traffic.
	if got := recorded.joined(); got != "100-115" {
		t.Fatalf("requested intervals = %q, want %q", got, "100-115")
	}
	if _, err := reader.FetchRange(ctx, 100, 16); err != nil {
		t.Fatal(err)
	}
	if got := recorded.count(); got != 1 {
		t.Fatalf("cached FetchRange issued %d requests, want 1", got)
	}
	data, err = reader.FetchRange(ctx, 110, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, payload[110:126]) {
		t.Fatalf("overlapping FetchRange returned %v", data)
	}
	// Only the 10 bytes that were not cached yet are downloaded.
	if got := recorded.joined(); got != "100-115,116-125" {
		t.Fatalf("requested intervals = %q", got)
	}
	if stats := reader.Stats(); stats.BytesDownloaded != 26 || stats.CachedBytes != 26 || stats.CachedBlocks != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestFetchRangeMergesConcurrentRequests(t *testing.T) {
	payload := make([]byte, 4096)
	server, recorded := newRangeTestServer(t, payload, 60*time.Millisecond)

	reader := NewHTTPRangeReader(server.URL, int64(len(payload)), server.Client())
	ctx := context.Background()
	var barrier, done sync.WaitGroup
	barrier.Add(1)
	for worker := 0; worker < 8; worker++ {
		done.Add(1)
		go func() {
			defer done.Done()
			barrier.Wait()
			data, err := reader.FetchRange(ctx, 2048, 512)
			if err != nil {
				t.Error(err)
				return
			}
			if len(data) != 512 {
				t.Errorf("FetchRange returned %d bytes, want 512", len(data))
			}
		}()
	}
	barrier.Done()
	done.Wait()
	if got := recorded.count(); got != 1 {
		t.Fatalf("concurrent FetchRange issued %d requests (%s), want 1", got, recorded.joined())
	}
}

func TestRangeCacheEvictsLeastRecentlyUsedBlocks(t *testing.T) {
	payload := make([]byte, 8192)
	server, recorded := newRangeTestServer(t, payload, 0)

	reader := NewHTTPRangeReader(server.URL, int64(len(payload)), server.Client())
	reader.SetMaxCacheBytes(2048)
	ctx := context.Background()
	for _, offset := range []int64{0, 1024, 2048} {
		if _, err := reader.FetchRange(ctx, offset, 1024); err != nil {
			t.Fatal(err)
		}
	}
	stats := reader.Stats()
	if stats.CachedBytes > 2048 {
		t.Fatalf("cache holds %d bytes, the limit is 2048", stats.CachedBytes)
	}
	if stats.CachedBlocks != 2 {
		t.Fatalf("cache holds %d blocks, want 2 after eviction", stats.CachedBlocks)
	}
	if got := recorded.count(); got != 3 {
		t.Fatalf("range requests = %d, want 3", got)
	}
	// The oldest block was dropped, so reading it again costs one request.
	if _, err := reader.FetchRange(ctx, 0, 1024); err != nil {
		t.Fatal(err)
	}
	if got := recorded.count(); got != 4 {
		t.Fatalf("range requests after eviction = %d, want 4", got)
	}
	if stats := reader.Stats(); stats.CachedBytes > 2048 {
		t.Fatalf("cache holds %d bytes after the re-read, the limit is 2048", stats.CachedBytes)
	}
	if got, want := recorded.downloaded(), int64(4096); got != want {
		t.Fatalf("downloaded %d bytes, want %d", got, want)
	}
}

func TestFetchRangeDiscoversUnknownSize(t *testing.T) {
	payload := make([]byte, 4096)
	for index := range payload {
		payload[index] = byte(index >> 8)
	}
	server, recorded := newRangeTestServer(t, payload, 0)

	reader := NewHTTPRangeReader(server.URL, 0, server.Client())
	data, err := reader.FetchRange(context.Background(), 512, 32)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, payload[512:544]) {
		t.Fatalf("FetchRange returned %v", data)
	}
	if reader.Size() != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", reader.Size(), len(payload))
	}
	// One byte to learn the length, then exactly the requested interval.
	if got := recorded.joined(); got != "0-0,512-543" {
		t.Fatalf("requested intervals = %q, want %q", got, "0-0,512-543")
	}
}

func TestFetchRangeValidatesArgumentsAndClampsToSize(t *testing.T) {
	payload := make([]byte, 64)
	for index := range payload {
		payload[index] = byte(index)
	}
	server, recorded := newRangeTestServer(t, payload, 0)

	reader := NewHTTPRangeReader(server.URL, int64(len(payload)), server.Client())
	ctx := context.Background()
	if _, err := reader.FetchRange(ctx, 0, maxRangeLength+1); err == nil {
		t.Fatal("FetchRange should reject a length beyond the limit")
	}
	if _, err := reader.FetchRange(ctx, 0, 0); err == nil {
		t.Fatal("FetchRange should reject a zero length")
	}
	if _, err := reader.FetchRange(ctx, -1, 8); err == nil {
		t.Fatal("FetchRange should reject a negative offset")
	}
	if _, err := reader.FetchRange(ctx, int64(len(payload)), 8); err == nil {
		t.Fatal("FetchRange should fail past the end of the file")
	}
	if got := recorded.count(); got != 0 {
		t.Fatalf("rejected FetchRange issued %d requests, want 0", got)
	}
	data, err := reader.FetchRange(ctx, 60, 32)
	if err != nil || len(data) != 4 || !bytes.Equal(data, payload[60:]) {
		t.Fatalf("clamped FetchRange = %d bytes, %v, %v", len(data), data, err)
	}
}

func TestRangeProxyServesSeekableHTTPRanges(t *testing.T) {
	payload := []byte("proxy payload for ffmpeg")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		value := strings.TrimPrefix(request.Header.Get("Range"), "bytes=")
		bounds := strings.Split(value, "-")
		if len(bounds) != 2 {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		start, _ := strconv.Atoi(bounds[0])
		end, _ := strconv.Atoi(bounds[1])
		response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		response.WriteHeader(http.StatusPartialContent)
		_, _ = response.Write(payload[start : end+1])
	}))
	defer server.Close()

	reader := NewHTTPRangeReader(server.URL, int64(len(payload)), server.Client())
	reader.ChunkSize = 4
	proxy, err := NewRangeProxy(reader)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	request, err := http.NewRequest(http.MethodGet, proxy.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=2-8")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusPartialContent || string(data) != string(payload[2:9]) {
		t.Fatalf("proxy response = %d %q", response.StatusCode, data)
	}
	if got := response.Header.Get("Content-Range"); got != "bytes 2-8/24" {
		t.Fatalf("content range = %q", got)
	}
}

func TestParseHTTPRange(t *testing.T) {
	tests := []struct {
		value      string
		start, end int64
		partial    bool
		wantError  bool
	}{
		{value: "", partial: false},
		{value: "bytes=2-5", start: 2, end: 5, partial: true},
		{value: "bytes=-4", start: 6, end: 9, partial: true},
		{value: "bytes=8-", start: 8, end: 9, partial: true},
		{value: "bytes=10-11", wantError: true},
	}
	for _, test := range tests {
		start, end, partial, err := parseHTTPRange(test.value, 10)
		if test.wantError {
			if err == nil {
				t.Fatalf("parseHTTPRange(%q) should fail", test.value)
			}
			continue
		}
		if err != nil || start != test.start || end != test.end || partial != test.partial {
			t.Fatalf("parseHTTPRange(%q) = %d-%d partial=%t err=%v", test.value, start, end, partial, err)
		}
	}
}

func TestRangeReaderRefreshesExpiredSignedURL(t *testing.T) {
	payload := []byte("fresh signed-url payload")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("token") != "fresh" {
			response.WriteHeader(http.StatusForbidden)
			return
		}
		start, end := 0, len(payload)-1
		if _, err := fmt.Sscanf(request.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		response.WriteHeader(http.StatusPartialContent)
		_, _ = response.Write(payload[start : end+1])
	}))
	defer server.Close()

	reader := NewHTTPRangeReader(server.URL+"?token=expired", int64(len(payload)), server.Client())
	reader.ChunkSize = int64(len(payload))
	var refreshes int
	reader.SetURLRefresher(func(context.Context) (string, error) {
		refreshes++
		return server.URL + "?token=fresh", nil
	})
	got := make([]byte, len(payload))
	if n, err := reader.ReadAt(got, 0); err != nil || n != len(payload) || string(got) != string(payload) {
		t.Fatalf("read after URL refresh = %d, %v, %q", n, err, got)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
}

func TestRangeReaderRetriesTransientForbiddenAfterRefresh(t *testing.T) {
	payload := []byte("transient 403 payload")
	var mu sync.Mutex
	freshRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("token") != "fresh" {
			response.WriteHeader(http.StatusForbidden)
			return
		}
		mu.Lock()
		freshRequests++
		attempt := freshRequests
		mu.Unlock()
		if attempt <= 2 {
			response.WriteHeader(http.StatusForbidden)
			return
		}
		var start, end int
		if _, err := fmt.Sscanf(request.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		response.WriteHeader(http.StatusPartialContent)
		_, _ = response.Write(payload[start : end+1])
	}))
	defer server.Close()

	reader := NewHTTPRangeReader(server.URL+"?token=expired", int64(len(payload)), server.Client())
	reader.MaxRetries = 3
	refreshes := 0
	reader.SetURLRefresher(func(context.Context) (string, error) {
		refreshes++
		return server.URL + "?token=fresh", nil
	})
	data, _, err := reader.fetchBytes(context.Background(), 0, int64(len(payload)-1))
	if err != nil {
		t.Fatalf("transient 403 should be retried: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("payload = %q, want %q", data, payload)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
}
