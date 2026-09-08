// Command catalog-sync imports an IMDb bulk-data slice into the identity
// store and, optionally, enriches each title with the complete TMDB cast and
// a bounded number of TMDB profile images for those cast members.
//
// The command is deliberately sequential at the movie/TMDB boundary: a
// single title can fan out into many API requests and TMDB rate limiting is
// easier to respect this way. Profile lookups within one title use a small,
// configurable worker pool; image downloads and face inference are handled by
// identity.ReferenceIngestor with bounded workers and are resumable.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"video-semantic-search/internal/identity"
	"video-semantic-search/internal/metadata"
	"video-semantic-search/internal/model"
)

type options struct {
	Basics         string
	Ratings        string
	Principals     string
	Names          string
	Akas           string
	Crew           string
	TitleIDFile    string
	IdentityDB     string
	LegacyFile     string
	SkipIMDbImport bool
	TMDB           bool
	MaxMovies      int
	BatchSize      int
	MaxImages      int
	MinReferences  int
	KeepImages     bool
	ProxyURL       string
	MinYear        int
	MaxYear        int
	IncludeAdult   bool
	FaceEndpoint   string
	FaceWorkers    int
	DownloadRoot   string
	MinDetScore    float64
	MaxImageBytes  int64
	RequestTimeout time.Duration
}

type runStats struct {
	Movies         int
	MoviesSkipped  int
	MoviesEnriched int
	MoviesFailed   int
	Persons        int
	CastRows       int
	Images         int
	ImagesReady    int
	ImagesSkipped  int
	ImagesRejected int
	ImagesFailed   int
}

