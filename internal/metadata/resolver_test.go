package metadata

import "testing"

func TestResolveFilename(t *testing.T) {
	result := ResolveFilename("Inception.2010.1080p.BluRay.x265-RARBG.mkv")
	if result.Title != "Inception" || result.Year == nil || *result.Year != 2010 {
		t.Fatalf("resolution = %+v", result)
	}

	result = ResolveFilename("流浪地球 2 (2023) 4K.mp4")
	if result.Title != "流浪地球 2" || result.Year == nil || *result.Year != 2023 {
		t.Fatalf("unicode resolution = %+v", result)
	}
}

func TestEntityIDIsStable(t *testing.T) {
	year := 2024
	if got, want := EntityID("movie", 123, "", "Anything", &year), "movie:tmdb:123"; got != want {
		t.Fatalf("TMDB id = %q, want %q", got, want)
	}
	first := EntityID("person", 0, "nm0001", "Someone", nil)
	second := EntityID("person", 0, "nm0001", "Different", nil)
	if first != second {
		t.Fatalf("external IDs are not stable: %q != %q", first, second)
	}
}
