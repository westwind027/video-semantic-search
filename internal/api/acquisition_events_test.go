package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"video-semantic-search/internal/acquisition"
	"video-semantic-search/internal/search"
	"video-semantic-search/internal/store"
)

func TestAcquisitionEventsStreamPushesTaskUpdates(t *testing.T) {
	t.Setenv("VIDEO_TASK_WORKERS", "1")
	directory := t.TempDir()
	index, err := store.NewFileStore(filepath.Join(directory, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	manager := acquisition.NewManager(immediateMediaProcessor{}, search.NewEngine(index, testEmbedder{}, false))
	handler := NewServerWithAcquisition(search.NewEngine(index, testEmbedder{}, false), index, testEmbedder{}, manager, filepath.Join(directory, "frames")).Handler()
	server := httptest.NewServer(handler)
	defer server.Close()

	client := server.Client()
	response, err := client.Get(server.URL + "/v1/acquisitions/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("events status = %d, body=%s", response.StatusCode, body)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("events content type = %q", contentType)
	}

	reader := bufio.NewReader(response.Body)
	initial := readAcquisitionEvent(t, reader)
	if initial.Type != acquisition.TaskEventSnapshot {
		t.Fatalf("initial event type = %q, want %q", initial.Type, acquisition.TaskEventSnapshot)
	}
	if len(initial.Tasks) != 0 {
		t.Fatalf("initial tasks = %+v, want empty", initial.Tasks)
	}

	task, err := manager.Submit(acquisition.Request{Source: "alipan", DriveID: "drive-1", FileID: "file-1", SourceName: "Movie.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	updated := readAcquisitionEvent(t, reader)
	if updated.Type != acquisition.TaskEventUpdated {
		t.Fatalf("task event type = %q, want %q", updated.Type, acquisition.TaskEventUpdated)
	}
	if updated.Task == nil || updated.Task.ID != task.ID {
		t.Fatalf("task event = %+v, want task %s", updated, task.ID)
	}
}

func readAcquisitionEvent(t *testing.T, reader *bufio.Reader) acquisition.TaskEvent {
	t.Helper()
	result := make(chan struct {
		event acquisition.TaskEvent
		err   error
	}, 1)
	go func() {
		var data strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				result <- struct {
					event acquisition.TaskEvent
					err   error
				}{err: err}
				return
			}
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if line == "" {
				break
			}
			if strings.HasPrefix(line, "data:") {
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		var event acquisition.TaskEvent
		if err := json.Unmarshal([]byte(data.String()), &event); err != nil {
			result <- struct {
				event acquisition.TaskEvent
				err   error
			}{err: err}
			return
		}
		result <- struct {
			event acquisition.TaskEvent
			err   error
		}{event: event}
	}()
	select {
	case outcome := <-result:
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		return outcome.event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for acquisition event")
		return acquisition.TaskEvent{}
	}
}