func main() {
	config := parseFlags()
	logger := log.New(os.Stdout, "catalog-sync: ", log.LstdFlags)

	titleIDs, err := loadTitleIDs(config.TitleIDFile)
	if err != nil {
		logger.Fatal(err)
	}
	paths := metadata.IMDbDatasetPaths{
		Basics: config.Basics, Ratings: config.Ratings, Principals: config.Principals, Names: config.Names,
		Akas: config.Akas, Crew: config.Crew,
	}
	importOptions := metadata.IMDbImportOptions{
		MinYear: config.MinYear, MaxYear: config.MaxYear, MaxMovies: config.MaxMovies,
		IncludeAdult: config.IncludeAdult, TitleIDs: titleIDs,
	}
	store, err := identity.NewSQLiteStoreWithLegacy(config.IdentityDB, config.LegacyFile)
	if err != nil {
		logger.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	if config.SkipIMDbImport {
		logger.Printf("skip IMDb metadata import by request; using existing database")
	} else {
		importer, ok := any(store).(identity.IMDbImporter)
		if !ok {
			logger.Fatal("identity store does not support streaming IMDb import")
		}
		importStats, importErr := importer.ImportIMDb(context.Background(), paths, importOptions)
		if importErr != nil {
			logger.Fatal(importErr)
		}
		logger.Printf("IMDb metadata import complete: movies=%d persons=%d cast=%d ratings=%d aliases=%d crew=%d selected_ids=%d", importStats.Movies, importStats.Persons, importStats.CastRows, importStats.Ratings, importStats.Aliases, importStats.Crew, len(titleIDs))
	}

	if !config.TMDB {
		return
	}
	if len(titleIDs) == 0 && config.MaxMovies == 0 && config.MinYear == 0 && config.MaxYear == 0 {
		logger.Fatal("--tmdb requires a bounded selection (--title-id-file, --max-movies, --min-year or --max-year)")
	}
	catalog, err := metadata.LoadCatalog(context.Background(), paths, importOptions)
	if err != nil {
		logger.Fatal(err)
	}
	logger.Printf("loaded bounded TMDB selection: movies=%d actors=%d selected_ids=%d", len(catalog.Titles), len(catalog.Names), len(titleIDs))

	var tmdb *metadata.TMDBClient
	var references *identity.ReferenceIngestor
	tmdb = metadata.NewTMDBClientFromEnv()
	if !tmdb.Configured() {
		logger.Fatal("--tmdb requires TMDB_API_KEY")
	}
	faceClient := identity.NewPythonClient(config.FaceEndpoint, config.RequestTimeout)
	if _, err := faceClient.Health(context.Background()); err != nil {
		logger.Fatalf("--tmdb requires a healthy Identity Service at %s: %v", config.FaceEndpoint, err)
	}
	references = identity.NewReferenceIngestor(store, faceClient, identity.ReferenceConfig{
		DownloadRoot:     config.DownloadRoot,
		ProxyURL:         config.ProxyURL,
		Workers:          config.FaceWorkers,
		MinDetScore:      float32(config.MinDetScore),
		MaxDownloadBytes: config.MaxImageBytes,
		KeepLocalFiles:   config.KeepImages,
	})

	stats := runStats{}
	existingPersons := make(map[string]model.Person, len(catalog.Names))
	for _, person := range catalog.Names {
		converted := model.Person{ID: metadata.EntityID("person", 0, person.ID, person.PrimaryName, person.BirthYear), IMDbID: person.ID, Name: person.PrimaryName, NormalizedName: normalizeName(person.PrimaryName), BirthYear: person.BirthYear, DeathYear: person.DeathYear, PrimaryProfessions: person.PrimaryProfessions, KnownForTitles: person.KnownForTitles}
		existingPersons[converted.ID] = converted
	}
	for _, title := range catalog.Titles {
		stats.Movies++
		existing, found := model.Movie{}, false
		if movieCatalog, supportsCatalog := any(store).(identity.MovieCatalog); supportsCatalog {
			existing, found = movieCatalog.FindMovieByIMDbID(title.ID)
		}
		if !found {
			existing, found = store.GetMovie(movieFromIMDb(title).ID)
		}
		if found && metadataFlag(existing.Metadata, "tmdb_full_cast") && (existing.TMDBStatus == identity.TMDBStatusComplete || metadataFlag(existing.Metadata, "tmdb_sync_complete")) {
			stats.MoviesSkipped++
			logger.Printf("movie %s %q already enriched, skip", title.ID, title.PrimaryTitle)
			continue
		}
		if err := syncIMDbTitle(context.Background(), store, tmdb, references, existingPersons, config.MaxImages, config.MinReferences, catalog, title, &stats, logger); err != nil {
			stats.MoviesFailed++
			logger.Printf("movie %s %q failed: %v", title.ID, title.PrimaryTitle, err)
		} else {
			logger.Printf("movie %s %q complete", title.ID, title.PrimaryTitle)
		}
	}
	logger.Printf("done movies=%d enriched=%d skipped=%d failed=%d persons=%d cast=%d images=%d ready=%d skipped_images=%d rejected=%d image_failed=%d",
		stats.Movies, stats.MoviesEnriched, stats.MoviesSkipped, stats.MoviesFailed, stats.Persons, stats.CastRows,
		stats.Images, stats.ImagesReady, stats.ImagesSkipped, stats.ImagesRejected, stats.ImagesFailed)
}

func parseFlags() options {
	var config options
	flag.StringVar(&config.Basics, "imdb-basics", "", "path to title.basics.tsv.gz (required)")
	flag.StringVar(&config.Ratings, "imdb-ratings", "", "path to title.ratings.tsv.gz")
	flag.StringVar(&config.Principals, "imdb-principals", "", "path to title.principals.tsv.gz")
	flag.StringVar(&config.Names, "imdb-names", "", "path to name.basics.tsv.gz")
	flag.StringVar(&config.Akas, "imdb-akas", "", "path to title.akas.tsv.gz")
	flag.StringVar(&config.Crew, "imdb-crew", "", "path to title.crew.tsv.gz")
	flag.StringVar(&config.TitleIDFile, "title-id-file", "", "optional file containing one IMDb tconst per line")
	flag.StringVar(&config.IdentityDB, "identity-db", getenvOr("VIDEO_SEARCH_IDENTITY_DB", "data/identity.db"), "SQLite identity database path")
	flag.StringVar(&config.LegacyFile, "identity-file", getenvOr("VIDEO_SEARCH_IDENTITY_FILE", "data/identity.json"), "legacy identity JSON migration source")
	flag.BoolVar(&config.SkipIMDbImport, "skip-imdb-import", false, "skip IMDb bulk import; requires the selected IMDb metadata to already exist in the database")
	flag.BoolVar(&config.TMDB, "tmdb", false, "sync full cast and all TMDB profile images")
	flag.IntVar(&config.MaxMovies, "max-movies", 0, "maximum selected movies; 0 means no limit")
	flag.IntVar(&config.BatchSize, "batch-size", envInt("CATALOG_BATCH_SIZE", 25), "number of TMDB titles between identity JSON checkpoints")
	flag.IntVar(&config.MaxImages, "max-images-per-person", envInt("IDENTITY_REFERENCE_MAX_PER_PERSON", 8), "maximum TMDB profile images to embed for each person")
	flag.IntVar(&config.MinReferences, "min-references", envInt("IDENTITY_MIN_REFERENCES", 5), "minimum ready face vectors per cast person for face-bank completion")
	flag.BoolVar(&config.KeepImages, "keep-reference-images", false, "keep downloaded TMDB profile images instead of deleting them after embedding")
	flag.StringVar(&config.ProxyURL, "image-proxy-url", getenvOr("IDENTITY_REFERENCE_PROXY_URL", getenvOr("TMDB_PROXY_URL", "")), "explicit HTTP/HTTPS proxy for TMDB profile-image downloads")
	flag.IntVar(&config.MinYear, "min-year", 0, "minimum start year")
	flag.IntVar(&config.MaxYear, "max-year", 0, "maximum start year")
	flag.BoolVar(&config.IncludeAdult, "include-adult", true, "include IMDb adult movie rows")
	flag.StringVar(&config.FaceEndpoint, "face-endpoint", getenvOr("IDENTITY_ENDPOINT", "http://127.0.0.1:7003"), "Identity Service endpoint")
	flag.IntVar(&config.FaceWorkers, "face-workers", envInt("IDENTITY_REFERENCE_WORKERS", 4), "concurrent reference image workers")
	flag.StringVar(&config.DownloadRoot, "reference-root", "data/identity-faces", "reference image download directory")
	flag.Float64Var(&config.MinDetScore, "min-det-score", envFloat("FACE_MIN_DET_SCORE", 0.60), "minimum face detection score")
	flag.Int64Var(&config.MaxImageBytes, "max-image-bytes", 20<<20, "maximum downloaded profile image size")
	flag.DurationVar(&config.RequestTimeout, "face-timeout", 2*time.Minute, "per-image Identity Service timeout")
	flag.Parse()
	if strings.TrimSpace(config.Basics) == "" {
		flag.Usage()
		log.Fatal("--imdb-basics is required")
	}
	if config.MinReferences <= 0 {
		config.MinReferences = 5
	}
	return config
}

func syncIMDbTitle(ctx context.Context, store identity.Store, tmdb *metadata.TMDBClient, references *identity.ReferenceIngestor, existingPersons map[string]model.Person, maxImagesPerPerson, minReferences int, catalog metadata.IMDbCatalog, title metadata.IMDbTitle, stats *runStats, logger *log.Logger) error {
	movie := movieFromIMDb(title)
	movie = mergeMovie(store, movie)
	if err := store.UpsertMovie(movie); err != nil {
		return err
	}

	localPersons, localCast := buildIMDbCastFromPersons(existingPersons, movie.ID, title.ID, catalog)
	// ImportIMDb has just populated every selected IMDb person in SQLite. The
	// old loop rewrote each person once per title, causing millions of tiny
	// transactions before the first TMDB request. Keep the fallback for custom
	// non-SQLite stores, but let the production adapter rely on the completed
	// import (or on the explicit --skip-imdb-import contract).
	_, sqliteStore := any(store).(*identity.SQLiteStore)
	for _, person := range localPersons {
		if !sqliteStore {
			if err := store.UpsertPerson(person); err != nil {
				return err
			}
		}
		existingPersons[person.ID] = person
		stats.Persons++
	}
	if len(localCast) > 0 {
		if err := store.ReplaceMovieCast(movie.ID, localCast); err != nil {
			return err
		}
		stats.CastRows += len(localCast)
	}

	if tmdb == nil {
		return nil
	}

	// A title may fan out to hundreds of profile-image requests. Give one
	// title its own deadline so a broken remote entry cannot hold the whole
	// catalog forever, while already imported IMDb data remains durable.
	tmdbCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	tmdbResult, err := tmdb.SyncMovieByIMDbIDForNames(tmdbCtx, title.ID, title.PrimaryTitle, title.StartYear, personNames(localPersons))
	cancel()
	if err != nil {
		return fmt.Errorf("TMDB enrichment: %w", err)
	}
	for _, warning := range tmdbResult.Warnings {
		logger.Printf("movie %s TMDB warning: %s", title.ID, warning)
	}
	stats.MoviesEnriched++

	preparer := identity.NewMoviePreparationService(store, tmdb, references, minReferences, maxImagesPerPerson, true)
	if err := preparer.ApplyTMDBResult(&movie, tmdbResult); err != nil {
		return err
	}
	if err := store.UpsertMovie(movie); err != nil {
		return err
	}
	stats.Persons += len(tmdbResult.Persons)
	canonicalCast := store.GetMovieCast(movie.ID)
	stats.CastRows += len(canonicalCast)
	images := make([]model.PersonImage, 0, len(tmdbResult.ProfileImage))
	seenPeople := make(map[string]struct{}, len(canonicalCast))
	for _, cast := range canonicalCast {
		if _, seen := seenPeople[cast.PersonID]; seen {
			continue
		}
		seenPeople[cast.PersonID] = struct{}{}
		vectors := make(map[string]struct{})
		for _, vector := range store.ListFaceVectors(cast.PersonID) {
			vectors[vector.ID] = struct{}{}
			if vector.ImageID != "" {
				vectors[vector.ImageID] = struct{}{}
			}
		}
		remaining := maxImagesPerPerson - len(store.ListFaceVectors(cast.PersonID))
		images = append(images, identity.SelectDiverseReferenceImages(store.ListPersonImages(cast.PersonID), vectors, remaining)...)
	}
	stats.Images += len(images)
	imageFailures := len(tmdbResult.Warnings)
	for _, result := range references.Ingest(ctx, images) {
		switch {
		case result.Error == nil && result.HasVector && result.Skipped:
			stats.ImagesSkipped++
		case result.Error == nil && result.HasVector:
			if result.Image.Status == "ready" {
				stats.ImagesReady++
			}
		case result.Image.Status == "rejected":
			stats.ImagesRejected++
		default:
			stats.ImagesFailed++
			imageFailures++
			logger.Printf("image %s failed: %v", result.Image.ID, result.Error)
		}
	}
	if imageFailures == 0 {
		movie.TMDBStatus = identity.TMDBStatusComplete
	} else {
		movie.TMDBStatus = identity.TMDBStatusPartial
	}
	castPeople := make(map[string]struct{})
	for _, item := range store.GetMovieCast(movie.ID) {
		if strings.TrimSpace(item.PersonID) != "" {
			castPeople[item.PersonID] = struct{}{}
		}
	}
	readyPeople := 0
	for personID := range castPeople {
		if len(store.ListFaceVectors(personID)) >= minReferences {
			readyPeople++
		}
	}
	faceComplete := len(castPeople) > 0 && readyPeople == len(castPeople)
	if faceComplete {
		movie.FaceBankStatus = identity.FaceBankStatusComplete
	} else {
		movie.FaceBankStatus = identity.FaceBankStatusPartial
	}
	movie.Metadata = mergeMetadata(movie.Metadata, map[string]any{
		"tmdb_sync_complete":     movie.TMDBStatus == identity.TMDBStatusComplete,
		"tmdb_full_cast":         true,
		"face_bank_complete":     faceComplete,
		"face_bank_ready_people": readyPeople,
	})
	if err := store.UpsertMovie(movie); err != nil {
		return err
	}
	return nil
}

func personNames(persons []model.Person) []string {
	result := make([]string, 0, len(persons))
	seen := make(map[string]struct{}, len(persons))
	for _, person := range persons {
		name := strings.TrimSpace(person.Name)
		if name == "" {
			continue
		}
		key := strings.ToLower(strings.Join(strings.Fields(name), " "))
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, name)
	}
	return result
}

