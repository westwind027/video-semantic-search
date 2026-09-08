package metadata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"video-semantic-search/internal/httpclient"
	"video-semantic-search/internal/model"
)

type TMDBClient struct {
	apiKey           string
	baseURL          string
	language         string
	httpClient       *http.Client
	profileWorkers   int
	profileSem       chan struct{}
	imageMu          sync.Mutex
	imageCache       map[int][]TMDBProfileImage
	imageInProgress  map[int]*tmdbProfileRequest
	personMu         sync.Mutex
	personCache      map[string]model.Person
	personInProgress map[string]*tmdbPersonRequest
}

type TMDBSyncResult struct {
	Movie        model.Movie
	Cast         []model.MovieCast
	Persons      []model.Person
	ProfileImage []model.PersonImage
	Warnings     []string
}

type TMDBProfileImage struct {
	FilePath    string  `json:"file_path"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	VoteAverage float32 `json:"vote_average"`
}

type tmdbProfileRequest struct {
	done     chan struct{}
	profiles []TMDBProfileImage
	err      error
}

type tmdbPersonRequest struct {
	done   chan struct{}
	person model.Person
	err    error
}

const (
	defaultTMDBProfileWorkers = 4
	maxTMDBProfileWorkers     = 16
)

func NewTMDBClient(apiKey, baseURL string, timeout time.Duration) *TMDBClient {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.themoviedb.org/3"
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	profileWorkers := tmdbProfileWorkerCount()
	// IMDb owns the canonical person name and cast relation. Keep TMDB lookups
	// in English so the name-based fallback can match IMDb primaryName instead
	// of comparing localized names such as "威尔·史密斯".
	return &TMDBClient{
		apiKey:           strings.TrimSpace(apiKey),
		baseURL:          strings.TrimRight(baseURL, "/"),
		language:         "en-US",
		httpClient:       httpclient.NewDirectClient(timeout),
		profileWorkers:   profileWorkers,
		profileSem:       make(chan struct{}, profileWorkers),
		imageCache:       make(map[int][]TMDBProfileImage),
		imageInProgress:  make(map[int]*tmdbProfileRequest),
		personCache:      make(map[string]model.Person),
		personInProgress: make(map[string]*tmdbPersonRequest),
	}
}

func NewTMDBClientFromEnv() *TMDBClient {
	client := NewTMDBClient(strings.TrimSpace(getenv("TMDB_API_KEY")), getenvOr("TMDB_BASE_URL", "https://api.themoviedb.org/3"), 30*time.Second)
	proxyURL := strings.TrimSpace(getenv("TMDB_PROXY_URL"))
	if proxyURL == "" {
		return client
	}
	if proxyClient, err := httpclient.NewProxyClient(30*time.Second, proxyURL); err == nil {
		client.httpClient = proxyClient
	}
	return client
}

// FindPersonByIMDbID resolves one IMDb person to the stable TMDB person ID.
// It is used by the lazy loader so an actor is looked up only when a movie
// being processed actually needs a reference vector.
func (c *TMDBClient) FindPersonByIMDbID(ctx context.Context, imdbID string) (model.Person, error) {
	imdbID = strings.TrimSpace(imdbID)
	if imdbID == "" {
		return model.Person{}, fmt.Errorf("IMDb person id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cacheKey := strings.ToLower(imdbID)
	c.personMu.Lock()
	if person, ok := c.personCache[cacheKey]; ok {
		c.personMu.Unlock()
		return cloneTMDBPerson(person), nil
	}
	if pending, ok := c.personInProgress[cacheKey]; ok {
		c.personMu.Unlock()
		select {
		case <-pending.done:
			return cloneTMDBPerson(pending.person), pending.err
		case <-ctx.Done():
			return model.Person{}, ctx.Err()
		}
	}
	pending := &tmdbPersonRequest{done: make(chan struct{})}
	c.personInProgress[cacheKey] = pending
	c.personMu.Unlock()

	var result struct {
		PersonResults []struct {
			ID          int    `json:"id"`
			Name        string `json:"name"`
			ProfilePath string `json:"profile_path"`
		} `json:"person_results"`
	}
	err := c.get(ctx, "/find/"+url.PathEscape(imdbID), url.Values{"external_source": []string{"imdb_id"}, "language": []string{c.language}}, &result)
	var person model.Person
	if err == nil {
		if len(result.PersonResults) == 0 || result.PersonResults[0].ID <= 0 || strings.TrimSpace(result.PersonResults[0].Name) == "" {
			err = fmt.Errorf("TMDB person not found for IMDb id %q", imdbID)
		} else {
			item := result.PersonResults[0]
			person = model.Person{ID: EntityID("person", item.ID, imdbID, item.Name, nil), IMDbID: imdbID, TMDBID: item.ID, Name: item.Name, NormalizedName: normalizeName(item.Name), Metadata: map[string]any{"source": "tmdb"}}
		}
	}
	c.personMu.Lock()
	delete(c.personInProgress, cacheKey)
	pending.err = err
	if err == nil {
		pending.person = cloneTMDBPerson(person)
		c.personCache[cacheKey] = cloneTMDBPerson(person)
	}
	close(pending.done)
	c.personMu.Unlock()
	if err != nil {
		return model.Person{}, err
	}
	return cloneTMDBPerson(person), nil
}

func (c *TMDBClient) Configured() bool { return c != nil && c.apiKey != "" }

// FindMovieByIMDbID resolves an IMDb title ID through TMDB's external-id
// endpoint. Using the stable ID avoids title translation and release-name
// ambiguity when importing a local IMDb dataset.
func (c *TMDBClient) FindMovieByIMDbID(ctx context.Context, imdbID string) (int, error) {
	imdbID = strings.TrimSpace(imdbID)
	if imdbID == "" {
		return 0, fmt.Errorf("IMDb title id is required")
	}
	var result struct {
		MovieResults []tmdbMovie `json:"movie_results"`
	}
	if err := c.get(ctx, "/find/"+url.PathEscape(imdbID), url.Values{"external_source": []string{"imdb_id"}, "language": []string{c.language}}, &result); err != nil {
		return 0, err
	}
	if len(result.MovieResults) == 0 || result.MovieResults[0].ID <= 0 {
		return 0, fmt.Errorf("TMDB movie not found for IMDb id %q", imdbID)
	}
	return result.MovieResults[0].ID, nil
}

// GetMovieMetadataByIMDbID fetches only movie metadata. It deliberately does
// not append credits; lazy video processing already has the IMDb principals
// locally and should not pay for a second cast expansion.
func (c *TMDBClient) GetMovieMetadataByIMDbID(ctx context.Context, imdbID string) (model.Movie, error) {
	tmdbID, err := c.FindMovieByIMDbID(ctx, imdbID)
	if err != nil {
		return model.Movie{}, err
	}
	var detail tmdbMovie
	if err := c.get(ctx, "/movie/"+strconv.Itoa(tmdbID), url.Values{"language": []string{c.language}}, &detail); err != nil {
		return model.Movie{}, err
	}
	if strings.TrimSpace(detail.Title) == "" {
		return model.Movie{}, fmt.Errorf("TMDB movie %d returned no title", tmdbID)
	}
	return model.Movie{
		IMDbID:        firstNonEmpty(detail.IMDbID, imdbID),
		TMDBID:        detail.ID,
		Title:         detail.Title,
		OriginalTitle: detail.OriginalTitle,
		Year:          yearFromDate(detail.ReleaseDate),
		Overview:      detail.Overview,
		PosterURL:     imageURL(detail.PosterPath),
		Metadata:      map[string]any{"source": "tmdb", "tmdb_metadata_loaded": true},
	}, nil
}

// GetPersonImages returns every profile image currently exposed by TMDB for a
// person. The caller decides which images to retain and how to build vectors.
func (c *TMDBClient) GetPersonImages(ctx context.Context, personID int) ([]TMDBProfileImage, error) {
	if personID <= 0 {
		return nil, fmt.Errorf("TMDB person id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.imageMu.Lock()
	if profiles, ok := c.imageCache[personID]; ok {
		result := cloneTMDBProfileImages(profiles)
		c.imageMu.Unlock()
		return result, nil
	}
	if pending, ok := c.imageInProgress[personID]; ok {
		c.imageMu.Unlock()
		select {
		case <-pending.done:
			return cloneTMDBProfileImages(pending.profiles), pending.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	pending := &tmdbProfileRequest{done: make(chan struct{})}
	c.imageInProgress[personID] = pending
	c.imageMu.Unlock()
	if c.profileSem != nil {
		select {
		case c.profileSem <- struct{}{}:
			defer func() { <-c.profileSem }()
		case <-ctx.Done():
			c.imageMu.Lock()
			delete(c.imageInProgress, personID)
			pending.err = ctx.Err()
			close(pending.done)
			c.imageMu.Unlock()
			return nil, ctx.Err()
		}
	}

	var result struct {
		Profiles []TMDBProfileImage `json:"profiles"`
	}
	err := c.get(ctx, "/person/"+strconv.Itoa(personID)+"/images", url.Values{"language": []string{c.language}}, &result)
	c.imageMu.Lock()
	delete(c.imageInProgress, personID)
	pending.err = err
	if err == nil {
		pending.profiles = cloneTMDBProfileImages(result.Profiles)
		c.imageCache[personID] = cloneTMDBProfileImages(result.Profiles)
	}
	close(pending.done)
	c.imageMu.Unlock()
	if err != nil {
		return nil, err
	}
	return cloneTMDBProfileImages(result.Profiles), nil
}

// SyncMovieByIMDbID imports the complete TMDB cast for one IMDb title and all
// profile images for each cast member. It is intentionally separate from the
// existing API's lightweight top-cast sync so a catalog builder can opt into
// the much larger, resumable image pipeline.
func (c *TMDBClient) SyncMovieByIMDbID(ctx context.Context, imdbID, fallbackTitle string, year *int) (TMDBSyncResult, error) {
	return c.syncMovieByIMDbID(ctx, imdbID, fallbackTitle, year, nil)
}

// SyncMovieByIMDbIDForNames is the bounded enrichment path used by the IMDb
// authoritative import and the movie preparation gate. TMDB credits can
// contain many extras that are not present in IMDb title.principals; filtering
// by the already imported English IMDb names avoids profile requests whose
// results would be discarded by the identity layer. An empty, non-nil names
// slice intentionally means that no TMDB cast member is eligible.
func (c *TMDBClient) SyncMovieByIMDbIDForNames(ctx context.Context, imdbID, fallbackTitle string, year *int, names []string) (TMDBSyncResult, error) {
	allowedNames := make(map[string]struct{}, len(names))
	for _, name := range names {
		if normalized := normalizeName(name); normalized != "" {
			allowedNames[normalized] = struct{}{}
		}
	}
	return c.syncMovieByIMDbID(ctx, imdbID, fallbackTitle, year, allowedNames)
}

func (c *TMDBClient) syncMovieByIMDbID(ctx context.Context, imdbID, fallbackTitle string, year *int, allowedNames map[string]struct{}) (TMDBSyncResult, error) {
	if !c.Configured() {
		return TMDBSyncResult{}, fmt.Errorf("TMDB_API_KEY is not configured")
	}
	tmdbID, err := c.FindMovieByIMDbID(ctx, imdbID)
	if err != nil {
		if strings.TrimSpace(fallbackTitle) == "" {
			return TMDBSyncResult{}, err
		}
		tmdbID, err = c.resolveMovieID(ctx, fallbackTitle, year)
		if err != nil {
			return TMDBSyncResult{}, err
		}
	}
	var detail tmdbMovie
	if err := c.get(ctx, "/movie/"+strconv.Itoa(tmdbID), url.Values{"append_to_response": []string{"credits"}, "language": []string{c.language}}, &detail); err != nil {
		return TMDBSyncResult{}, err
	}
	if strings.TrimSpace(detail.Title) == "" {
		return TMDBSyncResult{}, fmt.Errorf("TMDB movie %d returned no title", tmdbID)
	}
	movieIMDbID := strings.TrimSpace(detail.IMDbID)
	if movieIMDbID == "" {
		movieIMDbID = strings.TrimSpace(imdbID)
	}
	movieYear := yearFromDate(detail.ReleaseDate)
	movie := model.Movie{ID: EntityID("movie", detail.ID, movieIMDbID, detail.Title, movieYear), IMDbID: movieIMDbID, TMDBID: detail.ID, Title: detail.Title, OriginalTitle: detail.OriginalTitle, Year: movieYear, Overview: detail.Overview, PosterURL: imageURL(detail.PosterPath), Metadata: map[string]any{"source": "tmdb", "full_cast": true, "full_profile_images": true}}
	result := TMDBSyncResult{Movie: movie, Cast: make([]model.MovieCast, 0, len(detail.Credits.Cast)), Persons: make([]model.Person, 0, len(detail.Credits.Cast)), ProfileImage: make([]model.PersonImage, 0)}
	profileImageSize := ProfileImageSize()
	groups := make([]tmdbCastGroup, 0, len(detail.Credits.Cast))
	groupByPersonID := make(map[int]int, len(detail.Credits.Cast))
	for order, item := range detail.Credits.Cast {
		if item.ID <= 0 || strings.TrimSpace(item.Name) == "" {
			continue
		}
		if allowedNames != nil {
			if _, allowed := allowedNames[normalizeName(item.Name)]; !allowed {
				continue
			}
		}
		personID := EntityID("person", item.ID, "", item.Name, nil)
		if _, exists := groupByPersonID[item.ID]; !exists {
			groupByPersonID[item.ID] = len(groups)
			groups = append(groups, tmdbCastGroup{item: item, personID: personID})
			result.Persons = append(result.Persons, model.Person{ID: personID, TMDBID: item.ID, Name: item.Name, NormalizedName: normalizeName(item.Name), Metadata: map[string]any{"source": "tmdb"}})
		}
		result.Cast = append(result.Cast, model.MovieCast{MovieID: movie.ID, PersonID: personID, CharacterName: item.Character, BillingOrder: order, Source: "tmdb"})
	}

	profiles := make([][]TMDBProfileImage, len(groups))
	profileErrors := make([]error, len(groups))
	workers := c.profileWorkers
	if workers <= 0 {
		workers = defaultTMDBProfileWorkers
	}
	if workers > len(groups) {
		workers = len(groups)
	}
	if workers > 0 {
		jobs := make(chan int)
		var wait sync.WaitGroup
		for worker := 0; worker < workers; worker++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				for index := range jobs {
					profiles[index], profileErrors[index] = c.GetPersonImages(ctx, groups[index].item.ID)
				}
			}()
		}
		for index := range groups {
			jobs <- index
		}
		close(jobs)
		wait.Wait()
	}
	for index, group := range groups {
		if profileErrors[index] != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("get TMDB profile images for %s (%d): %v", group.item.Name, group.item.ID, profileErrors[index]))
			continue
		}
		if result.Persons[index].Metadata == nil {
			result.Persons[index].Metadata = map[string]any{}
		}
		result.Persons[index].Metadata["tmdb_profile_images_loaded"] = true
		for profileIndex, profile := range profiles[index] {
			if strings.TrimSpace(profile.FilePath) == "" {
				continue
			}
			imageID := profileImageID(group.item.ID, profile.FilePath)
			result.ProfileImage = append(result.ProfileImage, model.PersonImage{ID: imageID, PersonID: group.personID, SourceIndex: profileIndex, Source: "tmdb", SourceURL: imageURLSize(profile.FilePath, profileImageSize), Width: profile.Width, Height: profile.Height, VoteAverage: profile.VoteAverage, Status: "pending"})
		}
	}
	return result, nil
}

func (c *TMDBClient) SyncMovie(ctx context.Context, title string, year *int, tmdbID int) (TMDBSyncResult, error) {
	if !c.Configured() {
		return TMDBSyncResult{}, fmt.Errorf("TMDB_API_KEY is not configured")
	}
	if tmdbID == 0 {
		var err error
		tmdbID, err = c.resolveMovieID(ctx, title, year)
		if err != nil {
			return TMDBSyncResult{}, err
		}
	}
	var detail tmdbMovie
	if err := c.get(ctx, "/movie/"+strconv.Itoa(tmdbID), url.Values{"append_to_response": []string{"credits"}, "language": []string{c.language}}, &detail); err != nil {
		return TMDBSyncResult{}, err
	}
	if strings.TrimSpace(detail.Title) == "" {
		return TMDBSyncResult{}, fmt.Errorf("TMDB movie %d returned no title", tmdbID)
	}
	movieYear := yearFromDate(detail.ReleaseDate)
	movie := model.Movie{ID: EntityID("movie", detail.ID, detail.IMDbID, detail.Title, movieYear), IMDbID: detail.IMDbID, TMDBID: detail.ID, Title: detail.Title, OriginalTitle: detail.OriginalTitle, Year: movieYear, Overview: detail.Overview, PosterURL: imageURL(detail.PosterPath), Metadata: map[string]any{"source": "tmdb"}}
	result := TMDBSyncResult{Movie: movie, Cast: make([]model.MovieCast, 0), Persons: make([]model.Person, 0), ProfileImage: make([]model.PersonImage, 0)}
	for order, item := range detail.Credits.Cast {
		if order >= 40 {
			break
		}
		personID := EntityID("person", item.ID, "", item.Name, nil)
		person := model.Person{ID: personID, TMDBID: item.ID, Name: item.Name, NormalizedName: strings.ToLower(strings.TrimSpace(item.Name)), Metadata: map[string]any{"source": "tmdb"}}
		result.Persons = append(result.Persons, person)
		result.Cast = append(result.Cast, model.MovieCast{MovieID: movie.ID, PersonID: personID, CharacterName: item.Character, BillingOrder: order})
		if item.ProfilePath != "" {
			imageID := profileImageID(item.ID, item.ProfilePath)
			result.ProfileImage = append(result.ProfileImage, model.PersonImage{ID: imageID, PersonID: personID, SourceIndex: 0, Source: "tmdb", SourceURL: imageURL(item.ProfilePath), Status: "pending"})
		}
	}
	return result, nil
}

func (c *TMDBClient) resolveMovieID(ctx context.Context, title string, year *int) (int, error) {
	if strings.TrimSpace(title) == "" {
		return 0, fmt.Errorf("movie title is required")
	}
	query := url.Values{"query": []string{title}, "language": []string{c.language}, "include_adult": []string{"false"}}
	if year != nil {
		query.Set("year", strconv.Itoa(*year))
	}
	var result struct {
		Results []tmdbMovie `json:"results"`
	}
	if err := c.get(ctx, "/search/movie", query, &result); err != nil {
		return 0, err
	}
	if len(result.Results) == 0 {
		return 0, fmt.Errorf("TMDB movie not found for %q", title)
	}
	return result.Results[0].ID, nil
}

func (c *TMDBClient) get(ctx context.Context, path string, query url.Values, target any) error {
	query.Set("api_key", c.apiKey)
	endpoint := c.baseURL + path + "?" + query.Encode()
	for attempt := 0; attempt < 4; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		response, err := c.httpClient.Do(request)
		if err != nil {
			return fmt.Errorf("TMDB request: %w", err)
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			decodeErr := json.NewDecoder(response.Body).Decode(target)
			_ = response.Body.Close()
			if decodeErr != nil {
				return fmt.Errorf("decode TMDB response: %w", decodeErr)
			}
			return nil
		}
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		status := response.StatusCode
		retryAfter := response.Header.Get("Retry-After")
		_ = response.Body.Close()
		if (status == http.StatusTooManyRequests || status >= 500) && attempt < 3 {
			delay := time.Second * time.Duration(1<<attempt)
			if seconds, parseErr := strconv.Atoi(strings.TrimSpace(retryAfter)); parseErr == nil && seconds > 0 {
				delay = time.Duration(seconds) * time.Second
			}
			select {
			case <-time.After(delay):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if len(body) > 0 {
			return fmt.Errorf("TMDB returned HTTP %d: %s", status, strings.TrimSpace(string(body)))
		}
		return fmt.Errorf("TMDB returned HTTP %d", status)
	}
	return fmt.Errorf("TMDB request retries exhausted")
}

type tmdbMovie struct {
	ID            int    `json:"id"`
	IMDbID        string `json:"imdb_id"`
	Title         string `json:"title"`
	OriginalTitle string `json:"original_title"`
	ReleaseDate   string `json:"release_date"`
	Overview      string `json:"overview"`
	PosterPath    string `json:"poster_path"`
	Credits       struct {
		Cast []tmdbCast `json:"cast"`
	} `json:"credits"`
}

type tmdbCast struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Character   string `json:"character"`
	ProfilePath string `json:"profile_path"`
}

type tmdbCastGroup struct {
	item     tmdbCast
	personID string
}

func imageURL(path string) string {
	return imageURLSize(path, "w500")
}

func imageURLSize(path, size string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	if strings.TrimSpace(size) == "" {
		size = "w500"
	}
	return "https://image.tmdb.org/t/p/" + size + path
}

func profileImageID(personID int, filePath string) string {
	return ProfileImageID(personID, filePath)
}

// ProfileImageID is stable across catalog-sync and lazy per-movie loading.
func ProfileImageID(personID int, filePath string) string {
	digest := sha256.Sum256([]byte(strconv.Itoa(personID) + ":" + strings.TrimSpace(filePath)))
	return "person-image:tmdb:" + strconv.Itoa(personID) + ":" + hex.EncodeToString(digest[:8])
}

// ProfileImageURL returns an image.tmdb.org URL for a profile path.
func ProfileImageURL(path, size string) string { return imageURLSize(path, size) }

// ProfileImageSize returns the TMDB image size used by both the offline
// catalog synchronizer and the lazy video-ingestion path. w500 is sufficient
// for face detection while avoiding the much larger original downloads.
func ProfileImageSize() string {
	if size := getenv("TMDB_PROFILE_IMAGE_SIZE"); size != "" {
		return size
	}
	return "w500"
}

func yearFromDate(value string) *int {
	if len(value) < 4 {
		return nil
	}
	year, err := strconv.Atoi(value[:4])
	if err != nil {
		return nil
	}
	return &year
}

func getenv(name string) string { return strings.TrimSpace(os.Getenv(name)) }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func getenvOr(name, fallback string) string {
	if value := getenv(name); value != "" {
		return value
	}
	return fallback
}

func normalizeName(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func cloneTMDBProfileImages(images []TMDBProfileImage) []TMDBProfileImage {
	return append([]TMDBProfileImage(nil), images...)
}

func cloneTMDBPerson(person model.Person) model.Person {
	person.PrimaryProfessions = append([]string(nil), person.PrimaryProfessions...)
	person.KnownForTitles = append([]string(nil), person.KnownForTitles...)
	person.Aliases = append([]string(nil), person.Aliases...)
	if person.Metadata != nil {
		person.Metadata = make(map[string]any, len(person.Metadata))
		for key, value := range person.Metadata {
			person.Metadata[key] = value
		}
	}
	return person
}

func tmdbProfileWorkerCount() int {
	workers, err := strconv.Atoi(getenv("TMDB_PROFILE_WORKERS"))
	if err != nil || workers <= 0 {
		return defaultTMDBProfileWorkers
	}
	if workers > maxTMDBProfileWorkers {
		return maxTMDBProfileWorkers
	}
	return workers
}
