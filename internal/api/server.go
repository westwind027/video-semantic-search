package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"video-semantic-search/internal/acquisition"
	"video-semantic-search/internal/alipan"
	"video-semantic-search/internal/embedding"
	"video-semantic-search/internal/httpclient"
	"video-semantic-search/internal/identity"
	"video-semantic-search/internal/metadata"
	"video-semantic-search/internal/model"
	"video-semantic-search/internal/search"
	"video-semantic-search/internal/store"
)

type Server struct {
	engine                   *search.Engine
	store                    store.IndexStore
	embedder                 embedding.Client
	jobs                     *acquisition.Manager
	frameRoot                string
	alipan                   *alipan.Manager
	streams                  *streamProxy
	cache                    *streamCache
	identityStore            identity.Store
	identityTagger           identity.Tagger
	identityClient           identity.Client
	moviePreparer            identity.MoviePreparer
	tmdb                     *metadata.TMDBClient
	processedMovieMu         sync.Mutex
	processedMovieSignature  string
	processedMovieCache      []string
	processedMovieCacheValid bool
	// Signed CDN playlist URLs for transcoded playback, keyed by media.
	transcodeMu   sync.Mutex
	transcodeURLs map[string]transcodeEntry
}

func NewServer(engine *search.Engine, indexStore store.IndexStore, embedder embedding.Client) *Server {
	return NewServerWithAcquisition(engine, indexStore, embedder, nil, "data/frames")
}

func NewServerWithAcquisition(engine *search.Engine, indexStore store.IndexStore, embedder embedding.Client, jobs *acquisition.Manager, frameRoot string) *Server {
	return NewServerWithAcquisitionAndAliyun(engine, indexStore, embedder, jobs, frameRoot, alipan.NewManager(alipan.ConfigFromEnv()))
}

func NewServerWithAcquisitionAndAliyun(engine *search.Engine, indexStore store.IndexStore, embedder embedding.Client, jobs *acquisition.Manager, frameRoot string, connector *alipan.Manager) *Server {
	if frameRoot == "" {
		frameRoot = "data/frames"
	}
	return &Server{engine: engine, store: indexStore, embedder: embedder, jobs: jobs, frameRoot: frameRoot, alipan: connector, streams: newStreamProxy(connector), cache: newStreamCache(streamCacheConfig()), transcodeURLs: map[string]transcodeEntry{}, tmdb: metadata.NewTMDBClientFromEnv()}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/favicon.ico", func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1/media", s.handleMediaCollection)
	mux.HandleFunc("/v1/media/embeddings/rebuild", s.handleEmbeddingRebuildBatch)
	mux.HandleFunc("/v1/media/batch/delete", s.handleMediaBatchDelete)
	mux.HandleFunc("/v1/metadata/movies/resolve", s.handleMovieResolve)
	mux.HandleFunc("/v1/metadata/movies/prepare", s.handleMoviePrepare)
	mux.HandleFunc("/v1/metadata/movies", s.handleMovieCollection)
	mux.HandleFunc("/v1/metadata/movies/", s.handleMovieByID)
	mux.HandleFunc("/v1/metadata/persons", s.handlePersonCollection)
	mux.HandleFunc("/v1/metadata/persons/", s.handlePersonByID)
	mux.HandleFunc("/v1/persons/", s.handlePersonFaces)
	mux.HandleFunc("/v1/media/", s.handleMediaByID)
	mux.HandleFunc("/v1/files/inspect", s.handleFileInspect)
	mux.HandleFunc("/v1/files/validate", s.handleFileValidation)
	mux.HandleFunc("/v1/files/scan", s.handleFileScan)
	mux.HandleFunc("/v1/acquisitions", s.handleAcquisitionCollection)
	mux.HandleFunc("/v1/acquisitions/events", s.handleAcquisitionEvents)
	mux.HandleFunc("/v1/acquisitions/batch", s.handleAcquisitionBatch)
	mux.HandleFunc("/v1/acquisitions/batch/stop", s.handleAcquisitionBatchStop)
	mux.HandleFunc("/v1/acquisitions/batch/delete", s.handleAcquisitionBatchDelete)
	mux.HandleFunc("/v1/acquisitions/clear", s.handleAcquisitionClear)
	mux.HandleFunc("/v1/acquisitions/", s.handleAcquisitionByID)
	mux.HandleFunc("/v1/connectors/alipan/status", s.handleAlipanStatus)
	mux.HandleFunc("/v1/connectors/alipan/files", s.handleAlipanFiles)
	mux.HandleFunc("/v1/connectors/alipan/login/start", s.handleAlipanLoginStart)
	mux.HandleFunc("/v1/connectors/alipan/login/web/start", s.handleAlipanWebLoginStart)
	mux.HandleFunc("/v1/connectors/alipan/login/complete", s.handleAlipanLoginComplete)
	mux.HandleFunc("/v1/connectors/alipan/login/callback", s.handleAlipanLoginCallback)
	mux.HandleFunc("/v1/connectors/alipan/login/", s.handleAlipanLoginResource)
	mux.HandleFunc("/v1/connectors/alipan/logout", s.handleAlipanLogout)
	mux.HandleFunc("/v1/search", s.handleSearch)
	return mux
}

// ConfigureIdentity attaches the optional metadata and face-recognition
// components after construction. Existing callers can keep using the legacy
// constructors without enabling the identity pipeline.
func (s *Server) ConfigureIdentity(identityStore identity.Store, tagger identity.Tagger) {
	s.identityStore = identityStore
	s.identityTagger = tagger
	if service, ok := tagger.(*identity.TaggerService); ok {
		s.identityClient = service.Client()
	}
}

// ConfigureMoviePreparer attaches the idempotent IMDb/TMDB/face-bank
// preparation module used by the import UI. It is separate from Tagger so a
// caller can prepare metadata before any video frames are processed.
func (s *Server) ConfigureMoviePreparer(preparer identity.MoviePreparer) {
	s.moviePreparer = preparer
}

func (s *Server) handleRoot(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/" || request.Method != http.MethodGet {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	_, _ = io.WriteString(response, indexHTML)
}

func (s *Server) handleHealth(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(response)
		return
	}
	mediaCount, sceneCount := s.store.Stats()
	health := model.HealthResponse{Status: "ok", MediaCount: mediaCount, SceneCount: sceneCount}
	embeddingHealth, err := s.embedder.Health(request.Context())
	if err != nil {
		health.Status = "degraded"
		health.EmbeddingStatus = "unavailable"
		writeJSON(response, http.StatusOK, health)
		return
	}
	health.EmbeddingStatus = embeddingHealth.Status
	health.EmbeddingModel = embeddingHealth.Model
	health.EmbeddingDimension = embeddingHealth.Dimension
	if s.identityClient != nil {
		identityHealth, identityErr := s.identityClient.Health(request.Context())
		if identityErr != nil {
			health.IdentityStatus = "unavailable"
		} else {
			health.IdentityStatus = identityHealth.Status
			health.IdentityModel = identityHealth.Model
			health.IdentityDimension = identityHealth.Dimension
		}
	}
	writeJSON(response, http.StatusOK, health)
}

func (s *Server) handleMediaCollection(response http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		writeJSON(response, http.StatusOK, s.store.ListMedia())
		return
	}
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	var media model.Media
	if err := decodeJSON(response, request, &media); err != nil {
		return
	}
	if err := media.Validate(); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if media.MediaID == "" {
		var err error
		media.MediaID, err = model.NewMediaID()
		if err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
	}
	result, err := s.engine.Index(request.Context(), media)
	if err != nil {
		writeError(response, http.StatusBadGateway, fmt.Errorf("index media: %w", err))
		return
	}
	writeJSON(response, http.StatusCreated, model.IngestResponse{MediaID: media.MediaID, SceneCount: result.SceneCount, EmbeddingModel: result.Model, EmbeddingDimension: result.Dimension})
}

