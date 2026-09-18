package identity

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"video-semantic-search/internal/metadata"
	"video-semantic-search/internal/model"

	_ "modernc.org/sqlite"
)

// SQLiteStore is the durable catalog adapter. IMDb metadata is stored in
// normalised tables, while the small JSON fields in Movie and Person keep the
// existing Go interface compact. A single writer connection and WAL mode let
// the HTTP handlers read the catalogue while a bulk import is running.
type SQLiteStore struct {
	mu   sync.RWMutex
	db   *sql.DB
	path string
}

// IMDbImportStats describes one streaming import. It is deliberately small so
// catalog-sync can report progress without retaining the imported records.
type IMDbImportStats struct {
	Movies   int
	Persons  int
	CastRows int
	Ratings  int
	Aliases  int
	Crew     int
}

// PersonAuthorityCleanupStats reports historical rows removed by the IMDb
// authority migration. It is returned so startup/admin callers can log an
// auditable cleanup summary.
type PersonAuthorityCleanupStats struct {
	RemovedPersons      int
	RemovedCastRows     int
	RemovedPersonImages int
	RemovedFaceVectors  int
	RemovedScenePeople  int
}

// IMDbImporter is the deep catalog-import seam used by catalog-sync. The
// implementation owns batching, deduplication and SQL details; callers only
// supply the official dataset paths and selection policy.
type IMDbImporter interface {
	ImportIMDb(context.Context, metadata.IMDbDatasetPaths, metadata.IMDbImportOptions) (IMDbImportStats, error)
}

// NewSQLiteStore opens or creates a metadata database.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	return NewSQLiteStoreWithLegacy(path, "")
}

