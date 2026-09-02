package embedding

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPythonClientCallsTextAndHealthEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/healthz":
			_, _ = response.Write([]byte(`{"status":"ok","model":"fake","dimension":2,"modalities":["text"]}`))
		case "/v1/embeddings":
			_, _ = response.Write([]byte(`{"embeddings":[[1,0]],"model":"fake","dimension":2,"modality":"text"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client := NewPythonClient(server.URL, time.Second)
	health, err := client.Health(context.Background())
	if err != nil || health.Model != "fake" {
		t.Fatalf("health = %+v, err=%v", health, err)
	}
	result, err := client.EmbedText(context.Background(), []string{"test"}, "query")
	if err != nil || result.Dimension != 2 || len(result.Vectors) != 1 {
		t.Fatalf("result = %+v, err=%v", result, err)
	}
}
