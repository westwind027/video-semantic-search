package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"video-semantic-search/internal/model"
)

type SceneDocument struct {
	MediaID            string
	Type               string
	Title              string
	OriginalTitle      string
	Year               *int
	Language           []string
	Description        string
	Tags               []string
	SourceURL          string
	Scene              model.Scene
	Embedding          []float32
	EmbeddingModel     string
	EmbeddingDimension int
}

type Filter struct {
	MediaID   string
	Type      string
	Year      *int
	Language  string
	PersonID  string
	PersonIDs []string
}

type IndexStore interface {
	UpsertMedia(model.Media, [][]float32, string) error
	SearchDocuments(Filter) []SceneDocument
	GetMedia(string) (model.Media, bool)
	ListMedia() []model.Media
	DeleteMedia(string) bool
	Stats() (int, int)
	Close() error
}

// SceneDeleter is an optional management seam. Search adapters only need the
// IndexStore interface; adapters that own frame/vector lifecycle can also
// implement this capability.
type SceneDeleter interface {
	DeleteScene(string, string) (model.Scene, bool)
}

// ScenePeopleStore is the optional identity-management seam. It keeps face
// tags independent from the vector backend while allowing the current JSON
// store to persist them alongside each scene.
type ScenePeopleStore interface {
	UpdateScenePeople(string, string, []string) error
}

// ScenePeopleBatchStore is an optional write path for identity rebuilds. A
// batch avoids rewriting the complete JSON index once per scene.
type ScenePeopleUpdate struct {
	SceneID   string
	PersonIDs []string
}

type ScenePeopleBatchStore interface {
	UpdateScenePeopleBatch(string, []ScenePeopleUpdate) error
}

type storedScene struct {
	MediaID        string      `json:"media_id"`
	Scene          model.Scene `json:"scene"`
	Embedding      []float32   `json:"embedding"`
	EmbeddingModel string      `json:"embedding_model"`
	EmbeddingDim   int         `json:"embedding_dimension"`
}

type fileState struct {
	Version int                    `json:"version"`
	Media   map[string]model.Media `json:"media"`
	Scenes  map[string]storedScene `json:"scenes"`
}

type FileStore struct {
	mu    sync.RWMutex
	path  string
	state fileState
}

func NewFileStore(path string) (*FileStore, error) {
	if path == "" {
		return nil, fmt.Errorf("index path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create index directory: %w", err)
	}
	store := &FileStore{path: path, state: fileState{Version: 1, Media: map[string]model.Media{}, Scenes: map[string]storedScene{}}}
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}
	if len(content) == 0 {
		return store, nil
	}
	if err := json.Unmarshal(content, &store.state); err != nil {
		return nil, fmt.Errorf("decode index: %w", err)
	}
	if store.state.Media == nil {
		store.state.Media = map[string]model.Media{}
	}
	if store.state.Scenes == nil {
		store.state.Scenes = map[string]storedScene{}
	}
	return store, nil
}

func (s *FileStore) UpsertMedia(media model.Media, embeddings [][]float32, embeddingModel string) error {
	if len(media.Scenes) != len(embeddings) {
		return fmt.Errorf("embedding count %d does not match scene count %d", len(embeddings), len(media.Scenes))
	}
	if media.MediaID == "" {
		return fmt.Errorf("media_id is required before indexing")
	}
	if embeddingModel == "" {
		return fmt.Errorf("embedding model is required")
	}
	dimension := 0
	for index, vector := range embeddings {
		if len(vector) == 0 {
			return fmt.Errorf("embedding %d is empty", index)
		}
		if dimension == 0 {
			dimension = len(vector)
		} else if len(vector) != dimension {
			return fmt.Errorf("embedding %d has dimension %d, expected %d", index, len(vector), dimension)
		}
	}
	copyMedia := media
	copyMedia.Scenes = nil
	if copyMedia.Metadata == nil {
		copyMedia.Metadata = map[string]any{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Media[media.MediaID] = copyMedia
	for sceneID, scene := range s.state.Scenes {
		if scene.MediaID == media.MediaID {
			delete(s.state.Scenes, sceneID)
		}
	}
	for index, input := range media.Scenes {
		scene := input
		if scene.SceneID == "" {
			scene.SceneID = stableSceneID(media.MediaID, scene)
		}
		vector := append([]float32(nil), embeddings[index]...)
		s.state.Scenes[scene.SceneID] = storedScene{MediaID: media.MediaID, Scene: scene, Embedding: vector, EmbeddingModel: embeddingModel, EmbeddingDim: len(vector)}
	}
	return s.persistLocked()
}

func stableSceneID(mediaID string, scene model.Scene) string {
	return StableSceneID(mediaID, scene)
}

// StableSceneID is shared with optional scene annotation pipelines that need
// to persist tags before the vector store receives the media record.
func StableSceneID(mediaID string, scene model.Scene) string {
	value := fmt.Sprintf("%s:%0.6f:%0.6f:%s", mediaID, scene.Start, scene.End, scene.Preview)
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:16])
}

