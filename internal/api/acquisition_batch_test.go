package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"video-semantic-search/internal/acquisition"
	"video-semantic-search/internal/model"
	"video-semantic-search/internal/search"
	"video-semantic-search/internal/store"
)

type immediateMediaProcessor struct{}

func (immediateMediaProcessor) Process(_ context.Context, mediaID string, _ acquisition.Request, _ acquisition.ProgressFunc) (model.Media, error) {
	duration := 1.0
	return model.Media{MediaID: mediaID, Type: "movie", Title: "batch", Duration: &duration, Scenes: []model.Scene{{Start: 0, End: 1}}}, nil
}

type apiBlockingMediaProcessor struct {
	started chan struct{}
}

func (processor *apiBlockingMediaProcessor) Process(ctx context.Context, mediaID string, _ acquisition.Request, _ acquisition.ProgressFunc) (model.Media, error) {
	close(processor.started)
	<-ctx.Done()
	return model.Media{MediaID: mediaID}, ctx.Err()
}

func newAcquisitionTestServer(t *testing.T) (http.Handler, *acquisition.Manager, string) {
	t.Helper()
	directory := t.TempDir()
	index, err := store.NewFileStore(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { index.Close() })
	manager := acquisition.NewManager(immediateMediaProcessor{}, search.NewEngine(index, testEmbedder{}, false))
	handler := NewServerWithAcquisition(search.NewEngine(index, testEmbedder{}, false), index, testEmbedder{}, manager, filepath.Join(directory, "frames")).Handler()
	return handler, manager, directory
}

func waitForTaskState(t *testing.T, handler http.Handler, taskID, state string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var body map[string]any
	for time.Now().Before(deadline) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/acquisitions/"+taskID, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET task status = %d, body=%s", response.Code, response.Body.String())
		}
		body = map[string]any{}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["state"] == state {
			return body
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s never reached state %q, last = %v", taskID, state, body)
	return nil
}

func TestAcquisitionBatchCreatesEveryTaskInOneRequest(t *testing.T) {
	handler, _, directory := newAcquisitionTestServer(t)
	first := filepath.Join(directory, "first.mp4")
	second := filepath.Join(directory, "second.mp4")
	for index, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte(fmt.Sprintf("content %d", index)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	payload := fmt.Sprintf(`{"items":[{"local_path":%q},{"local_path":%q},{"local_path":%q}]}`, first, second, filepath.Join(directory, "missing.mp4"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/acquisitions/batch", strings.NewReader(payload)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("batch status = %d, body=%s", response.Code, response.Body.String())
	}
	var batch struct {
		Tasks []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"tasks"`
		Failures []struct {
			Index int    `json:"index"`
			Error string `json:"error"`
		} `json:"failures"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Tasks) != 2 {
		t.Fatalf("batch returned %d tasks, want 2: %s", len(batch.Tasks), response.Body.String())
	}
	if len(batch.Failures) != 1 || batch.Failures[0].Index != 2 {
		t.Fatalf("batch failures = %+v, want one failure at index 2", batch.Failures)
	}
	for _, task := range batch.Tasks {
		current := waitForTaskState(t, handler, task.ID, "completed")
		if current["media_id"] == "" {
			t.Fatalf("completed task %s has no media_id: %v", task.ID, current)
		}
	}
}

func TestAcquisitionBatchRejectsEmptyItems(t *testing.T) {
	handler, _, _ := newAcquisitionTestServer(t)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/acquisitions/batch", strings.NewReader(`{"items":[]}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("empty batch status = %d, want 400", response.Code)
	}
}

func TestAcquisitionDeleteRemovesTerminalTask(t *testing.T) {
	handler, _, directory := newAcquisitionTestServer(t)
	path := filepath.Join(directory, "movie.mp4")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/acquisitions", strings.NewReader(fmt.Sprintf(`{"local_path":%q}`, path))))
	if response.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, body=%s", response.Code, response.Body.String())
	}
	var task struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	waitForTaskState(t, handler, task.ID, "completed")

	// A terminal task is removed from the list instead of stopped.
	deleted := httptest.NewRecorder()
	handler.ServeHTTP(deleted, httptest.NewRequest(http.MethodDelete, "/v1/acquisitions/"+task.ID, nil))
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete terminal task status = %d", deleted.Code)
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/v1/acquisitions/"+task.ID, nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("get removed task status = %d, want 404", missing.Code)
	}

	// Clearing only touches terminal states; completed tasks survive a
	// default clear.
	cleared := httptest.NewRecorder()
	handler.ServeHTTP(cleared, httptest.NewRequest(http.MethodPost, "/v1/acquisitions/clear", strings.NewReader(`{"states":["failed","canceled"]}`)))
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear status = %d", cleared.Code)
	}
}

