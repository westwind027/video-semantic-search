package store

import (
	"path/filepath"
	"testing"

	"video-semantic-search/internal/model"
)

func TestFileStoreUpdatesAndFiltersScenePeople(t *testing.T) {
	index, err := NewFileStore(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	media := model.Media{MediaID: "media-1", Title: "identity", Scenes: []model.Scene{{SceneID: "scene-1", Start: 0, End: 1}, {SceneID: "scene-2", Start: 1, End: 2}}}
	if err := index.UpsertMedia(media, [][]float32{{1, 0}, {0, 1}}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := index.UpdateScenePeople("media-1", "scene-2", []string{"person-a"}); err != nil {
		t.Fatal(err)
	}
	documents := index.SearchDocuments(Filter{PersonID: "person-a"})
	if len(documents) != 1 || documents[0].Scene.SceneID != "scene-2" {
		t.Fatalf("person-filtered documents = %+v", documents)
	}
	loaded, ok := index.GetMedia("media-1")
	if !ok || len(loaded.Scenes) != 2 || len(loaded.Scenes[1].PersonIDs) != 1 {
		t.Fatalf("loaded media = %+v, ok=%v", loaded, ok)
	}
	documents = index.SearchDocuments(Filter{PersonIDs: []string{"person-missing", "person-a"}})
	if len(documents) != 1 || documents[0].Scene.SceneID != "scene-2" {
		t.Fatalf("multi-person-filtered documents = %+v", documents)
	}
}
