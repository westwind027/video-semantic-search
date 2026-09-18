package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"video-semantic-search/internal/model"
)

const playerCommandPendingTTL = 30 * time.Second

// playerCommand is the small, browser-facing control message. Keeping this
// independent from model.Media makes the control channel safe to extend
// without changing search or index response schemas.
type playerCommand struct {
	CommandID  string  `json:"command_id"`
	Action     string  `json:"action"`
	MediaID    string  `json:"media_id"`
	Time       float64 `json:"time"`
	Fullscreen bool    `json:"fullscreen"`
	Autoplay   bool    `json:"autoplay"`
}

type playerControlRequest struct {
	Action     string   `json:"action,omitempty"`
	MediaID    string   `json:"media_id"`
	Time       *float64 `json:"time,omitempty"`
	At         *float64 `json:"at,omitempty"`
	Position   *float64 `json:"position,omitempty"`
	Fullscreen *bool    `json:"fullscreen,omitempty"`
	Autoplay   *bool    `json:"autoplay,omitempty"`
}

type pendingPlayerCommand struct {
	command   playerCommand
	expiresAt time.Time
}

// playerControlHub fans commands out to all currently open browser pages.
// Subscriber channels are buffered and publishing never waits on a browser;
// a command is retained briefly only when no page accepted it immediately.
type playerControlHub struct {
	mu          sync.Mutex
	subscribers map[chan playerCommand]struct{}
	pending     *pendingPlayerCommand
}

func newPlayerControlHub() *playerControlHub {
	return &playerControlHub{subscribers: make(map[chan playerCommand]struct{})}
}

func (h *playerControlHub) subscribe() (<-chan playerCommand, func()) {
	channel := make(chan playerCommand, 8)
	h.mu.Lock()
	if h.pending != nil {
		if time.Now().Before(h.pending.expiresAt) {
			channel <- h.pending.command
			h.pending = nil
		} else {
			h.pending = nil
		}
	}
	h.subscribers[channel] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			h.mu.Lock()
			if _, ok := h.subscribers[channel]; ok {
				delete(h.subscribers, channel)
				close(channel)
			}
			h.mu.Unlock()
		})
	}
	return channel, unsubscribe
}

func (h *playerControlHub) publish(command playerCommand) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	delivered := false
	for channel := range h.subscribers {
		select {
		case channel <- command:
			delivered = true
		default:
			// A slow/disconnected browser must not block an external caller.
		}
	}
	if !delivered {
		h.pending = &pendingPlayerCommand{command: command, expiresAt: time.Now().Add(playerCommandPendingTTL)}
	}
	return delivered
}

func (s *Server) handlePlayerControl(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	var input playerControlRequest
	if err := decodeJSON(response, request, &input); err != nil {
		return
	}
	action := strings.ToLower(strings.TrimSpace(input.Action))
	if action == "" {
		action = "open"
	}
	if action != "open" && action != "close" {
		writeError(response, http.StatusBadRequest, fmt.Errorf("action must be open or close"))
		return
	}
	if s.playerControl == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("player control is not configured"))
		return
	}
	mediaID := strings.TrimSpace(input.MediaID)
	timestamp := 0.0
	switch {
	case input.Time != nil:
		timestamp = *input.Time
	case input.At != nil:
		timestamp = *input.At
	case input.Position != nil:
		timestamp = *input.Position
	}
	if action == "open" && mediaID == "" {
		writeError(response, http.StatusBadRequest, fmt.Errorf("media_id is required for open action"))
		return
	}
	if action == "open" && s.store == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("index store is not configured"))
		return
	}
	if action == "open" {
		if _, ok := s.store.GetMedia(mediaID); !ok {
			writeError(response, http.StatusNotFound, fmt.Errorf("media %q not found", mediaID))
			return
		}
	}
	if action == "open" && (math.IsNaN(timestamp) || math.IsInf(timestamp, 0) || timestamp < 0) {
		writeError(response, http.StatusBadRequest, fmt.Errorf("time must be a finite non-negative number"))
		return
	}
	if action == "open" {
		media, _ := s.store.GetMedia(mediaID)
		if media.Duration != nil && timestamp > *media.Duration {
			writeError(response, http.StatusBadRequest, fmt.Errorf("time %.3f exceeds media duration %.3f", timestamp, *media.Duration))
			return
		}
	}

	commandID, err := model.NewMediaID()
	if err != nil {
		writeError(response, http.StatusInternalServerError, fmt.Errorf("create command id: %w", err))
		return
	}
	fullscreen := false
	autoplay := false
	if action == "open" {
		fullscreen = true
		if input.Fullscreen != nil {
			fullscreen = *input.Fullscreen
		}
		autoplay = true
		if input.Autoplay != nil {
			autoplay = *input.Autoplay
		}
	}
	command := playerCommand{CommandID: commandID, Action: action, MediaID: mediaID, Time: timestamp, Fullscreen: fullscreen, Autoplay: autoplay}
	delivered := s.playerControl.publish(command)
	writeJSON(response, http.StatusAccepted, map[string]any{
		"status":     "accepted",
		"command_id": commandID,
		"action":     action,
		"media_id":   mediaID,
		"time":       timestamp,
		"fullscreen": fullscreen,
		"autoplay":   autoplay,
		"delivered":  delivered,
	})
}

func (s *Server) handlePlayerEvents(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(response)
		return
	}
	if s.playerControl == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("player control is not configured"))
		return
	}
	flusher, ok := response.(http.Flusher)
	if !ok {
		writeError(response, http.StatusInternalServerError, fmt.Errorf("streaming is not supported"))
		return
	}
	commands, unsubscribe := s.playerControl.subscribe()
	defer unsubscribe()
	response.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	response.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	response.Header().Set("Connection", "keep-alive")
	response.Header().Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	flusher.Flush()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case command, open := <-commands:
			if !open {
				return
			}
			if err := writePlayerSSE(response, flusher, command); err != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(response, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writePlayerSSE(response http.ResponseWriter, flusher http.Flusher, command playerCommand) error {
	payload, err := json.Marshal(command)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(response, "data: %s\n\n", payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