// NewSQLiteStoreWithLegacy opens the database and, when it is empty, imports a
// legacy identity JSON file exactly once. The JSON file is never modified or
// deleted; it remains a recoverable migration source.
func NewSQLiteStoreWithLegacy(path, legacyPath string) (*SQLiteStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("identity database path is required")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create identity database directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open identity database: %w", err)
	}
	// SQLite is very effective with one connection for this workload: the
	// store serialises mutations while reads still use the database indexes.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &SQLiteStore{db: db, path: path}
	if err := store.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if strings.TrimSpace(legacyPath) != "" {
		if err := store.migrateLegacyIfEmpty(legacyPath); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if _, err := store.repairPersonDuplicates(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := store.CleanupNonIMDbPeople(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// NewStoreFromEnv makes SQLite the production adapter. VIDEO_SEARCH_IDENTITY_FILE
// is retained as a migration source for installations created by the MVP.
func NewStoreFromEnv() (Store, error) {
	databasePath := envOrDefault("VIDEO_SEARCH_IDENTITY_DB", "data/identity.db")
	legacyPath := envOrDefault("VIDEO_SEARCH_IDENTITY_FILE", "data/identity.json")
	return NewSQLiteStoreWithLegacy(databasePath, legacyPath)
}

func (s *SQLiteStore) initSchema() error {
	statements := []string{
		`PRAGMA busy_timeout = 10000`,
		`PRAGMA foreign_keys = ON`,
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS movies (
			id TEXT PRIMARY KEY,
			imdb_id TEXT NOT NULL DEFAULT '',
			tmdb_id INTEGER NOT NULL DEFAULT 0,
			tmdb_status TEXT NOT NULL DEFAULT '',
			face_bank_status TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL,
			normalized_title TEXT NOT NULL DEFAULT '',
			original_title TEXT NOT NULL DEFAULT '',
			normalized_original_title TEXT NOT NULL DEFAULT '',
			year INTEGER,
			start_year INTEGER,
			end_year INTEGER,
			title_type TEXT NOT NULL DEFAULT '',
			is_adult INTEGER NOT NULL DEFAULT 0,
			runtime_minutes INTEGER NOT NULL DEFAULT 0,
			genres_json TEXT NOT NULL DEFAULT '[]',
			alternate_titles_json TEXT NOT NULL DEFAULT '[]',
			directors_json TEXT NOT NULL DEFAULT '[]',
			writers_json TEXT NOT NULL DEFAULT '[]',
			average_rating REAL NOT NULL DEFAULT 0,
			vote_count INTEGER NOT NULL DEFAULT 0,
			overview TEXT NOT NULL DEFAULT '',
			poster_url TEXT NOT NULL DEFAULT '',
			metadata_json TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS movies_imdb_id_unique ON movies(imdb_id) WHERE imdb_id <> ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS movies_tmdb_id_unique ON movies(tmdb_id) WHERE tmdb_id > 0`,
		`CREATE INDEX IF NOT EXISTS movies_title_idx ON movies(normalized_title, year)`,
		`CREATE INDEX IF NOT EXISTS movies_original_title_idx ON movies(normalized_original_title, year)`,
		`CREATE TABLE IF NOT EXISTS movie_aliases (
			movie_id TEXT NOT NULL,
			title TEXT NOT NULL,
			ordering INTEGER NOT NULL DEFAULT 0,
			region TEXT NOT NULL DEFAULT '',
			language TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(movie_id, title, region, language)
		)`,
		`CREATE INDEX IF NOT EXISTS movie_aliases_title_idx ON movie_aliases(title)`,
		`CREATE TABLE IF NOT EXISTS persons (
			id TEXT PRIMARY KEY,
			imdb_id TEXT NOT NULL DEFAULT '',
			tmdb_id INTEGER NOT NULL DEFAULT 0,
			name TEXT NOT NULL,
			normalized_name TEXT NOT NULL DEFAULT '',
			birth_year INTEGER,
			death_year INTEGER,
			primary_professions_json TEXT NOT NULL DEFAULT '[]',
			known_for_titles_json TEXT NOT NULL DEFAULT '[]',
			aliases_json TEXT NOT NULL DEFAULT '[]',
			metadata_json TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS persons_imdb_id_unique ON persons(imdb_id) WHERE imdb_id <> ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS persons_tmdb_id_unique ON persons(tmdb_id) WHERE tmdb_id > 0`,
		`CREATE INDEX IF NOT EXISTS persons_name_idx ON persons(normalized_name, name)`,
		`CREATE INDEX IF NOT EXISTS persons_name_only_idx ON persons(normalized_name, id) WHERE imdb_id = '' AND tmdb_id = 0`,
		`CREATE TABLE IF NOT EXISTS movie_cast (
			movie_id TEXT NOT NULL,
			person_id TEXT NOT NULL,
			imdb_name_id TEXT NOT NULL DEFAULT '',
			category TEXT NOT NULL DEFAULT '',
			character_name TEXT NOT NULL DEFAULT '',
			billing_order INTEGER NOT NULL DEFAULT 0,
			source TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(movie_id, person_id, imdb_name_id, billing_order, source)
		)`,
		`CREATE INDEX IF NOT EXISTS movie_cast_person_idx ON movie_cast(person_id, movie_id)`,
		`CREATE TABLE IF NOT EXISTS person_images (
			id TEXT PRIMARY KEY,
			person_id TEXT NOT NULL,
			source_index INTEGER NOT NULL DEFAULT 0,
			source TEXT NOT NULL DEFAULT '',
			source_url TEXT NOT NULL DEFAULT '',
			local_path TEXT NOT NULL DEFAULT '',
			width INTEGER NOT NULL DEFAULT 0,
			height INTEGER NOT NULL DEFAULT 0,
			vote_average REAL NOT NULL DEFAULT 0,
			quality_score REAL NOT NULL DEFAULT 0,
			face_count INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS person_images_person_idx ON person_images(person_id, source_index, id)`,
		`CREATE TABLE IF NOT EXISTS face_vectors (
			id TEXT PRIMARY KEY,
			person_id TEXT NOT NULL,
			image_id TEXT NOT NULL,
			quality REAL NOT NULL DEFAULT 0,
			model TEXT NOT NULL DEFAULT '',
			vector BLOB NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS face_vectors_image_unique ON face_vectors(image_id)`,
		`CREATE INDEX IF NOT EXISTS face_vectors_person_idx ON face_vectors(person_id, quality DESC, id)`,
		`CREATE TABLE IF NOT EXISTS scene_people (
			media_id TEXT NOT NULL,
			scene_id TEXT NOT NULL,
			person_id TEXT NOT NULL,
			confidence REAL NOT NULL DEFAULT 0,
			match_count INTEGER NOT NULL DEFAULT 0,
			best_score REAL NOT NULL DEFAULT 0,
			PRIMARY KEY(media_id, scene_id, person_id)
		)`,
		`CREATE INDEX IF NOT EXISTS scene_people_person_idx ON scene_people(person_id, media_id, scene_id)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("initialize identity database: %w", err)
		}
	}
	return nil
}

func (s *SQLiteStore) migrateLegacyIfEmpty(path string) error {
	if path == ":memory:" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect legacy identity store: %w", err)
	}
	var movieCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM movies`).Scan(&movieCount); err != nil {
		return fmt.Errorf("count identity movies: %w", err)
	}
	if movieCount > 0 {
		return nil
	}
	legacy, err := NewFileStore(path)
	if err != nil {
		return fmt.Errorf("open legacy identity store: %w", err)
	}
	defer legacy.Close()

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin identity migration: %w", err)
	}
	rollback := func() { _ = tx.Rollback() }
	movieIDs := make(map[string]string, len(legacy.state.Movies))
	personIDs := make(map[string]string, len(legacy.state.Persons))
	movieKeys := make([]string, 0, len(legacy.state.Movies))
	for id := range legacy.state.Movies {
		movieKeys = append(movieKeys, id)
	}
	sort.Strings(movieKeys)
	for _, id := range movieKeys {
		canonical, upsertErr := upsertMovieTx(tx, legacy.state.Movies[id])
		if upsertErr != nil {
			rollback()
			return fmt.Errorf("migrate movie %s: %w", id, upsertErr)
		}
		movieIDs[id] = canonical
	}
	personKeys := make([]string, 0, len(legacy.state.Persons))
	for id := range legacy.state.Persons {
		personKeys = append(personKeys, id)
	}
	sort.Strings(personKeys)
	for _, id := range personKeys {
		canonical, upsertErr := upsertPersonTx(tx, legacy.state.Persons[id])
		if upsertErr != nil {
			rollback()
			return fmt.Errorf("migrate person %s: %w", id, upsertErr)
		}
		personIDs[id] = canonical
	}
	for oldMovieID, cast := range legacy.state.MovieCast {
		movieID := movieIDs[oldMovieID]
		if movieID == "" {
			continue
		}
		for _, item := range cast {
			item.MovieID = movieID
			item.PersonID = firstNonEmptyString(personIDs[item.PersonID], item.PersonID)
			if err := insertCastTx(tx, item); err != nil {
				rollback()
				return fmt.Errorf("migrate cast %s: %w", oldMovieID, err)
			}
		}
	}
	for _, image := range legacy.state.PersonImages {
		image.PersonID = firstNonEmptyString(personIDs[image.PersonID], image.PersonID)
		if err := upsertPersonImageTx(tx, image); err != nil {
			rollback()
			return fmt.Errorf("migrate person image %s: %w", image.ID, err)
		}
	}
	for _, vector := range legacy.state.FaceVectors {
		vector.PersonID = firstNonEmptyString(personIDs[vector.PersonID], vector.PersonID)
		if err := upsertFaceVectorTx(tx, vector); err != nil {
			rollback()
			return fmt.Errorf("migrate face vector %s: %w", vector.ID, err)
		}
	}
	for key, people := range legacy.state.ScenePeople {
		parts := strings.SplitN(key, "\x00", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		if err := replaceScenePeopleTx(tx, parts[0], parts[1], people, personIDs); err != nil {
			rollback()
			return fmt.Errorf("migrate scene people %s: %w", key, err)
		}
	}
	if _, err := repairPersonDuplicatesTx(tx); err != nil {
		rollback()
		return fmt.Errorf("repair migrated person duplicates: %w", err)
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO schema_meta(key, value) VALUES('legacy_json_migrated', ?)`, path); err != nil {
		rollback()
		return fmt.Errorf("mark legacy identity migration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit identity migration: %w", err)
	}
	return nil
}

// repairPersonDuplicates merges legacy name-only rows into the one external
// identity with the same normalized name. Rows are deliberately not merged
// when more than one IMDb/TMDB identity has that name: two real people can
// share a display name.
func (s *SQLiteStore) repairPersonDuplicates() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin person duplicate repair: %w", err)
	}
	repaired, err := repairPersonDuplicatesTx(tx)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit person duplicate repair: %w", err)
	}
	return repaired, nil
}

// CleanupNonIMDbPeople makes the IMDb authority rule durable for databases
// written by older binaries. TMDB cast rows, people without an IMDb ID, and
// rows pointing at missing/non-IMDb people cannot participate in the current
// face bank, so their dependent data is removed in one transaction.
func (s *SQLiteStore) CleanupNonIMDbPeople() (PersonAuthorityCleanupStats, error) {
	var stats PersonAuthorityCleanupStats
	if s == nil || s.db == nil {
		return stats, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return stats, fmt.Errorf("begin IMDb person authority cleanup: %w", err)
	}
	rollback := func(cleanupErr error) (PersonAuthorityCleanupStats, error) {
		_ = tx.Rollback()
		return PersonAuthorityCleanupStats{}, cleanupErr
	}
	deleteAndCount := func(query string) (int, error) {
		result, execErr := tx.Exec(query)
		if execErr != nil {
			return 0, execErr
		}
		count, countErr := result.RowsAffected()
		return int(count), countErr
	}
	if stats.RemovedFaceVectors, err = deleteAndCount(`DELETE FROM face_vectors WHERE NOT EXISTS (SELECT 1 FROM persons p WHERE p.id = face_vectors.person_id AND trim(p.imdb_id) <> '')`); err != nil {
		return rollback(fmt.Errorf("remove non-IMDb face vectors: %w", err))
	}
	if stats.RemovedPersonImages, err = deleteAndCount(`DELETE FROM person_images WHERE NOT EXISTS (SELECT 1 FROM persons p WHERE p.id = person_images.person_id AND trim(p.imdb_id) <> '')`); err != nil {
		return rollback(fmt.Errorf("remove non-IMDb person images: %w", err))
	}
	if stats.RemovedScenePeople, err = deleteAndCount(`DELETE FROM scene_people WHERE NOT EXISTS (SELECT 1 FROM persons p WHERE p.id = scene_people.person_id AND trim(p.imdb_id) <> '')`); err != nil {
		return rollback(fmt.Errorf("remove non-IMDb scene people: %w", err))
	}
	if stats.RemovedCastRows, err = deleteAndCount(`DELETE FROM movie_cast WHERE lower(trim(source)) = 'tmdb' OR NOT EXISTS (SELECT 1 FROM persons p WHERE p.id = movie_cast.person_id AND trim(p.imdb_id) <> '')`); err != nil {
		return rollback(fmt.Errorf("remove non-authoritative cast rows: %w", err))
	}
	if stats.RemovedPersons, err = deleteAndCount(`DELETE FROM persons WHERE trim(imdb_id) = ''`); err != nil {
		return rollback(fmt.Errorf("remove non-IMDb persons: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return PersonAuthorityCleanupStats{}, fmt.Errorf("commit IMDb person authority cleanup: %w", err)
	}
	return stats, nil
}

func (s *SQLiteStore) UpsertMovie(movie model.Movie) error {
	if strings.TrimSpace(movie.ID) == "" || strings.TrimSpace(movie.Title) == "" {
		return fmt.Errorf("movie id and title are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := upsertMovieTx(tx, movie); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) GetMovie(id string) (model.Movie, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	movie, err := queryMovie(s.db.QueryRow(movieSelect+` WHERE id = ?`, strings.TrimSpace(id)))
	if err == nil {
		s.attachMovieAliases(&movie)
	}
	return movie, err == nil
}

func (s *SQLiteStore) FindMovie(title string, year *int) (model.Movie, bool) {
	want := normalize(title)
	if want == "" {
		return model.Movie{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	query := movieSelect + ` WHERE normalized_title = ? OR normalized_original_title = ? OR EXISTS (SELECT 1 FROM movie_aliases a WHERE a.movie_id = movies.id AND lower(a.title) = ?) ORDER BY CASE WHEN year IS NULL THEN 1 ELSE 0 END, year LIMIT 1`
	args := []any{want, want, want}
	if year != nil {
		query = movieSelect + ` WHERE (normalized_title = ? OR normalized_original_title = ? OR EXISTS (SELECT 1 FROM movie_aliases a WHERE a.movie_id = movies.id AND lower(a.title) = ?)) AND (year IS NULL OR year = ?) ORDER BY CASE WHEN year IS NULL THEN 1 ELSE 0 END LIMIT 1`
		args = append(args, *year)
	}
	movie, err := queryMovie(s.db.QueryRow(query, args...))
	if err == nil {
		s.attachMovieAliases(&movie)
	}
	return movie, err == nil
}

func (s *SQLiteStore) FindMovieByIMDbID(imdbID string) (model.Movie, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	movie, err := queryMovie(s.db.QueryRow(movieSelect+` WHERE imdb_id = ? AND imdb_id <> '' LIMIT 1`, strings.ToLower(strings.TrimSpace(imdbID))))
	if err == nil {
		s.attachMovieAliases(&movie)
	}
	return movie, err == nil
}

// SearchMovies is the bounded movie-picker query used by administrative UI.
// ListMovies remains available for migration and small test stores.
func (s *SQLiteStore) SearchMovies(query string, limit int) []model.Movie {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	want := normalize(query)
	pattern := "%" + want + "%"
	prefix := want + "%"
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(movieSelect+` WHERE ? = '' OR normalized_title LIKE ? OR normalized_original_title LIKE ? OR EXISTS (SELECT 1 FROM movie_aliases a WHERE a.movie_id = movies.id AND lower(a.title) LIKE ?) ORDER BY CASE WHEN normalized_title = ? OR normalized_original_title = ? THEN 0 WHEN normalized_title LIKE ? OR normalized_original_title LIKE ? THEN 1 ELSE 2 END, normalized_title, year LIMIT ?`, want, pattern, pattern, pattern, want, want, prefix, prefix, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]model.Movie, 0, limit)
	for rows.Next() {
		movie, scanErr := scanMovie(rows)
		if scanErr == nil {
			result = append(result, movie)
		}
	}
	_ = rows.Close()
	for index := range result {
		s.attachMovieAliases(&result[index])
	}
	return result
}

// SearchMoviesByTitle is the fast title-only path used when resolving a
// processed media filename. Alternate titles remain available through
// SearchMovies, but should not be scanned for every actor-picker request.
func (s *SQLiteStore) SearchMoviesByTitle(query string, limit int) []model.Movie {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	want := normalize(query)
	pattern := "%" + want + "%"
	prefix := want + "%"
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(movieSelect+` WHERE ? = '' OR normalized_title LIKE ? OR normalized_original_title LIKE ? ORDER BY CASE WHEN normalized_title = ? OR normalized_original_title = ? THEN 0 WHEN normalized_title LIKE ? OR normalized_original_title LIKE ? THEN 1 ELSE 2 END, normalized_title, year LIMIT ?`, want, pattern, pattern, want, want, prefix, prefix, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]model.Movie, 0, limit)
	for rows.Next() {
		movie, scanErr := scanMovie(rows)
		if scanErr == nil {
			result = append(result, movie)
		}
	}
	for index := range result {
		s.attachMovieAliases(&result[index])
	}
	return result
}

// SearchMoviesByExactTitle uses the indexed alias table for resolver lookups.
// It deliberately avoids the broad LIKE query used by the administrative
// movie picker: a full IMDb import has millions of aliases, so a release name
// must not scan them just to resolve one title token.
func (s *SQLiteStore) SearchMoviesByExactTitle(query string, limit int) []model.Movie {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	want := normalize(query)
	variants := exactTitleVariants(query)
	if want == "" || len(variants) == 0 {
		return nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(variants)), ",")
	args := []any{want, want}
	for _, variant := range variants {
		args = append(args, variant)
	}
	args = append(args, limit)
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Resolve candidate IDs through the two indexed tables first. Combining
	// the full movie row with the alias EXISTS predicate makes SQLite scan the
	// entire movies table on the production catalog.
	idRows, err := s.db.Query(`SELECT id FROM movies WHERE normalized_title = ? OR normalized_original_title = ?
		UNION SELECT movie_id FROM movie_aliases WHERE title IN (`+placeholders+`) LIMIT ?`, args...)
	if err != nil {
		return nil
	}
	defer idRows.Close()
	ids := make([]string, 0, limit)
	for idRows.Next() {
		var id string
		if scanErr := idRows.Scan(&id); scanErr == nil {
			ids = append(ids, id)
		}
	}
	result := make([]model.Movie, 0, limit)
	for _, id := range ids {
		movie, scanErr := queryMovie(s.db.QueryRow(movieSelect+` WHERE id = ?`, id))
		if scanErr != nil {
			continue
		}
		s.attachMovieAliases(&movie)
		result = append(result, movie)
	}
	sort.Slice(result, func(left, right int) bool {
		leftYear, rightYear := 0, 0
		if result[left].Year != nil {
			leftYear = *result[left].Year
		}
		if result[right].Year != nil {
			rightYear = *result[right].Year
		}
		if leftYear != rightYear {
			return leftYear > rightYear
		}
		return result[left].ID < result[right].ID
	})
	return result
}

func exactTitleVariants(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	variants := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return
		}
		if _, exists := seen[candidate]; exists {
			return
		}
		seen[candidate] = struct{}{}
		variants = append(variants, candidate)
	}
	add(value)
	add(strings.ToLower(value))
	runes := []rune(strings.ToLower(value))
	if len(runes) > 0 {
		runes[0] = unicode.ToUpper(runes[0])
		add(string(runes))
	}
	return variants
}

func (s *SQLiteStore) ListMovies() []model.Movie {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(movieSelect + ` ORDER BY normalized_title, year`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]model.Movie, 0)
	for rows.Next() {
		movie, scanErr := scanMovie(rows)
		if scanErr == nil {
			result = append(result, movie)
		}
	}
	_ = rows.Close()
	for index := range result {
		s.attachMovieAliases(&result[index])
	}
	return result
}

func (s *SQLiteStore) ReplaceMovieCast(movieID string, cast []model.MovieCast) error {
	movieID = strings.TrimSpace(movieID)
	if movieID == "" {
		return fmt.Errorf("movie id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM movie_cast WHERE movie_id = ?`, movieID); err != nil {
		_ = tx.Rollback()
		return err
	}
	seen := make(map[string]struct{}, len(cast))
	for _, item := range cast {
		item.MovieID = movieID
		item.PersonID = canonicalPersonIDTx(tx, item.PersonID)
		if item.PersonID == "" {
			_ = tx.Rollback()
			return fmt.Errorf("cast person id is required")
		}
		key := strings.Join([]string{item.PersonID, item.IMDbNameID, strconv.Itoa(item.BillingOrder), item.Source}, "\x00")
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		if err := insertCastTx(tx, item); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLiteStore) GetMovieCast(movieID string) []model.MovieCast {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT movie_id, person_id, imdb_name_id, category, character_name, billing_order, source FROM movie_cast WHERE movie_id = ? ORDER BY billing_order, person_id`, strings.TrimSpace(movieID))
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]model.MovieCast, 0)
	for rows.Next() {
		var item model.MovieCast
		if err := rows.Scan(&item.MovieID, &item.PersonID, &item.IMDbNameID, &item.Category, &item.CharacterName, &item.BillingOrder, &item.Source); err == nil {
			result = append(result, item)
		}
	}
	return result
}

func (s *SQLiteStore) UpsertPerson(person model.Person) error {
	if strings.TrimSpace(person.ID) == "" || strings.TrimSpace(person.Name) == "" {
		return fmt.Errorf("person id and name are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := upsertPersonTx(tx, person); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) UpsertPersons(persons []model.Person) error {
	if len(persons) == 0 {
		return nil
	}
	for _, person := range persons {
		if strings.TrimSpace(person.ID) == "" || strings.TrimSpace(person.Name) == "" {
			return fmt.Errorf("person id and name are required")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, person := range persons {
		if _, err := upsertPersonTx(tx, person); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLiteStore) GetPerson(id string) (model.Person, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	person, err := queryPerson(s.db.QueryRow(personSelect+` WHERE id = ?`, strings.TrimSpace(id)))
	return person, err == nil
}

func (s *SQLiteStore) FindPerson(query string) (model.Person, bool) {
	want := normalize(query)
	if want == "" {
		return model.Person{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	person, err := queryPerson(s.db.QueryRow(personSelect+` WHERE normalized_name = ? OR EXISTS (SELECT 1 FROM json_each(aliases_json) WHERE lower(value) = ?) ORDER BY id LIMIT 1`, want, want))
	return person, err == nil
}

func (s *SQLiteStore) FindPersonByIMDbID(imdbID string) (model.Person, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	person, err := queryPerson(s.db.QueryRow(personSelect+` WHERE imdb_id = ? AND imdb_id <> '' LIMIT 1`, strings.ToLower(strings.TrimSpace(imdbID))))
	return person, err == nil
}

func (s *SQLiteStore) FindPersonByTMDBID(tmdbID int) (model.Person, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	person, err := queryPerson(s.db.QueryRow(personSelect+` WHERE tmdb_id = ? AND tmdb_id > 0 LIMIT 1`, tmdbID))
	return person, err == nil
}

func (s *SQLiteStore) ListPersons() []model.Person {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(personSelect + ` ORDER BY normalized_name, id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]model.Person, 0)
	for rows.Next() {
		person, scanErr := scanPerson(rows)
		if scanErr == nil {
			result = append(result, person)
		}
	}
	return result
}

func (s *SQLiteStore) SearchPersons(query string, limit int) []model.Person {
	return s.searchPersons(query, limit, false, nil, false, 1)
}

func (s *SQLiteStore) SearchReadyPersons(query string, limit int) []model.Person {
	return s.searchPersons(query, limit, true, nil, false, 1)
}

func (s *SQLiteStore) SearchReadyPersonsWithMinimum(query string, limit, minimumReferences int) []model.Person {
	return s.searchPersons(query, limit, true, nil, false, minimumReferences)
}

// SearchReadyPersonsForMovies returns only ready IMDb people that occur in
// authoritative IMDb cast rows for the supplied processed movies.
func (s *SQLiteStore) SearchReadyPersonsForMovies(query string, limit int, movieIDs []string) []model.Person {
	if len(movieIDs) == 0 {
		return []model.Person{}
	}
	return s.searchPersons(query, limit, true, movieIDs, true, 1)
}

func (s *SQLiteStore) SearchReadyPersonsForMoviesWithMinimum(query string, limit int, movieIDs []string, minimumReferences int) []model.Person {
	if len(movieIDs) == 0 {
		return []model.Person{}
	}
	return s.searchPersons(query, limit, true, movieIDs, true, minimumReferences)
}

func (s *SQLiteStore) searchPersons(query string, limit int, readyOnly bool, movieIDs []string, restrictMovies bool, minimumReferences int) []model.Person {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if minimumReferences < 1 {
		minimumReferences = 1
	}
	want := normalize(query)
	s.mu.RLock()
	defer s.mu.RUnlock()
	pattern := "%" + want + "%"
	prefix := want + "%"
	where := ` WHERE (? = '' OR normalized_name LIKE ? OR lower(imdb_id) LIKE ? OR EXISTS (SELECT 1 FROM json_each(aliases_json) WHERE lower(value) LIKE ?))`
	args := []any{want, pattern, pattern, pattern}
	if readyOnly {
		where = ` WHERE trim(persons.imdb_id) <> '' AND` + strings.TrimPrefix(where, " WHERE")
		where += ` AND EXISTS (SELECT 1 FROM face_vectors v WHERE v.person_id = persons.id GROUP BY v.person_id HAVING COUNT(*) >= ?)`
		args = append(args, minimumReferences)
	}
	if restrictMovies {
		placeholders := make([]string, 0, len(movieIDs))
		for _, movieID := range movieIDs {
			movieID = strings.TrimSpace(movieID)
			if movieID == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, movieID)
		}
		if len(placeholders) == 0 {
			return []model.Person{}
		}
		where += ` AND EXISTS (SELECT 1 FROM movie_cast c WHERE c.person_id = persons.id AND c.source = 'imdb_principals' AND c.movie_id IN (` + strings.Join(placeholders, ",") + `))`
	}
	args = append(args, want, prefix, limit)
	rows, err := s.db.Query(personSelect+where+` ORDER BY CASE WHEN normalized_name = ? THEN 0 WHEN normalized_name LIKE ? THEN 1 ELSE 2 END, normalized_name, id LIMIT ?`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]model.Person, 0, limit)
	for rows.Next() {
		person, scanErr := scanPerson(rows)
		if scanErr == nil {
			result = append(result, person)
		}
	}
	return result
}

