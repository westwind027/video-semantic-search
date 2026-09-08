package acquisition

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"video-semantic-search/internal/embedding"
	"video-semantic-search/internal/identity"
	"video-semantic-search/internal/model"
	"video-semantic-search/internal/search"
	"video-semantic-search/internal/store"
)

type blockingProcessor struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
}

func (p *blockingProcessor) Process(ctx context.Context, _ string, _ Request, _ ProgressFunc) (model.Media, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	close(p.started)
	<-ctx.Done()
	return model.Media{}, ctx.Err()
}

func (p *blockingProcessor) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// concurrentProcessor tracks the peak number of simultaneously running
// Process calls, which is how a worker pool's width is measured.
type concurrentProcessor struct {
	mu       sync.Mutex
	inFlight int
	peak     int
}

func (p *concurrentProcessor) Process(ctx context.Context, mediaID string, _ Request, _ ProgressFunc) (model.Media, error) {
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > p.peak {
		p.peak = p.inFlight
	}
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return model.Media{}, ctx.Err()
	case <-time.After(30 * time.Millisecond):
	}
	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
	duration := 1.0
	return model.Media{MediaID: mediaID, Type: "movie", Title: "queued", Duration: &duration, Scenes: []model.Scene{{Start: 0, End: 1}}}, nil
}

func (p *concurrentProcessor) Peak() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak
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

// profileProcessor mimics VideoProcessor's frame_source metadata so the
// extraction-profile dedup logic has something to compare against.
type profileProcessor struct {
	mu       sync.Mutex
	calls    int
	mediaIDs []string
}

func (p *profileProcessor) Process(_ context.Context, mediaID string, request Request, _ ProgressFunc) (model.Media, error) {
	p.mu.Lock()
	p.calls++
	p.mediaIDs = append(p.mediaIDs, mediaID)
	p.mu.Unlock()
	duration := 1.0
	return model.Media{
		MediaID: mediaID, Type: "movie", Title: "profile", Duration: &duration,
		Scenes:   []model.Scene{{Start: 0, End: 1}},
		Metadata: map[string]any{"frame_source": desiredFrameSource(request, false)},
	}, nil
}