func (s *Server) handleMediaByID(response http.ResponseWriter, request *http.Request) {
	path := strings.TrimPrefix(request.URL.Path, "/v1/media/")
	parts := strings.Split(path, "/")
	if len(parts) == 3 && parts[1] == "frames" && (request.Method == http.MethodGet || request.Method == http.MethodHead) {
		s.handleFrame(response, request, parts[0], parts[2])
		return
	}
	if len(parts) == 2 && parts[1] == "stream" && (request.Method == http.MethodGet || request.Method == http.MethodHead) {
		s.handleMediaStream(response, request, parts[0])
		return
	}
	if len(parts) >= 2 && parts[1] == "transcode" && request.Method == http.MethodGet {
		switch {
		case len(parts) == 2:
			s.handleMediaTranscode(response, request, parts[0])
		case parts[2] == "playlist":
			s.handleTranscodePlaylist(response, request, parts[0])
		case parts[2] == "proxy":
			s.handleTranscodeProxy(response, request, parts[0])
		default:
			http.NotFound(response, request)
		}
		return
	}
	if len(parts) == 3 && parts[1] == "scenes" && request.Method == http.MethodDelete {
		s.handleSceneDelete(response, parts[0], parts[2])
		return
	}
	if len(parts) == 3 && parts[1] == "embeddings" && parts[2] == "rebuild" && request.Method == http.MethodPost {
		s.handleEmbeddingRebuild(response, request, parts[0])
		return
	}
	if len(parts) == 3 && parts[1] == "persons" && parts[2] == "rebuild" && request.Method == http.MethodPost {
		s.handlePersonRebuild(response, request, parts[0])
		return
	}
	mediaID, err := url.PathUnescape(path)
	if err != nil || mediaID == "" || strings.Contains(mediaID, "/") {
		http.NotFound(response, request)
		return
	}
	switch request.Method {
	case http.MethodGet:
		media, ok := s.store.GetMedia(mediaID)
		if !ok {
			writeError(response, http.StatusNotFound, fmt.Errorf("media not found"))
			return
		}
		writeJSON(response, http.StatusOK, media)
	case http.MethodDelete:
		if err := s.deleteMedia(mediaID); err != nil {
			status := http.StatusInternalServerError
			if strings.Contains(err.Error(), "media not found") {
				status = http.StatusNotFound
			}
			writeError(response, status, err)
			return
		}
		response.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(response)
	}
}

type embeddingRebuildRequest struct {
	Profile string `json:"profile,omitempty"`
}

type embeddingRebuildBatchRequest struct {
	MediaIDs []string `json:"media_ids"`
	Profile  string   `json:"profile,omitempty"`
}

type embeddingRebuildBatchResponse struct {
	Tasks    []acquisition.Task        `json:"tasks"`
	Failures []embeddingRebuildFailure `json:"failures,omitempty"`
}

type mediaBatchDeleteRequest struct {
	MediaIDs []string `json:"media_ids"`
}

type mediaBatchDeleteResponse struct {
	Deleted  []string             `json:"deleted"`
	Failures []mediaDeleteFailure `json:"failures,omitempty"`
}

type mediaDeleteFailure struct {
	MediaID string `json:"media_id,omitempty"`
	Error   string `json:"error"`
}

func (s *Server) handleMediaBatchDelete(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	var batchRequest mediaBatchDeleteRequest
	if err := decodeJSON(response, request, &batchRequest); err != nil {
		return
	}
	if len(batchRequest.MediaIDs) == 0 {
		writeError(response, http.StatusBadRequest, fmt.Errorf("media_ids must contain at least one media id"))
		return
	}
	if len(batchRequest.MediaIDs) > 1000 {
		writeError(response, http.StatusBadRequest, fmt.Errorf("media_ids cannot contain more than 1000 items"))
		return
	}

	result := mediaBatchDeleteResponse{Deleted: []string{}}
	seen := make(map[string]struct{}, len(batchRequest.MediaIDs))
	for _, rawMediaID := range batchRequest.MediaIDs {
		mediaID := strings.TrimSpace(rawMediaID)
		if mediaID == "" {
			result.Failures = append(result.Failures, mediaDeleteFailure{Error: "media_id is required"})
			continue
		}
		if _, exists := seen[mediaID]; exists {
			continue
		}
		seen[mediaID] = struct{}{}
		if err := s.deleteMedia(mediaID); err != nil {
			result.Failures = append(result.Failures, mediaDeleteFailure{MediaID: mediaID, Error: err.Error()})
			continue
		}
		result.Deleted = append(result.Deleted, mediaID)
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) deleteMedia(mediaID string) error {
	if _, ok := s.store.GetMedia(mediaID); !ok {
		return fmt.Errorf("media not found")
	}
	if !s.store.DeleteMedia(mediaID) {
		return fmt.Errorf("delete media index failed")
	}
	if frameDir, ok := safeFrameDir(s.frameRoot, mediaID); ok {
		if err := os.RemoveAll(frameDir); err != nil {
			return fmt.Errorf("remove media frames: %w", err)
		}
	}
	return nil
}

type embeddingRebuildFailure struct {
	MediaID string `json:"media_id"`
	Error   string `json:"error"`
}

func (s *Server) handleEmbeddingRebuildBatch(response http.ResponseWriter, request *http.Request) {
	if s.jobs == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("acquisition manager is not configured"))
		return
	}
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	var batchRequest embeddingRebuildBatchRequest
	if err := decodeJSON(response, request, &batchRequest); err != nil {
		return
	}
	if len(batchRequest.MediaIDs) == 0 {
		writeError(response, http.StatusBadRequest, fmt.Errorf("media_ids must contain at least one media id"))
		return
	}
	if len(batchRequest.MediaIDs) > 1000 {
		writeError(response, http.StatusBadRequest, fmt.Errorf("media_ids cannot contain more than 1000 items"))
		return
	}
	profile := embedding.ImageProfileOriginal
	if strings.TrimSpace(batchRequest.Profile) != "" {
		profile = embedding.ImageProfile(strings.ToLower(strings.TrimSpace(batchRequest.Profile)))
	}
	if profile != embedding.ImageProfileOriginal && profile != embedding.ImageProfileCompressed {
		writeError(response, http.StatusBadRequest, fmt.Errorf("unsupported image profile %q", profile))
		return
	}
	result := embeddingRebuildBatchResponse{Tasks: []acquisition.Task{}}
	seen := make(map[string]struct{}, len(batchRequest.MediaIDs))
	for _, rawMediaID := range batchRequest.MediaIDs {
		mediaID := strings.TrimSpace(rawMediaID)
		if mediaID == "" {
			result.Failures = append(result.Failures, embeddingRebuildFailure{Error: "media_id is required"})
			continue
		}
		if _, exists := seen[mediaID]; exists {
			continue
		}
		seen[mediaID] = struct{}{}
		task, err := s.jobs.RebuildEmbeddings(mediaID, profile)
		if err != nil {
			result.Failures = append(result.Failures, embeddingRebuildFailure{MediaID: mediaID, Error: err.Error()})
			continue
		}
		result.Tasks = append(result.Tasks, task)
	}
	writeJSON(response, http.StatusAccepted, result)
}

