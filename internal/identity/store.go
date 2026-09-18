package identity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"video-semantic-search/internal/model"
)

type Store interface {
	UpsertMovie(model.Movie) error
	GetMovie(string) (model.Movie, bool)
	FindMovie(string, *int) (model.Movie, bool)
	ListMovies() []model.Movie
	ReplaceMovieCast(string, []model.MovieCast) error
	GetMovieCast(string) []model.MovieCast
	UpsertPerson(model.Person) error
	GetPerson(string) (model.Person, bool)
	FindPerson(string) (model.Person, bool)
	ListPersons() []model.Person
	UpsertPersonImage(model.PersonImage) error
	ListPersonImages(string) []model.PersonImage
	UpsertFaceVector(model.FaceVector) error
	DeleteFaceVector(string) error
	ListFaceVectors(string) []model.FaceVector
	ReplaceScenePeople(string, string, []model.ScenePerson) error
	GetScenePeople(string, string) []model.ScenePerson
	Close() error
}

// ScenePeopleUpdate is one scene's durable identity annotation.
type ScenePeopleUpdate struct {
	SceneID string
	People  []model.ScenePerson
}

// ScenePeopleBatchStore is an optional persistence fast path. Implementations
// can commit all scene annotations in one transaction while older adapters
// continue to use ReplaceScenePeople.
type ScenePeopleBatchStore interface {
	ReplaceScenePeopleBatch(string, []ScenePeopleUpdate) error
}

// PersonBatchStore is an optional persistence fast path for metadata sync.
// Implementations commit a group of person upserts in one transaction.
type PersonBatchStore interface {
	UpsertPersons([]model.Person) error
}

// PersonImageBatchStore is an optional persistence fast path for metadata and
// lazy reference loading. Implementations commit a group of image rows in one
// transaction where the backing store supports transactions.
type PersonImageBatchStore interface {
	UpsertPersonImages([]model.PersonImage) error
}

// PersonSearcher is the optional, bounded lookup path used by the web actor
// picker. It avoids serializing the complete local person catalog for every
// keystroke while keeping the catalog itself local and authoritative.
type PersonSearcher interface {
	SearchPersons(string, int) []model.Person
}

// PersonCatalog is the optional identity lookup seam used by TMDB
// enrichment and the search actor picker. Implementations must treat IMDb
// and TMDB IDs as aliases of one person. SearchReadyPersons returns only
// IMDb-backed people with at least one ready face vector; implementations
// that support ProcessedMoviePersonSearcher apply the same rule to the
// processed-movie scope.
type PersonCatalog interface {
	FindPersonByIMDbID(string) (model.Person, bool)
	FindPersonByTMDBID(int) (model.Person, bool)
	SearchReadyPersons(string, int) []model.Person
	FaceVectorCount(string) int
}

// MinimumReferencePersonCatalog is the readiness-aware actor-picker seam.
// It is optional so older/custom stores can continue to expose the legacy
// "at least one vector" lookup while production stores apply the configured
// face-bank minimum.
type MinimumReferencePersonCatalog interface {
	SearchReadyPersonsWithMinimum(string, int, int) []model.Person
}

// ProcessedMoviePersonSearcher narrows the actor picker to IMDb cast members
// of movies that already have extracted frames in the main media index. It is
// optional so older/custom stores can continue to serve the broader ready
// person query.
type ProcessedMoviePersonSearcher interface {
	SearchReadyPersonsForMovies(string, int, []string) []model.Person
}

// MinimumReferenceProcessedMoviePersonSearcher applies the same configured
// face-bank minimum while restricting the actor picker to processed movies.
type MinimumReferenceProcessedMoviePersonSearcher interface {
	SearchReadyPersonsForMoviesWithMinimum(string, int, []string, int) []model.Person
}

