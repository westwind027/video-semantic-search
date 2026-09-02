package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"video-semantic-search/internal/embedding"
	"video-semantic-search/internal/model"
	"video-semantic-search/internal/search"
	"video-semantic-search/internal/store"
)

type blockingProcessor struct {
	started chan struct{}
}

func (p *blockingProcessor) Process(ctx context.Context, _ string, _ Request, _ ProgressFunc) (model.Media, error) {
	close(p.started)
	<-ctx.Done()
	return model.Media{}, ctx.Err()
}

type managerEmbedder struct{}

type immediateProcessor struct {
	mu    sync.Mutex
	calls int
}

func (p *immediateProcessor) Process(_ context.Context, mediaID string, _ Request, _ ProgressFunc) (model.Media, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	duration := 1.0
	return model.Media{MediaID: mediaID, Type: "movie", Title: "dedupe", Duration: &duration, Scenes: []model.Scene{{Start: 0, End: 1}}}, nil
}

func (p *immediateProcessor) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (managerEmbedder) EmbedText(context.Context, []string, string) (embedding.Result, error) {
	return embedding.Result{Vectors: [][]float32{{1}}, Model: "test", Dimension: 1}, nil
}

func (managerEmbedder) EmbedImages(context.Context, []string) (embedding.Result, error) {
	return embedding.Result{Vectors: [][]float32{{1}}, Model: "test", Dimension: 1}, nil
}

func (managerEmbedder) Health(context.Context) (embedding.Health, error) {
	return embedding.Health{Status: "ok", Model: "test", Dimension: 1}, nil
}

func TestManagerStopsRunningTask(t *testing.T) {
	videoPath := filepath.Join(t.TempDir(), "movie.mp4")
	if err := os.WriteFile(videoPath, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	processor := &blockingProcessor{started: make(chan struct{})}
	manager := NewManager(processor, search.NewEngine(index, managerEmbedder{}, false))
	task, err := manager.Submit(Request{LocalPath: videoPath})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("processor did not start")
	}
	if _, ok := manager.Stop(task.ID); !ok {
		t.Fatal("Stop returned false for running task")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, ok := manager.Get(task.ID)
		if ok && current.State == "canceled" && current.Stage == StageCanceled {
			return
		}
		time.Sleep(time.Millisecond)
	}
	current, _ := manager.Get(task.ID)
	if current.State != "canceled" {
		t.Fatalf("task state = %s, want canceled", current.State)
	}
}

func TestManagerPersistsTaskStateAcrossReload(t *testing.T) {
	videoPath := filepath.Join(t.TempDir(), "movie.mp4")
	if err := os.WriteFile(videoPath, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	taskFile := filepath.Join(t.TempDir(), "tasks.json")
	processor := &blockingProcessor{started: make(chan struct{})}
	engine := search.NewEngine(index, managerEmbedder{}, false)
	manager := NewManagerWithTaskFile(processor, engine, taskFile)
	task, err := manager.Submit(Request{LocalPath: videoPath})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("processor did not start")
	}
	if _, ok := manager.Stop(task.ID); !ok {
		t.Fatal("Stop returned false for running task")
	}

	reloaded := NewManagerWithTaskFile(processor, engine, taskFile)
	current, ok := reloaded.Get(task.ID)
	if !ok {
		t.Fatal("persisted task was not reloaded")
	}
	if current.State != "canceled" || current.Stage != StageCanceled {
		t.Fatalf("reloaded task = %+v", current)
	}
}

func TestManagerDeduplicatesSameContentAcrossPaths(t *testing.T) {
	directory := t.TempDir()
	firstPath := filepath.Join(directory, "first.mp4")
	secondPath := filepath.Join(directory, "renamed.mp4")
	content := []byte("the same video bytes")
	if err := os.WriteFile(firstPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := store.NewFileStore(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	processor := &immediateProcessor{}
	manager := NewManager(processor, search.NewEngine(index, managerEmbedder{}, false))
	first, err := manager.Submit(Request{LocalPath: firstPath})
	if err != nil {
		t.Fatal(err)
	}
	waitForManagerState(t, manager, first.ID, "completed")
	second, err := manager.Submit(Request{LocalPath: secondPath})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.State != "completed" {
		t.Fatalf("duplicate task = %+v, original = %+v", second, first)
	}
	if calls := processor.Calls(); calls != 1 {
		t.Fatalf("processor calls = %d, want 1", calls)
	}
}

func TestManagerDeduplicatesRemoteFileByFingerprint(t *testing.T) {
	directory := t.TempDir()
	index, err := store.NewFileStore(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	processor := &immediateProcessor{}
	manager := NewManager(processor, search.NewEngine(index, managerEmbedder{}, false))
	request := Request{Source: "alipan", DriveID: "drive-1", FileID: "file-1", SourceName: "云盘视频.mp4", SourceFingerprint: "remote-content-hash"}
	first, err := manager.Submit(request)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentFingerprint != "remote-content-hash" || first.ContentSHA256 != "" {
		t.Fatalf("remote task fingerprint fields = %+v", first)
	}
	waitForManagerState(t, manager, first.ID, "completed")
	second, err := manager.Submit(request)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.State != "completed" {
		t.Fatalf("remote duplicate task = %+v, original = %+v", second, first)
	}
	if calls := processor.Calls(); calls != 1 {
		t.Fatalf("processor calls = %d, want 1", calls)
	}
}

func TestManagerReprocessesChangedContentAtSamePath(t *testing.T) {
	videoPath := filepath.Join(t.TempDir(), "movie.mp4")
	if err := os.WriteFile(videoPath, []byte("version one"), 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	processor := &immediateProcessor{}
	manager := NewManager(processor, search.NewEngine(index, managerEmbedder{}, false))
	first, err := manager.Submit(Request{LocalPath: videoPath})
	if err != nil {
		t.Fatal(err)
	}
	waitForManagerState(t, manager, first.ID, "completed")
	if err := os.WriteFile(videoPath, []byte("version two"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Submit(Request{LocalPath: videoPath})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("changed file reused task %s", first.ID)
	}
	waitForManagerState(t, manager, second.ID, "completed")
	if calls := processor.Calls(); calls != 2 {
		t.Fatalf("processor calls = %d, want 2", calls)
	}
}

func TestManagerSkipsLegacyIndexedFileAtSamePath(t *testing.T) {
	videoPath := filepath.Join(t.TempDir(), "legacy.mp4")
	if err := os.WriteFile(videoPath, []byte("legacy video"), 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if err := index.UpsertMedia(model.Media{
		MediaID: "legacy-media",
		Type:    "movie",
		Title:   "legacy",
		Metadata: map[string]any{
			"local_path": videoPath,
		},
		Scenes: []model.Scene{{Start: 0, End: 1}},
	}, [][]float32{{1}}, "legacy-model"); err != nil {
		t.Fatal(err)
	}
	processor := &immediateProcessor{}
	manager := NewManager(processor, search.NewEngine(index, managerEmbedder{}, false))
	task, err := manager.Submit(Request{LocalPath: videoPath})
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "legacy-media" || task.State != "completed" {
		t.Fatalf("legacy duplicate task = %+v", task)
	}
	if calls := processor.Calls(); calls != 0 {
		t.Fatalf("processor calls = %d, want 0", calls)
	}
}

func waitForManagerState(t *testing.T, manager *Manager, taskID, state string) Task {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if task, ok := manager.Get(taskID); ok && task.State == state {
			return task
		}
		time.Sleep(time.Millisecond)
	}
	task, _ := manager.Get(taskID)
	t.Fatalf("task = %+v, want state %s", task, state)
	return Task{}
}