func importIMDbCatalog(store *identity.FileStore, catalog metadata.IMDbCatalog, stats *runStats) error {
	existingMovies := make(map[string]model.Movie)
	for _, movie := range store.ListMovies() {
		existingMovies[movie.ID] = movie
		if movie.IMDbID != "" {
			existingMovies["imdb:"+movie.IMDbID] = movie
		}
	}
	existingPersons := make(map[string]model.Person)
	for _, person := range store.ListPersons() {
		existingPersons[person.ID] = person
	}
	movies := make([]model.Movie, 0, len(catalog.Titles))
	persons := make(map[string]model.Person)
	casts := make(map[string][]model.MovieCast, len(catalog.Titles))
	for _, title := range catalog.Titles {
		movie := mergeMovieFromMap(existingMovies, movieFromIMDb(title))
		existingMovies[movie.ID] = movie
		if movie.IMDbID != "" {
			existingMovies["imdb:"+movie.IMDbID] = movie
		}
		movies = append(movies, movie)
		localPersons, localCast := buildIMDbCastFromPersons(existingPersons, movie.ID, title.ID, catalog)
		for _, person := range localPersons {
			persons[person.ID] = person
			existingPersons[person.ID] = person
		}
		if len(localCast) > 0 {
			casts[movie.ID] = localCast
		}
	}
	personList := make([]model.Person, 0, len(persons))
	for _, person := range persons {
		personList = append(personList, person)
	}
	if err := store.ApplyCatalog(movies, personList, casts); err != nil {
		return err
	}
	stats.Movies = len(movies)
	stats.Persons = len(personList)
	for _, movieCast := range casts {
		stats.CastRows += len(movieCast)
	}
	return nil
}

