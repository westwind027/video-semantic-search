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
	"time"

	"video-semantic-search/internal/alipan"
	"video-semantic-search/internal/embedding"
	"video-semantic-search/internal/model"
	"video-semantic-search/internal/search"
	"video-semantic-search/internal/store"
)

type testEmbedder struct{}

func (testEmbedder) EmbedText(context.Context, []string, string) (embedding.Result, error) {
	return embedding.Result{Vectors: [][]float32{{1, 0}}, Model: "test", Dimension: 2, Modality: "text"}, nil
}

func TestServerServesFrameForGetAndHead(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	embedder := testEmbedder{}
	engine := search.NewEngine(index, embedder, false)
	media := model.Media{MediaID: "frame-demo", Title: "Frame demo", Scenes: []model.Scene{{Start: 0, End: 1, Preview: "/v1/media/frame-demo/frames/frame.jpg"}}}
	if err := index.UpsertMedia(media, [][]float32{{1, 0}}, "test"); err != nil {
		t.Fatal(err)
	}
	frameRoot := t.TempDir()
	frameDir := filepath.Join(frameRoot, "frame-demo")
	if err := os.MkdirAll(frameDir, 0o755); err != nil {
		t.Fatal(err)
	}
	framePath := filepath.Join(frameDir, "frame.jpg")
	if err := os.WriteFile(framePath, []byte("fake-jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := NewServerWithAcquisition(engine, index, embedder, nil, frameRoot).Handler()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		request := httptest.NewRequest(method, "/v1/media/frame-demo/frames/frame.jpg", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s frame status = %d, body=%s", method, response.Code, response.Body.String())
		}
	}
}

func (testEmbedder) EmbedImages(context.Context, []string) (embedding.Result, error) {
	return embedding.Result{Vectors: [][]float32{{1, 0}}, Model: "test", Dimension: 2, Modality: "image"}, nil
}

func (testEmbedder) Health(context.Context) (embedding.Health, error) {
	return embedding.Health{Status: "ok", Model: "test", Dimension: 2}, nil
}

func TestServerIngestSearchAndGet(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	embedder := testEmbedder{}
	engine := search.NewEngine(index, embedder, false)
	server := NewServer(engine, index, embedder)
	handler := server.Handler()

	body := `{"media_id":"demo","title":"雨夜追车","scenes":[{"start":1,"end":5,"caption":"一个男人站在雨中"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/media", strings.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("ingest status = %d, body=%s", response.Code, response.Body.String())
	}
	var ingest model.IngestResponse
	if err := json.Unmarshal(response.Body.Bytes(), &ingest); err != nil {
		t.Fatal(err)
	}
	if ingest.MediaID != "demo" || ingest.SceneCount != 1 {
		t.Fatalf("ingest response = %+v", ingest)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"query":"雨中"}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"media_id":"demo"`) {
		t.Fatalf("search response = %d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/media/demo", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "一个男人站在雨中") {
		t.Fatalf("get response = %d %s", response.Code, response.Body.String())
	}
}

