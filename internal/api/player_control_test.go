package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"video-semantic-search/internal/model"
	"video-semantic-search/internal/search"
	"video-semantic-search/internal/store"
)

func TestPlayerControlStreamsCommandToOpenPage(t *testing.T) {
	index, err := store.NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	duration := 120.0
	media := model.Media{
		MediaID:  "control-demo",
		Title:    "Control demo",
		Duration: &duration,
		Scenes:   []model.Scene{{Start: 0, End: 10}},
	}
	if err := index.UpsertMedia(media, [][]float32{{1, 0}}, "test"); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(search.NewEngine(index, testEmbedder{}, false), index, testEmbedder{}).Handler()
	server := httptest.NewServer(handler)
	defer server.Close()

	client := server.Client()
	eventsResponse, err := client.Get(server.URL + "/v1/player/events")
	if err != nil {
		t.Fatal(err)
	}
	defer eventsResponse.Body.Close()
	if eventsResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(eventsResponse.Body)
		t.Fatalf("player events status = %d, body=%s", eventsResponse.StatusCode, body)
	}
	if contentType := eventsResponse.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("player events content type = %q", contentType)
	}

	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/player/control", bytes.NewBufferString(`{"media_id":"control-demo","time":37.5}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	controlResponse, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer controlResponse.Body.Close()
	if controlResponse.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(controlResponse.Body)
		t.Fatalf("player control status = %d, body=%s", controlResponse.StatusCode, body)
	}
	var accepted struct {
		CommandID  string  `json:"command_id"`
		Action     string  `json:"action"`
		Delivered  bool    `json:"delivered"`
		Time       float64 `json:"time"`
		Fullscreen bool    `json:"fullscreen"`
		Autoplay   bool    `json:"autoplay"`
	}
	if err := json.NewDecoder(controlResponse.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.CommandID == "" || accepted.Action != "open" || !accepted.Delivered || accepted.Time != 37.5 || !accepted.Fullscreen || !accepted.Autoplay {
		t.Fatalf("accepted response = %+v", accepted)
	}

	data, err := readPlayerSSEDataWithTimeout(bufio.NewReader(eventsResponse.Body), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var command playerCommand
	if err := json.Unmarshal([]byte(data), &command); err != nil {
		t.Fatal(err)
	}
	if command.CommandID != accepted.CommandID || command.Action != "open" || command.MediaID != "control-demo" || command.Time != 37.5 || !command.Fullscreen || !command.Autoplay {
		t.Fatalf("player command = %+v", command)
	}

	for name, body := range map[string]string{
		"unknown media":  `{"media_id":"missing","time":1}`,
		"negative time":  `{"media_id":"control-demo","time":-1}`,
		"past duration":  `{"media_id":"control-demo","time":121}`,
		"unknown action": `{"action":"toggle","media_id":"control-demo"}`,
	} {
		t.Run(name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/player/control", bytes.NewBufferString(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			expectedStatus := http.StatusBadRequest
			if name == "unknown media" {
				expectedStatus = http.StatusNotFound
			}
			if response.StatusCode != expectedStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, expectedStatus)
			}
		})
	}

	closeRequest, err := http.NewRequest(http.MethodPost, server.URL+"/v1/player/control", bytes.NewBufferString(`{"action":"close"}`))
	if err != nil {
		t.Fatal(err)
	}
	closeRequest.Header.Set("Content-Type", "application/json")
	closeResponse, err := client.Do(closeRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse.Body.Close()
	if closeResponse.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(closeResponse.Body)
		t.Fatalf("close control status = %d, body=%s", closeResponse.StatusCode, body)
	}
	var closed struct {
		CommandID string `json:"command_id"`
		Action    string `json:"action"`
		Delivered bool   `json:"delivered"`
	}
	if err := json.NewDecoder(closeResponse.Body).Decode(&closed); err != nil {
		t.Fatal(err)
	}
	if closed.CommandID == "" || closed.Action != "close" || !closed.Delivered {
		t.Fatalf("close response = %+v", closed)
	}
	data, err = readPlayerSSEDataWithTimeout(bufio.NewReader(eventsResponse.Body), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var closeCommand playerCommand
	if err := json.Unmarshal([]byte(data), &closeCommand); err != nil {
		t.Fatal(err)
	}
	if closeCommand.CommandID != closed.CommandID || closeCommand.Action != "close" {
		t.Fatalf("close command = %+v", closeCommand)
	}
}

func readPlayerSSEDataWithTimeout(reader *bufio.Reader, timeout time.Duration) (string, error) {
	result := make(chan struct {
		data string
		err  error
	}, 1)
	go func() {
		var data strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				result <- struct {
					data string
					err  error
				}{err: err}
				return
			}
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if line == "" {
				if data.Len() > 0 {
					result <- struct {
						data string
						err  error
					}{data: data.String()}
					return
				}
				continue
			}
			if strings.HasPrefix(line, "data:") {
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
	}()
	select {
	case outcome := <-result:
		return outcome.data, outcome.err
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out waiting for player event")
	}
}