func (s *Server) handleEmbeddingRebuild(response http.ResponseWriter, request *http.Request, rawMediaID string) {
	if s.jobs == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("acquisition manager is not configured"))
		return
	}
	mediaID, err := url.PathUnescape(rawMediaID)
	if err != nil || mediaID == "" || strings.Contains(mediaID, "/") {
		writeError(response, http.StatusNotFound, fmt.Errorf("media not found"))
		return
	}
	profile := embedding.ImageProfileOriginal
	if request.ContentLength != 0 {
		var rebuildRequest embeddingRebuildRequest
		if err := decodeJSON(response, request, &rebuildRequest); err != nil {
			return
		}
		if strings.TrimSpace(rebuildRequest.Profile) != "" {
			profile = embedding.ImageProfile(strings.ToLower(strings.TrimSpace(rebuildRequest.Profile)))
		}
	}
	task, err := s.jobs.RebuildEmbeddings(mediaID, profile)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeError(response, status, err)
		return
	}
	writeJSON(response, http.StatusAccepted, task)
}

func (s *Server) handleSceneDelete(response http.ResponseWriter, rawMediaID, rawSceneID string) {
	mediaID, err := url.PathUnescape(rawMediaID)
	if err != nil || mediaID == "" || strings.Contains(mediaID, "/") {
		writeError(response, http.StatusNotFound, fmt.Errorf("media not found"))
		return
	}
	sceneID, err := url.PathUnescape(rawSceneID)
	if err != nil || sceneID == "" || strings.Contains(sceneID, "/") {
		writeError(response, http.StatusNotFound, fmt.Errorf("scene not found"))
		return
	}
	deleter, ok := s.store.(store.SceneDeleter)
	if !ok {
		writeError(response, http.StatusNotImplemented, fmt.Errorf("scene deletion is not supported by the index store"))
		return
	}
	scene, ok := deleter.DeleteScene(mediaID, sceneID)
	if !ok {
		writeError(response, http.StatusNotFound, fmt.Errorf("scene not found"))
		return
	}
	if scene.PreviewPath != "" {
		frameDir, safe := safeFrameDir(s.frameRoot, mediaID)
		if !safe {
			response.WriteHeader(http.StatusNoContent)
			return
		}
		framePath, absErr := filepath.Abs(scene.PreviewPath)
		if absErr != nil {
			response.WriteHeader(http.StatusNoContent)
			return
		}
		framePath = filepath.Clean(framePath)
		if relative, relErr := filepath.Rel(frameDir, framePath); relErr == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			_ = os.Remove(framePath)
		}
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleFrame(response http.ResponseWriter, request *http.Request, rawMediaID, rawFilename string) {
	mediaID, err := url.PathUnescape(rawMediaID)
	if err != nil || mediaID == "" || strings.Contains(mediaID, "/") {
		http.NotFound(response, request)
		return
	}
	filename, err := url.PathUnescape(rawFilename)
	if err != nil || filename == "" || filepath.Base(filename) != filename {
		http.NotFound(response, request)
		return
	}
	if _, ok := s.store.GetMedia(mediaID); !ok {
		http.NotFound(response, request)
		return
	}
	frameDir, safe := safeFrameDir(s.frameRoot, mediaID)
	if !safe {
		http.NotFound(response, request)
		return
	}
	framePath := filepath.Join(frameDir, filename)
	http.ServeFile(response, request, framePath)
}

func safeFrameDir(root, mediaID string) (string, bool) {
	if strings.TrimSpace(mediaID) == "" {
		return "", false
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	absoluteCandidate, err := filepath.Abs(filepath.Join(absoluteRoot, mediaID))
	if err != nil {
		return "", false
	}
	relative, err := filepath.Rel(absoluteRoot, absoluteCandidate)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return absoluteCandidate, true
}

func (s *Server) handleAcquisitionCollection(response http.ResponseWriter, request *http.Request) {
	if s.jobs == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("acquisition manager is not configured"))
		return
	}
	switch request.Method {
	case http.MethodGet:
		writeJSON(response, http.StatusOK, s.jobs.List())
	case http.MethodPost:
		var acquisitionRequest acquisition.Request
		if err := decodeJSON(response, request, &acquisitionRequest); err != nil {
			return
		}
		task, err := s.jobs.SubmitContext(request.Context(), acquisitionRequest)
		if err != nil {
			writeError(response, http.StatusBadRequest, err)
			return
		}
		writeJSON(response, http.StatusAccepted, task)
	default:
		methodNotAllowed(response)
	}
}

// handleAcquisitionEvents streams task snapshots and state changes to the
// browser. The Manager publishes non-blocking events, so a disconnected or
// slow browser cannot delay acquisition workers.
func (s *Server) handleAcquisitionEvents(response http.ResponseWriter, request *http.Request) {
	if s.jobs == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("acquisition manager is not configured"))
		return
	}
	if request.Method != http.MethodGet {
		methodNotAllowed(response)
		return
	}
	flusher, ok := response.(http.Flusher)
	if !ok {
		writeError(response, http.StatusInternalServerError, fmt.Errorf("streaming is not supported"))
		return
	}
	events, tasks, unsubscribe := s.jobs.SubscribeTaskEvents()
	defer unsubscribe()
	response.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	response.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	response.Header().Set("Connection", "keep-alive")
	response.Header().Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	flusher.Flush()
	if err := writeAcquisitionSSE(response, flusher, acquisition.TaskEvent{Type: acquisition.TaskEventSnapshot, Tasks: tasks}); err != nil {
		return
	}

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case event, open := <-events:
			if !open {
				return
			}
			if err := writeAcquisitionSSE(response, flusher, event); err != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := io.WriteString(response, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeAcquisitionSSE(response http.ResponseWriter, flusher http.Flusher, event acquisition.TaskEvent) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(response, "data: %s\n\n", payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

type acquisitionBatchRequest struct {
	Items []acquisition.Request `json:"items"`
}

type acquisitionBatchFailure struct {
	Index      int    `json:"index"`
	SourceName string `json:"source_name,omitempty"`
	Error      string `json:"error"`
}

type acquisitionBatchResponse struct {
	Tasks    []acquisition.Task        `json:"tasks"`
	Failures []acquisitionBatchFailure `json:"failures,omitempty"`
}

// handleAcquisitionBatch creates many tasks in one round trip. Submissions no
// longer read file contents, so this stays cheap even for hundreds of files;
// the concurrency limit only protects the filesystem walk of validation.
func (s *Server) handleAcquisitionBatch(response http.ResponseWriter, request *http.Request) {
	if s.jobs == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("acquisition manager is not configured"))
		return
	}
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	var batchRequest acquisitionBatchRequest
	if err := decodeJSON(response, request, &batchRequest); err != nil {
		return
	}
	if len(batchRequest.Items) == 0 {
		writeError(response, http.StatusBadRequest, fmt.Errorf("items must contain at least one acquisition request"))
		return
	}
	const batchConcurrency = 4
	results := make([]acquisition.Task, len(batchRequest.Items))
	failures := make(map[int]acquisitionBatchFailure)
	semaphore := make(chan struct{}, batchConcurrency)
	var waitGroup sync.WaitGroup
	var mu sync.Mutex
	for index, item := range batchRequest.Items {
		waitGroup.Add(1)
		semaphore <- struct{}{}
		go func(index int, item acquisition.Request) {
			defer waitGroup.Done()
			defer func() { <-semaphore }()
			task, err := s.jobs.SubmitContext(request.Context(), item)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures[index] = acquisitionBatchFailure{Index: index, SourceName: item.SourceName, Error: err.Error()}
				return
			}
			results[index] = task
		}(index, item)
	}
	waitGroup.Wait()
	tasks := make([]acquisition.Task, 0, len(results))
	for _, task := range results {
		if task.ID != "" {
			tasks = append(tasks, task)
		}
	}
	failureList := make([]acquisitionBatchFailure, 0, len(failures))
	for index := 0; index < len(batchRequest.Items); index++ {
		if failure, ok := failures[index]; ok {
			failureList = append(failureList, failure)
		}
	}
	writeJSON(response, http.StatusAccepted, acquisitionBatchResponse{Tasks: tasks, Failures: failureList})
}