func TestServerDeletesSceneAndFrame(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	frameRoot := t.TempDir()
	frameDir := filepath.Join(frameRoot, "managed")
	if err := os.MkdirAll(frameDir, 0o755); err != nil {
		t.Fatal(err)
	}
	framePath := filepath.Join(frameDir, "frame.jpg")
	if err := os.WriteFile(framePath, []byte("fake-jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	media := model.Media{MediaID: "managed", Title: "Managed", Scenes: []model.Scene{{SceneID: "scene-1", Start: 0, End: 1, Preview: "/v1/media/managed/frames/frame.jpg", PreviewPath: framePath}}}
	if err := index.UpsertMedia(media, [][]float32{{1, 0}}, "test"); err != nil {
		t.Fatal(err)
	}
	handler := NewServerWithAcquisition(search.NewEngine(index, testEmbedder{}, false), index, testEmbedder{}, nil, frameRoot).Handler()
	request := httptest.NewRequest(http.MethodDelete, "/v1/media/managed/scenes/scene-1", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete scene status = %d, body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(framePath); !os.IsNotExist(err) {
		t.Fatalf("frame still exists after delete, err=%v", err)
	}
	managed, ok := index.GetMedia("managed")
	if !ok || len(managed.Scenes) != 0 {
		t.Fatalf("managed media after scene delete = %+v, exists=%v", managed, ok)
	}
}

func TestServerDeletesMediaAndFrameDirectory(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	frameRoot := t.TempDir()
	frameDir := filepath.Join(frameRoot, "whole-media")
	if err := os.MkdirAll(frameDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frameDir, "frame.jpg"), []byte("fake-jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	media := model.Media{MediaID: "whole-media", Title: "Whole media", Scenes: []model.Scene{{SceneID: "scene-1", Start: 0, End: 1}}}
	if err := index.UpsertMedia(media, [][]float32{{1, 0}}, "test"); err != nil {
		t.Fatal(err)
	}
	handler := NewServerWithAcquisition(search.NewEngine(index, testEmbedder{}, false), index, testEmbedder{}, nil, frameRoot).Handler()
	request := httptest.NewRequest(http.MethodDelete, "/v1/media/whole-media", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete media status = %d, body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(frameDir); !os.IsNotExist(err) {
		t.Fatalf("frame directory still exists after media delete, err=%v", err)
	}
}

func TestServerBatchDeletesMediaAndFrameDirectories(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	frameRoot := t.TempDir()
	mediaIDs := []string{"batch-one", "batch-two"}
	for _, mediaID := range mediaIDs {
		frameDir := filepath.Join(frameRoot, mediaID)
		if err := os.MkdirAll(frameDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(frameDir, "frame.jpg"), []byte("fake-jpeg"), 0o600); err != nil {
			t.Fatal(err)
		}
		media := model.Media{MediaID: mediaID, Title: mediaID, Scenes: []model.Scene{{SceneID: mediaID + "-scene", Start: 0, End: 1}}}
		if err := index.UpsertMedia(media, [][]float32{{1, 0}}, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if err := index.UpsertMedia(model.Media{MediaID: "keep", Title: "Keep", Scenes: []model.Scene{{SceneID: "keep-scene", Start: 0, End: 1}}, Metadata: map[string]any{}}, [][]float32{{1, 0}}, "test"); err != nil {
		t.Fatal(err)
	}

	handler := NewServerWithAcquisition(search.NewEngine(index, testEmbedder{}, false), index, testEmbedder{}, nil, frameRoot).Handler()
	payload, err := json.Marshal(map[string]any{"media_ids": []string{mediaIDs[0], mediaIDs[1], mediaIDs[0], "missing"}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/media/batch/delete", strings.NewReader(string(payload)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("batch delete status = %d, body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Deleted  []string `json:"deleted"`
		Failures []struct {
			MediaID string `json:"media_id"`
			Error   string `json:"error"`
		} `json:"failures"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != len(mediaIDs) || len(result.Failures) != 1 || result.Failures[0].MediaID != "missing" {
		t.Fatalf("batch delete result = %+v", result)
	}
	for _, mediaID := range mediaIDs {
		if _, err := os.Stat(filepath.Join(frameRoot, mediaID)); !os.IsNotExist(err) {
			t.Fatalf("frame directory for %s still exists, err=%v", mediaID, err)
		}
		get := httptest.NewRecorder()
		handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/media/"+mediaID, nil))
		if get.Code != http.StatusNotFound {
			t.Fatalf("GET deleted media %s status = %d", mediaID, get.Code)
		}
	}
	if _, ok := index.GetMedia("keep"); !ok {
		t.Fatal("batch delete removed an unselected media")
	}
}

func TestServerScansAndValidatesVideoPaths(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	directory := t.TempDir()
	videoPath := filepath.Join(directory, "clip.mp4")
	if err := os.WriteFile(videoPath, []byte("not-a-real-video"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(search.NewEngine(index, testEmbedder{}, false), index, testEmbedder{}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/files/scan", strings.NewReader(`{"directory":"`+directory+`"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "clip.mp4") {
		t.Fatalf("scan response = %d %s", response.Code, response.Body.String())
	}
}

func TestServerInspectsLocalFileAndDirectory(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	root := t.TempDir()
	filePath := filepath.Join(root, "clip.mp4")
	if err := os.WriteFile(filePath, []byte("video bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(search.NewEngine(index, testEmbedder{}, false), index, testEmbedder{}).Handler()

	tests := []struct {
		name string
		path string
		kind string
	}{
		{name: "file", path: filePath, kind: "file"},
		{name: "directory", path: root, kind: "directory"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(fileInspectRequest{Path: test.path})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/files/inspect", strings.NewReader(string(body)))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("inspect status = %d, body=%s", response.Code, response.Body.String())
			}
			var result fileInspection
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Kind != test.kind || !result.Available {
				t.Fatalf("inspect result = %+v", result)
			}
		})
	}
}

func TestServerAlipanLoginStatusAndQRCode(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	connector := alipan.NewManager(alipan.Config{
		ClientID:      "client-id",
		ClientSecret:  "client-secret",
		RedirectURI:   "oob",
		AuthorizeURL:  alipan.DefaultAuthorizeURL,
		TokenEndpoint: alipan.DefaultTokenEndpoint,
		APIBaseURL:    alipan.DefaultAPIBaseURL,
		ConfigPath:    filepath.Join(t.TempDir(), "profile.json"),
	})
	handler := NewServerWithAcquisitionAndAliyun(search.NewEngine(index, testEmbedder{}, false), index, testEmbedder{}, nil, t.TempDir(), connector).Handler()

	request := httptest.NewRequest(http.MethodGet, "/v1/connectors/alipan/status", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "access_token") {
		t.Fatalf("status response = %d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/connectors/alipan/login/start", strings.NewReader(`{}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("login start status = %d, body=%s", response.Code, response.Body.String())
	}
	var login alipan.LoginStart
	if err := json.Unmarshal(response.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, login.QRCodeURL, nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("qr response = %d content-type=%s", response.Code, response.Header().Get("Content-Type"))
	}
}

func TestServerAlipanTickstepStatusCompletesAfterScan(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/qrcode/create"):
			_ = json.NewEncoder(response).Encode(map[string]any{"code": 0, "data": map[string]any{"tokenId": "ticket-api", "tokenUrl": "https://example.test/?tokenId=ticket-api"}})
		case strings.HasSuffix(request.URL.Path, "/qrcode/result"):
			_ = json.NewEncoder(response).Encode(map[string]any{"code": 0, "data": map[string]any{"qrCodeStatus": "CONFIRMED"}})
		case strings.HasSuffix(request.URL.Path, "/common/ticket-api/login"):
			_ = json.NewEncoder(response).Encode(map[string]any{"code": 0, "data": map[string]any{"openapi": map[string]any{"accessToken": "api-access", "expired": time.Now().Add(time.Hour).Unix()}}})
		case request.URL.Path == "/adrive/v1.0/user/getDriveInfo":
			_ = json.NewEncoder(response).Encode(map[string]any{"user_id": "api-user", "name": "页面用户", "default_drive_id": "drive-api"})
		default:
			http.NotFound(response, request)
		}
	}))
	defer broker.Close()

	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	connector := alipan.NewManager(alipan.Config{
		LoginMode:         "tickstep",
		TickstepBrokerURL: broker.URL,
		APIBaseURL:        broker.URL,
		ConfigPath:        filepath.Join(t.TempDir(), "profile.json"),
	})
	handler := NewServerWithAcquisitionAndAliyun(search.NewEngine(index, testEmbedder{}, false), index, testEmbedder{}, nil, t.TempDir(), connector).Handler()

	request := httptest.NewRequest(http.MethodPost, "/v1/connectors/alipan/login/start", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("tickstep start status = %d, body=%s", response.Code, response.Body.String())
	}
	var login alipan.LoginStart
	if err := json.Unmarshal(response.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}

	request = httptest.NewRequest(http.MethodGet, login.QRCodeURL[:len(login.QRCodeURL)-len("/qr")]+"/status", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state":"completed"`) {
		t.Fatalf("tickstep status = %d, body=%s", response.Code, response.Body.String())
	}
}
