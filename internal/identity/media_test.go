package identity

import (
	"path/filepath"
	"testing"

	"video-semantic-search/internal/model"
)

func TestResolveMovieForMediaUsesLegacyReleaseFilename(t *testing.T) {
	store, err := NewFileStore(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	year := 2006
	want := model.Movie{ID: "movie-pursuit", IMDbID: "tt0454921", Title: "The Pursuit of Happyness", Year: &year}
	if err := store.UpsertMovie(want); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMovie(model.Movie{ID: "movie-pursuit-unknown-year", Title: "Pursuit"}); err != nil {
		t.Fatal(err)
	}
	media := model.Media{Title: "[当幸福来敲门(国英双语)].The.Pursuit.0f.Happyness.2006.BluRay.mkv"}
	got, ok := ResolveMovieForMedia(store, media)
	if !ok || got.ID != want.ID {
		t.Fatalf("resolved movie = %+v, ok=%t", got, ok)
	}
	service := NewMoviePreparationService(store, nil, nil, 1, 2, false)
	got, ok = service.resolveMovie(MoviePreparationRequest{FileName: media.Title})
	if !ok || got.ID != want.ID {
		t.Fatalf("prepared movie = %+v, ok=%t", got, ok)
	}
}