func (p *profileProcessor) Calls() int {
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

type progressIdentityTagger struct {
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
}

func (t *progressIdentityTagger) TagMedia(_ context.Context, media model.Media) (model.Media, error) {
	return media, nil
}

func (t *progressIdentityTagger) RebuildMedia(ctx context.Context, media model.Media) (model.Media, error) {
	return t.wait(ctx, media)
}

func (t *progressIdentityTagger) RebuildMediaWithProgress(ctx context.Context, media model.Media, progress identity.ProgressFunc) (model.Media, error) {
	t.startedOnce.Do(func() { close(t.started) })
	progress(1, len(media.Scenes))
	return t.wait(ctx, media)
}

func (t *progressIdentityTagger) wait(ctx context.Context, media model.Media) (model.Media, error) {
	t.startedOnce.Do(func() { close(t.started) })
	select {
	case <-t.release:
		return media, nil
	case <-ctx.Done():
		return model.Media{}, ctx.Err()
	}
}

var _ identity.Tagger = (*progressIdentityTagger)(nil)

func TestManagerReportsIdentityRebuildProgress(t *testing.T) {
	directory := t.TempDir()
	index, err := store.NewFileStore(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	engine := search.NewEngine(index, managerEmbedder{}, false)
	media := model.Media{MediaID: "identity-progress-media", Title: "Identity progress", Scenes: []model.Scene{{Start: 0, End: 1}}}
	if _, err := engine.Index(context.Background(), media); err != nil {
		t.Fatal(err)
	}
	tagger := &progressIdentityTagger{started: make(chan struct{}), release: make(chan struct{})}
	manager := NewManager(&immediateProcessor{}, engine)
	manager.SetIdentityTagger(tagger)
	task, err := manager.RebuildPersons(media.MediaID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-tagger.started:
	case <-time.After(time.Second):
		t.Fatal("identity rebuild did not start")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, ok := manager.Get(task.ID)
		if ok && current.State == "running" && current.Percent > 0.02 && current.Percent < 1 {
			close(tagger.release)
			waitForManagerState(t, manager, task.ID, "completed")
			return
		}
		time.Sleep(time.Millisecond)
	}
	close(tagger.release)
	current, _ := manager.Get(task.ID)
	t.Fatalf("identity rebuild progress = %+v, want an intermediate percent above 0.02", current)
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
	// The content fingerprint of a local file is computed by the worker, so
	// the duplicate is accepted as a queued task first and only folded into
	// the completed media once the worker has hashed its content.
	second, err := manager.Submit(Request{LocalPath: secondPath})
	if err != nil {
		t.Fatal(err)
	}
	waitForManagerState(t, manager, second.ID, "completed")
	current, _ := manager.Get(second.ID)
	if current.MediaID != first.ID || current.State != "completed" {
		t.Fatalf("duplicate task = %+v, original = %+v", current, first)
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

func TestManagerReprocessesWhenExtractionProfileChanges(t *testing.T) {
	videoPath := filepath.Join(t.TempDir(), "movie.mp4")
	if err := os.WriteFile(videoPath, []byte("same content"), 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	processor := &profileProcessor{}
	manager := NewManager(processor, search.NewEngine(index, managerEmbedder{}, false))
	first, err := manager.Submit(Request{LocalPath: videoPath, FastMode: true})
	if err != nil {
		t.Fatal(err)
	}
	waitForManagerState(t, manager, first.ID, "completed")
	// Re-submitting the identical file with the other extraction profile must
	// not be folded into deduplication: a new task reprocesses the video and
	// reuses the original media ID so the index is replaced, not duplicated.
	second, err := manager.Submit(Request{LocalPath: videoPath, FastMode: false})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("profile change reused task %s", first.ID)
	}
	waitForManagerState(t, manager, second.ID, "completed")
	current, _ := manager.Get(second.ID)
	if current.MediaID != first.ID {
		t.Fatalf("reprocessed task media id = %s, want %s", current.MediaID, first.ID)
	}
	if calls := processor.Calls(); calls != 2 {
		t.Fatalf("processor calls = %d, want 2", calls)
	}
	processor.mu.Lock()
	secondMediaID := processor.mediaIDs[1]
	processor.mu.Unlock()
	if secondMediaID != first.ID {
		t.Fatalf("processor media id = %s, want %s", secondMediaID, first.ID)
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

func TestManagerRunsTasksWithLimitedWorkers(t *testing.T) {
	t.Setenv("VIDEO_TASK_WORKERS", "1")
	directory := t.TempDir()
	index, err := store.NewFileStore(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	processor := &concurrentProcessor{}
	manager := NewManager(processor, search.NewEngine(index, managerEmbedder{}, false))
	var tasks []Task
	for i := 0; i < 4; i++ {
		path := filepath.Join(directory, fmt.Sprintf("movie-%d.mp4", i))
		if err := os.WriteFile(path, []byte(fmt.Sprintf("content %d", i)), 0o600); err != nil {
			t.Fatal(err)
		}
		task, err := manager.Submit(Request{LocalPath: path})
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	for _, task := range tasks {
		waitForManagerState(t, manager, task.ID, "completed")
	}
	if peak := processor.Peak(); peak != 1 {
		t.Fatalf("peak concurrent processor calls = %d, want 1", peak)
	}
}

func TestManagerStopCancelsQueuedTask(t *testing.T) {
	t.Setenv("VIDEO_TASK_WORKERS", "1")
	directory := t.TempDir()
	index, err := store.NewFileStore(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	firstPath := filepath.Join(directory, "first.mp4")
	secondPath := filepath.Join(directory, "second.mp4")
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	processor := &blockingProcessor{started: make(chan struct{})}
	manager := NewManager(processor, search.NewEngine(index, managerEmbedder{}, false))
	first, err := manager.Submit(Request{LocalPath: firstPath})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("processor did not start")
	}
	second, err := manager.Submit(Request{LocalPath: secondPath})
	if err != nil {
		t.Fatal(err)
	}
	if stopped := manager.StopMany([]string{second.ID, second.ID}); len(stopped) != 1 || stopped[0] != second.ID {
		t.Fatalf("StopMany returned %v for queued task", stopped)
	}
	current, ok := manager.Get(second.ID)
	if !ok || current.State != "canceled" {
		t.Fatalf("queued task = %+v, want canceled", current)
	}
	if stopped := manager.StopMany([]string{first.ID}); len(stopped) != 1 || stopped[0] != first.ID {
		t.Fatalf("StopMany returned %v for running task", stopped)
	}
	waitForManagerState(t, manager, first.ID, "canceled")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if processor.Calls() == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("processor calls = %d, want 1: the canceled queued task must never reach the processor", processor.Calls())
}

func TestManagerRemoveAndClearTerminalTasks(t *testing.T) {
	t.Setenv("VIDEO_TASK_WORKERS", "1")
	directory := t.TempDir()
	index, err := store.NewFileStore(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	var paths []string
	for i := 0; i < 3; i++ {
		path := filepath.Join(directory, fmt.Sprintf("movie-%d.mp4", i))
		if err := os.WriteFile(path, []byte(fmt.Sprintf("content %d", i)), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	processor := &concurrentProcessor{}
	manager := NewManager(processor, search.NewEngine(index, managerEmbedder{}, false))
	first, err := manager.Submit(Request{LocalPath: paths[0]})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Submit(Request{LocalPath: paths[1]})
	if err != nil {
		t.Fatal(err)
	}
	blocking := &blockingProcessor{started: make(chan struct{})}
	blockingManager := NewManager(blocking, search.NewEngine(index, managerEmbedder{}, false))
	running, err := blockingManager.Submit(Request{LocalPath: paths[2]})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("processor did not start")
	}
	waitForManagerState(t, manager, first.ID, "completed")
	waitForManagerState(t, manager, second.ID, "completed")
	if manager.Remove(running.ID) {
		t.Fatal("Remove accepted a running task")
	}
	if removed := manager.RemoveMany([]string{first.ID, first.ID}); len(removed) != 1 || removed[0] != first.ID {
		t.Fatalf("RemoveMany rejected a completed task: %v", removed)
	}
	if _, exists := manager.Get(first.ID); exists {
		t.Fatal("removed task is still listed")
	}
	if removed := manager.Clear([]string{"queued", "running", "bogus"}); removed != 0 {
		t.Fatalf("Clear removed %d tasks for non-terminal states", removed)
	}
	if removed := manager.Clear([]string{"completed"}); removed != 1 {
		t.Fatalf("Clear removed %d completed tasks, want 1", removed)
	}
	if _, exists := manager.Get(second.ID); exists {
		t.Fatal("cleared task is still listed")
	}
	if _, ok := blockingManager.Stop(running.ID); !ok {
		t.Fatal("Stop returned false for running task")
	}
	waitForManagerState(t, blockingManager, running.ID, "canceled")
	if removed := manager.Clear([]string{"failed", "canceled"}); removed != 0 {
		t.Fatalf("Clear removed %d tasks from a different manager", removed)
	}
	if removed := blockingManager.Clear([]string{"failed", "canceled"}); removed != 1 {
		t.Fatalf("Clear removed %d canceled tasks, want 1", removed)
	}
}

func TestManagerRebuildsEmbeddingsWithoutReprocessingVideo(t *testing.T) {
	directory := t.TempDir()
	index, err := store.NewFileStore(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	engine := search.NewEngine(index, managerEmbedder{}, false)
	media := model.Media{MediaID: "rebuild-media", Title: "Rebuild", Scenes: []model.Scene{{Start: 0, End: 1, PreviewPath: filepath.Join(directory, "frame.jpg")}}}
	if _, err := engine.Index(context.Background(), media); err != nil {
		t.Fatal(err)
	}
	processor := &immediateProcessor{}
	manager := NewManager(processor, engine)
	task, err := manager.RebuildEmbeddings(media.MediaID, embedding.ImageProfileCompressed)
	if err != nil {
		t.Fatal(err)
	}
	if task.Operation != "embedding_rebuild" || task.EmbeddingProfile != "compressed" {
		t.Fatalf("rebuild task = %+v", task)
	}
	completed := waitForManagerState(t, manager, task.ID, "completed")
	if completed.EmbeddingProfile != "compressed" || processor.Calls() != 0 {
		t.Fatalf("completed task = %+v, processor calls = %d", completed, processor.Calls())
	}
	stored, ok := index.GetMedia(media.MediaID)
	if !ok || stored.Metadata["embedding_profile"] != "compressed" {
		t.Fatalf("stored media metadata = %+v", stored.Metadata)
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