// MovieCatalog is the indexed movie lookup seam used by import and ingest
// preparation. It avoids scanning the complete IMDb catalogue for one title.
type MovieCatalog interface {
	FindMovieByIMDbID(string) (model.Movie, bool)
}

// MovieSearcher keeps catalogue administration bounded once the full IMDb
// snapshot is imported.
type MovieSearcher interface {
	SearchMovies(string, int) []model.Movie
}

// MovieTitleSearcher is the fast resolver path for release filenames. It
// searches primary/original IMDb titles without the expensive alternate-title
// scan used by the administrative movie picker.
type MovieTitleSearcher interface {
	SearchMoviesByTitle(string, int) []model.Movie
}

// MovieExactTitleSearcher is the bounded lookup path used while resolving a
// release filename. Production SQLite implementations can answer exact IMDb
// alias queries through the alias index without scanning the full catalog.
type MovieExactTitleSearcher interface {
	SearchMoviesByExactTitle(string, int) []model.Movie
}

type fileState struct {
	Version      int                            `json:"version"`
	Movies       map[string]model.Movie         `json:"movies"`
	MovieCast    map[string][]model.MovieCast   `json:"movie_cast"`
	Persons      map[string]model.Person        `json:"persons"`
	PersonImages map[string]model.PersonImage   `json:"person_images"`
	FaceVectors  map[string]model.FaceVector    `json:"face_vectors"`
	ScenePeople  map[string][]model.ScenePerson `json:"scene_people"`
}

type FileStore struct {
	mu         sync.RWMutex
	path       string
	state      fileState
	batchDepth int
	batchDirty bool
}

func NewFileStore(path string) (*FileStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("identity store path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create identity store directory: %w", err)
	}
	store := &FileStore{path: path, state: newFileState()}
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read identity store: %w", err)
	}
	if len(content) > 0 {
		if err := json.Unmarshal(content, &store.state); err != nil {
			return nil, fmt.Errorf("decode identity store: %w", err)
		}
	}
	store.state.ensureMaps()
	return store, nil
}

func newFileState() fileState {
	state := fileState{Version: 1}
	state.ensureMaps()
	return state
}

func (s *fileState) ensureMaps() {
	if s.Version == 0 {
		s.Version = 1
	}
	if s.Movies == nil {
		s.Movies = map[string]model.Movie{}
	}
	if s.MovieCast == nil {
		s.MovieCast = map[string][]model.MovieCast{}
	}
	if s.Persons == nil {
		s.Persons = map[string]model.Person{}
	}
	if s.PersonImages == nil {
		s.PersonImages = map[string]model.PersonImage{}
	}
	if s.FaceVectors == nil {
		s.FaceVectors = map[string]model.FaceVector{}
	}
	if s.ScenePeople == nil {
		s.ScenePeople = map[string][]model.ScenePerson{}
	}
}

