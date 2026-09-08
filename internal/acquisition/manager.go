package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"video-semantic-search/internal/embedding"
	"video-semantic-search/internal/identity"
	"video-semantic-search/internal/model"
	"video-semantic-search/internal/search"
)

const (
	defaultTaskWorkers = 2
	taskQueueCapacity  = 1024
)

type jobKind string

const (
	jobKindAcquisition jobKind = "acquisition"
	jobKindRebuild     jobKind = "embedding_rebuild"
	jobKindIdentity    jobKind = "identity_rebuild"
)

// queuedJob is one task waiting for a worker. Submission only enqueues cheap
// records; the heavy work happens in the worker that claims it.
type queuedJob struct {
	kind         jobKind
	taskID       string
	request      Request
	mediaID      string
	imageProfile embedding.ImageProfile
	identity     bool
	ctx          context.Context
	cancel       context.CancelFunc
}

type fileHashFunc func(context.Context, string) (string, error)

type Task struct {
	ID                 string       `json:"id"`
	State              string       `json:"state"`
	Stage              Stage        `json:"stage"`
	Percent            float32      `json:"percent"`
	Message            string       `json:"message,omitempty"`
	MediaID            string       `json:"media_id,omitempty"`
	SceneCount         int          `json:"scene_count,omitempty"`
	EmbeddingModel     string       `json:"embedding_model,omitempty"`
	EmbeddingDimension int          `json:"embedding_dimension,omitempty"`
	EmbeddingProfile   string       `json:"embedding_profile,omitempty"`
	Operation          string       `json:"operation,omitempty"`
	LocalPath          string       `json:"local_path,omitempty"`
	Source             string       `json:"source,omitempty"`
	DriveID            string       `json:"drive_id,omitempty"`
	FileID             string       `json:"file_id,omitempty"`
	SourceName         string       `json:"source_name,omitempty"`
	MovieID            string       `json:"movie_id,omitempty"`
	ContentSHA256      string       `json:"content_sha256,omitempty"`
	ContentFingerprint string       `json:"content_fingerprint,omitempty"`
	Media              *model.Media `json:"media,omitempty"`
	Error              string       `json:"error,omitempty"`
	CreatedAt          time.Time    `json:"created_at"`
	UpdatedAt          time.Time    `json:"updated_at"`
}

type Manager struct {
	processor Processor
	engine    *search.Engine
	taskFile  string
	workers   int
	identity  identity.Tagger
	preparer  identity.MoviePreparer

	mu          sync.RWMutex
	tasks       map[string]Task
	cancel      map[string]context.CancelFunc
	queue       chan queuedJob
	subscribers map[chan TaskEvent]struct{}
	hasher      fileHashFunc
	lastPersist time.Time
}

// SetIdentityTagger enables optional cast-filtered face tagging. Keeping this
// as a setter preserves the existing constructor/API for deployments that do
// not run the Python identity service yet.
func (m *Manager) SetIdentityTagger(tagger identity.Tagger) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.identity = tagger
	m.mu.Unlock()
}

// SetMoviePreparer attaches the metadata/readiness gate to the worker side of
// acquisition. Submission stays cheap: a queued task is allowed to exist even
// when TMDB or Identity is slow or temporarily unavailable.
func (m *Manager) SetMoviePreparer(preparer identity.MoviePreparer) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.preparer = preparer
	m.mu.Unlock()
}

func NewManager(processor Processor, engine *search.Engine) *Manager {
	return newManager(processor, engine, "")
}

// NewManagerWithTaskFile creates a manager that keeps a lightweight task
// history on disk. The media payload is intentionally omitted from snapshots;
// completed media can always be loaded from the index.
func NewManagerWithTaskFile(processor Processor, engine *search.Engine, taskFile string) *Manager {
	return newManager(processor, engine, taskFile)
}

func newManager(processor Processor, engine *search.Engine, taskFile string) *Manager {
	workers := taskWorkersFromEnv()
	manager := &Manager{
		processor:   processor,
		engine:      engine,
		taskFile:    taskFile,
		workers:     workers,
		tasks:       make(map[string]Task),
		cancel:      make(map[string]context.CancelFunc),
		queue:       make(chan queuedJob, taskQueueCapacity),
		subscribers: make(map[chan TaskEvent]struct{}),
		hasher:      hashFile,
	}
	for i := 0; i < workers; i++ {
		go manager.worker()
	}
	manager.loadTasks()
	return manager
}

