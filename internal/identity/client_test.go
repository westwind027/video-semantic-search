package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPythonClientEmbedBatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/faces/embed-batch" {
			http.NotFound(response, request)
			return
		}
		var payload struct {
			Images []string `json:"images"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		results := make([]EmbedResult, len(payload.Images))
		for index := range results {
			results[index] = EmbedResult{Model: "test-face", Dimension: 2, Faces: []Face{{DetScore: .9, Embedding: []float32{1, 0}}}}
		}
		_ = json.NewEncoder(response).Encode(map[string]any{"results": results})
	}))
	defer server.Close()

	client := NewPythonClient(server.URL, time.Second)
	results, err := client.EmbedBatch(context.Background(), []string{"frame-1.jpg", "frame-2.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Dimension != 2 || len(results[1].Faces) != 1 {
		t.Fatalf("batch results = %+v", results)
	}
}

func TestPythonClientEmbedBatchFallsBackToSingleEndpoint(t *testing.T) {
	var singleCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/faces/embed-batch" {
			http.NotFound(response, request)
			return
		}
		if request.URL.Path != "/v1/faces/embed" {
			http.NotFound(response, request)
			return
		}
		singleCalls.Add(1)
		_ = json.NewEncoder(response).Encode(EmbedResult{Model: "test-face", Dimension: 2, Faces: []Face{{DetScore: .9, Embedding: []float32{1, 0}}}})
	}))
	defer server.Close()

	client := NewPythonClient(server.URL, time.Second)
	results, err := client.EmbedBatch(context.Background(), []string{"frame-1.jpg", "frame-2.jpg", "frame-3.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || singleCalls.Load() != 3 {
		t.Fatalf("fallback results=%d single_calls=%d", len(results), singleCalls.Load())
	}
}
