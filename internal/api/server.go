package api

import (
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

	"video-semantic-search/internal/acquisition"
	"video-semantic-search/internal/alipan"
	"video-semantic-search/internal/embedding"
	"video-semantic-search/internal/model"
	"video-semantic-search/internal/search"
	"video-semantic-search/internal/store"
)

type Server struct {
	engine    *search.Engine
	store     store.IndexStore
	embedder  embedding.Client
	jobs      *acquisition.Manager
	frameRoot string
	alipan    *alipan.Manager
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
	return &Server{engine: engine, store: indexStore, embedder: embedder, jobs: jobs, frameRoot: frameRoot, alipan: connector}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/media", s.handleMediaCollection)
	mux.HandleFunc("/v1/media/embeddings/rebuild", s.handleEmbeddingRebuildBatch)
	mux.HandleFunc("/v1/media/batch/delete", s.handleMediaBatchDelete)
	mux.HandleFunc("/v1/media/", s.handleMediaByID)
	mux.HandleFunc("/v1/files/inspect", s.handleFileInspect)
	mux.HandleFunc("/v1/files/validate", s.handleFileValidation)
	mux.HandleFunc("/v1/files/scan", s.handleFileScan)
	mux.HandleFunc("/v1/acquisitions", s.handleAcquisitionCollection)
	mux.HandleFunc("/v1/acquisitions/batch", s.handleAcquisitionBatch)
	mux.HandleFunc("/v1/acquisitions/batch/stop", s.handleAcquisitionBatchStop)
	mux.HandleFunc("/v1/acquisitions/batch/delete", s.handleAcquisitionBatchDelete)
	mux.HandleFunc("/v1/acquisitions/clear", s.handleAcquisitionClear)
	mux.HandleFunc("/v1/acquisitions/", s.handleAcquisitionByID)
	mux.HandleFunc("/v1/connectors/alipan/status", s.handleAlipanStatus)
	mux.HandleFunc("/v1/connectors/alipan/files", s.handleAlipanFiles)
	mux.HandleFunc("/v1/connectors/alipan/login/start", s.handleAlipanLoginStart)
	mux.HandleFunc("/v1/connectors/alipan/login/complete", s.handleAlipanLoginComplete)
	mux.HandleFunc("/v1/connectors/alipan/login/callback", s.handleAlipanLoginCallback)
	mux.HandleFunc("/v1/connectors/alipan/login/", s.handleAlipanLoginResource)
	mux.HandleFunc("/v1/connectors/alipan/logout", s.handleAlipanLogout)
	mux.HandleFunc("/v1/search", s.handleSearch)
	return mux
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
	if len(parts) == 3 && parts[1] == "scenes" && request.Method == http.MethodDelete {
		s.handleSceneDelete(response, parts[0], parts[2])
		return
	}
	if len(parts) == 3 && parts[1] == "embeddings" && parts[2] == "rebuild" && request.Method == http.MethodPost {
		s.handleEmbeddingRebuild(response, request, parts[0])
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
	result, err := s.engine.Search(request.Context(), searchRequest)
	if err != nil {
		writeError(response, http.StatusBadGateway, fmt.Errorf("search: %w", err))
		return
	}
	writeJSON(response, http.StatusOK, result)
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