func (s *SQLiteStore) FaceVectorCount(personID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM face_vectors WHERE person_id = ?`, strings.TrimSpace(personID)).Scan(&count); err != nil {
		return 0
	}
	return count
}

func (s *SQLiteStore) UpsertPersonImage(image model.PersonImage) error {
	if strings.TrimSpace(image.ID) == "" || strings.TrimSpace(image.PersonID) == "" {
		return fmt.Errorf("person image id and person id are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	image.PersonID = canonicalPersonIDTx(tx, image.PersonID)
	if err := upsertPersonImageTx(tx, image); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) UpsertPersonImages(images []model.PersonImage) error {
	if len(images) == 0 {
		return nil
	}
	for _, image := range images {
		if strings.TrimSpace(image.ID) == "" || strings.TrimSpace(image.PersonID) == "" {
			return fmt.Errorf("person image id and person id are required")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, image := range images {
		image.PersonID = canonicalPersonIDTx(tx, image.PersonID)
		if err := upsertPersonImageTx(tx, image); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLiteStore) ListPersonImages(personID string) []model.PersonImage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT id, person_id, source_index, source, source_url, local_path, width, height, vote_average, quality_score, face_count, status FROM person_images WHERE person_id = ? ORDER BY source_index, id`, strings.TrimSpace(personID))
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]model.PersonImage, 0)
	for rows.Next() {
		var image model.PersonImage
		if err := rows.Scan(&image.ID, &image.PersonID, &image.SourceIndex, &image.Source, &image.SourceURL, &image.LocalPath, &image.Width, &image.Height, &image.VoteAverage, &image.QualityScore, &image.FaceCount, &image.Status); err == nil {
			result = append(result, image)
		}
	}
	return result
}