// taskWorkersFromEnv bounds how many tasks process concurrently. Every worker
// drives its own ffmpeg/embedding pipeline, so more workers trade throughput
// for CPU, GPU and remote bandwidth contention.
func taskWorkersFromEnv() int {
	value := strings.TrimSpace(os.Getenv("VIDEO_TASK_WORKERS"))
	if value == "" {
		return defaultTaskWorkers
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return defaultTaskWorkers
	}
	return parsed
}

func (m *Manager) Submit(request Request) (Task, error) {
	return m.SubmitContext(context.Background(), request)
}

func (m *Manager) SubmitContext(ctx context.Context, request Request) (Task, error) {
	if m == nil || m.processor == nil || m.engine == nil {
		return Task{}, fmt.Errorf("acquisition manager is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	path, err := request.Validate()
	if err != nil {
		return Task{}, err
	}
	request.LocalPath = path
	// A local content fingerprint is NOT computed here: submission must stay
	// fast so a batch import queues every file immediately, and the worker
	// that claims the task pays for hashing once, right before processing.
	contentFingerprint := strings.TrimSpace(request.SourceFingerprint)
	if request.IsRemote() && contentFingerprint == "" {
		contentFingerprint = fmt.Sprintf("alipan:%s:%s", strings.TrimSpace(request.DriveID), strings.TrimSpace(request.FileID))
	}
	mediaID, err := model.NewMediaID()
	if err != nil {
		return Task{}, fmt.Errorf("create acquisition id: %w", err)
	}
	now := time.Now().UTC()
	task := Task{ID: mediaID, State: "queued", Stage: StageQueued, Percent: 0, Message: "任务已创建，等待处理", MediaID: mediaID, LocalPath: path, Source: request.Source, DriveID: request.DriveID, FileID: request.FileID, SourceName: request.SourceName, MovieID: request.MovieID, CreatedAt: now, UpdatedAt: now}
	if request.IsRemote() {
		task.ContentFingerprint = contentFingerprint
	}
	m.mu.Lock()
	if contentFingerprint != "" {
		if existing, ok := m.findRunningTaskLocked(contentFingerprint); ok {
			m.mu.Unlock()
			return existing, nil
		}
	}
	// Without a fingerprint this only matches legacy index entries stored by
	// local path, which FindMediaByFingerprint supports.
	if media, ok := m.engine.FindMediaByFingerprint(contentFingerprint, path); ok {
		if extractionProfileChanged(media, request, m.defaultFastMode()) {
			// The caller switched the frame-extraction profile（快速采样/
			// 关键帧检测）: re-extract and re-index instead of folding into
			// deduplication. The new run reuses the existing media ID so the
			// store upserts over the old record instead of duplicating it.
			task.MediaID = media.MediaID
			task.Message = "关键帧提取方式已变更，将重新提取并重建索引"
		} else {
			if existing, exists := m.tasks[media.MediaID]; exists && existing.State == "completed" {
				m.mu.Unlock()
				return existing, nil
			}
			duplicate := duplicateTask(media, request, contentFingerprint, now)
			m.tasks[duplicate.ID] = duplicate
			m.persistLocked(true)
			m.publishTaskLocked(duplicate)
			m.mu.Unlock()
			return duplicate, nil
		}
	}
	workContext, cancel := context.WithCancel(context.Background())
	if ahead := len(m.queue); ahead > 0 {
		task.Message = fmt.Sprintf("任务已创建，前方还有 %d 个任务排队", ahead)
	}
	m.tasks[task.ID] = task
	m.cancel[task.ID] = cancel
	m.persistLocked(true)
	select {
	case m.queue <- queuedJob{kind: jobKindAcquisition, taskID: task.ID, request: request, ctx: workContext, cancel: cancel}:
		m.publishTaskLocked(task)
	default:
		delete(m.tasks, task.ID)
		delete(m.cancel, task.ID)
		cancel()
		m.mu.Unlock()
		return Task{}, fmt.Errorf("task queue is full, try again later")
	}
	m.mu.Unlock()
	return task, nil
}

// RebuildEmbeddings queues a vector-only task for an indexed video. It reuses
// the existing extracted frames and never invokes the video processor.
func (m *Manager) RebuildEmbeddings(mediaID string, profile embedding.ImageProfile) (Task, error) {
	if m == nil || m.engine == nil {
		return Task{}, fmt.Errorf("acquisition manager is not configured")
	}
	mediaID = strings.TrimSpace(mediaID)
	if mediaID == "" {
		return Task{}, fmt.Errorf("media_id is required")
	}
	if profile == "" {
		profile = embedding.ImageProfileOriginal
	}
	if profile != embedding.ImageProfileOriginal && profile != embedding.ImageProfileCompressed {
		return Task{}, fmt.Errorf("unsupported image profile %q", profile)
	}
	media, ok := m.engine.GetMedia(mediaID)
	if !ok {
		return Task{}, fmt.Errorf("media %q not found", mediaID)
	}

	now := time.Now().UTC()
	m.mu.Lock()
	for _, existing := range m.tasks {
		if existing.Operation == string(jobKindRebuild) && existing.MediaID == mediaID && (existing.State == "queued" || existing.State == "running") {
			m.mu.Unlock()
			return existing, nil
		}
	}
	taskID, err := model.NewMediaID()
	if err != nil {
		m.mu.Unlock()
		return Task{}, fmt.Errorf("create rebuild task id: %w", err)
	}
	task := Task{
		ID:               taskID,
		State:            "queued",
		Stage:            StageQueued,
		Percent:          0,
		Message:          "等待重新生成嵌入",
		MediaID:          mediaID,
		SceneCount:       len(media.Scenes),
		SourceName:       media.Title,
		EmbeddingProfile: string(profile),
		Operation:        string(jobKindRebuild),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	workContext, cancel := context.WithCancel(context.Background())
	if ahead := len(m.queue); ahead > 0 {
		task.Message = fmt.Sprintf("等待重新生成嵌入，前方还有 %d 个任务排队", ahead)
	}
	m.tasks[task.ID] = task
	m.cancel[task.ID] = cancel
	m.persistLocked(true)
	select {
	case m.queue <- queuedJob{kind: jobKindRebuild, taskID: task.ID, mediaID: mediaID, imageProfile: profile, ctx: workContext, cancel: cancel}:
		m.publishTaskLocked(task)
	default:
		delete(m.tasks, task.ID)
		delete(m.cancel, task.ID)
		cancel()
		m.mu.Unlock()
		return Task{}, fmt.Errorf("task queue is full, try again later")
	}
	m.mu.Unlock()
	return task, nil
}

// RebuildPersons queues identity-only tagging for an indexed video. It reuses
// existing representative frames and never recalculates WeMM vectors.
func (m *Manager) RebuildPersons(mediaID string) (Task, error) {
	if m == nil || m.engine == nil {
		return Task{}, fmt.Errorf("acquisition manager is not configured")
	}
	m.mu.RLock()
	tagger := m.identity
	m.mu.RUnlock()
	if tagger == nil {
		return Task{}, fmt.Errorf("identity service is not configured")
	}
	mediaID = strings.TrimSpace(mediaID)
	if mediaID == "" {
		return Task{}, fmt.Errorf("media_id is required")
	}
	media, ok := m.engine.GetMedia(mediaID)
	if !ok {
		return Task{}, fmt.Errorf("media %q not found", mediaID)
	}
	now := time.Now().UTC()
	m.mu.Lock()
	for _, existing := range m.tasks {
		if existing.Operation == string(jobKindIdentity) && existing.MediaID == mediaID && (existing.State == "queued" || existing.State == "running") {
			m.mu.Unlock()
			return existing, nil
		}
	}
	taskID, err := model.NewMediaID()
	if err != nil {
		m.mu.Unlock()
		return Task{}, fmt.Errorf("create identity task id: %w", err)
	}
	task := Task{ID: taskID, State: "queued", Stage: StageQueued, Message: "等待重建人物标签", MediaID: mediaID, SceneCount: len(media.Scenes), SourceName: media.Title, Operation: string(jobKindIdentity), CreatedAt: now, UpdatedAt: now}
	workContext, cancel := context.WithCancel(context.Background())
	m.tasks[taskID] = task
	m.cancel[taskID] = cancel
	m.persistLocked(true)
	select {
	case m.queue <- queuedJob{kind: jobKindIdentity, taskID: taskID, mediaID: mediaID, ctx: workContext, cancel: cancel, identity: true}:
		m.publishTaskLocked(task)
	default:
		delete(m.tasks, taskID)
		delete(m.cancel, taskID)
		cancel()
		m.mu.Unlock()
		return Task{}, fmt.Errorf("task queue is full, try again later")
	}
	m.mu.Unlock()
	return task, nil
}

func (m *Manager) Get(taskID string) (Task, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	task, ok := m.tasks[taskID]
	return task, ok
}

func (m *Manager) List() []Task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.listLocked()
}

func (m *Manager) listLocked() []Task {
	result := make([]Task, 0, len(m.tasks))
	for _, task := range m.tasks {
		task.Media = nil
		result = append(result, task)
	}
	sort.SliceStable(result, func(left, right int) bool { return result[left].UpdatedAt.After(result[right].UpdatedAt) })
	return result
}

func (m *Manager) Stop(taskID string) (Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok || task.State == "completed" || task.State == "failed" || task.State == "canceled" {
		return Task{}, false
	}
	if cancel := m.cancel[taskID]; cancel != nil {
		cancel()
	}
	task.State = "canceled"
	task.Stage = StageCanceled
	task.Message = "任务已停止"
	task.UpdatedAt = time.Now().UTC()
	m.tasks[taskID] = task
	m.persistLocked(true)
	m.publishTaskLocked(task)
	return task, true
}

// StopMany cancels every queued or running task in taskIDs. It returns the
// IDs that were actually transitioned to canceled, preserving the first
// occurrence order and ignoring duplicate or terminal IDs.
func (m *Manager) StopMany(taskIDs []string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	stopped := make([]string, 0, len(taskIDs))
	seen := make(map[string]struct{}, len(taskIDs))
	now := time.Now().UTC()
	for _, taskID := range taskIDs {
		taskID = strings.TrimSpace(taskID)
		if taskID == "" {
			continue
		}
		if _, exists := seen[taskID]; exists {
			continue
		}
		seen[taskID] = struct{}{}
		task, ok := m.tasks[taskID]
		if !ok || task.State == "completed" || task.State == "failed" || task.State == "canceled" {
			continue
		}
		if cancel := m.cancel[taskID]; cancel != nil {
			cancel()
		}
		task.State = "canceled"
		task.Stage = StageCanceled
		task.Message = "任务已停止"
		task.UpdatedAt = now
		m.tasks[taskID] = task
		m.publishTaskLocked(task)
		stopped = append(stopped, taskID)
	}
	if len(stopped) > 0 {
		m.persistLocked(true)
	}
	return stopped
}

func (m *Manager) worker() {
	for job := range m.queue {
		m.execute(job)
	}
}

// execute claims one queued job. A task stopped while it was queued is
// dropped here without ever reaching the processor.
func (m *Manager) execute(job queuedJob) {
	defer func() {
		m.mu.Lock()
		delete(m.cancel, job.taskID)
		m.mu.Unlock()
		job.cancel()
	}()
	m.mu.RLock()
	task, ok := m.tasks[job.taskID]
	m.mu.RUnlock()
	if !ok || task.State != "queued" {
		return
	}
	if job.kind == jobKindRebuild {
		m.runRebuild(job.taskID, job.mediaID, job.imageProfile, job.ctx)
		return
	}
	if job.kind == jobKindIdentity {
		m.runIdentityRebuild(job.taskID, job.mediaID, job.ctx)
		return
	}
	m.run(job.taskID, job.request, job.ctx)
}

func (m *Manager) runIdentityRebuild(taskID, mediaID string, workContext context.Context) {
	m.update(taskID, Progress{Stage: StageIdentity, Percent: 0.02, Message: "正在识别人脸并重建人物标签"})
	m.mu.RLock()
	tagger := m.identity
	m.mu.RUnlock()
	media, ok := m.engine.GetMedia(mediaID)
	if !ok || tagger == nil {
		m.fail(taskID, fmt.Errorf("identity media or service is unavailable"))
		return
	}
	var (
		tagged model.Media
		err    error
	)
	if progressTagger, ok := tagger.(identity.ProgressTagger); ok {
		tagged, err = progressTagger.RebuildMediaWithProgress(workContext, media, func(done, total int) {
			percent := float32(0.02)
			if total > 0 {
				percent += 0.81 * float32(done) / float32(total)
			}
			m.update(taskID, Progress{
				Stage:   StageIdentity,
				Percent: percent,
				Message: fmt.Sprintf("正在识别人脸并重建人物标签 %d/%d", done, total),
			})
		})
	} else {
		tagged, err = tagger.RebuildMedia(workContext, media)
	}
	if err != nil {
		if workContext.Err() != nil {
			return
		}
		m.fail(taskID, err)
		return
	}
	m.update(taskID, Progress{Stage: StageIdentity, Percent: 0.96, Message: "正在保存人物标签"})
	if err := m.engine.UpdateScenePeople(tagged); err != nil {
		m.fail(taskID, err)
		return
	}
	m.mu.Lock()
	task, exists := m.tasks[taskID]
	if exists && task.State != "canceled" {
		task.State = "completed"
		task.Stage = StageCompleted
		task.Percent = 1
		task.Message = "已完成所有人物标签重建"
		task.SceneCount = len(tagged.Scenes)
		task.UpdatedAt = time.Now().UTC()
		m.tasks[taskID] = task
		m.persistLocked(true)
		m.publishTaskLocked(task)
	}
	m.mu.Unlock()
}

func (m *Manager) runRebuild(taskID, mediaID string, profile embedding.ImageProfile, workContext context.Context) {
	m.update(taskID, Progress{Stage: StageEmbedding, Percent: 0.02, Message: "正在重新生成视频嵌入"})
	result, err := m.engine.RebuildEmbeddings(workContext, mediaID, profile, func(done, total int) {
		percent := float32(0.02)
		if total > 0 {
			percent += 0.97 * float32(done) / float32(total)
		}
		m.update(taskID, Progress{Stage: StageEmbedding, Percent: percent, Message: fmt.Sprintf("正在重新生成嵌入 %d/%d", done, total)})
	})
	if err != nil {
		if workContext.Err() != nil {
			return
		}
		m.fail(taskID, err)
		return
	}
	m.mu.Lock()
	task, ok := m.tasks[taskID]
	if ok && task.State != "canceled" {
		task.State = "completed"
		task.Stage = StageCompleted
		task.Percent = 1
		task.Message = "已完成视频嵌入重建"
		task.SceneCount = result.SceneCount
		task.EmbeddingModel = result.Model
		task.EmbeddingDimension = result.Dimension
		task.EmbeddingProfile = string(profile)
		if result.Profile != "" {
			task.EmbeddingProfile = result.Profile
		}
		task.UpdatedAt = time.Now().UTC()
		m.tasks[taskID] = task
		m.persistLocked(true)
		m.publishTaskLocked(task)
	}
	m.mu.Unlock()
}

func (m *Manager) run(taskID string, request Request, workContext context.Context) {
	// The content fingerprint of a local file is computed here instead of at
	// submission: submitting a batch must not wait on reading every file, so
	// the worker pays for hashing once, right before processing starts.
	if !request.IsRemote() {
		// Claim the task before touching the whole file. Without this update a
		// multi-gigabyte local file remains visually queued while the worker is
		// already busy reading it.
		m.update(taskID, Progress{Stage: StageHashing, Percent: 0.005, Message: "正在计算文件指纹"})

		// Metadata preparation does not depend on the local file bytes. Run it
		// alongside hashing so TMDB/Identity I/O can overlap the disk read. The
		// hash result is still required before deduplication or video processing.
		hashContext, cancelHash := context.WithCancel(workContext)
		hashResult := make(chan struct {
			digest string
			err    error
		}, 1)
		go func() {
			hasher := m.hasher
			if hasher == nil {
				hasher = hashFile
			}
			digest, err := hasher(hashContext, request.LocalPath)
			hashResult <- struct {
				digest string
				err    error
			}{digest: digest, err: err}
		}()

		prepareErr := m.prepareMovie(taskID, &request, workContext)
		if prepareErr != nil {
			cancelHash()
			if workContext.Err() == nil {
				m.fail(taskID, prepareErr)
			}
			return
		}
		result := <-hashResult
		cancelHash()
		digest, err := result.digest, result.err
		if err != nil {
			if workContext.Err() == nil {
				m.fail(taskID, fmt.Errorf("calculate video SHA-256: %w", err))
			}
			return
		}
		if m.claimFingerprint(taskID, digest, request) {
			return
		}
	} else if err := m.prepareMovie(taskID, &request, workContext); err != nil {
		if workContext.Err() == nil {
			m.fail(taskID, err)
		}
		return
	}
	m.update(taskID, Progress{Stage: StageParsing, Percent: 0.02, Message: "任务开始"})
	// A re-submission that changed the extraction profile keeps the original
	// media ID so indexing overwrites the old record; processing must target
	// that ID（frame directory and store key）rather than the fresh task ID.
	processID := taskID
	m.mu.RLock()
	if pending, ok := m.tasks[taskID]; ok && pending.MediaID != "" {
		processID = pending.MediaID
	}
	m.mu.RUnlock()
	media, err := m.processor.Process(workContext, processID, request, func(progress Progress) {
		m.update(taskID, progress)
	})
	if err != nil {
		if workContext.Err() != nil {
			return
		}
		m.fail(taskID, err)
		return
	}
	if workContext.Err() != nil {
		return
	}
	m.mu.RLock()
	tagger := m.identity
	m.mu.RUnlock()
	if tagger != nil {
		m.update(taskID, Progress{Stage: StageIdentity, Percent: 0.84, Message: "正在根据影片 cast 识别人脸"})
		tagged, tagErr := tagger.TagMedia(workContext, media)
		if tagErr != nil {
			if workContext.Err() != nil {
				return
			}
			m.fail(taskID, tagErr)
			return
		}
		media = tagged
	}
	m.mu.RLock()
	task, taskExists := m.tasks[taskID]
	m.mu.RUnlock()
	fingerprint := taskFingerprint(task)
	if !taskExists || fingerprint == "" {
		m.fail(taskID, fmt.Errorf("task content fingerprint is unavailable"))
		return
	}
	media.Metadata = ensureFingerprintMetadata(media.Metadata, fingerprint, request.IsRemote())
	m.update(taskID, Progress{Stage: StageEmbedding, Percent: 0.88, Message: fmt.Sprintf("正在为 %d 个画面生成嵌入", len(media.Scenes))})
	result, err := m.engine.IndexWithProgress(workContext, media, func(done, total int) {
		percent := float32(0.88)
		if total > 0 {
			percent += 0.11 * float32(done) / float32(total)
		}
		m.update(taskID, Progress{
			Stage:   StageEmbedding,
			Percent: percent,
			Message: fmt.Sprintf("正在为 %d/%d 个画面生成嵌入", done, total),
		})
	})
	if err != nil {
		if workContext.Err() != nil {
			return
		}
		m.fail(taskID, err)
		return
	}
	m.mu.Lock()
	task, ok := m.tasks[taskID]
	if ok && task.State != "canceled" {
		task.State = "completed"
		task.Stage = StageCompleted
		task.Percent = 1
		task.Message = "已完成并加入累计索引"
		task.SceneCount = result.SceneCount
		task.EmbeddingModel = result.Model
		task.EmbeddingDimension = result.Dimension
		task.EmbeddingProfile = result.Profile
		task.UpdatedAt = time.Now().UTC()
		m.tasks[taskID] = task
		m.persistLocked(true)
		m.publishTaskLocked(task)
	}
	m.mu.Unlock()
}

func (m *Manager) prepareMovie(taskID string, request *Request, workContext context.Context) error {
	m.mu.RLock()
	preparer := m.preparer
	m.mu.RUnlock()
	if preparer == nil || request == nil {
		return nil
	}

	fileName := strings.TrimSpace(request.SourceName)
	if fileName == "" && strings.TrimSpace(request.LocalPath) != "" {
		fileName = filepath.Base(request.LocalPath)
	}
	m.update(taskID, Progress{Stage: StagePreparing, Percent: 0.01, Message: "正在准备 IMDb/TMDB/人脸库"})
	prepared, err := preparer.Prepare(workContext, identity.MoviePreparationRequest{
		MovieID:  request.MovieID,
		Title:    request.Title,
		FileName: fileName,
	})
	if err != nil {
		return fmt.Errorf("准备影片元数据: %w", err)
	}
	if !prepared.Found {
		if message := strings.TrimSpace(prepared.Message); message != "" {
			return fmt.Errorf("%s", message)
		}
		return fmt.Errorf("未找到 IMDb 影片元数据")
	}
	if !prepared.Ready {
		if message := strings.TrimSpace(prepared.Message); message != "" {
			return fmt.Errorf("%s", message)
		}
		return fmt.Errorf("影片元数据或人脸向量库尚未完成")
	}
	if strings.TrimSpace(prepared.Movie.ID) == "" {
		return nil
	}
	request.MovieID = prepared.Movie.ID
	m.mu.Lock()
	if task, ok := m.tasks[taskID]; ok && task.State != "canceled" {
		task.MovieID = prepared.Movie.ID
		task.UpdatedAt = time.Now().UTC()
		m.tasks[taskID] = task
		m.persistLocked(true)
		m.publishTaskLocked(task)
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) update(taskID string, progress Progress) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok || task.State == "canceled" {
		return
	}
	task.State = "running"
	task.Stage = progress.Stage
	if progress.Percent >= task.Percent {
		task.Percent = progress.Percent
	}
	if task.Percent > 1 {
		task.Percent = 1
	}
	task.Message = progress.Message
	task.UpdatedAt = time.Now().UTC()
	m.tasks[taskID] = task
	m.persistLocked(false)
	m.publishTaskLocked(task)
}

func (m *Manager) fail(taskID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok || task.State == "canceled" {
		return
	}
	task.State = "failed"
	task.Stage = StageFailed
	task.Message = "处理失败"
	task.Error = err.Error()
	task.UpdatedAt = time.Now().UTC()
	m.tasks[taskID] = task
	m.persistLocked(true)
	m.publishTaskLocked(task)
}

// claimFingerprint records the computed content hash of a local task and folds
// the task into an existing one when the same content is already queued,
// running or indexed. It returns true when the task needs no further work.
func (m *Manager) claimFingerprint(taskID, digest string, request Request) bool {
	m.mu.Lock()
	task, ok := m.tasks[taskID]
	if !ok || task.State == "canceled" || task.State == "failed" || task.State == "completed" {
		// Stopped or finalized while hashing; the state is already final.
		m.mu.Unlock()
		return true
	}
	if existing, ok := m.findRunningTaskLocked(digest); ok && existing.ID != taskID {
		task.State = "canceled"
		task.Stage = StageCanceled
		task.Message = fmt.Sprintf("与任务 %s 内容相同，已跳过", shortTaskID(existing.ID))
		task.UpdatedAt = time.Now().UTC()
		m.tasks[taskID] = task
		m.persistLocked(true)
		m.publishTaskLocked(task)
		m.mu.Unlock()
		return true
	}
	task.ContentSHA256 = digest
	task.UpdatedAt = time.Now().UTC()
	m.tasks[taskID] = task
	m.persistLocked(true)
	m.publishTaskLocked(task)
	// Same lookup pattern as Submit: the engine has its own locking, and the
	// tiny race window just means a duplicate is caught by the next check.
	media, mediaFound := m.engine.FindMediaByFingerprint(digest, request.LocalPath)
	if mediaFound && extractionProfileChanged(media, request, m.defaultFastMode()) {
		// Profile changed since the media was indexed: keep processing and
		// reuse the stored media ID so the new record replaces the old one.
		task.MediaID = media.MediaID
		task.Message = "关键帧提取方式已变更，将重新提取并重建索引"
		m.tasks[taskID] = task
		m.persistLocked(true)
		m.publishTaskLocked(task)
		m.mu.Unlock()
		return false
	}
	if mediaFound {
		task.State = "completed"
		task.Stage = StageCompleted
		task.Percent = 1
		task.Message = "视频内容已存在，已跳过重复处理"
		task.MediaID = media.MediaID
		task.SceneCount = len(media.Scenes)
		task.UpdatedAt = time.Now().UTC()
		m.tasks[taskID] = task
		m.persistLocked(true)
		m.publishTaskLocked(task)
		m.mu.Unlock()
		return true
	}
	m.mu.Unlock()
	return false
}

func shortTaskID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// defaultFastMode returns the processor-wide fast-sampling default. The
// Manager usually holds a *VideoProcessor; test fakes fall back to false.
func (m *Manager) defaultFastMode() bool {
	if vp, ok := m.processor.(*VideoProcessor); ok {
		return vp.FastMode
	}
	return false
}

// Remove deletes one terminal task record. Queued and running tasks must go
// through Stop first so their worker contexts are canceled properly.
func (m *Manager) Remove(taskID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[taskID]
	if !ok || task.State != "completed" && task.State != "failed" && task.State != "canceled" {
		return false
	}
	delete(m.tasks, taskID)
	m.persistLocked(true)
	m.publishTaskRemovedLocked(taskID)
	return true
}

// RemoveMany deletes selected terminal task records in one persistence
// operation. Active tasks are intentionally left untouched; callers must
// stop them first so their worker contexts are canceled properly.
func (m *Manager) RemoveMany(taskIDs []string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	removed := make([]string, 0, len(taskIDs))
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
		task, ok := m.tasks[taskID]
		if !ok || task.State != "completed" && task.State != "failed" && task.State != "canceled" {
			continue
		}
		delete(m.tasks, taskID)
		m.publishTaskRemovedLocked(taskID)
		removed = append(removed, taskID)
	}
	if len(removed) > 0 {
		m.persistLocked(true)
	}
	return removed
}

// Clear deletes every terminal task whose state is listed. Unknown or
// non-terminal states are ignored.
func (m *Manager) Clear(states []string) int {
	removable := make(map[string]bool, len(states))
	for _, state := range states {
		if state == "completed" || state == "failed" || state == "canceled" {
			removable[state] = true
		}
	}
	if len(removable) == 0 {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := 0
	for id, task := range m.tasks {
		if removable[task.State] {
			delete(m.tasks, id)
			m.publishTaskRemovedLocked(id)
			removed++
		}
	}
	if removed > 0 {
		m.persistLocked(true)
	}
	return removed
}

func (m *Manager) findRunningTaskLocked(fingerprint string) (Task, bool) {
	for _, task := range m.tasks {
		if taskFingerprint(task) != fingerprint {
			continue
		}
		if task.State == "queued" || task.State == "running" {
			return task, true
		}
	}
	return Task{}, false
}

func duplicateTask(media model.Media, request Request, fingerprint string, now time.Time) Task {
	task := Task{
		ID:         media.MediaID,
		State:      "completed",
		Stage:      StageCompleted,
		Percent:    1,
		Message:    "视频内容已存在，已跳过重复处理",
		MediaID:    media.MediaID,
		SceneCount: len(media.Scenes),
		LocalPath:  request.LocalPath,
		Source:     request.Source,
		DriveID:    request.DriveID,
		FileID:     request.FileID,
		SourceName: request.SourceName,
		MovieID:    request.MovieID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if request.IsRemote() {
		task.ContentFingerprint = fingerprint
	} else {
		task.ContentSHA256 = fingerprint
	}
	return task
}

func taskFingerprint(task Task) string {
	if strings.TrimSpace(task.ContentFingerprint) != "" {
		return task.ContentFingerprint
	}
	return task.ContentSHA256
}

// desiredFrameSource reports the frame_source metadata value that processing
// this request would produce. It mirrors the choices made by VideoProcessor,
// including the remote special case: an indexed remote container submitted
// with the accurate profile still samples keyframes instead of running a full
// sequential scene scan, and that difference is visible in the frame table.
func desiredFrameSource(request Request, defaultFast bool) string {
	fast := defaultFast || request.FastMode
	switch {
	case request.IsRemote() && !fast:
		return "remote_keyframe_sample"
	case !fast:
		return "ffmpeg_scene_detect"
	default:
		return "uniform_fast_sample"
	}
}

// extractionProfileChanged reports whether the requested frame-extraction
// profile differs from the one recorded on the stored media. Deduplication
// must not fold a re-submission that switches between 快速采样 and 关键帧
// 检测: the user expects a fresh extraction and a rebuilt index.
func extractionProfileChanged(media model.Media, request Request, defaultFast bool) bool {
	desired := desiredFrameSource(request, defaultFast)
	stored, _ := media.Metadata["frame_source"].(string)
	if stored == "" {
		// Legacy records only carry the fast_mode flag, and a remote accurate
		// run is indistinguishable from fast mode without frame_source.
		flag, ok := media.Metadata["fast_mode"].(bool)
		if !ok || (request.IsRemote() && !(defaultFast || request.FastMode)) {
			return false
		}
		return flag != (defaultFast || request.FastMode)
	}
	if desired == "remote_keyframe_sample" {
		return !strings.HasPrefix(stored, "remote_")
	}
	return stored != desired
}

func ensureFingerprintMetadata(metadata map[string]any, fingerprint string, remote bool) map[string]any {
	result := make(map[string]any, len(metadata)+2)
	for key, value := range metadata {
		result[key] = value
	}
	if remote {
		result["content_fingerprint"] = fingerprint
		result["content_fingerprint_source"] = "aliyun_drive_content_hash_or_file_identity"
	} else {
		result["content_sha256"] = fingerprint
		result["content_hash_algorithm"] = "sha256"
	}
	return result
}

func hashFile(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	digest := sha256.New()
	buffer := make([]byte, 1024*1024)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			if _, err := digest.Write(buffer[:count]); err != nil {
				return "", err
			}
		}
		if readErr == io.EOF {
			return hex.EncodeToString(digest.Sum(nil)), nil
		}
		if readErr != nil {
			return "", readErr
		}
	}
}

type taskSnapshot struct {
	Tasks []Task `json:"tasks"`
}

func (m *Manager) loadTasks() {
	if m.taskFile == "" {
		return
	}
	data, err := os.ReadFile(m.taskFile)
	if err != nil {
		return
	}
	var snapshot taskSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return
	}
	changed := false
	for _, task := range snapshot.Tasks {
		if task.ID == "" {
			continue
		}
		if task.State == "queued" || task.State == "running" {
			task.State = "failed"
			task.Stage = StageFailed
			task.Message = "服务重启，任务未完成"
			task.Error = "服务重启，任务未完成"
			task.UpdatedAt = time.Now().UTC()
			changed = true
		}
		task.Media = nil
		m.tasks[task.ID] = task
	}
	if changed {
		m.persistLocked(true)
	}
}

func (m *Manager) persistLocked(force bool) {
	if m.taskFile == "" {
		return
	}
	if !force && !m.lastPersist.IsZero() && time.Since(m.lastPersist) < 500*time.Millisecond {
		return
	}
	tasks := make([]Task, 0, len(m.tasks))
	for _, task := range m.tasks {
		task.Media = nil
		tasks = append(tasks, task)
	}
	sort.SliceStable(tasks, func(left, right int) bool {
		return tasks[left].UpdatedAt.After(tasks[right].UpdatedAt)
	})
	data, err := json.Marshal(taskSnapshot{Tasks: tasks})
	if err != nil {
		return
	}
	directory := filepath.Dir(m.taskFile)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return
	}
	temporary := m.taskFile + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(temporary, m.taskFile); err != nil {
		return
	}
	m.lastPersist = time.Now()
}