func TestAcquisitionBatchDeleteRemovesSelectedTerminalTasks(t *testing.T) {
	handler, _, directory := newAcquisitionTestServer(t)
	paths := []string{filepath.Join(directory, "first.mp4"), filepath.Join(directory, "second.mp4")}
	// Distinct contents: identical files would be folded by local content
	// deduplication, and this test only needs two independent media records.
	for index, path := range paths {
		if err := os.WriteFile(path, []byte(fmt.Sprintf("content-%d", index)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	payload := fmt.Sprintf(`{"items":[{"local_path":%q},{"local_path":%q}]}`, paths[0], paths[1])
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/v1/acquisitions/batch", strings.NewReader(payload)))
	if created.Code != http.StatusAccepted {
		t.Fatalf("batch submit status = %d, body=%s", created.Code, created.Body.String())
	}
	var batch struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Tasks) != len(paths) {
		t.Fatalf("created tasks = %+v", batch.Tasks)
	}
	for _, task := range batch.Tasks {
		waitForTaskState(t, handler, task.ID, "completed")
	}

	ids := []string{batch.Tasks[0].ID, batch.Tasks[1].ID, batch.Tasks[0].ID}
	body, err := json.Marshal(acquisitionTaskBatchRequest{TaskIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	deleted := httptest.NewRecorder()
	handler.ServeHTTP(deleted, httptest.NewRequest(http.MethodPost, "/v1/acquisitions/batch/delete", strings.NewReader(string(body))))
	if deleted.Code != http.StatusOK {
		t.Fatalf("batch delete status = %d, body=%s", deleted.Code, deleted.Body.String())
	}
	var result acquisitionTaskBatchResponse
	if err := json.Unmarshal(deleted.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != len(paths) || len(result.Failures) != 0 {
		t.Fatalf("batch delete result = %+v", result)
	}
	for _, task := range batch.Tasks {
		missing := httptest.NewRecorder()
		handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/v1/acquisitions/"+task.ID, nil))
		if missing.Code != http.StatusNotFound {
			t.Fatalf("deleted task %s status = %d", task.ID, missing.Code)
		}
	}
}

func TestAcquisitionBatchStopCancelsQueuedAndRunningTasks(t *testing.T) {
	t.Setenv("VIDEO_TASK_WORKERS", "1")
	directory := t.TempDir()
	index, err := store.NewFileStore(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	paths := []string{filepath.Join(directory, "first.mp4"), filepath.Join(directory, "second.mp4")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	processor := &apiBlockingMediaProcessor{started: make(chan struct{})}
	engine := search.NewEngine(index, testEmbedder{}, false)
	manager := acquisition.NewManager(processor, engine)
	handler := NewServerWithAcquisition(engine, index, testEmbedder{}, manager, filepath.Join(directory, "frames")).Handler()

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/v1/acquisitions", strings.NewReader(fmt.Sprintf(`{"local_path":%q}`, paths[0]))))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first submit status = %d, body=%s", first.Code, first.Body.String())
	}
	var firstTask struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstTask); err != nil {
		t.Fatal(err)
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("first task did not start")
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/acquisitions", strings.NewReader(fmt.Sprintf(`{"local_path":%q}`, paths[1]))))
	if second.Code != http.StatusAccepted {
		t.Fatalf("second submit status = %d, body=%s", second.Code, second.Body.String())
	}
	var secondTask struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondTask); err != nil {
		t.Fatal(err)
	}

	stopPayload, err := json.Marshal(acquisitionTaskBatchRequest{TaskIDs: []string{firstTask.ID, secondTask.ID, secondTask.ID}})
	if err != nil {
		t.Fatal(err)
	}
	stopped := httptest.NewRecorder()
	handler.ServeHTTP(stopped, httptest.NewRequest(http.MethodPost, "/v1/acquisitions/batch/stop", strings.NewReader(string(stopPayload))))
	if stopped.Code != http.StatusOK {
		t.Fatalf("batch stop status = %d, body=%s", stopped.Code, stopped.Body.String())
	}
	var result acquisitionTaskBatchResponse
	if err := json.Unmarshal(stopped.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Stopped) != 2 || len(result.Failures) != 0 {
		t.Fatalf("batch stop result = %+v", result)
	}
	waitForTaskState(t, handler, firstTask.ID, "canceled")
	waitForTaskState(t, handler, secondTask.ID, "canceled")
}

func TestEmbeddingRebuildEndpointQueuesVectorOnlyTask(t *testing.T) {
	handler, manager, directory := newAcquisitionTestServer(t)
	path := filepath.Join(directory, "movie.mp4")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/v1/acquisitions", strings.NewReader(fmt.Sprintf(`{"local_path":%q}`, path))))
	if first.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, body=%s", first.Code, first.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	waitForTaskState(t, handler, created.ID, "completed")

	rebuild := httptest.NewRecorder()
	handler.ServeHTTP(rebuild, httptest.NewRequest(http.MethodPost, "/v1/media/"+created.ID+"/embeddings/rebuild", strings.NewReader(`{"profile":"compressed"}`)))
	if rebuild.Code != http.StatusAccepted {
		t.Fatalf("rebuild status = %d, body=%s", rebuild.Code, rebuild.Body.String())
	}
	var task struct {
		ID               string `json:"id"`
		Operation        string `json:"operation"`
		EmbeddingProfile string `json:"embedding_profile"`
	}
	if err := json.Unmarshal(rebuild.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.Operation != "embedding_rebuild" || task.EmbeddingProfile != "compressed" {
		t.Fatalf("rebuild task = %+v", task)
	}
	completed := waitForTaskState(t, handler, task.ID, "completed")
	if completed["embedding_profile"] != "compressed" {
		t.Fatalf("completed rebuild task = %v", completed)
	}
	if _, ok := manager.Get(task.ID); !ok {
		t.Fatal("rebuild task is missing from manager")
	}
}

func TestEmbeddingRebuildBatchEndpointQueuesSelectedVideos(t *testing.T) {
	handler, _, directory := newAcquisitionTestServer(t)
	paths := []string{filepath.Join(directory, "one.mp4"), filepath.Join(directory, "two.mp4")}
	for index, path := range paths {
		if err := os.WriteFile(path, []byte(fmt.Sprintf("content-%d", index)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	requestBody, err := json.Marshal(map[string]any{"items": []map[string]string{
		{"local_path": paths[0]},
		{"local_path": paths[1]},
	}})
	if err != nil {
		t.Fatal(err)
	}
	acquire := httptest.NewRecorder()
	handler.ServeHTTP(acquire, httptest.NewRequest(http.MethodPost, "/v1/acquisitions/batch", strings.NewReader(string(requestBody))))
	if acquire.Code != http.StatusAccepted {
		t.Fatalf("acquire status = %d, body=%s", acquire.Code, acquire.Body.String())
	}
	var acquired struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(acquire.Body.Bytes(), &acquired); err != nil {
		t.Fatal(err)
	}
	if len(acquired.Tasks) != len(paths) {
		t.Fatalf("acquired tasks = %v", acquired.Tasks)
	}
	ids := make([]string, 0, len(acquired.Tasks))
	for _, task := range acquired.Tasks {
		completed := waitForTaskState(t, handler, task.ID, "completed")
		mediaID, ok := completed["media_id"].(string)
		if !ok || mediaID == "" {
			t.Fatalf("completed task %s has no media_id: %v", task.ID, completed)
		}
		ids = append(ids, mediaID)
	}
	payload := fmt.Sprintf(`{"media_ids":[%q,%q],"profile":"compressed"}`, ids[0], ids[1])
	rebuild := httptest.NewRecorder()
	handler.ServeHTTP(rebuild, httptest.NewRequest(http.MethodPost, "/v1/media/embeddings/rebuild", strings.NewReader(payload)))
	if rebuild.Code != http.StatusAccepted {
		t.Fatalf("batch rebuild status = %d, body=%s", rebuild.Code, rebuild.Body.String())
	}
	var response struct {
		Tasks []struct {
			ID               string `json:"id"`
			EmbeddingProfile string `json:"embedding_profile"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rebuild.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Tasks) != len(ids) {
		t.Fatalf("rebuild tasks = %v", response.Tasks)
	}
	for _, task := range response.Tasks {
		if task.EmbeddingProfile != "compressed" {
			t.Fatalf("rebuild profile = %+v", task)
		}
		waitForTaskState(t, handler, task.ID, "completed")
	}
}