func (s *SQLiteStore) ClearRemoteLocalPaths() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(`UPDATE person_images SET local_path = '' WHERE source_url <> '' AND local_path <> ''`)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	return int(count), err
}

func (s *SQLiteStore) UpsertFaceVector(vector model.FaceVector) error {
	if strings.TrimSpace(vector.ID) == "" || strings.TrimSpace(vector.PersonID) == "" || len(vector.Vector) == 0 {
		return fmt.Errorf("face vector id, person id and vector are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	vector.PersonID = canonicalPersonIDTx(tx, vector.PersonID)
	if err := upsertFaceVectorTx(tx, vector); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) DeleteFaceVector(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM face_vectors WHERE id = ?`, strings.TrimSpace(id))
	return err
}

func (s *SQLiteStore) ListFaceVectors(personID string) []model.FaceVector {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT id, person_id, image_id, quality, model, vector FROM face_vectors WHERE person_id = ? ORDER BY quality DESC, id`, strings.TrimSpace(personID))
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]model.FaceVector, 0)
	for rows.Next() {
		var vector model.FaceVector
		var blob []byte
		if err := rows.Scan(&vector.ID, &vector.PersonID, &vector.ImageID, &vector.Quality, &vector.Model, &blob); err != nil {
			continue
		}
		vector.Vector = decodeVector(blob)
		if len(vector.Vector) > 0 {
			result = append(result, vector)
		}
	}
	return result
}

func (s *SQLiteStore) ReplaceScenePeople(mediaID, sceneID string, people []model.ScenePerson) error {
	return s.ReplaceScenePeopleBatch(mediaID, []ScenePeopleUpdate{{SceneID: sceneID, People: people}})
}