type acquisitionClearRequest struct {
	States []string `json:"states"`
}

type acquisitionTaskBatchRequest struct {
	TaskIDs []string `json:"task_ids"`
}

type acquisitionTaskBatchFailure struct {
	TaskID string `json:"task_id"`
	Error  string `json:"error"`
}

type acquisitionTaskBatchResponse struct {
	Stopped  []string                      `json:"stopped,omitempty"`
	Removed  []string                      `json:"removed,omitempty"`
	Failures []acquisitionTaskBatchFailure `json:"failures,omitempty"`
}

func (s *Server) handleAcquisitionBatchStop(response http.ResponseWriter, request *http.Request) {
	s.handleAcquisitionBatchTaskOperation(response, request, true)
}

func (s *Server) handleAcquisitionBatchDelete(response http.ResponseWriter, request *http.Request) {
	s.handleAcquisitionBatchTaskOperation(response, request, false)
}

func (s *Server) handleAcquisitionBatchTaskOperation(response http.ResponseWriter, request *http.Request, stop bool) {
	if s.jobs == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("acquisition manager is not configured"))
		return
	}
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	var payload acquisitionTaskBatchRequest
	if err := decodeJSON(response, request, &payload); err != nil {
		return
	}
	if len(payload.TaskIDs) == 0 || len(payload.TaskIDs) > 500 {
		writeError(response, http.StatusBadRequest, fmt.Errorf("task_ids must contain between 1 and 500 items"))
		return
	}
	taskIDs := uniqueTaskIDs(payload.TaskIDs)
	if len(taskIDs) == 0 {
		writeError(response, http.StatusBadRequest, fmt.Errorf("task_ids must contain at least one non-empty task id"))
		return
	}
	result := acquisitionTaskBatchResponse{}
	if stop {
		result.Stopped = s.jobs.StopMany(taskIDs)
	} else {
		result.Removed = s.jobs.RemoveMany(taskIDs)
	}
	stopped := make(map[string]struct{}, len(result.Stopped))
	for _, taskID := range result.Stopped {
		stopped[taskID] = struct{}{}
	}
	removed := make(map[string]struct{}, len(result.Removed))
	for _, taskID := range result.Removed {
		removed[taskID] = struct{}{}
	}
	for _, taskID := range taskIDs {
		if _, ok := stopped[taskID]; ok {
			continue
		}
		if _, ok := removed[taskID]; ok {
			continue
		}
		task, exists := s.jobs.Get(taskID)
		if !exists {
			result.Failures = append(result.Failures, acquisitionTaskBatchFailure{TaskID: taskID, Error: "任务不存在"})
			continue
		}
		if stop {
			result.Failures = append(result.Failures, acquisitionTaskBatchFailure{TaskID: taskID, Error: fmt.Sprintf("任务已处于%s状态", task.State)})
			continue
		}
		result.Failures = append(result.Failures, acquisitionTaskBatchFailure{TaskID: taskID, Error: "任务仍在处理，请先停止"})
	}
	writeJSON(response, http.StatusOK, result)
}

func uniqueTaskIDs(taskIDs []string) []string {
	result := make([]string, 0, len(taskIDs))
	seen := make(map[string]struct{}, len(taskIDs))
	for _, taskID := range taskIDs {
		taskID = strings.TrimSpace(taskID)
		if taskID == "" {
			continue
		}
		if _, exists := seen[taskID]; exists {
			continue
		}
		seen[taskID] = struct{}{}
		result = append(result, taskID)
	}
	return result
}

// handleAcquisitionClear deletes terminal task records. By default only
// failed and canceled tasks are removed, so completed history survives.
func (s *Server) handleAcquisitionClear(response http.ResponseWriter, request *http.Request) {
	if s.jobs == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("acquisition manager is not configured"))
		return
	}
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	states := []string{"failed", "canceled"}
	if request.ContentLength != 0 {
		var clearRequest acquisitionClearRequest
		if err := decodeJSON(response, request, &clearRequest); err != nil {
			return
		}
		if len(clearRequest.States) > 0 {
			states = clearRequest.States
		}
	}
	writeJSON(response, http.StatusOK, map[string]int{"removed": s.jobs.Clear(states)})
}

func (s *Server) handleAcquisitionByID(response http.ResponseWriter, request *http.Request) {
	if s.jobs == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("acquisition manager is not configured"))
		return
	}
	taskID, err := url.PathUnescape(strings.TrimPrefix(request.URL.Path, "/v1/acquisitions/"))
	if err != nil || taskID == "" || strings.Contains(taskID, "/") {
		http.NotFound(response, request)
		return
	}
	switch request.Method {
	case http.MethodGet:
		task, ok := s.jobs.Get(taskID)
		if !ok {
			writeError(response, http.StatusNotFound, fmt.Errorf("acquisition task not found"))
			return
		}
		writeJSON(response, http.StatusOK, task)
	case http.MethodDelete:
		if _, ok := s.jobs.Stop(taskID); !ok {
			// Terminal tasks cannot be stopped, but they can be removed from
			// the list so the queue view stays readable.
			if !s.jobs.Remove(taskID) {
				if _, exists := s.jobs.Get(taskID); !exists {
					writeError(response, http.StatusNotFound, fmt.Errorf("acquisition task not found"))
					return
				}
				writeError(response, http.StatusConflict, fmt.Errorf("acquisition task is still running, stop it first"))
				return
			}
		}
		response.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(response)
	}
}

func (s *Server) handleAlipanStatus(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(response)
		return
	}
	if s.alipan == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("阿里云盘 connector 未配置"))
		return
	}
	writeJSON(response, http.StatusOK, s.alipan.StatusContext(request.Context()))
}

func (s *Server) handleAlipanFiles(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(response)
		return
	}
	if s.alipan == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("阿里云盘 connector 未配置"))
		return
	}
	limit := 100
	if value := strings.TrimSpace(request.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			writeError(response, http.StatusBadRequest, fmt.Errorf("invalid limit"))
			return
		}
		limit = parsed
	}
	driveID := request.URL.Query().Get("drive_id")
	parentFileID := request.URL.Query().Get("parent_file_id")
	if recursive, _ := strconv.ParseBool(request.URL.Query().Get("recursive")); recursive {
		maxFiles := limit
		if maxFiles <= 0 || maxFiles > 1000 {
			maxFiles = 500
		}
		items, err := s.alipan.ListVideoFiles(request.Context(), driveID, parentFileID, maxFiles)
		if err != nil {
			writeError(response, http.StatusBadGateway, err)
			return
		}
		if strings.TrimSpace(driveID) == "" {
			status := s.alipan.Status()
			if status.Profile != nil {
				driveID = status.Profile.ActiveDriveID
			}
		}
		response.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		writeJSON(response, http.StatusOK, alipan.FileList{DriveID: driveID, ParentFileID: parentFileID, Items: items})
		return
	}
	files, err := s.alipan.ListFiles(request.Context(), driveID, parentFileID, request.URL.Query().Get("marker"), limit)
	if err != nil {
		writeError(response, http.StatusBadGateway, err)
		return
	}
	response.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	writeJSON(response, http.StatusOK, files)
}

func (s *Server) handleAlipanLoginStart(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	if s.alipan == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("阿里云盘 connector 未配置"))
		return
	}
	login, err := s.alipan.StartLogin()
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusCreated, login)
}

// handleAlipanWebLoginStart starts a passport QR login for the browser-session
// (web) token system used by transcoded playback — separate from the OpenAPI
// tokens the regular login flow produces.
func (s *Server) handleAlipanWebLoginStart(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	if s.alipan == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("阿里云盘 connector 未配置"))
		return
	}
	login, err := s.alipan.StartWebLogin()
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusCreated, login)
}