func movieFromIMDb(title metadata.IMDbTitle) model.Movie {
	return model.Movie{
		ID:              metadata.EntityID("movie", 0, title.ID, title.PrimaryTitle, title.StartYear),
		IMDbID:          title.ID,
		Title:           title.PrimaryTitle,
		OriginalTitle:   title.OriginalTitle,
		Year:            title.StartYear,
		StartYear:       title.StartYear,
		EndYear:         title.EndYear,
		TitleType:       title.TitleType,
		IsAdult:         title.IsAdult,
		RuntimeMinutes:  title.RuntimeMinutes,
		Genres:          append([]string(nil), title.Genres...),
		AlternateTitles: append([]string(nil), title.AlternateTitles...),
		Directors:       append([]string(nil), title.Directors...),
		Writers:         append([]string(nil), title.Writers...),
		AverageRating:   title.AverageRating,
		VoteCount:       title.VoteCount,
		Metadata: map[string]any{
			"source":          "imdb_bulk_dataset",
			"imdb_title_type": title.TitleType,
		},
	}
}

func mergeMovieFromMap(existing map[string]model.Movie, incoming model.Movie) model.Movie {
	current, ok := existing[incoming.ID]
	if !ok && incoming.IMDbID != "" {
		current, ok = existing["imdb:"+incoming.IMDbID]
	}
	if !ok {
		return incoming
	}
	incoming.ID = current.ID
	if incoming.TMDBID == 0 {
		incoming.TMDBID = current.TMDBID
	}
	if incoming.TMDBStatus == "" {
		incoming.TMDBStatus = current.TMDBStatus
	}
	if incoming.FaceBankStatus == "" {
		incoming.FaceBankStatus = current.FaceBankStatus
	}
	if incoming.Overview == "" {
		incoming.Overview = current.Overview
	}
	if incoming.PosterURL == "" {
		incoming.PosterURL = current.PosterURL
	}
	if len(incoming.AlternateTitles) == 0 {
		incoming.AlternateTitles = append([]string(nil), current.AlternateTitles...)
	}
	if len(incoming.Directors) == 0 {
		incoming.Directors = append([]string(nil), current.Directors...)
	}
	if len(incoming.Writers) == 0 {
		incoming.Writers = append([]string(nil), current.Writers...)
	}
	incoming.Metadata = mergeMetadata(current.Metadata, incoming.Metadata)
	return incoming
}

