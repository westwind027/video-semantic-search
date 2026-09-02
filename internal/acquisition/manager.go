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
	"strings"
	"sync"
	"time"

	"video-semantic-search/internal/model"
	"video-semantic-search/internal/search"
)

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
	LocalPath          string       `json:"local_path,omitempty"`
	Source             string       `json:"source,omitempty"`
	DriveID            string       `json:"drive_id,omitempty"`
	FileID             string       `json:"file_id,omitempty"`
	SourceName         string       `json:"source_name,omitempty"`
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

	mu          sync.RWMutex
	tasks       map[string]Task
	cancel      map[string]context.CancelFunc
	lastPersist time.Time
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
	manager := &Manager{processor: processor, engine: engine, taskFile: taskFile, tasks: make(map[string]Task), cancel: make(map[string]context.CancelFunc)}
	manager.loadTasks()
	return manager
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
	contentFingerprint := strings.TrimSpace(request.SourceFingerprint)
	if request.IsRemote() {
		if contentFingerprint == "" {
			contentFingerprint = fmt.Sprintf("alipan:%s:%s", strings.TrimSpace(request.DriveID), strings.TrimSpace(request.FileID))
		}
	} else {
		contentFingerprint, err = hashFile(ctx, path)
		if err != nil {
			return Task{}, fmt.Errorf("calculate video SHA-256: %w", err)
		}
	}
	mediaID, err := model.NewMediaID()
	if err != nil {
		return Task{}, fmt.Errorf("create acquisition id: %w", err)
	}
	now := time.Now().UTC()
	task := Task{ID: mediaID, State: "queued", Stage: StageQueued, Percent: 0, Message: "任务已创建", MediaID: mediaID, LocalPath: path, Source: request.Source, DriveID: request.DriveID, FileID: request.FileID, SourceName: request.SourceName, CreatedAt: now, UpdatedAt: now}
	if request.IsRemote() {
		task.ContentFingerprint = contentFingerprint
	} else {
		task.ContentSHA256 = contentFingerprint
	}
	m.mu.Lock()
	if existing, ok := m.findRunningTaskLocked(contentFingerprint); ok {
		m.mu.Unlock()
		return existing, nil
	}
	if media, ok := m.engine.FindMediaByFingerprint(contentFingerprint, path); ok {
		if existing, exists := m.tasks[media.MediaID]; exists && existing.State == "completed" {
			m.mu.Unlock()
			return existing, nil
		}
		duplicate := duplicateTask(media, request, contentFingerprint, now)
		m.tasks[duplicate.ID] = duplicate
		m.persistLocked(true)
		m.mu.Unlock()
		return duplicate, nil
	}
	workContext, cancel := context.WithCancel(context.Background())
	m.tasks[task.ID] = task
	m.cancel[task.ID] = cancel
	m.persistLocked(true)
	m.mu.Unlock()
	go m.run(task.ID, request, workContext)
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
	result := make([]Task, 0, len(m.tasks))
	for _, task := range m.tasks {
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
	return task, true
}

func (m *Manager) run(taskID string, request Request, workContext context.Context) {
	defer func() {
		m.mu.Lock()
		delete(m.cancel, taskID)
		m.mu.Unlock()
	}()
	m.update(taskID, Progress{Stage: StageParsing, Percent: 0.02, Message: "任务开始"})
	media, err := m.processor.Process(workContext, taskID, request, func(progress Progress) {
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
		task.UpdatedAt = time.Now().UTC()
		m.tasks[taskID] = task
		m.persistLocked(true)
	}
	m.mu.Unlock()
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