func (s *FileStore) UpsertMovie(movie model.Movie) error {
	if strings.TrimSpace(movie.ID) == "" || strings.TrimSpace(movie.Title) == "" {
		return fmt.Errorf("movie id and title are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Movies[movie.ID] = cloneMovie(movie)
	return s.persistLocked()
}

// ApplyCatalog atomically applies a metadata-only catalog batch. The normal
// single-record methods intentionally persist immediately for the HTTP API;
// bulk import must not rewrite an ever-growing JSON file once per title.
func (s *FileStore) ApplyCatalog(movies []model.Movie, persons []model.Person, casts map[string][]model.MovieCast) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, movie := range movies {
		if strings.TrimSpace(movie.ID) == "" || strings.TrimSpace(movie.Title) == "" {
			return fmt.Errorf("movie id and title are required")
		}
	}
	for _, person := range persons {
		if strings.TrimSpace(person.ID) == "" || strings.TrimSpace(person.Name) == "" {
			return fmt.Errorf("person id and name are required")
		}
	}
	for movieID, movieCast := range casts {
		if strings.TrimSpace(movieID) == "" {
			return fmt.Errorf("movie id is required")
		}
		for _, item := range movieCast {
			if strings.TrimSpace(item.PersonID) == "" {
				return fmt.Errorf("cast person id is required")
			}
		}
	}
	for _, movie := range movies {
		s.state.Movies[movie.ID] = cloneMovie(movie)
	}
	for _, person := range persons {
		if strings.TrimSpace(person.NormalizedName) == "" {
			person.NormalizedName = normalize(person.Name)
		}
		s.state.Persons[person.ID] = clonePerson(person)
	}
	for movieID, movieCast := range casts {
		copyCast := make([]model.MovieCast, 0, len(movieCast))
		seen := map[string]struct{}{}
		for _, item := range movieCast {
			item.MovieID = movieID
			if _, ok := seen[item.PersonID]; ok {
				continue
			}
			seen[item.PersonID] = struct{}{}
			copyCast = append(copyCast, item)
		}
		s.state.MovieCast[movieID] = copyCast
	}
	return s.persistLocked()
}

// BeginBatch defers the JSON rewrite performed by the mutating methods until
// CommitBatch. Catalog imports use bounded batches because one avatar/vector
// can otherwise rewrite the entire identity file.
func (s *FileStore) BeginBatch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchDepth++
}

// CommitBatch durably writes all changes made since the matching BeginBatch.
// Calling it with no active batch is an error so an import cannot silently
// believe it has checkpointed when it has not.
func (s *FileStore) CommitBatch() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.batchDepth == 0 {
		return fmt.Errorf("identity batch is not active")
	}
	s.batchDepth--
	if s.batchDepth > 0 || !s.batchDirty {
		return nil
	}
	if err := s.persistNowLocked(); err != nil {
		return err
	}
	s.batchDirty = false
	return nil
}

func (s *FileStore) GetMovie(id string) (model.Movie, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	movie, ok := s.state.Movies[strings.TrimSpace(id)]
	return cloneMovie(movie), ok
}

func (s *FileStore) FindMovie(title string, year *int) (model.Movie, bool) {
	want := normalize(title)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, movie := range s.state.Movies {
		if normalize(movie.Title) != want && normalize(movie.OriginalTitle) != want {
			continue
		}
		// A locally imported movie may not have a release year yet. In that
		// case title matching is still useful; reject only a known mismatch.
		if year != nil && movie.Year != nil && *movie.Year != *year {
			continue
		}
		return cloneMovie(movie), true
	}
	return model.Movie{}, false
}

func (s *FileStore) FindMovieByIMDbID(imdbID string) (model.Movie, bool) {
	want := strings.ToLower(strings.TrimSpace(imdbID))
	if want == "" {
		return model.Movie{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, movie := range s.state.Movies {
		if strings.ToLower(strings.TrimSpace(movie.IMDbID)) == want {
			return cloneMovie(movie), true
		}
	}
	return model.Movie{}, false
}

func (s *FileStore) ListMovies() []model.Movie {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]model.Movie, 0, len(s.state.Movies))
	for _, movie := range s.state.Movies {
		result = append(result, cloneMovie(movie))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Title < result[j].Title })
	return result
}