func buildIMDbCast(store identity.Store, movieID, titleID string, catalog metadata.IMDbCatalog) ([]model.Person, []model.MovieCast) {
	existing := make(map[string]model.Person)
	for _, person := range store.ListPersons() {
		existing[person.ID] = person
	}
	return buildIMDbCastFromPersons(existing, movieID, titleID, catalog)
}

func buildIMDbCastFromPersons(existing map[string]model.Person, movieID, titleID string, catalog metadata.IMDbCatalog) ([]model.Person, []model.MovieCast) {
	principals := catalog.Principals[titleID]
	persons := make([]model.Person, 0, len(principals))
	cast := make([]model.MovieCast, 0, len(principals))
	for _, principal := range principals {
		name, ok := catalog.Names[principal.NameID]
		if !ok || strings.TrimSpace(name.PrimaryName) == "" {
			continue
		}
		person := model.Person{
			ID:                 metadata.EntityID("person", 0, name.ID, name.PrimaryName, name.BirthYear),
			IMDbID:             name.ID,
			Name:               name.PrimaryName,
			NormalizedName:     normalizeName(name.PrimaryName),
			BirthYear:          name.BirthYear,
			DeathYear:          name.DeathYear,
			PrimaryProfessions: append([]string(nil), name.PrimaryProfessions...),
			KnownForTitles:     append([]string(nil), name.KnownForTitles...),
			Metadata:           map[string]any{"source": "imdb_bulk_dataset"},
		}
		if current, ok := existing[person.ID]; ok {
			person = mergePerson(current, person)
		}
		persons = append(persons, person)
		cast = append(cast, model.MovieCast{MovieID: movieID, PersonID: person.ID, IMDbNameID: name.ID, Category: principal.Category, CharacterName: principal.Characters, BillingOrder: principal.Ordering, Source: "imdb_principals"})
	}
	return persons, cast
}

