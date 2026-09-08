package identity

import (
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"video-semantic-search/internal/metadata"
	"video-semantic-search/internal/model"
)

func TestSQLiteStoreDeduplicatesExternalIDs(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertPerson(model.Person{ID: "person:external:nm0001", IMDbID: "nm0001", Name: "Actor One"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPerson(model.Person{ID: "person:tmdb:7", TMDBID: 7, Name: "Actor One", Metadata: map[string]any{"from_tmdb": true}}); err != nil {
		t.Fatal(err)
	}
	person, ok := store.FindPersonByIMDbID("nm0001")
	if !ok || person.ID != "person:external:nm0001" || person.TMDBID != 0 {
		t.Fatalf("IMDb person = %+v, ok=%t", person, ok)
	}
	// An incoming record with both external IDs is the operation that binds
	// the two identities; the existing canonical IMDb row wins.
	if err := store.UpsertPerson(model.Person{ID: "person:tmdb:7", IMDbID: "nm0001", TMDBID: 7, Name: "Actor One"}); err != nil {
		t.Fatal(err)
	}
	person, ok = store.FindPersonByTMDBID(7)
	if !ok || person.ID != "person:external:nm0001" {
		t.Fatalf("canonical person = %+v, ok=%t", person, ok)
	}

	if err := store.UpsertMovie(model.Movie{ID: "movie:external:tt0001", IMDbID: "tt0001", Title: "Movie One"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMovie(model.Movie{ID: "movie:tmdb:42", IMDbID: "tt0001", TMDBID: 42, Title: "Movie One"}); err != nil {
		t.Fatal(err)
	}
	movie, ok := store.FindMovieByIMDbID("tt0001")
	if !ok || movie.ID != "movie:external:tt0001" || movie.TMDBID != 42 {
		t.Fatalf("canonical movie = %+v, ok=%t", movie, ok)
	}
}

func TestSQLiteStoreReplacesScenePeopleInOneBatch(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, person := range []model.Person{
		{ID: "person-batch-a", IMDbID: "nm0001", Name: "Batch A"},
		{ID: "person-batch-b", IMDbID: "nm0002", Name: "Batch B"},
	} {
		if err := store.UpsertPerson(person); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReplaceScenePeopleBatch("media-batch", []ScenePeopleUpdate{
		{SceneID: "scene-1", People: []model.ScenePerson{{SceneID: "scene-1", PersonID: "person-batch-a", BestScore: .91}}},
		{SceneID: "scene-2", People: []model.ScenePerson{{SceneID: "scene-2", PersonID: "person-batch-b", BestScore: .87}}},
	}); err != nil {
		t.Fatal(err)
	}
	if people := store.GetScenePeople("media-batch", "scene-1"); len(people) != 1 || people[0].PersonID != "person-batch-a" {
		t.Fatalf("scene 1 people = %+v", people)
	}
	if people := store.GetScenePeople("media-batch", "scene-2"); len(people) != 1 || people[0].PersonID != "person-batch-b" {
		t.Fatalf("scene 2 people = %+v", people)
	}
}

func TestSQLiteStoreRepairsNameOnlyPersonDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPerson(model.Person{ID: "person:external:nm1535523", IMDbID: "nm1535523", TMDBID: 120724, Name: "Jaden Smith"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPerson(model.Person{ID: "person-jaden-smith", Name: "Jaden Smith"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertFaceVector(model.FaceVector{ID: "jaden-legacy-vector", PersonID: "person-jaden-smith", ImageID: "jaden-legacy-vector", Vector: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertFaceVector(model.FaceVector{ID: "jaden-canonical-vector", PersonID: "person:external:nm1535523", ImageID: "jaden-canonical-vector", Vector: []float32{0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	people := store.SearchReadyPersons("Jaden Smith", 10)
	if len(people) != 1 || people[0].ID != "person:external:nm1535523" {
		t.Fatalf("ready people = %+v", people)
	}
	if _, ok := store.GetPerson("person-jaden-smith"); ok {
		t.Fatal("name-only duplicate still exists")
	}
	if got := store.FaceVectorCount("person:external:nm1535523"); got != 2 {
		t.Fatalf("canonical vector count = %d, legacy count = %d, want 2", got, store.FaceVectorCount("person-jaden-smith"))
	}
}

func TestSQLiteStoreKeepsDistinctSameNameExternalPersons(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, person := range []model.Person{
		{ID: "person:external:nm0000226", IMDbID: "nm0000226", Name: "Will Smith"},
		{ID: "person:external:nm0810337", IMDbID: "nm0810337", Name: "Will Smith"},
	} {
		if err := store.UpsertPerson(person); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertFaceVector(model.FaceVector{ID: person.ID + ":vector", PersonID: person.ID, ImageID: person.ID + ":image", Vector: []float32{1, 0}}); err != nil {
			t.Fatal(err)
		}
	}
	people := store.SearchReadyPersons("Will Smith", 10)
	if len(people) != 2 {
		t.Fatalf("same-name external people = %+v, want two identities", people)
	}
}

func TestSQLiteStoreSearchReadyPersonsForMoviesUsesProcessedIMDbCast(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, movie := range []model.Movie{
		{ID: "movie-ready", IMDbID: "tt0001", Title: "Ready Movie"},
		{ID: "movie-not-ready", IMDbID: "tt0002", Title: "Not Ready Movie"},
	} {
		if err := store.UpsertMovie(movie); err != nil {
			t.Fatal(err)
		}
	}
	people := []model.Person{
		{ID: "person-ready", IMDbID: "nm0001", Name: "Ready Actor"},
		{ID: "person-not-ready", IMDbID: "nm0002", Name: "Not Ready Actor"},
	}
	for _, person := range people {
		if err := store.UpsertPerson(person); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertFaceVector(model.FaceVector{ID: person.ID + ":vector", PersonID: person.ID, ImageID: person.ID + ":image", Vector: []float32{1, 0}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReplaceMovieCast("movie-ready", []model.MovieCast{{MovieID: "movie-ready", PersonID: "person-ready", IMDbNameID: "nm0001", Source: "imdb_principals"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceMovieCast("movie-not-ready", []model.MovieCast{{MovieID: "movie-not-ready", PersonID: "person-not-ready", IMDbNameID: "nm0002", Source: "imdb_principals"}}); err != nil {
		t.Fatal(err)
	}
	ready := store.SearchReadyPersonsForMovies("", 10, []string{"movie-ready"})
	if len(ready) != 1 || ready[0].ID != "person-ready" {
		t.Fatalf("processed movie people = %+v", ready)
	}
	if got := store.SearchReadyPersonsForMovies("", 10, nil); len(got) != 0 {
		t.Fatalf("empty processed movie scope = %+v", got)
	}
}

func TestSQLiteStoreCleanupNonIMDbPeopleRemovesHistoricalTMDBRows(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertMovie(model.Movie{ID: "movie-1", IMDbID: "tt0001", Title: "Movie One"}); err != nil {
		t.Fatal(err)
	}
	for _, person := range []model.Person{
		{ID: "person-imdb", IMDbID: "nm0001", Name: "IMDb Actor"},
		{ID: "person-tmdb", TMDBID: 7, Name: "TMDB Actor"},
	} {
		if err := store.UpsertPerson(person); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReplaceMovieCast("movie-1", []model.MovieCast{
		{MovieID: "movie-1", PersonID: "person-imdb", IMDbNameID: "nm0001", Source: "imdb_principals"},
		{MovieID: "movie-1", PersonID: "person-tmdb", Source: "tmdb"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPersonImage(model.PersonImage{ID: "image-tmdb", PersonID: "person-tmdb", Source: "tmdb", SourceURL: "https://image/tmdb.jpg"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertFaceVector(model.FaceVector{ID: "vector-tmdb", PersonID: "person-tmdb", ImageID: "image-tmdb", Vector: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceScenePeople("media-1", "scene-1", []model.ScenePerson{{SceneID: "scene-1", PersonID: "person-tmdb"}}); err != nil {
		t.Fatal(err)
	}
	stats, err := store.CleanupNonIMDbPeople()
	if err != nil {
		t.Fatal(err)
	}
	if stats.RemovedPersons != 1 || stats.RemovedCastRows != 1 || stats.RemovedPersonImages != 1 || stats.RemovedFaceVectors != 1 || stats.RemovedScenePeople != 1 {
		t.Fatalf("cleanup stats = %+v", stats)
	}
	if _, ok := store.GetPerson("person-tmdb"); ok || len(store.GetMovieCast("movie-1")) != 1 || len(store.ListPersonImages("person-tmdb")) != 0 || len(store.ListFaceVectors("person-tmdb")) != 0 || len(store.GetScenePeople("media-1", "scene-1")) != 0 {
		t.Fatal("historical TMDB rows remain after cleanup")
	}
}

func TestSQLiteStoreStreamsFullIMDbRelations(t *testing.T) {
	directory := t.TempDir()
	basics := writeSQLiteDataset(t, directory, "title.basics.tsv.gz", "tconst\ttitleType\tprimaryTitle\toriginalTitle\tisAdult\tstartYear\tendYear\truntimeMinutes\tgenres\n"+
		"tt0001\tmovie\tA Movie\tA Movie\t0\t2024\t\\N\t120\tDrama,Comedy\n")
	ratings := writeSQLiteDataset(t, directory, "title.ratings.tsv.gz", "tconst\taverageRating\tnumVotes\n"+
		"tt0001\t8.7\t12345\n")
	principals := writeSQLiteDataset(t, directory, "title.principals.tsv.gz", "tconst\tordering\tnconst\tcategory\tjob\tcharacters\n"+
		"tt0001\t1\tnm0001\tactor\t\\N\t[\\\"Hero\\\"]\n"+
		"tt0001\t2\tnm-missing\tactress\t\\N\t[\\\"Missing\\\"]\n")
	names := writeSQLiteDataset(t, directory, "name.basics.tsv.gz", "nconst\tprimaryName\tbirthYear\tdeathYear\tprimaryProfession\tknownForTitles\n"+
		"nm0001\tActor One\t1980\t\\N\tactor,producer\ttt0001\n")
	akAs := writeSQLiteDataset(t, directory, "title.akas.tsv.gz", "titleId\tordering\ttitle\tregion\tlanguage\ttypes\tattributes\tisOriginalTitle\n"+
		"tt0001\t1\tA Movie Alternate\tUS\ten\t\\N\t\\N\t0\n")
	crew := writeSQLiteDataset(t, directory, "title.crew.tsv.gz", "tconst\tdirectors\twriters\n"+
		"tt0001\tnm0003\tnm0004,nm0005\n")

	store, err := NewSQLiteStore(filepath.Join(directory, "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	stats, err := store.ImportIMDb(context.Background(), metadata.IMDbDatasetPaths{Basics: basics, Ratings: ratings, Principals: principals, Names: names, Akas: akAs, Crew: crew}, metadata.IMDbImportOptions{IncludeAdult: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Movies != 1 || stats.CastRows != 1 || stats.Persons != 1 {
		t.Fatalf("import stats = %+v", stats)
	}
	movie, ok := store.FindMovieByIMDbID("tt0001")
	if !ok || movie.AverageRating != 8.7 || movie.VoteCount != 12345 || len(movie.Directors) != 1 || len(movie.AlternateTitles) != 1 {
		t.Fatalf("movie = %+v, ok=%t", movie, ok)
	}
	if movies := store.SearchMovies("A Movie", 10); len(movies) != 1 || len(movies[0].AlternateTitles) != 1 {
		t.Fatalf("movie search = %+v", movies)
	}
	if movies := store.SearchMovies("Movie", 10); len(movies) != 1 || movies[0].ID != movie.ID {
		t.Fatalf("contains movie search = %+v", movies)
	}
	if aliasMovie, aliasOK := store.FindMovie("A Movie Alternate", nil); !aliasOK || aliasMovie.ID != movie.ID {
		t.Fatalf("alternate title lookup = %+v, ok=%t", aliasMovie, aliasOK)
	}
	cast := store.GetMovieCast(movie.ID)
	if len(cast) != 1 || !strings.Contains(cast[0].CharacterName, "Hero") {
		t.Fatalf("cast = %+v", cast)
	}
	person, ok := store.FindPersonByIMDbID("nm0001")
	if !ok || person.Name != "Actor One" {
		t.Fatalf("person = %+v, ok=%t", person, ok)
	}
}

func writeSQLiteDataset(t *testing.T, directory, name, content string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := gzip.NewWriter(file)
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