func (s *FileStore) SearchMovies(query string, limit int) []model.Movie {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	want := normalize(query)
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]model.Movie, 0, limit)
	for _, movie := range s.state.Movies {
		if want != "" && !strings.Contains(normalize(movie.Title), want) && !strings.Contains(normalize(movie.OriginalTitle), want) {
			continue
		}
		result = append(result, cloneMovie(movie))
	}
	sort.Slice(result, func(left, right int) bool {
		leftTitle := normalize(result[left].Title)
		rightTitle := normalize(result[right].Title)
		if want != "" {
			leftRank := movieSearchRank(leftTitle, want)
			rightRank := movieSearchRank(rightTitle, want)
			if leftRank != rightRank {
				return leftRank < rightRank
			}
		}
		if leftTitle != rightTitle {
			return leftTitle < rightTitle
		}
		return result[left].ID < result[right].ID
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

// SearchMoviesByTitle uses the same in-memory bounded scan as SearchMovies;
// the distinction matters only for the SQLite adapter, where aliases are a
// separate large table.
func (s *FileStore) SearchMoviesByTitle(query string, limit int) []model.Movie {
	return s.SearchMovies(query, limit)
}

func (s *FileStore) SearchMoviesByExactTitle(query string, limit int) []model.Movie {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	want := normalizeLoose(query)
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]model.Movie, 0, limit)
	for _, movie := range s.state.Movies {
		matched := normalizeLoose(movie.Title) == want || normalizeLoose(movie.OriginalTitle) == want
		if !matched {
			for _, alias := range movie.AlternateTitles {
				if normalizeLoose(alias) == want {
					matched = true
					break
				}
			}
		}
		if matched {
			result = append(result, cloneMovie(movie))
		}
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
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func movieSearchRank(title, query string) int {
	if title == query {
		return 0
	}
	if strings.HasPrefix(title, query) {
		return 1
	}
	return 2
}

func (s *FileStore) ReplaceMovieCast(movieID string, cast []model.MovieCast) error {
	movieID = strings.TrimSpace(movieID)
	if movieID == "" {
		return fmt.Errorf("movie id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	copyCast := make([]model.MovieCast, 0, len(cast))
	seen := map[string]struct{}{}
	for _, item := range cast {
		item.MovieID = movieID
		if strings.TrimSpace(item.PersonID) == "" {
			return fmt.Errorf("cast person id is required")
		}
		if _, ok := seen[item.PersonID]; ok {
			continue
		}
		seen[item.PersonID] = struct{}{}
		copyCast = append(copyCast, item)
	}
	s.state.MovieCast[movieID] = copyCast
	return s.persistLocked()
}

func (s *FileStore) GetMovieCast(movieID string) []model.MovieCast {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]model.MovieCast(nil), s.state.MovieCast[strings.TrimSpace(movieID)]...)
}

func (s *FileStore) UpsertPerson(person model.Person) error {
	if strings.TrimSpace(person.ID) == "" || strings.TrimSpace(person.Name) == "" {
		return fmt.Errorf("person id and name are required")
	}
	if strings.TrimSpace(person.NormalizedName) == "" {
		person.NormalizedName = normalize(person.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Persons[person.ID] = clonePerson(person)
	return s.persistLocked()
}

func (s *FileStore) UpsertPersons(persons []model.Person) error {
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
	for _, person := range persons {
		if strings.TrimSpace(person.NormalizedName) == "" {
			person.NormalizedName = normalize(person.Name)
		}
		s.state.Persons[person.ID] = clonePerson(person)
	}
	return s.persistLocked()
}

func (s *FileStore) GetPerson(id string) (model.Person, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	person, ok := s.state.Persons[strings.TrimSpace(id)]
	return clonePerson(person), ok
}

func (s *FileStore) FindPerson(query string) (model.Person, bool) {
	want := normalize(query)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, person := range s.state.Persons {
		if normalize(person.Name) == want || normalize(person.NormalizedName) == want {
			return clonePerson(person), true
		}
		for _, alias := range person.Aliases {
			if normalize(alias) == want {
				return clonePerson(person), true
			}
		}
	}
	return model.Person{}, false
}

func (s *FileStore) FindPersonByIMDbID(imdbID string) (model.Person, bool) {
	want := strings.ToLower(strings.TrimSpace(imdbID))
	if want == "" {
		return model.Person{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, person := range s.state.Persons {
		if strings.ToLower(strings.TrimSpace(person.IMDbID)) == want {
			return clonePerson(person), true
		}
	}
	return model.Person{}, false
}

func (s *FileStore) FindPersonByTMDBID(tmdbID int) (model.Person, bool) {
	if tmdbID <= 0 {
		return model.Person{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, person := range s.state.Persons {
		if person.TMDBID == tmdbID {
			return clonePerson(person), true
		}
	}
	return model.Person{}, false
}

func (s *FileStore) ListPersons() []model.Person {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]model.Person, 0, len(s.state.Persons))
	for _, person := range s.state.Persons {
		result = append(result, clonePerson(person))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// SearchPersons returns a small, deterministic slice of local people. Exact
// and prefix matches are preferred over substring matches so the UI remains
// useful even when the IMDb catalog contains many similarly named people.
func (s *FileStore) SearchPersons(query string, limit int) []model.Person {
	query = normalize(query)
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	type candidate struct {
		person model.Person
		rank   int
	}
	s.mu.RLock()
	candidates := make([]candidate, 0)
	for _, person := range s.state.Persons {
		rank := 3
		if query == "" {
			rank = 0
		} else {
			fields := append([]string{person.Name, person.NormalizedName, person.IMDbID}, person.Aliases...)
			for _, field := range fields {
				value := normalize(field)
				if value == query {
					rank = 0
					break
				}
				if rank > 1 && strings.HasPrefix(value, query) {
					rank = 1
					continue
				}
				if rank > 2 && strings.Contains(value, query) {
					rank = 2
				}
			}
		}
		if rank < 3 {
			// Keep the value shallow while ranking. Deep-copy only the bounded
			// result below; this matters for the large IMDb catalog.
			candidates = append(candidates, candidate{person: person, rank: rank})
		}
	}

	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].rank != candidates[right].rank {
			return candidates[left].rank < candidates[right].rank
		}
		leftName := normalize(candidates[left].person.Name)
		rightName := normalize(candidates[right].person.Name)
		if leftName != rightName {
			return leftName < rightName
		}
		return candidates[left].person.ID < candidates[right].person.ID
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	result := make([]model.Person, 0, len(candidates))
	for _, item := range candidates {
		result = append(result, clonePerson(item.person))
	}
	s.mu.RUnlock()
	return result
}

// SearchReadyPersons is the bounded actor-picker query. A person is ready
// when at least one face vector is available; this prevents the UI from
// offering filters that can never match a scene.
func (s *FileStore) SearchReadyPersons(query string, limit int) []model.Person {
	return s.searchReadyPersons(query, limit, nil, false, 1)
}

func (s *FileStore) SearchReadyPersonsWithMinimum(query string, limit, minimumReferences int) []model.Person {
	return s.searchReadyPersons(query, limit, nil, false, minimumReferences)
}

// SearchReadyPersonsForMovies returns only ready IMDb people that occur in
// authoritative IMDb cast rows for the supplied processed movies.
func (s *FileStore) SearchReadyPersonsForMovies(query string, limit int, movieIDs []string) []model.Person {
	if len(movieIDs) == 0 {
		return []model.Person{}
	}
	return s.searchReadyPersons(query, limit, movieIDs, true, 1)
}

func (s *FileStore) SearchReadyPersonsForMoviesWithMinimum(query string, limit int, movieIDs []string, minimumReferences int) []model.Person {
	if len(movieIDs) == 0 {
		return []model.Person{}
	}
	return s.searchReadyPersons(query, limit, movieIDs, true, minimumReferences)
}

func (s *FileStore) searchReadyPersons(query string, limit int, movieIDs []string, restrictMovies bool, minimumReferences int) []model.Person {
	query = normalize(query)
	if minimumReferences < 1 {
		minimumReferences = 1
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	s.mu.RLock()
	counts := make(map[string]int)
	for _, vector := range s.state.FaceVectors {
		if strings.TrimSpace(vector.PersonID) != "" {
			counts[vector.PersonID]++
		}
	}
	eligible := make(map[string]struct{})
	if restrictMovies {
		wantedMovies := make(map[string]struct{}, len(movieIDs))
		for _, movieID := range movieIDs {
			movieID = strings.TrimSpace(movieID)
			if movieID != "" {
				wantedMovies[movieID] = struct{}{}
			}
		}
		for movieID, cast := range s.state.MovieCast {
			if _, wanted := wantedMovies[movieID]; !wanted {
				continue
			}
			for _, item := range cast {
				if item.Source != "imdb_principals" {
					continue
				}
				person, ok := s.state.Persons[item.PersonID]
				if ok && strings.TrimSpace(person.IMDbID) != "" {
					eligible[person.ID] = struct{}{}
				}
			}
		}
	}
	type candidate struct {
		person model.Person
		rank   int
	}
	candidates := make([]candidate, 0)
	for _, person := range s.state.Persons {
		if strings.TrimSpace(person.IMDbID) == "" {
			continue
		}
		if counts[person.ID] < minimumReferences {
			continue
		}
		if restrictMovies {
			if _, ok := eligible[person.ID]; !ok {
				continue
			}
		}
		rank := 3
		if query == "" {
			rank = 0
		} else {
			for _, field := range append([]string{person.Name, person.NormalizedName, person.IMDbID}, person.Aliases...) {
				value := normalize(field)
				if value == query {
					rank = 0
					break
				}
				if rank > 1 && strings.HasPrefix(value, query) {
					rank = 1
				}
				if rank > 2 && strings.Contains(value, query) {
					rank = 2
				}
			}
		}
		if rank < 3 {
			candidates = append(candidates, candidate{person: person, rank: rank})
		}
	}
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].rank != candidates[right].rank {
			return candidates[left].rank < candidates[right].rank
		}
		leftName := normalize(candidates[left].person.Name)
		rightName := normalize(candidates[right].person.Name)
		if leftName != rightName {
			return leftName < rightName
		}
		return candidates[left].person.ID < candidates[right].person.ID
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	result := make([]model.Person, 0, len(candidates))
	for _, item := range candidates {
		result = append(result, clonePerson(item.person))
	}
	s.mu.RUnlock()
	return result
}

func (s *FileStore) FaceVectorCount(personID string) int {
	want := strings.TrimSpace(personID)
	if want == "" {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, vector := range s.state.FaceVectors {
		if vector.PersonID == want {
			count++
		}
	}
	return count
}

func (s *FileStore) UpsertPersonImage(image model.PersonImage) error {
	if strings.TrimSpace(image.ID) == "" || strings.TrimSpace(image.PersonID) == "" {
		return fmt.Errorf("person image id and person id are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.PersonImages[image.ID] = image
	return s.persistLocked()
}

func (s *FileStore) UpsertPersonImages(images []model.PersonImage) error {
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
	for _, image := range images {
		s.state.PersonImages[image.ID] = image
	}
	return s.persistLocked()
}

func (s *FileStore) ListPersonImages(personID string) []model.PersonImage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]model.PersonImage, 0)
	for _, image := range s.state.PersonImages {
		if image.PersonID == strings.TrimSpace(personID) {
			result = append(result, image)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// ClearRemoteLocalPaths removes cached file paths from remotely sourced
// images. The image bytes are intentionally ephemeral in vector-first mode;
// keeping a stale path after the file has been removed makes the record look
// reusable when it is not.
func (s *FileStore) ClearRemoteLocalPaths() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cleared := 0
	for id, image := range s.state.PersonImages {
		if strings.TrimSpace(image.LocalPath) == "" || strings.TrimSpace(image.SourceURL) == "" {
			continue
		}
		image.LocalPath = ""
		s.state.PersonImages[id] = image
		cleared++
	}
	if cleared == 0 {
		return 0, nil
	}
	return cleared, s.persistLocked()
}

func (s *FileStore) UpsertFaceVector(vector model.FaceVector) error {
	if strings.TrimSpace(vector.ID) == "" || strings.TrimSpace(vector.PersonID) == "" || len(vector.Vector) == 0 {
		return fmt.Errorf("face vector id, person id and vector are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	vector.Vector = append([]float32(nil), vector.Vector...)
	s.state.FaceVectors[vector.ID] = vector
	return s.persistLocked()
}

func (s *FileStore) DeleteFaceVector(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.state.FaceVectors[strings.TrimSpace(id)]; !exists {
		return nil
	}
	delete(s.state.FaceVectors, strings.TrimSpace(id))
	return s.persistLocked()
}

func (s *FileStore) ListFaceVectors(personID string) []model.FaceVector {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]model.FaceVector, 0)
	for _, vector := range s.state.FaceVectors {
		if vector.PersonID != strings.TrimSpace(personID) {
			continue
		}
		copyVector := vector
		copyVector.Vector = append([]float32(nil), vector.Vector...)
		result = append(result, copyVector)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func scenePeopleKey(mediaID, sceneID string) string {
	return strings.TrimSpace(mediaID) + "\x00" + strings.TrimSpace(sceneID)
}

func (s *FileStore) ReplaceScenePeople(mediaID, sceneID string, people []model.ScenePerson) error {
	return s.ReplaceScenePeopleBatch(mediaID, []ScenePeopleUpdate{{SceneID: sceneID, People: people}})
}

func (s *FileStore) ReplaceScenePeopleBatch(mediaID string, updates []ScenePeopleUpdate) error {
	mediaID = strings.TrimSpace(mediaID)
	if mediaID == "" {
		return fmt.Errorf("media id is required")
	}
	if len(updates) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := make(map[string][]model.ScenePerson, len(updates))
	for _, update := range updates {
		sceneID := strings.TrimSpace(update.SceneID)
		if sceneID == "" {
			return fmt.Errorf("scene id is required")
		}
		key := scenePeopleKey(mediaID, sceneID)
		if _, exists := previous[key]; !exists {
			previous[key] = append([]model.ScenePerson(nil), s.state.ScenePeople[key]...)
		}
		s.state.ScenePeople[key] = append([]model.ScenePerson(nil), update.People...)
	}
	if err := s.persistLocked(); err != nil {
		for key, people := range previous {
			s.state.ScenePeople[key] = people
		}
		return err
	}
	return nil
}

func (s *FileStore) GetScenePeople(mediaID, sceneID string) []model.ScenePerson {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]model.ScenePerson(nil), s.state.ScenePeople[scenePeopleKey(mediaID, sceneID)]...)
}

func (s *FileStore) Close() error { return nil }

func (s *FileStore) persistLocked() error {
	if s.batchDepth > 0 {
		s.batchDirty = true
		return nil
	}
	return s.persistNowLocked()
}

func (s *FileStore) persistNowLocked() error {
	content, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode identity store: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".video-identity-*")
	if err != nil {
		return fmt.Errorf("create identity temporary: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write identity temporary: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync identity temporary: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close identity temporary: %w", err)
	}
	if err := os.Rename(temporaryName, s.path); err != nil {
		return fmt.Errorf("replace identity store: %w", err)
	}
	return nil
}

func normalize(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func cloneMovie(movie model.Movie) model.Movie {
	movie.Genres = append([]string(nil), movie.Genres...)
	movie.AlternateTitles = append([]string(nil), movie.AlternateTitles...)
	movie.Directors = append([]string(nil), movie.Directors...)
	movie.Writers = append([]string(nil), movie.Writers...)
	movie.Metadata = cloneMetadata(movie.Metadata)
	return movie
}

func clonePerson(person model.Person) model.Person {
	person.PrimaryProfessions = append([]string(nil), person.PrimaryProfessions...)
	person.KnownForTitles = append([]string(nil), person.KnownForTitles...)
	person.Aliases = append([]string(nil), person.Aliases...)
	person.Metadata = cloneMetadata(person.Metadata)
	return person
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