type alipanLoginCompleteRequest struct {
	SessionID string `json:"session_id"`
	Code      string `json:"code"`
}

func (s *Server) handleAlipanLoginComplete(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	if s.alipan == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("阿里云盘 connector 未配置"))
		return
	}
	var payload alipanLoginCompleteRequest
	if err := decodeJSON(response, request, &payload); err != nil {
		return
	}
	if strings.TrimSpace(payload.SessionID) == "" {
		writeError(response, http.StatusBadRequest, fmt.Errorf("session_id is required"))
		return
	}
	profile, err := s.alipan.CompleteLogin(request.Context(), payload.SessionID, payload.Code)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusOK, profile)
}

func (s *Server) handleAlipanLoginCallback(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(response)
		return
	}
	if s.alipan == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("阿里云盘 connector 未配置"))
		return
	}
	state := strings.TrimSpace(request.URL.Query().Get("state"))
	code := strings.TrimSpace(request.URL.Query().Get("code"))
	if state == "" || code == "" {
		writeError(response, http.StatusBadRequest, fmt.Errorf("阿里云盘回调缺少 state 或 code"))
		return
	}
	if _, err := s.alipan.HandleCallback(request.Context(), state, code); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(response, `<!doctype html><meta charset="utf-8"><title>阿里云盘登录完成</title><style>body{font:16px system-ui;padding:40px;color:#172033}strong{color:#536dfe}</style><strong>阿里云盘登录完成</strong><p>可以关闭此页面，返回视频语义搜索。</p>`)
}

func (s *Server) handleAlipanLoginResource(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(response)
		return
	}
	if s.alipan == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("阿里云盘 connector 未配置"))
		return
	}
	path := strings.TrimPrefix(request.URL.Path, "/v1/connectors/alipan/login/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || (parts[1] != "qr" && parts[1] != "status") {
		http.NotFound(response, request)
		return
	}
	if parts[1] == "status" {
		response.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		status, err := s.alipan.PollLogin(request.Context(), parts[0])
		if err != nil {
			writeError(response, http.StatusBadGateway, err)
			return
		}
		writeJSON(response, http.StatusOK, status)
		return
	}
	image, err := s.alipan.QRCode(parts[0])
	if err != nil {
		writeError(response, http.StatusNotFound, err)
		return
	}
	response.Header().Set("Content-Type", "image/png")
	response.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(image)
}

func (s *Server) handleAlipanLogout(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	if s.alipan == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("阿里云盘 connector 未配置"))
		return
	}
	if err := s.alipan.Logout(); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

type movieResolveRequest struct {
	MovieID  string `json:"movie_id,omitempty"`
	Title    string `json:"title,omitempty"`
	FileName string `json:"file_name,omitempty"`
	Year     *int   `json:"year,omitempty"`
	TMDBID   int    `json:"tmdb_id,omitempty"`
}

type movieCastRequest struct {
	Cast []model.MovieCast `json:"cast"`
}

func (s *Server) handleMovieCollection(response http.ResponseWriter, request *http.Request) {
	if s.identityStore == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("identity metadata store is not configured"))
		return
	}
	switch request.Method {
	case http.MethodGet:
		query := strings.TrimSpace(request.URL.Query().Get("q"))
		limit := 20
		if value := strings.TrimSpace(request.URL.Query().Get("limit")); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 {
				writeError(response, http.StatusBadRequest, fmt.Errorf("limit must be a positive integer"))
				return
			}
			limit = parsed
		}
		if limit > 100 {
			limit = 100
		}
		if searcher, ok := s.identityStore.(identity.MovieSearcher); ok {
			writeJSON(response, http.StatusOK, searcher.SearchMovies(query, limit))
			return
		}
		writeJSON(response, http.StatusOK, s.identityStore.ListMovies())
	case http.MethodPost:
		var movie model.Movie
		if err := decodeJSON(response, request, &movie); err != nil {
			return
		}
		if strings.TrimSpace(movie.ID) == "" {
			movie.ID = metadata.EntityID("movie", movie.TMDBID, movie.IMDbID, movie.Title, movie.Year)
		}
		if err := s.identityStore.UpsertMovie(movie); err != nil {
			writeError(response, http.StatusBadRequest, err)
			return
		}
		writeJSON(response, http.StatusCreated, movie)
	default:
		methodNotAllowed(response)
	}
}

func (s *Server) handleMovieResolve(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	if s.identityStore == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("identity metadata store is not configured"))
		return
	}
	var payload movieResolveRequest
	if err := decodeJSON(response, request, &payload); err != nil {
		return
	}
	title := strings.TrimSpace(payload.Title)
	resolution := metadata.Resolution{Title: title, Year: payload.Year}
	if title == "" {
		resolution = metadata.ResolveFilename(payload.FileName)
		title = resolution.Title
	}
	if payload.Year != nil {
		resolution.Year = payload.Year
	}
	if title == "" {
		writeError(response, http.StatusBadRequest, fmt.Errorf("title or file_name is required"))
		return
	}
	media := model.Media{MovieID: payload.MovieID, Title: payload.Title, Year: resolution.Year}
	if strings.TrimSpace(payload.FileName) != "" {
		if strings.TrimSpace(media.Title) == "" {
			media.Title = payload.FileName
		} else {
			media.Metadata = map[string]any{"remote_name": payload.FileName}
		}
	}
	movie, found := identity.ResolveMovieForMedia(s.identityStore, media)
	writeJSON(response, http.StatusOK, map[string]any{"resolution": resolution, "found": found, "movie": optionalMovie(movie, found)})
}