func (s *FileStore) SearchDocuments(filter Filter) []SceneDocument {
	s.mu.RLock()
	defer s.mu.RUnlock()
	documents := make([]SceneDocument, 0, len(s.state.Scenes))
	for _, stored := range s.state.Scenes {
		media, ok := s.state.Media[stored.MediaID]
		if !ok || !matchesFilter(media, Filter{MediaID: filter.MediaID, Type: filter.Type, Year: filter.Year, Language: filter.Language}) {
			continue
		}
		if filter.PersonID != "" && !containsString(stored.Scene.PersonIDs, filter.PersonID) {
			continue
		}
		if len(filter.PersonIDs) > 0 && !containsAnyString(stored.Scene.PersonIDs, filter.PersonIDs) {
			continue
		}
		documents = append(documents, SceneDocument{MediaID: media.MediaID, Type: media.Type, Title: media.Title, OriginalTitle: media.OriginalTitle, Year: cloneInt(media.Year), Language: append([]string(nil), media.Language...), Description: media.Description, Tags: append([]string(nil), media.Tags...), SourceURL: media.SourceURL, Scene: stored.Scene, Embedding: append([]float32(nil), stored.Embedding...), EmbeddingModel: stored.EmbeddingModel, EmbeddingDimension: stored.EmbeddingDim})
	}
	sort.Slice(documents, func(left, right int) bool {
		if documents[left].MediaID == documents[right].MediaID {
			return documents[left].Scene.Start < documents[right].Scene.Start
		}
		return documents[left].MediaID < documents[right].MediaID
	})
	return documents
}

func matchesFilter(media model.Media, filter Filter) bool {
	if filter.MediaID != "" && media.MediaID != filter.MediaID {
		return false
	}
	if filter.Type != "" && media.Type != filter.Type {
		return false
	}
	if filter.Year != nil && (media.Year == nil || *media.Year != *filter.Year) {
		return false
	}
	if filter.Language != "" {
		found := false
		for _, language := range media.Language {
			if language == filter.Language {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (s *FileStore) GetMedia(mediaID string) (model.Media, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	media, ok := s.state.Media[mediaID]
	if !ok {
		return model.Media{}, false
	}
	media.Scenes = make([]model.Scene, 0)
	for _, stored := range s.state.Scenes {
		if stored.MediaID == mediaID {
			media.Scenes = append(media.Scenes, stored.Scene)
		}
	}
	sort.Slice(media.Scenes, func(left, right int) bool { return media.Scenes[left].Start < media.Scenes[right].Start })
	return media, true
}

func (s *FileStore) ListMedia() []model.Media {
	s.mu.RLock()
	defer s.mu.RUnlock()
	mediaList := make([]model.Media, 0, len(s.state.Media))
	for _, media := range s.state.Media {
		copyMedia := media
		copyMedia.Language = append([]string(nil), media.Language...)
		copyMedia.Tags = append([]string(nil), media.Tags...)
		copyMedia.Metadata = cloneMetadata(media.Metadata)
		for _, stored := range s.state.Scenes {
			if stored.MediaID == media.MediaID {
				copyMedia.Scenes = append(copyMedia.Scenes, stored.Scene)
			}
		}
		sort.Slice(copyMedia.Scenes, func(left, right int) bool { return copyMedia.Scenes[left].Start < copyMedia.Scenes[right].Start })
		mediaList = append(mediaList, copyMedia)
	}
	sort.Slice(mediaList, func(left, right int) bool { return mediaList[left].MediaID < mediaList[right].MediaID })
	return mediaList
}

func (s *FileStore) DeleteMedia(mediaID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Media[mediaID]; !ok {
		return false
	}
	delete(s.state.Media, mediaID)
	for sceneID, scene := range s.state.Scenes {
		if scene.MediaID == mediaID {
			delete(s.state.Scenes, sceneID)
		}
	}
	return s.persistLocked() == nil
}

func (s *FileStore) DeleteScene(mediaID, sceneID string) (model.Scene, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.state.Scenes[sceneID]
	if !ok || stored.MediaID != mediaID {
		return model.Scene{}, false
	}
	delete(s.state.Scenes, sceneID)
	if err := s.persistLocked(); err != nil {
		s.state.Scenes[sceneID] = stored
		return model.Scene{}, false
	}
	return stored.Scene, true
}

func (s *FileStore) UpdateScenePeople(mediaID, sceneID string, personIDs []string) error {
	return s.UpdateScenePeopleBatch(mediaID, []ScenePeopleUpdate{{SceneID: sceneID, PersonIDs: personIDs}})
}

func (s *FileStore) UpdateScenePeopleBatch(mediaID string, updates []ScenePeopleUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(mediaID) == "" {
		return fmt.Errorf("media id is required")
	}
	if len(updates) == 0 {
		return nil
	}
	previous := make(map[string][]string, len(updates))
	for _, update := range updates {
		stored, ok := s.state.Scenes[update.SceneID]
		if !ok || stored.MediaID != mediaID {
			return fmt.Errorf("scene not found")
		}
		if _, exists := previous[update.SceneID]; !exists {
			previous[update.SceneID] = append([]string(nil), stored.Scene.PersonIDs...)
		}
		stored.Scene.PersonIDs = uniqueStrings(update.PersonIDs)
		s.state.Scenes[update.SceneID] = stored
	}
	if err := s.persistLocked(); err != nil {
		for sceneID, personIDs := range previous {
			stored := s.state.Scenes[sceneID]
			stored.Scene.PersonIDs = personIDs
			s.state.Scenes[sceneID] = stored
		}
		return err
	}
	return nil
}

func (s *FileStore) Stats() (int, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.state.Media), len(s.state.Scenes)
}

func (s *FileStore) Close() error { return nil }

func (s *FileStore) persistLocked() error {
	content, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode index: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".video-search-index-*")
	if err != nil {
		return fmt.Errorf("create temporary index: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary index: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary index: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary index: %w", err)
	}
	if err := os.Rename(temporaryName, s.path); err != nil {
		return fmt.Errorf("replace index: %w", err)
	}
	return nil
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneMetadata(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsAnyString(values, targets []string) bool {
	for _, target := range targets {
		if containsString(values, target) {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
