package source

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRangeReaderFetchesAlignedChunksAndSharesCache(t *testing.T) {
	payload := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		value := request.Header.Get("Range")
		if value == "" {
			t.Error("range header is missing")
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		var start, end int
		if _, err := fmt.Sscanf(value, "bytes=%d-%d", &start, &end); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		if start < 0 || end >= len(payload) || start > end {
			response.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		atomic.AddInt32(&requests, 1)
		response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		response.WriteHeader(http.StatusPartialContent)
		_, _ = response.Write(payload[start : end+1])
	}))
	defer server.Close()

	reader := NewHTTPRangeReader(server.URL, int64(len(payload)), server.Client())
	reader.ChunkSize = 8
	buffer := make([]byte, 12)
	n, err := reader.ReadAt(buffer, 5)
	if err != nil || n != len(buffer) || string(buffer) != string(payload[5:17]) {
		t.Fatalf("first read = %d bytes, %v, %q", n, err, buffer)
	}
	n, err = reader.ReadAt(buffer[:4], 7)
	if err != nil || n != 4 || string(buffer[:4]) != string(payload[7:11]) {
		t.Fatalf("cached read = %d bytes, %v, %q", n, err, buffer[:4])
	}
	if got := atomic.LoadInt32(&requests); got != 3 {
		t.Fatalf("range requests = %d, want 3", got)
	}
	stats := reader.Stats()
	if stats.BytesDownloaded != 24 || stats.CachedChunks != 3 {
		t.Fatalf("stats = %+v", stats)
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