func (s *Server) handleMoviePrepare(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	if s.moviePreparer == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("movie preparation service is not configured"))
		return
	}
	var payload movieResolveRequest
	if err := decodeJSON(response, request, &payload); err != nil {
		return
	}
	result, err := s.moviePreparer.Prepare(request.Context(), identity.MoviePreparationRequest{MovieID: payload.MovieID, Title: payload.Title, FileName: payload.FileName, Year: payload.Year})
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) handleMovieByID(response http.ResponseWriter, request *http.Request) {
	if s.identityStore == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("identity metadata store is not configured"))
		return
	}
	path := strings.TrimPrefix(request.URL.Path, "/v1/metadata/movies/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(response, request)
		return
	}
	movieID, err := url.PathUnescape(parts[0])
	if err != nil || movieID == "" {
		http.NotFound(response, request)
		return
	}
	if len(parts) == 2 && parts[1] == "cast" {
		if request.Method != http.MethodPost {
			methodNotAllowed(response)
			return
		}
		var payload movieCastRequest
		if err := decodeJSON(response, request, &payload); err != nil {
			return
		}
		if _, ok := s.identityStore.GetMovie(movieID); !ok {
			writeError(response, http.StatusNotFound, fmt.Errorf("movie not found"))
			return
		}
		for index := range payload.Cast {
			person, personOK := s.identityStore.GetPerson(payload.Cast[index].PersonID)
			if !personOK || strings.TrimSpace(person.IMDbID) == "" {
				writeError(response, http.StatusBadRequest, fmt.Errorf("cast person %q is not an IMDb person", payload.Cast[index].PersonID))
				return
			}
			payload.Cast[index].Source = "imdb_principals"
		}
		if err := s.identityStore.ReplaceMovieCast(movieID, payload.Cast); err != nil {
			writeError(response, http.StatusBadRequest, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"movie_id": movieID, "cast": s.identityStore.GetMovieCast(movieID)})
		return
	}
	if len(parts) == 2 && parts[1] == "sync" {
		if request.Method != http.MethodPost {
			methodNotAllowed(response)
			return
		}
		if s.tmdb == nil || !s.tmdb.Configured() {
			writeError(response, http.StatusServiceUnavailable, fmt.Errorf("TMDB_API_KEY is not configured"))
			return
		}
		var payload movieResolveRequest
		if request.ContentLength != 0 {
			if err := decodeJSON(response, request, &payload); err != nil {
				return
			}
		}
		stored, storedOK := s.identityStore.GetMovie(movieID)
		if !storedOK {
			writeError(response, http.StatusNotFound, fmt.Errorf("movie not found"))
			return
		}
		if payload.Title == "" && storedOK {
			payload.Title, payload.Year, payload.TMDBID = stored.Title, stored.Year, stored.TMDBID
		}
		result, syncErr := s.tmdb.SyncMovie(request.Context(), payload.Title, payload.Year, payload.TMDBID)
		if syncErr != nil {
			writeError(response, http.StatusBadGateway, syncErr)
			return
		}
		result.Movie.ID = movieID
		if result.Movie.IMDbID == "" {
			result.Movie.IMDbID = stored.IMDbID
		}
		preparer := identity.NewMoviePreparationService(s.identityStore, s.tmdb, nil, 5, 8, false)
		if err := preparer.ApplyTMDBResult(&stored, result); err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
		// This endpoint intentionally remains the lightweight administrative
		// sync (top cast only). The import preflight will upgrade it to the
		// complete cast/profile snapshot before a movie is submitted.
		stored.TMDBStatus = identity.TMDBStatusPartial
		stored.Metadata = mergeMetadata(stored.Metadata, map[string]any{"tmdb_sync_complete": false, "tmdb_full_cast": false, "tmdb_sync_scope": "top_cast"})
		if err := s.identityStore.UpsertMovie(stored); err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
		result.Movie = stored
		result.Cast = s.identityStore.GetMovieCast(movieID)
		writeJSON(response, http.StatusOK, result)
		return
	}
	if len(parts) != 1 || request.Method != http.MethodGet {
		methodNotAllowed(response)
		return
	}
	movie, ok := s.identityStore.GetMovie(movieID)
	if !ok {
		writeError(response, http.StatusNotFound, fmt.Errorf("movie not found"))
		return
	}
	cast := s.identityStore.GetMovieCast(movieID)
	people := make([]model.Person, 0, len(cast))
	seenPeople := make(map[string]struct{}, len(cast))
	for _, member := range cast {
		personID := strings.TrimSpace(member.PersonID)
		if personID == "" {
			continue
		}
		if _, seen := seenPeople[personID]; seen {
			continue
		}
		person, personOK := s.identityStore.GetPerson(personID)
		if !personOK {
			continue
		}
		seenPeople[personID] = struct{}{}
		people = append(people, person)
	}
	writeJSON(response, http.StatusOK, map[string]any{"movie": movie, "cast": cast, "people": people})
}

func (s *Server) handlePersonCollection(response http.ResponseWriter, request *http.Request) {
	if s.identityStore == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("identity metadata store is not configured"))
		return
	}
	switch request.Method {
	case http.MethodGet:
		query := strings.TrimSpace(request.URL.Query().Get("q"))
		if query == "" {
			query = strings.TrimSpace(request.URL.Query().Get("query"))
		}
		limitValue := strings.TrimSpace(request.URL.Query().Get("limit"))
		limit := 20
		if limitValue != "" {
			parsed, err := strconv.Atoi(limitValue)
			if err != nil || parsed < 1 {
				writeError(response, http.StatusBadRequest, fmt.Errorf("limit must be a positive integer"))
				return
			}
			limit = parsed
		}
		if limit > 100 {
			limit = 100
		}
		if catalog, ok := s.identityStore.(identity.PersonCatalog); ok {
			if scoped, scopedOK := s.identityStore.(identity.ProcessedMoviePersonSearcher); scopedOK {
				writeJSON(response, http.StatusOK, scoped.SearchReadyPersonsForMovies(query, limit, s.processedMovieIDs()))
				return
			}
			writeJSON(response, http.StatusOK, catalog.SearchReadyPersons(query, limit))
			return
		}
		// Keep a safe fallback for alternative identity stores that predate the
		// readiness-aware catalog interface.
		if searcher, ok := s.identityStore.(identity.PersonSearcher); ok {
			writeJSON(response, http.StatusOK, searcher.SearchPersons(query, limit))
			return
		}
		writeJSON(response, http.StatusOK, s.identityStore.ListPersons())
	case http.MethodPost:
		var person model.Person
		if err := decodeJSON(response, request, &person); err != nil {
			return
		}
		if strings.TrimSpace(person.ID) == "" {
			person.ID = metadata.EntityID("person", person.TMDBID, person.IMDbID, person.Name, nil)
		}
		if strings.TrimSpace(person.IMDbID) == "" {
			writeError(response, http.StatusBadRequest, fmt.Errorf("person must originate from an IMDb actor relation"))
			return
		}
		if err := s.identityStore.UpsertPerson(person); err != nil {
			writeError(response, http.StatusBadRequest, err)
			return
		}
		writeJSON(response, http.StatusCreated, person)
	default:
		methodNotAllowed(response)
	}
}

func (s *Server) processedMovieIDs() []string {
	if s == nil || s.store == nil || s.identityStore == nil {
		return []string{}
	}
	mediaList := s.store.ListMedia()
	signature := processedMediaSignature(mediaList)
	s.processedMovieMu.Lock()
	defer s.processedMovieMu.Unlock()
	if s.processedMovieCacheValid && s.processedMovieSignature == signature {
		return append([]string(nil), s.processedMovieCache...)
	}
	seen := make(map[string]struct{})
	for _, media := range mediaList {
		// ListMedia returns the stored scene records, so a non-empty scene set is
		// the durable evidence that this media completed frame extraction/indexing.
		if len(media.Scenes) == 0 {
			continue
		}
		if movie, ok := identity.ResolveMovieForMedia(s.identityStore, media); ok && movie.ID != "" {
			seen[movie.ID] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for movieID := range seen {
		result = append(result, movieID)
	}
	sort.Strings(result)
	s.processedMovieSignature = signature
	s.processedMovieCache = append([]string(nil), result...)
	s.processedMovieCacheValid = true
	return append([]string(nil), result...)
}

func processedMediaSignature(mediaList []model.Media) string {
	var builder strings.Builder
	for _, media := range mediaList {
		builder.WriteString(media.MediaID)
		builder.WriteByte('\x00')
		builder.WriteString(media.MovieID)
		builder.WriteByte('\x00')
		builder.WriteString(media.Title)
		builder.WriteByte('\x00')
		if media.Year != nil {
			builder.WriteString(strconv.Itoa(*media.Year))
		}
		builder.WriteByte('\x00')
		if media.Metadata != nil {
			if movieID, ok := media.Metadata["movie_id"].(string); ok {
				builder.WriteString(movieID)
			}
		}
		builder.WriteByte('\x00')
		builder.WriteString(strconv.Itoa(len(media.Scenes)))
		builder.WriteByte('\x01')
	}
	return builder.String()
}

func (s *Server) handlePersonByID(response http.ResponseWriter, request *http.Request) {
	if s.identityStore == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("identity metadata store is not configured"))
		return
	}
	personID := strings.TrimPrefix(request.URL.Path, "/v1/metadata/persons/")
	personID, err := url.PathUnescape(strings.Trim(personID, "/"))
	if err != nil || personID == "" || strings.Contains(personID, "/") {
		http.NotFound(response, request)
		return
	}
	if request.Method != http.MethodGet {
		methodNotAllowed(response)
		return
	}
	person, ok := s.identityStore.GetPerson(personID)
	if !ok {
		writeError(response, http.StatusNotFound, fmt.Errorf("person not found"))
		return
	}
	writeJSON(response, http.StatusOK, person)
}

type personImagesRequest struct {
	Images []model.PersonImage `json:"images,omitempty"`
}