func (s *SQLiteStore) ReplaceScenePeopleBatch(mediaID string, updates []ScenePeopleUpdate) error {
	mediaID = strings.TrimSpace(mediaID)
	if mediaID == "" {
		return fmt.Errorf("media id is required")
	}
	if len(updates) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, update := range updates {
		if err := replaceScenePeopleTx(tx, mediaID, strings.TrimSpace(update.SceneID), update.People, nil); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLiteStore) GetScenePeople(mediaID, sceneID string) []model.ScenePerson {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT scene_id, person_id, confidence, match_count, best_score FROM scene_people WHERE media_id = ? AND scene_id = ? ORDER BY best_score DESC, person_id`, strings.TrimSpace(mediaID), strings.TrimSpace(sceneID))
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]model.ScenePerson, 0)
	for rows.Next() {
		var person model.ScenePerson
		if err := rows.Scan(&person.SceneID, &person.PersonID, &person.Confidence, &person.MatchCount, &person.BestScore); err == nil {
			result = append(result, person)
		}
	}
	return result
}

func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

const movieSelect = `SELECT id, imdb_id, tmdb_id, tmdb_status, face_bank_status, title, normalized_title, original_title, normalized_original_title, year, start_year, end_year, title_type, is_adult, runtime_minutes, genres_json, alternate_titles_json, directors_json, writers_json, average_rating, vote_count, overview, poster_url, metadata_json FROM movies`

const personSelect = `SELECT id, imdb_id, tmdb_id, name, normalized_name, birth_year, death_year, primary_professions_json, known_for_titles_json, aliases_json, metadata_json FROM persons`

func (s *SQLiteStore) attachMovieAliases(movie *model.Movie) {
	if movie == nil || movie.ID == "" {
		return
	}
	rows, err := s.db.Query(`SELECT title FROM movie_aliases WHERE movie_id = ? ORDER BY ordering, title`, movie.ID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var title string
		if rows.Scan(&title) == nil {
			movie.AlternateTitles = appendUniqueStrings(movie.AlternateTitles, title)
		}
	}
}

type scanner interface {
	Scan(...any) error
}

func queryMovie(row scanner) (model.Movie, error) { return scanMovie(row) }

func scanMovie(row scanner) (model.Movie, error) {
	var movie model.Movie
	var normalizedTitle, normalizedOriginal string
	var year, startYear, endYear sql.NullInt64
	var adult int
	var genres, alternate, directors, writers, rawMetadata string
	if err := row.Scan(&movie.ID, &movie.IMDbID, &movie.TMDBID, &movie.TMDBStatus, &movie.FaceBankStatus, &movie.Title, &normalizedTitle, &movie.OriginalTitle, &normalizedOriginal, &year, &startYear, &endYear, &movie.TitleType, &adult, &movie.RuntimeMinutes, &genres, &alternate, &directors, &writers, &movie.AverageRating, &movie.VoteCount, &movie.Overview, &movie.PosterURL, &rawMetadata); err != nil {
		return model.Movie{}, err
	}
	movie.IsAdult = adult != 0
	movie.Year = nullInt(year)
	movie.StartYear = nullInt(startYear)
	movie.EndYear = nullInt(endYear)
	movie.Genres = decodeStrings(genres)
	movie.AlternateTitles = decodeStrings(alternate)
	movie.Directors = decodeStrings(directors)
	movie.Writers = decodeStrings(writers)
	movie.Metadata = decodeMetadata(rawMetadata)
	return movie, nil
}

func queryPerson(row scanner) (model.Person, error) { return scanPerson(row) }

func scanPerson(row scanner) (model.Person, error) {
	var person model.Person
	var birthYear, deathYear sql.NullInt64
	var professions, knownFor, aliases, rawMetadata string
	if err := row.Scan(&person.ID, &person.IMDbID, &person.TMDBID, &person.Name, &person.NormalizedName, &birthYear, &deathYear, &professions, &knownFor, &aliases, &rawMetadata); err != nil {
		return model.Person{}, err
	}
	person.BirthYear = nullInt(birthYear)
	person.DeathYear = nullInt(deathYear)
	person.PrimaryProfessions = decodeStrings(professions)
	person.KnownForTitles = decodeStrings(knownFor)
	person.Aliases = decodeStrings(aliases)
	person.Metadata = decodeMetadata(rawMetadata)
	return person, nil
}

func upsertMovieTx(tx *sql.Tx, incoming model.Movie) (string, error) {
	if strings.TrimSpace(incoming.ID) == "" || strings.TrimSpace(incoming.Title) == "" {
		return "", fmt.Errorf("movie id and title are required")
	}
	canonicalID := findMovieIDTx(tx, incoming.ID, incoming.IMDbID, incoming.TMDBID)
	if canonicalID != "" && canonicalID != incoming.ID {
		if existing, err := queryMovie(tx.QueryRow(movieSelect+` WHERE id = ?`, canonicalID)); err == nil {
			incoming = mergeMovie(existing, incoming)
			incoming.ID = canonicalID
		}
	} else if canonicalID == incoming.ID {
		if existing, err := queryMovie(tx.QueryRow(movieSelect+` WHERE id = ?`, canonicalID)); err == nil {
			incoming = mergeMovie(existing, incoming)
		}
	}
	if canonicalID == "" {
		canonicalID = incoming.ID
	}
	incoming.ID = canonicalID
	_, err := tx.Exec(`INSERT INTO movies(id, imdb_id, tmdb_id, tmdb_status, face_bank_status, title, normalized_title, original_title, normalized_original_title, year, start_year, end_year, title_type, is_adult, runtime_minutes, genres_json, alternate_titles_json, directors_json, writers_json, average_rating, vote_count, overview, poster_url, metadata_json)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET imdb_id=excluded.imdb_id, tmdb_id=excluded.tmdb_id, tmdb_status=excluded.tmdb_status, face_bank_status=excluded.face_bank_status, title=excluded.title, normalized_title=excluded.normalized_title, original_title=excluded.original_title, normalized_original_title=excluded.normalized_original_title, year=excluded.year, start_year=excluded.start_year, end_year=excluded.end_year, title_type=excluded.title_type, is_adult=excluded.is_adult, runtime_minutes=excluded.runtime_minutes, genres_json=excluded.genres_json, alternate_titles_json=excluded.alternate_titles_json, directors_json=excluded.directors_json, writers_json=excluded.writers_json, average_rating=excluded.average_rating, vote_count=excluded.vote_count, overview=excluded.overview, poster_url=excluded.poster_url, metadata_json=excluded.metadata_json`, movieArgs(incoming)...)
	return canonicalID, err
}

func upsertPersonTx(tx *sql.Tx, incoming model.Person) (string, error) {
	if strings.TrimSpace(incoming.ID) == "" || strings.TrimSpace(incoming.Name) == "" {
		return "", fmt.Errorf("person id and name are required")
	}
	if incoming.NormalizedName == "" {
		incoming.NormalizedName = normalize(incoming.Name)
	}
	candidates := personIDsTx(tx, incoming.ID, incoming.IMDbID, incoming.TMDBID)
	canonicalID := incoming.ID
	if incoming.IMDbID != "" {
		if candidate := lookupPersonIDTx(tx, `imdb_id = ? AND imdb_id <> ''`, strings.ToLower(incoming.IMDbID)); candidate != "" {
			canonicalID = candidate
		}
	} else if candidate := lookupPersonIDTx(tx, `id = ?`, incoming.ID); candidate != "" {
		canonicalID = candidate
	} else if incoming.TMDBID > 0 {
		if candidate := lookupPersonIDTx(tx, `tmdb_id = ? AND tmdb_id > 0`, strconv.Itoa(incoming.TMDBID)); candidate != "" {
			canonicalID = candidate
		}
	}
	merged := model.Person{ID: canonicalID, Name: incoming.Name, NormalizedName: incoming.NormalizedName}
	for _, candidate := range candidates {
		existing, err := queryPerson(tx.QueryRow(personSelect+` WHERE id = ?`, candidate))
		if err != nil {
			continue
		}
		if candidate != canonicalID {
			merged = mergePerson(merged, existing)
			if err := mergePersonRowTx(tx, candidate, canonicalID); err != nil {
				return "", err
			}
		} else {
			merged = mergePerson(existing, merged)
		}
	}
	merged = mergePerson(merged, incoming)
	merged.ID = canonicalID
	_, err := tx.Exec(`INSERT INTO persons(id, imdb_id, tmdb_id, name, normalized_name, birth_year, death_year, primary_professions_json, known_for_titles_json, aliases_json, metadata_json)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET imdb_id=excluded.imdb_id, tmdb_id=excluded.tmdb_id, name=excluded.name, normalized_name=excluded.normalized_name, birth_year=excluded.birth_year, death_year=excluded.death_year, primary_professions_json=excluded.primary_professions_json, known_for_titles_json=excluded.known_for_titles_json, aliases_json=excluded.aliases_json, metadata_json=excluded.metadata_json`, personArgs(merged)...)
	return canonicalID, err
}

func findMovieIDTx(tx *sql.Tx, id, imdbID string, tmdbID int) string {
	var found string
	if strings.TrimSpace(id) != "" && tx.QueryRow(`SELECT id FROM movies WHERE id = ?`, id).Scan(&found) == nil {
		return found
	}
	if strings.TrimSpace(imdbID) != "" && tx.QueryRow(`SELECT id FROM movies WHERE imdb_id = ? AND imdb_id <> ''`, strings.ToLower(strings.TrimSpace(imdbID))).Scan(&found) == nil {
		return found
	}
	if tmdbID > 0 && tx.QueryRow(`SELECT id FROM movies WHERE tmdb_id = ? AND tmdb_id > 0`, tmdbID).Scan(&found) == nil {
		return found
	}
	return ""
}

func findPersonIDTx(tx *sql.Tx, id, imdbID string, tmdbID int) string {
	ids := personIDsTx(tx, id, imdbID, tmdbID)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func personIDsTx(tx *sql.Tx, id, imdbID string, tmdbID int) []string {
	result := make([]string, 0, 3)
	for _, candidate := range []string{
		lookupPersonIDTx(tx, `id = ?`, strings.TrimSpace(id)),
		lookupPersonIDTx(tx, `imdb_id = ? AND imdb_id <> ''`, strings.ToLower(strings.TrimSpace(imdbID))),
		lookupPersonIDTx(tx, `tmdb_id = ? AND tmdb_id > 0`, strconv.Itoa(tmdbID)),
	} {
		if candidate == "" {
			continue
		}
		seen := false
		for _, existing := range result {
			if existing == candidate {
				seen = true
				break
			}
		}
		if !seen {
			result = append(result, candidate)
		}
	}
	return result
}

func lookupPersonIDTx(tx *sql.Tx, predicate, value string) string {
	if strings.TrimSpace(value) == "" || value == "0" {
		return ""
	}
	var found string
	if tx.QueryRow(`SELECT id FROM persons WHERE `+predicate+` LIMIT 1`, value).Scan(&found) == nil {
		return found
	}
	return ""
}

func mergePersonRowTx(tx *sql.Tx, duplicateID, canonicalID string) error {
	if duplicateID == "" || duplicateID == canonicalID {
		return nil
	}
	for _, statement := range []string{
		`UPDATE OR IGNORE movie_cast SET person_id = ? WHERE person_id = ?`,
		`UPDATE OR IGNORE person_images SET person_id = ? WHERE person_id = ?`,
		`UPDATE OR IGNORE face_vectors SET person_id = ? WHERE person_id = ?`,
		`UPDATE OR IGNORE scene_people SET person_id = ? WHERE person_id = ?`,
	} {
		if _, err := tx.Exec(statement, canonicalID, duplicateID); err != nil {
			return err
		}
	}
	for _, statement := range []string{
		`DELETE FROM movie_cast WHERE person_id = ?`,
		`DELETE FROM person_images WHERE person_id = ?`,
		`DELETE FROM face_vectors WHERE person_id = ?`,
		`DELETE FROM scene_people WHERE person_id = ?`,
		`DELETE FROM persons WHERE id = ?`,
	} {
		if _, err := tx.Exec(statement, duplicateID); err != nil {
			return err
		}
	}
	return nil
}

func repairPersonDuplicatesTx(tx *sql.Tx) (int, error) {
	rows, err := tx.Query(`SELECT id, normalized_name FROM persons WHERE imdb_id = '' AND tmdb_id = 0 AND normalized_name <> '' ORDER BY normalized_name, id`)
	if err != nil {
		return 0, fmt.Errorf("list name-only persons: %w", err)
	}
	type candidate struct {
		id   string
		name string
	}
	candidates := make([]candidate, 0)
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.name); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan name-only person: %w", err)
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("read name-only persons: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close name-only persons: %w", err)
	}

	repaired := 0
	for _, item := range candidates {
		externalIDs, err := externalPersonIDsByNameTx(tx, item.name)
		if err != nil {
			return repaired, err
		}
		if len(externalIDs) != 1 || externalIDs[0] == item.id {
			continue
		}
		canonicalID := externalIDs[0]
		canonical, err := queryPerson(tx.QueryRow(personSelect+` WHERE id = ?`, canonicalID))
		if err != nil {
			return repaired, fmt.Errorf("load canonical person %s: %w", canonicalID, err)
		}
		duplicate, err := queryPerson(tx.QueryRow(personSelect+` WHERE id = ?`, item.id))
		if err != nil {
			continue
		}
		merged := mergePerson(canonical, duplicate)
		merged.ID = canonicalID
		if err := updatePersonTx(tx, merged); err != nil {
			return repaired, fmt.Errorf("merge person metadata %s into %s: %w", item.id, canonicalID, err)
		}
		if err := mergePersonRowTx(tx, item.id, canonicalID); err != nil {
			return repaired, fmt.Errorf("move person data %s into %s: %w", item.id, canonicalID, err)
		}
		repaired++
	}
	return repaired, nil
}

func externalPersonIDsByNameTx(tx *sql.Tx, normalizedName string) ([]string, error) {
	rows, err := tx.Query(`SELECT id FROM persons WHERE normalized_name = ? AND (imdb_id <> '' OR tmdb_id > 0) ORDER BY CASE WHEN imdb_id <> '' THEN 0 ELSE 1 END, id`, normalizedName)
	if err != nil {
		return nil, fmt.Errorf("list external persons named %s: %w", normalizedName, err)
	}
	defer rows.Close()
	result := make([]string, 0, 1)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan external person named %s: %w", normalizedName, err)
		}
		result = append(result, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read external persons named %s: %w", normalizedName, err)
	}
	return result, nil
}

func updatePersonTx(tx *sql.Tx, person model.Person) error {
	values := personArgs(person)
	args := append([]any(nil), values[1:]...)
	args = append(args, person.ID)
	_, err := tx.Exec(`UPDATE persons SET imdb_id = ?, tmdb_id = ?, name = ?, normalized_name = ?, birth_year = ?, death_year = ?, primary_professions_json = ?, known_for_titles_json = ?, aliases_json = ?, metadata_json = ? WHERE id = ?`, args...)
	return err
}

func canonicalPersonIDTx(tx *sql.Tx, id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	var found string
	if tx.QueryRow(`SELECT id FROM persons WHERE id = ?`, id).Scan(&found) == nil {
		return found
	}
	if strings.HasPrefix(id, "person:tmdb:") {
		if tmdbID, err := strconv.Atoi(strings.TrimPrefix(id, "person:tmdb:")); err == nil && tx.QueryRow(`SELECT id FROM persons WHERE tmdb_id = ? AND tmdb_id > 0`, tmdbID).Scan(&found) == nil {
			return found
		}
	}
	return id
}

func insertCastTx(tx *sql.Tx, item model.MovieCast) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO movie_cast(movie_id, person_id, imdb_name_id, category, character_name, billing_order, source) VALUES(?, ?, ?, ?, ?, ?, ?)`, item.MovieID, item.PersonID, item.IMDbNameID, item.Category, item.CharacterName, item.BillingOrder, item.Source)
	return err
}

