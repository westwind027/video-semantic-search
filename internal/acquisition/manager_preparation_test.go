package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"video-semantic-search/internal/identity"
	"video-semantic-search/internal/model"
	"video-semantic-search/internal/search"
	"video-semantic-search/internal/store"
)

type blockingMoviePreparer struct {
	started  chan struct{}
	release  chan struct{}
	requests chan identity.MoviePreparationRequest
	once     sync.Once
}

type blockingFileHasher struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *blockingFileHasher) Hash(ctx context.Context, _ string) (string, error) {
	h.once.Do(func() { close(h.started) })
	select {
	case <-h.release:
		return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (p *blockingMoviePreparer) Prepare(ctx context.Context, request identity.MoviePreparationRequest) (identity.MoviePreparationResult, error) {
	p.once.Do(func() { close(p.started) })
	p.requests <- request
	select {
	case <-p.release:
		return identity.MoviePreparationResult{Found: true, Ready: true, Movie: model.Movie{ID: "movie-prepared"}}, nil
	case <-ctx.Done():
		return identity.MoviePreparationResult{}, ctx.Err()
	}
}

func TestManagerSubmitsBeforeMoviePreparationRuns(t *testing.T) {
	t.Setenv("VIDEO_TASK_WORKERS", "1")
	directory := t.TempDir()
	index, err := store.NewFileStore(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	engine := search.NewEngine(index, managerEmbedder{}, false)
	manager := NewManager(&immediateProcessor{}, engine)
	preparer := &blockingMoviePreparer{started: make(chan struct{}), release: make(chan struct{}), requests: make(chan identity.MoviePreparationRequest, 1)}
	manager.SetMoviePreparer(preparer)

	startedAt := time.Now()
	task, err := manager.Submit(Request{Source: "alipan", DriveID: "drive-1", FileID: "file-1", SourceName: "Movie.2006.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("submission waited for movie preparation: %s", elapsed)
	}
	select {
	case request := <-preparer.requests:
		if request.FileName != "Movie.2006.mkv" {
			t.Fatalf("preparation file name = %q", request.FileName)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not start movie preparation")
	}

	close(preparer.release)
	current := waitForManagerState(t, manager, task.ID, "completed")
	if current.MovieID != "movie-prepared" {
		t.Fatalf("task movie_id = %q, want prepared movie id", current.MovieID)
	}
}

func TestManagerStartsLocalTaskBeforeFingerprintCompletes(t *testing.T) {
	t.Setenv("VIDEO_TASK_WORKERS", "1")
	directory := t.TempDir()
	videoPath := filepath.Join(directory, "movie.mp4")
	if err := os.WriteFile(videoPath, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := store.NewFileStore(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	manager := NewManager(&immediateProcessor{}, search.NewEngine(index, managerEmbedder{}, false))
	hasher := &blockingFileHasher{started: make(chan struct{}), release: make(chan struct{})}
	manager.hasher = hasher.Hash
	preparer := &blockingMoviePreparer{started: make(chan struct{}), release: make(chan struct{}), requests: make(chan identity.MoviePreparationRequest, 1)}
	manager.SetMoviePreparer(preparer)

	task, err := manager.Submit(Request{LocalPath: videoPath, SourceName: "Movie.2006.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-hasher.started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start fingerprinting")
	}
	select {
	case <-preparer.started:
	case <-time.After(time.Second):
		t.Fatal("movie preparation did not overlap fingerprinting")
	}
	current, ok := manager.Get(task.ID)
	if !ok || current.State != "running" {
		t.Fatalf("task while fingerprinting = %+v, want running", current)
	}

	close(preparer.release)
	close(hasher.release)
	waitForManagerState(t, manager, task.ID, "completed")
}