type faceImageResult struct {
	model.PersonImage
	HasVector bool `json:"has_vector"`
}

func (s *Server) handlePersonFaces(response http.ResponseWriter, request *http.Request) {
	if s.identityStore == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("identity metadata store is not configured"))
		return
	}
	path := strings.TrimPrefix(request.URL.Path, "/v1/persons/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] != "faces" {
		http.NotFound(response, request)
		return
	}
	personID, err := url.PathUnescape(parts[0])
	if err != nil || personID == "" {
		http.NotFound(response, request)
		return
	}
	if _, ok := s.identityStore.GetPerson(personID); !ok {
		writeError(response, http.StatusNotFound, fmt.Errorf("person not found"))
		return
	}
	switch {
	case len(parts) == 2 && request.Method == http.MethodGet:
		s.writePersonImages(response, personID)
	case len(parts) == 2 && request.Method == http.MethodPost:
		var payload personImagesRequest
		if err := decodeJSON(response, request, &payload); err != nil {
			return
		}
		if len(payload.Images) == 0 {
			writeError(response, http.StatusBadRequest, fmt.Errorf("images must contain at least one item"))
			return
		}
		s.processPersonImages(response, request, personID, payload.Images)
	case len(parts) == 3 && (parts[2] == "sync" || parts[2] == "rebuild") && request.Method == http.MethodPost:
		if parts[2] == "sync" {
			var payload personImagesRequest
			if request.ContentLength != 0 {
				if err := decodeJSON(response, request, &payload); err != nil {
					return
				}
			}
			if len(payload.Images) == 0 {
				writeError(response, http.StatusBadRequest, fmt.Errorf("sync requires images with local_path or source_url"))
				return
			}
			s.processPersonImages(response, request, personID, payload.Images)
			return
		}
		images := s.identityStore.ListPersonImages(personID)
		if len(images) == 0 {
			writeError(response, http.StatusBadRequest, fmt.Errorf("person has no reference images"))
			return
		}
		s.processPersonImages(response, request, personID, images)
	default:
		http.NotFound(response, request)
	}
}

func (s *Server) writePersonImages(response http.ResponseWriter, personID string) {
	images := s.identityStore.ListPersonImages(personID)
	vectors := make(map[string]struct{})
	for _, vector := range s.identityStore.ListFaceVectors(personID) {
		vectors[vector.ImageID] = struct{}{}
	}
	result := make([]faceImageResult, 0, len(images))
	for _, image := range images {
		_, hasVector := vectors[image.ID]
		result = append(result, faceImageResult{PersonImage: image, HasVector: hasVector})
	}
	writeJSON(response, http.StatusOK, map[string]any{"person_id": personID, "images": result})
}

func (s *Server) processPersonImages(response http.ResponseWriter, request *http.Request, personID string, images []model.PersonImage) {
	if s.identityTagger == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("identity service is not configured"))
		return
	}
	if _, ok := s.identityTagger.(*identity.TaggerService); !ok {
		writeError(response, http.StatusNotImplemented, fmt.Errorf("reference face ingestion is not supported by this identity adapter"))
		return
	}
	// Reference ingestion is kept on the server so it can persist the image
	// lifecycle and one selected face vector atomically from the user's point
	// of view. The tagger exposes the model client through this helper.
	faceClient := identityClient(s.identityTagger)
	if faceClient == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("identity face client is not configured"))
		return
	}
	config := identity.ConfigFromEnv()
	results := make([]faceImageResult, 0, len(images))
	failures := make([]map[string]string, 0)
	for _, image := range images {
		if image.ID == "" {
			image.ID, _ = model.NewMediaID()
		}
		image.PersonID = personID
		path, pathErr := s.prepareReferenceImage(request.Context(), personID, image)
		if pathErr != nil {
			image.Status = "failed"
			_ = s.identityStore.UpsertPersonImage(image)
			failures = append(failures, map[string]string{"image_id": image.ID, "error": pathErr.Error()})
			results = append(results, faceImageResult{PersonImage: image})
			continue
		}
		image.LocalPath = path
		faceResult, embedErr := faceClient.Embed(request.Context(), path)
		image.FaceCount = len(faceResult.Faces)
		if embedErr != nil {
			image.Status = "failed"
			_ = s.identityStore.UpsertPersonImage(image)
			failures = append(failures, map[string]string{"image_id": image.ID, "error": embedErr.Error()})
			results = append(results, faceImageResult{PersonImage: image})
			continue
		}
		if len(faceResult.Faces) != 1 || faceResult.Faces[0].DetScore < config.MinDetScore {
			image.Status = "rejected"
			_ = s.identityStore.DeleteFaceVector(image.ID)
			_ = s.identityStore.UpsertPersonImage(image)
			failures = append(failures, map[string]string{"image_id": image.ID, "error": "reference must contain exactly one face with sufficient detection score"})
			results = append(results, faceImageResult{PersonImage: image})
			continue
		}
		face := faceResult.Faces[0]
		image.QualityScore = face.Quality
		image.Status = "ready"
		if err := s.identityStore.UpsertPersonImage(image); err != nil {
			failures = append(failures, map[string]string{"image_id": image.ID, "error": err.Error()})
			continue
		}
		if err := s.identityStore.UpsertFaceVector(model.FaceVector{ID: image.ID, PersonID: personID, ImageID: image.ID, Quality: face.Quality, Model: faceResult.Model, Vector: face.Embedding}); err != nil {
			failures = append(failures, map[string]string{"image_id": image.ID, "error": err.Error()})
			continue
		}
		results = append(results, faceImageResult{PersonImage: image, HasVector: true})
	}
	writeJSON(response, http.StatusOK, map[string]any{"person_id": personID, "images": results, "failures": failures})
}

func identityClient(tagger identity.Tagger) identity.Client {
	service, ok := tagger.(*identity.TaggerService)
	if !ok {
		return nil
	}
	return service.Client()
}

func (s *Server) prepareReferenceImage(ctx context.Context, personID string, image model.PersonImage) (string, error) {
	if strings.TrimSpace(image.LocalPath) != "" {
		path := acquisition.NormalizeLocalPath(image.LocalPath)
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return "", fmt.Errorf("reference image is not a non-empty file")
		}
		return filepath.Abs(path)
	}
	if strings.TrimSpace(image.SourceURL) == "" {
		return "", fmt.Errorf("local_path or source_url is required")
	}
	client := httpclient.NewDirectClient(30 * time.Second)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, image.SourceURL, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("reference image returned HTTP %d", response.StatusCode)
	}
	root := filepath.Join("data", "identity-faces", personID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(root, "reference-*.image")
	if err != nil {
		return "", err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := io.CopyN(temporary, io.LimitReader(response.Body, 20<<20), 20<<20); err != nil && err != io.EOF {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	finalPath := strings.TrimSuffix(name, ".image") + filepath.Ext(image.SourceURL)
	if filepath.Ext(finalPath) == "" {
		finalPath += ".jpg"
	}
	if err := os.Rename(name, finalPath); err != nil {
		return "", err
	}
	return finalPath, nil
}

func (s *Server) handlePersonRebuild(response http.ResponseWriter, request *http.Request, rawMediaID string) {
	if s.jobs == nil || s.identityTagger == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("identity task service is not configured"))
		return
	}
	mediaID, err := url.PathUnescape(rawMediaID)
	if err != nil || mediaID == "" {
		writeError(response, http.StatusNotFound, fmt.Errorf("media not found"))
		return
	}
	task, err := s.jobs.RebuildPersons(mediaID)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusAccepted, task)
}

func optionalMovie(movie model.Movie, found bool) any {
	if !found {
		return nil
	}
	return movie
}

type fileValidationRequest struct {
	Paths []string `json:"paths"`
}