func mergeMovie(store identity.Store, incoming model.Movie) model.Movie {
	if existing, ok := store.GetMovie(incoming.ID); ok {
		return mergeMovieRecord(existing, incoming)
	}
	if catalog, ok := store.(identity.MovieCatalog); ok {
		if existing, found := catalog.FindMovieByIMDbID(incoming.IMDbID); found {
			return mergeMovieRecord(existing, incoming)
		}
	}
	return incoming
}

func mergeMovieRecord(existing, incoming model.Movie) model.Movie {
	incoming.ID = existing.ID
	if incoming.TMDBID == 0 {
		incoming.TMDBID = existing.TMDBID
	}
	if incoming.TMDBStatus == "" {
		incoming.TMDBStatus = existing.TMDBStatus
	}
	if incoming.FaceBankStatus == "" {
		incoming.FaceBankStatus = existing.FaceBankStatus
	}
	if incoming.Overview == "" {
		incoming.Overview = existing.Overview
	}
	if incoming.PosterURL == "" {
		incoming.PosterURL = existing.PosterURL
	}
	if len(incoming.AlternateTitles) == 0 {
		incoming.AlternateTitles = append([]string(nil), existing.AlternateTitles...)
	}
	if len(incoming.Directors) == 0 {
		incoming.Directors = append([]string(nil), existing.Directors...)
	}
	if len(incoming.Writers) == 0 {
		incoming.Writers = append([]string(nil), existing.Writers...)
	}
	incoming.Metadata = mergeMetadata(existing.Metadata, incoming.Metadata)
	return incoming
}

func mergeTMDBMovie(imdbMovie, tmdbMovie model.Movie) model.Movie {
	if tmdbMovie.TMDBID > 0 {
		imdbMovie.TMDBID = tmdbMovie.TMDBID
	}
	if tmdbMovie.Title != "" {
		imdbMovie.Title = tmdbMovie.Title
	}
	if tmdbMovie.OriginalTitle != "" {
		imdbMovie.OriginalTitle = tmdbMovie.OriginalTitle
	}
	if tmdbMovie.Year != nil {
		imdbMovie.Year = tmdbMovie.Year
	}
	if tmdbMovie.Overview != "" {
		imdbMovie.Overview = tmdbMovie.Overview
	}
	if tmdbMovie.PosterURL != "" {
		imdbMovie.PosterURL = tmdbMovie.PosterURL
	}
	imdbMovie.Metadata = mergeMetadata(imdbMovie.Metadata, tmdbMovie.Metadata)
	return imdbMovie
}