func upsertPersonImageTx(tx *sql.Tx, image model.PersonImage) error {
	_, err := tx.Exec(`INSERT INTO person_images(id, person_id, source_index, source, source_url, local_path, width, height, vote_average, quality_score, face_count, status)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET person_id=excluded.person_id, source_index=excluded.source_index, source=excluded.source, source_url=excluded.source_url, local_path=CASE WHEN excluded.local_path <> '' THEN excluded.local_path ELSE person_images.local_path END, width=excluded.width, height=excluded.height, vote_average=excluded.vote_average, quality_score=excluded.quality_score, face_count=excluded.face_count, status=excluded.status`, image.ID, image.PersonID, image.SourceIndex, image.Source, image.SourceURL, image.LocalPath, image.Width, image.Height, image.VoteAverage, image.QualityScore, image.FaceCount, image.Status)
	return err
}

func upsertFaceVectorTx(tx *sql.Tx, vector model.FaceVector) error {
	_, err := tx.Exec(`INSERT INTO face_vectors(id, person_id, image_id, quality, model, vector) VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET person_id=excluded.person_id, image_id=excluded.image_id, quality=excluded.quality, model=excluded.model, vector=excluded.vector`, vector.ID, vector.PersonID, vector.ImageID, vector.Quality, vector.Model, encodeVector(vector.Vector))
	return err
}