type fileScanRequest struct {
	Directory string `json:"directory"`
	Recursive bool   `json:"recursive,omitempty"`
}

type fileInspectRequest struct {
	Path string `json:"path"`
}

type fileInspection struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Available bool   `json:"available"`
	Size      int64  `json:"size,omitempty"`
	Error     string `json:"error,omitempty"`
}

type fileValidation struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Size      int64  `json:"size,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (s *Server) handleFileInspect(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	var payload fileInspectRequest
	if err := decodeJSON(response, request, &payload); err != nil {
		return
	}
	if strings.TrimSpace(payload.Path) == "" {
		writeError(response, http.StatusBadRequest, fmt.Errorf("path is required"))
		return
	}
	writeJSON(response, http.StatusOK, inspectSource(payload.Path))
}

func inspectSource(candidate string) fileInspection {
	trimmed := acquisition.NormalizeLocalPath(candidate)
	result := fileInspection{Path: trimmed, Name: filepath.Base(trimmed)}
	if trimmed == "" {
		result.Error = "path is required"
		return result
	}
	path, err := filepath.Abs(trimmed)
	if err != nil {
		result.Error = fmt.Sprintf("resolve path: %v", err)
		return result
	}
	result.Path = path
	result.Name = filepath.Base(path)
	info, err := os.Stat(path)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if info.IsDir() {
		result.Kind = "directory"
		result.Available = true
		return result
	}
	result.Kind = "file"
	if !info.Mode().IsRegular() {
		result.Error = "path must be a regular file"
		return result
	}
	result.Size = info.Size()
	if info.Size() == 0 {
		result.Error = "file is empty"
		return result
	}
	result.Available = true
	return result
}

func (s *Server) handleFileValidation(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	var payload fileValidationRequest
	if err := decodeJSON(response, request, &payload); err != nil {
		return
	}
	if len(payload.Paths) == 0 || len(payload.Paths) > 100 {
		writeError(response, http.StatusBadRequest, fmt.Errorf("paths must contain between 1 and 100 items"))
		return
	}
	files := make([]fileValidation, 0, len(payload.Paths))
	for _, candidate := range payload.Paths {
		files = append(files, inspectFile(candidate))
	}
	writeJSON(response, http.StatusOK, map[string]any{"files": files})
}

func (s *Server) handleFileScan(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	var payload fileScanRequest
	if err := decodeJSON(response, request, &payload); err != nil {
		return
	}
	normalizedDirectory := acquisition.NormalizeLocalPath(payload.Directory)
	directory, err := filepath.Abs(normalizedDirectory)
	if err != nil || strings.TrimSpace(normalizedDirectory) == "" {
		writeError(response, http.StatusBadRequest, fmt.Errorf("directory is required"))
		return
	}
	info, err := os.Stat(directory)
	if err != nil {
		writeError(response, http.StatusBadRequest, fmt.Errorf("stat directory: %w", err))
		return
	}
	if !info.IsDir() {
		writeError(response, http.StatusBadRequest, fmt.Errorf("directory must be a directory"))
		return
	}
	paths, err := videoPaths(directory, payload.Recursive)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	files := make([]fileValidation, 0, len(paths))
	for _, path := range paths {
		files = append(files, inspectFile(path))
	}
	writeJSON(response, http.StatusOK, map[string]any{"directory": directory, "files": files})
}

func inspectFile(candidate string) fileValidation {
	trimmed := acquisition.NormalizeLocalPath(candidate)
	result := fileValidation{Path: trimmed, Name: filepath.Base(trimmed)}
	path, err := (acquisition.Request{LocalPath: trimmed}).Validate()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	info, err := os.Stat(path)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Path = path
	result.Name = info.Name()
	result.Size = info.Size()
	result.Available = true
	return result
}

func videoPaths(directory string, recursive bool) ([]string, error) {
	const maxFiles = 500
	paths := make([]string, 0)
	isVideo := func(path string) bool {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".mp4", ".mkv", ".mov", ".avi", ".webm", ".m4v", ".ts", ".flv", ".wmv", ".mpeg", ".mpg":
			return true
		default:
			return false
		}
	}
	if recursive {
		err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if path != directory && strings.HasPrefix(entry.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if isVideo(path) {
				paths = append(paths, path)
				if len(paths) > maxFiles {
					return fmt.Errorf("directory contains more than %d video files", maxFiles)
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan directory: %w", err)
		}
	} else {
		entries, err := os.ReadDir(directory)
		if err != nil {
			return nil, fmt.Errorf("read directory: %w", err)
		}
		for _, entry := range entries {
			if !entry.IsDir() && isVideo(entry.Name()) {
				paths = append(paths, filepath.Join(directory, entry.Name()))
				if len(paths) > maxFiles {
					return nil, fmt.Errorf("directory contains more than %d video files", maxFiles)
				}
			}
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func (s *Server) handleSearch(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response)
		return
	}
	var searchRequest model.SearchRequest
	if err := decodeJSON(response, request, &searchRequest); err != nil {
		return
	}
	if strings.TrimSpace(searchRequest.Query) == "" {
		writeError(response, http.StatusBadRequest, fmt.Errorf("query is required"))
		return
	}
	if strings.TrimSpace(searchRequest.PersonID) == "" && strings.TrimSpace(searchRequest.Person) != "" {
		if s.identityStore == nil {
			writeError(response, http.StatusServiceUnavailable, fmt.Errorf("identity metadata store is not configured"))
			return
		}
		person, ok := s.identityStore.FindPerson(searchRequest.Person)
		if !ok {
			writeError(response, http.StatusBadRequest, fmt.Errorf("person %q not found", searchRequest.Person))
			return
		}
		if catalog, ok := s.identityStore.(identity.PersonCatalog); ok && catalog.FaceVectorCount(person.ID) == 0 {
			writeError(response, http.StatusBadRequest, fmt.Errorf("person %q has no completed face vector bank", searchRequest.Person))
			return
		}
		searchRequest.PersonID = person.ID
	}
	result, err := s.engine.Search(request.Context(), searchRequest)
	if err != nil {
		writeError(response, http.StatusBadGateway, fmt.Errorf("search: %w", err))
		return
	}
	// Attach the true source geometry so the player can ignore broken
	// SAR/DAR flags in the stream (e.g. 2.2:1 rips flagged DAR 16:9 that
	// Chromium would otherwise stretch).
	for index := range result.Results {
		media, ok := s.store.GetMedia(result.Results[index].MediaID)
		if !ok {
			continue
		}
		result.Results[index].Width = metadataInt(media.Metadata, "width")
		result.Results[index].Height = metadataInt(media.Metadata, "height")
	}
	writeJSON(response, http.StatusOK, result)
}

func mergeMetadata(existing, incoming map[string]any) map[string]any {
	if len(existing) == 0 && len(incoming) == 0 {
		return nil
	}
	result := make(map[string]any, len(existing)+len(incoming))
	for key, value := range existing {
		result[key] = value
	}
	for key, value := range incoming {
		result[key] = value
	}
	return result
}

// metadataInt reads an integer from decode-produced metadata (numbers arrive
// as float64) without failing on absent or malformed values.
func metadataInt(metadata map[string]any, key string) int {
	switch value := metadata[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	case json.Number:
		parsed, err := value.Int64()
		if err == nil {
			return int(parsed)
		}
	}
	return 0
}

func decodeJSON(response http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(response, request.Body, 16<<20)
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(target); err != nil {
		writeError(response, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return err
	}
	return nil
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func writeError(response http.ResponseWriter, status int, err error) {
	writeJSON(response, status, map[string]string{"error": err.Error()})
}

func methodNotAllowed(response http.ResponseWriter) {
	response.Header().Set("Allow", "GET, POST, DELETE")
	writeError(response, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
}