func canonicalPerson(store identity.Store, incoming model.Person, nameIndex map[string][]model.Person) model.Person {
	if matches := nameIndex[normalizeName(incoming.Name)]; len(matches) > 0 {
		incoming.ID = matches[0].ID
		incoming.IMDbID = matches[0].IMDbID
		incoming.BirthYear = firstYear(incoming.BirthYear, matches[0].BirthYear)
		incoming.DeathYear = firstYear(incoming.DeathYear, matches[0].DeathYear)
		incoming.PrimaryProfessions = append([]string(nil), matches[0].PrimaryProfessions...)
		incoming.KnownForTitles = append([]string(nil), matches[0].KnownForTitles...)
	}
	if existing, ok := store.GetPerson(incoming.ID); ok {
		return mergePerson(existing, incoming)
	}
	return incoming
}

func mergePerson(existing, incoming model.Person) model.Person {
	if incoming.Name == "" {
		incoming.Name = existing.Name
	}
	if incoming.NormalizedName == "" {
		incoming.NormalizedName = existing.NormalizedName
	}
	if incoming.IMDbID == "" {
		incoming.IMDbID = existing.IMDbID
	}
	if incoming.TMDBID == 0 {
		incoming.TMDBID = existing.TMDBID
	}
	if incoming.BirthYear == nil {
		incoming.BirthYear = existing.BirthYear
	}
	if incoming.DeathYear == nil {
		incoming.DeathYear = existing.DeathYear
	}
	if len(incoming.PrimaryProfessions) == 0 {
		incoming.PrimaryProfessions = append([]string(nil), existing.PrimaryProfessions...)
	}
	if len(incoming.KnownForTitles) == 0 {
		incoming.KnownForTitles = append([]string(nil), existing.KnownForTitles...)
	}
	incoming.Aliases = append([]string(nil), existing.Aliases...)
	incoming.Metadata = mergeMetadata(existing.Metadata, incoming.Metadata)
	return incoming
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

func metadataFlag(values map[string]any, key string) bool {
	value, ok := values[key]
	if !ok {
		return false
	}
	flag, ok := value.(bool)
	return ok && flag
}

func findLocalCastByPerson(cast []model.MovieCast, personID string) (model.MovieCast, bool) {
	for _, item := range cast {
		if item.PersonID == personID {
			return item, true
		}
	}
	return model.MovieCast{}, false
}

// TMDBSyncResult intentionally stores image ownership through the TMDB
// person ID in PersonImage.PersonID. These helpers recover that ID without
// adding a second public field to the durable model.
func tmdbPersonID(persons []model.Person, candidate string) int {
	for _, person := range persons {
		if person.ID == candidate {
			return person.TMDBID
		}
	}
	return 0
}

func tmdbPersonIDByImage(result metadata.TMDBSyncResult, image model.PersonImage) int {
	for _, person := range result.Persons {
		if person.ID == image.PersonID {
			return person.TMDBID
		}
	}
	return 0
}

func canonicalPersonIDByName(persons []model.Person, candidate string, canonicalByTMDB map[int]string) string {
	for _, person := range persons {
		if person.ID == candidate {
			return canonicalByTMDB[person.TMDBID]
		}
	}
	return ""
}

func firstYear(first, second *int) *int {
	if first != nil {
		return first
	}
	return second
}

func normalizeName(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func loadTitleIDs(path string) (map[string]struct{}, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open title id file: %w", err)
	}
	defer file.Close()
	ids := make(map[string]struct{})
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		value := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if value != "" {
			ids[value] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read title id file: %w", err)
	}
	return ids, nil
}

func getenvOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envFloat(name string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