func replaceScenePeopleTx(tx *sql.Tx, mediaID, sceneID string, people []model.ScenePerson, personIDs map[string]string) error {
	if mediaID == "" || sceneID == "" {
		return fmt.Errorf("media id and scene id are required")
	}
	if _, err := tx.Exec(`DELETE FROM scene_people WHERE media_id = ? AND scene_id = ?`, mediaID, sceneID); err != nil {
		return err
	}
	for _, person := range people {
		personID := person.PersonID
		if personIDs != nil {
			personID = firstNonEmptyString(personIDs[personID], personID)
		}
		if strings.TrimSpace(personID) == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO scene_people(media_id, scene_id, person_id, confidence, match_count, best_score) VALUES(?, ?, ?, ?, ?, ?)`, mediaID, sceneID, personID, person.Confidence, person.MatchCount, person.BestScore); err != nil {
			return err
		}
	}
	return nil
}

func movieArgs(movie model.Movie) []any {
	return []any{movie.ID, movie.IMDbID, movie.TMDBID, movie.TMDBStatus, movie.FaceBankStatus, movie.Title, normalize(movie.Title), movie.OriginalTitle, normalize(movie.OriginalTitle), nullableInt(movie.Year), nullableInt(movie.StartYear), nullableInt(movie.EndYear), movie.TitleType, boolInt(movie.IsAdult), movie.RuntimeMinutes, encodeStrings(movie.Genres), encodeStrings(movie.AlternateTitles), encodeStrings(movie.Directors), encodeStrings(movie.Writers), movie.AverageRating, movie.VoteCount, movie.Overview, movie.PosterURL, encodeMetadata(movie.Metadata)}
}

func personArgs(person model.Person) []any {
	return []any{person.ID, person.IMDbID, person.TMDBID, person.Name, normalize(person.Name), nullableInt(person.BirthYear), nullableInt(person.DeathYear), encodeStrings(person.PrimaryProfessions), encodeStrings(person.KnownForTitles), encodeStrings(person.Aliases), encodeMetadata(person.Metadata)}
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullInt(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	result := int(value.Int64)
	return &result
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func encodeStrings(values []string) string {
	if values == nil {
		return "[]"
	}
	content, _ := json.Marshal(values)
	return string(content)
}

func decodeStrings(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var result []string
	if json.Unmarshal([]byte(value), &result) != nil {
		return nil
	}
	return result
}

func encodeMetadata(value map[string]any) string {
	if value == nil {
		return "{}"
	}
	content, _ := json.Marshal(value)
	return string(content)
}

func decodeMetadata(value string) map[string]any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var result map[string]any
	if json.Unmarshal([]byte(value), &result) != nil {
		return nil
	}
	return result
}

func encodeVector(values []float32) []byte {
	result := make([]byte, len(values)*4)
	for index, value := range values {
		binary.LittleEndian.PutUint32(result[index*4:], math.Float32bits(value))
	}
	return result
}

func decodeVector(value []byte) []float32 {
	if len(value) == 0 || len(value)%4 != 0 {
		return nil
	}
	result := make([]float32, len(value)/4)
	for index := range result {
		result[index] = math.Float32frombits(binary.LittleEndian.Uint32(value[index*4:]))
	}
	return result
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

const (
	lookupIMDbMovieSQL     = `SELECT id FROM movies WHERE id = ? OR (imdb_id = ? AND imdb_id <> '') LIMIT 1`
	lookupIMDbMovieByIDSQL = `SELECT id FROM movies WHERE imdb_id = ? AND imdb_id <> '' LIMIT 1`
	lookupIMDbMovieRowSQL  = movieSelect + ` WHERE id = ?`
	upsertIMDbMovieSQL     = `INSERT INTO movies(id, imdb_id, tmdb_id, tmdb_status, face_bank_status, title, normalized_title, original_title, normalized_original_title, year, start_year, end_year, title_type, is_adult, runtime_minutes, genres_json, alternate_titles_json, directors_json, writers_json, average_rating, vote_count, overview, poster_url, metadata_json)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET imdb_id=excluded.imdb_id, tmdb_id=excluded.tmdb_id, tmdb_status=excluded.tmdb_status, face_bank_status=excluded.face_bank_status, title=excluded.title, normalized_title=excluded.normalized_title, original_title=excluded.original_title, normalized_original_title=excluded.normalized_original_title, year=excluded.year, start_year=excluded.start_year, end_year=excluded.end_year, title_type=excluded.title_type, is_adult=excluded.is_adult, runtime_minutes=excluded.runtime_minutes, genres_json=excluded.genres_json, alternate_titles_json=excluded.alternate_titles_json, directors_json=excluded.directors_json, writers_json=excluded.writers_json, average_rating=excluded.average_rating, vote_count=excluded.vote_count, overview=excluded.overview, poster_url=excluded.poster_url, metadata_json=excluded.metadata_json`
	updateIMDbRatingSQL = `UPDATE movies SET average_rating = ?, vote_count = ? WHERE imdb_id = ? AND imdb_id <> ''`
	insertIMDbAliasSQL  = `INSERT OR IGNORE INTO movie_aliases(movie_id, title, ordering, region, language) VALUES(?, ?, ?, ?, ?)`
	updateIMDbCrewSQL   = `UPDATE movies SET directors_json = ?, writers_json = ? WHERE imdb_id = ? AND imdb_id <> ''`
	insertIMDbCastSQL   = `INSERT OR IGNORE INTO movie_cast(movie_id, person_id, imdb_name_id, category, character_name, billing_order, source) VALUES(?, ?, ?, ?, ?, ?, ?)`
	upsertIMDbPersonSQL = `INSERT INTO persons(id, imdb_id, tmdb_id, name, normalized_name, birth_year, death_year, primary_professions_json, known_for_titles_json, aliases_json, metadata_json)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET imdb_id=excluded.imdb_id, name=excluded.name, normalized_name=excluded.normalized_name, birth_year=excluded.birth_year, death_year=excluded.death_year, primary_professions_json=excluded.primary_professions_json, known_for_titles_json=excluded.known_for_titles_json`
)

// imdbImportStatements keeps the hot path of a full snapshot import on
// prepared statements. The old generic upsert helpers remain the safe path
// for interactive writes and the rare external-ID collision fallback.
type imdbImportStatements struct {
	tx                *sql.Tx
	lookupMovie       *sql.Stmt
	lookupMovieByIMDb *sql.Stmt
	lookupMovieRow    *sql.Stmt
	upsertMovie       *sql.Stmt
	updateRating      *sql.Stmt
	insertAlias       *sql.Stmt
	updateCrew        *sql.Stmt
	insertCast        *sql.Stmt
	upsertPerson      *sql.Stmt
}

func newIMDbImportStatements(tx *sql.Tx) (*imdbImportStatements, error) {
	result := &imdbImportStatements{tx: tx}
	queries := []struct {
		target **sql.Stmt
		query  string
	}{
		{&result.lookupMovie, lookupIMDbMovieSQL},
		{&result.lookupMovieByIMDb, lookupIMDbMovieByIDSQL},
		{&result.lookupMovieRow, lookupIMDbMovieRowSQL},
		{&result.upsertMovie, upsertIMDbMovieSQL},
		{&result.updateRating, updateIMDbRatingSQL},
		{&result.insertAlias, insertIMDbAliasSQL},
		{&result.updateCrew, updateIMDbCrewSQL},
		{&result.insertCast, insertIMDbCastSQL},
		{&result.upsertPerson, upsertIMDbPersonSQL},
	}
	for _, item := range queries {
		statement, err := tx.Prepare(item.query)
		if err != nil {
			result.close()
			return nil, fmt.Errorf("prepare IMDb import statement: %w", err)
		}
		*item.target = statement
	}
	return result, nil
}

func (s *imdbImportStatements) close() {
	if s == nil {
		return
	}
	for _, statement := range []*sql.Stmt{s.lookupMovie, s.lookupMovieByIMDb, s.lookupMovieRow, s.upsertMovie, s.updateRating, s.insertAlias, s.updateCrew, s.insertCast, s.upsertPerson} {
		if statement != nil {
			_ = statement.Close()
		}
	}
}

func (s *imdbImportStatements) upsertMovieRecord(movie model.Movie) (string, error) {
	var canonicalID string
	err := s.lookupMovie.QueryRow(movie.ID, strings.ToLower(strings.TrimSpace(movie.IMDbID))).Scan(&canonicalID)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if err == nil {
		existing, queryErr := queryMovie(s.lookupMovieRow.QueryRow(canonicalID))
		if queryErr != nil {
			return "", queryErr
		}
		movie = mergeMovie(existing, movie)
	} else {
		canonicalID = movie.ID
	}
	movie.ID = canonicalID
	if _, err := s.upsertMovie.Exec(movieArgs(movie)...); err != nil {
		// A legacy row can have a non-standard ID while sharing an IMDb ID. The
		// indexed lookup above handles the normal case; retain the generic merge
		// path as a correctness fallback for unusual historical data.
		fallbackID, fallbackErr := upsertMovieTx(s.tx, movie)
		if fallbackErr != nil {
			return "", err
		}
		return fallbackID, nil
	}
	return canonicalID, nil
}

func (s *imdbImportStatements) upsertPersonRecord(person model.Person) error {
	if _, err := s.upsertPerson.Exec(personArgs(person)...); err != nil {
		// Preserve TMDB-enriched rows when a historical record uses another local
		// ID for the same IMDb person.
		if _, fallbackErr := upsertPersonTx(s.tx, person); fallbackErr != nil {
			return err
		}
	}
	return nil
}

// ImportIMDb streams the complete movie catalog into SQLite. Hot statements
// are prepared once per transaction, while only the selected actor nconsts
// are retained between principals and name.basics; the full title/person
// records are never materialised in Go memory.
func (s *SQLiteStore) ImportIMDb(ctx context.Context, paths metadata.IMDbDatasetPaths, options metadata.IMDbImportOptions) (IMDbImportStats, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return IMDbImportStats{}, fmt.Errorf("begin IMDb import: %w", err)
	}
	rollback := func(importErr error) (IMDbImportStats, error) {
		_ = tx.Rollback()
		return IMDbImportStats{}, importErr
	}
	statements, err := newIMDbImportStatements(tx)
	if err != nil {
		return rollback(err)
	}
	defer statements.close()
	stats := IMDbImportStats{}
	actorNameIDs := make(map[string]struct{})
	err = metadata.StreamIMDb(ctx, paths, options, metadata.IMDbStreamHandlers{
		Title: func(title metadata.IMDbTitle) error {
			canonical, err := statements.upsertMovieRecord(movieFromIMDbTitle(title))
			if err == nil && canonical != "" {
				stats.Movies++
			}
			return err
		},
		Rating: func(titleID string, rating float32, votes int64) error {
			result, err := statements.updateRating.Exec(rating, votes, strings.ToLower(titleID))
			if err == nil {
				if count, rowsErr := result.RowsAffected(); rowsErr == nil && count > 0 {
					stats.Ratings += int(count)
				}
			}
			return err
		},
		AlternateTitle: func(alias metadata.IMDbAlternateTitle) error {
			var movieID string
			if err := statements.lookupMovieByIMDb.QueryRow(strings.ToLower(alias.TitleID)).Scan(&movieID); err != nil {
				if err == sql.ErrNoRows {
					return nil
				}
				return err
			}
			result, err := statements.insertAlias.Exec(movieID, alias.Title, alias.Ordering, alias.Region, alias.Language)
			if err == nil {
				if count, rowsErr := result.RowsAffected(); rowsErr == nil {
					stats.Aliases += int(count)
				}
			}
			return err
		},
		Crew: func(titleID string, directors, writers []string) error {
			result, err := statements.updateCrew.Exec(encodeStrings(directors), encodeStrings(writers), strings.ToLower(titleID))
			if err == nil {
				if count, rowsErr := result.RowsAffected(); rowsErr == nil && count > 0 {
					stats.Crew += int(count)
				}
			}
			return err
		},
		Principal: func(principal metadata.IMDbPrincipal) error {
			var movieID string
			if err := statements.lookupMovieByIMDb.QueryRow(strings.ToLower(principal.TitleID)).Scan(&movieID); err != nil {
				if err == sql.ErrNoRows {
					return nil
				}
				return err
			}
			actorNameIDs[principal.NameID] = struct{}{}
			result, err := statements.insertCast.Exec(movieID, metadata.EntityID("person", 0, principal.NameID, "", nil), principal.NameID, principal.Category, principal.Characters, principal.Ordering, "imdb_principals")
			if err == nil {
				if count, rowsErr := result.RowsAffected(); rowsErr == nil {
					stats.CastRows += int(count)
				}
			}
			return err
		},
		NameFilter: func(nameID string) bool {
			_, ok := actorNameIDs[nameID]
			return ok
		},
		Name: func(name metadata.IMDbName) error {
			person := personFromIMDbName(name)
			if err := statements.upsertPersonRecord(person); err != nil {
				return err
			}
			stats.Persons++
			return nil
		},
	})
	if err != nil {
		return rollback(fmt.Errorf("stream IMDb catalog: %w", err))
	}
	if _, err := repairPersonDuplicatesTx(tx); err != nil {
		return rollback(fmt.Errorf("repair IMDb person duplicates: %w", err))
	}
	// principals is streamed before name.basics, so a current snapshot can
	// contain a principal whose name row is absent. Do not leave that relation
	// as an apparent IMDb cast member; the relationship is valid only when its
	// authoritative IMDb Person was imported as well.
	if result, err := tx.Exec(`DELETE FROM movie_cast
		WHERE lower(trim(source)) = 'imdb_principals'
		  AND NOT EXISTS (
			SELECT 1 FROM persons p
			WHERE p.id = movie_cast.person_id AND trim(p.imdb_id) <> ''
		)`); err != nil {
		return rollback(fmt.Errorf("remove IMDb cast without person: %w", err))
	} else if removed, rowsErr := result.RowsAffected(); rowsErr == nil {
		stats.CastRows -= int(removed)
		if stats.CastRows < 0 {
			stats.CastRows = 0
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO schema_meta(key, value) VALUES('imdb_imported', datetime('now'))`); err != nil {
		return rollback(fmt.Errorf("mark IMDb import: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return IMDbImportStats{}, fmt.Errorf("commit IMDb import: %w", err)
	}
	return stats, nil
}

func movieFromIMDbTitle(title metadata.IMDbTitle) model.Movie {
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
		Metadata:        map[string]any{"source": "imdb_bulk_dataset", "imdb_title_type": title.TitleType},
	}
}

func personFromIMDbName(name metadata.IMDbName) model.Person {
	return model.Person{
		ID:                 metadata.EntityID("person", 0, name.ID, name.PrimaryName, name.BirthYear),
		IMDbID:             name.ID,
		Name:               name.PrimaryName,
		NormalizedName:     normalize(name.PrimaryName),
		BirthYear:          name.BirthYear,
		DeathYear:          name.DeathYear,
		PrimaryProfessions: append([]string(nil), name.PrimaryProfessions...),
		KnownForTitles:     append([]string(nil), name.KnownForTitles...),
		Metadata:           map[string]any{"source": "imdb_bulk_dataset"},
	}
}

func mergeMovie(existing, incoming model.Movie) model.Movie {
	incoming.ID = existing.ID
	if incoming.IMDbID == "" {
		incoming.IMDbID = existing.IMDbID
	}
	if incoming.TMDBID == 0 {
		incoming.TMDBID = existing.TMDBID
	}
	if incoming.TMDBStatus == "" {
		incoming.TMDBStatus = existing.TMDBStatus
	}
	if incoming.FaceBankStatus == "" {
		incoming.FaceBankStatus = existing.FaceBankStatus
	}
	if incoming.Title == "" {
		incoming.Title = existing.Title
	}
	if incoming.OriginalTitle == "" {
		incoming.OriginalTitle = existing.OriginalTitle
	}
	if incoming.Year == nil {
		incoming.Year = existing.Year
	}
	if incoming.StartYear == nil {
		incoming.StartYear = existing.StartYear
	}
	if incoming.EndYear == nil {
		incoming.EndYear = existing.EndYear
	}
	if len(incoming.Genres) == 0 {
		incoming.Genres = append([]string(nil), existing.Genres...)
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
	if incoming.AverageRating == 0 {
		incoming.AverageRating = existing.AverageRating
	}
	if incoming.VoteCount == 0 {
		incoming.VoteCount = existing.VoteCount
	}
	if incoming.Overview == "" {
		incoming.Overview = existing.Overview
	}
	if incoming.PosterURL == "" {
		incoming.PosterURL = existing.PosterURL
	}
	incoming.Metadata = mergeMetadata(existing.Metadata, incoming.Metadata)
	return incoming
}

func mergePerson(existing, incoming model.Person) model.Person {
	incoming.ID = existing.ID
	if incoming.IMDbID == "" {
		incoming.IMDbID = existing.IMDbID
	}
	if incoming.TMDBID == 0 {
		incoming.TMDBID = existing.TMDBID
	}
	if incoming.Name == "" {
		incoming.Name = existing.Name
	}
	if incoming.NormalizedName == "" {
		incoming.NormalizedName = existing.NormalizedName
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
	incoming.Aliases = appendUniqueStrings(existing.Aliases, incoming.Aliases...)
	incoming.Metadata = mergeMetadata(existing.Metadata, incoming.Metadata)
	return incoming
}

func appendUniqueStrings(values []string, additions ...string) []string {
	result := append([]string(nil), values...)
	seen := make(map[string]struct{}, len(result)+len(additions))
	for _, value := range result {
		seen[value] = struct{}{}
	}
	for _, value := range additions {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
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

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
