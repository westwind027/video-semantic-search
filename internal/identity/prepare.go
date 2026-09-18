package identity

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"video-semantic-search/internal/metadata"
	"video-semantic-search/internal/model"
)

const (
	TMDBStatusPending     = "pending"
	TMDBStatusComplete    = "complete"
	TMDBStatusPartial     = "partial"
	TMDBStatusUnavailable = "unavailable"
	TMDBStatusFailed      = "failed"

	FaceBankStatusPending     = "pending"
	FaceBankStatusComplete    = "complete"
	FaceBankStatusPartial     = "partial"
	FaceBankStatusUnavailable = "unavailable"
)

// MoviePreparationRequest is the small input shared by the web handler and
// acquisition worker. The preparation module resolves the IMDb movie,
// performs missing TMDB enrichment, deduplicates people, and fills the shared
// face bank before video processing starts.
type MoviePreparationRequest struct {
	MovieID  string
	Title    string
	FileName string
	Year     *int
}

type MoviePreparationResult struct {
	Resolution         metadata.Resolution `json:"resolution"`
	Found              bool                `json:"found"`
	Ready              bool                `json:"ready"`
	IdentityRequired   bool                `json:"identity_required"`
	Movie              model.Movie         `json:"movie,omitempty"`
	TMDBStatus         string              `json:"tmdb_status,omitempty"`
	FaceBankStatus     string              `json:"face_bank_status,omitempty"`
	CastCount          int                 `json:"cast_count"`
	ReadyPersonCount   int                 `json:"ready_person_count"`
	RequiredReferences int                 `json:"required_references"`
	ReferenceMax       int                 `json:"reference_max"`
	MissingPersonIDs   []string            `json:"missing_person_ids,omitempty"`
	MissingPersonNames []string            `json:"missing_person_names,omitempty"`
	Message            string              `json:"message,omitempty"`
}

// MoviePreparer is the seam used by the explicit metadata endpoint and the
// acquisition worker. Callers do not need to know whether a movie was already
// enriched; Prepare is idempotent and returns the durable readiness state.
type MoviePreparer interface {
	Prepare(context.Context, MoviePreparationRequest) (MoviePreparationResult, error)
}

type MoviePreparationService struct {
	store            Store
	tmdb             *metadata.TMDBClient
	references       *ReferenceIngestor
	minReferences    int
	maxReferences    int
	identityRequired bool
}

func NewMoviePreparationService(store Store, tmdb *metadata.TMDBClient, references *ReferenceIngestor, minReferences, maxReferences int, identityRequired bool) *MoviePreparationService {
	if minReferences <= 0 {
		minReferences = 5
	}
	if maxReferences <= 0 {
		maxReferences = 8
	}
	if maxReferences < minReferences {
		maxReferences = minReferences
	}
	return &MoviePreparationService{store: store, tmdb: tmdb, references: references, minReferences: minReferences, maxReferences: maxReferences, identityRequired: identityRequired}
}

func (s *MoviePreparationService) Prepare(ctx context.Context, request MoviePreparationRequest) (MoviePreparationResult, error) {
	if s == nil || s.store == nil {
		return MoviePreparationResult{}, fmt.Errorf("movie preparation service is not configured")
	}
	result := MoviePreparationResult{IdentityRequired: s.identityRequired, RequiredReferences: s.minReferences, ReferenceMax: s.maxReferences, MissingPersonIDs: []string{}, MissingPersonNames: []string{}}
	if ctx == nil {
		ctx = context.Background()
	}

	movie, found := s.resolveMovie(request)
	result.Resolution = resolveRequest(request)
	result.Found = found
	if !found {
		result.Message = "未找到 IMDb 影片元数据，请先导入 IMDb catalog"
		return result, nil
	}
	statusChanged := false
	if movie.TMDBStatus == "" && hasMetadataFlag(movie.Metadata, "tmdb_sync_complete") {
		movie.TMDBStatus = TMDBStatusComplete
		statusChanged = true
	}
	if movie.FaceBankStatus == "" && hasMetadataFlag(movie.Metadata, "face_bank_complete") {
		movie.FaceBankStatus = FaceBankStatusComplete
		statusChanged = true
	}
	// A lightweight /sync result is not sufficient for this workflow. The
	// import gate requires the complete TMDB cast/profile snapshot so the face
	// bank can be shared across movies and roles.
	needsFullTMDB := !hasMetadataFlag(movie.Metadata, "tmdb_full_cast")

	if movie.TMDBStatus != TMDBStatusComplete || needsFullTMDB {
		if err := s.syncTMDB(ctx, &movie); err != nil {
			movie.TMDBStatus = TMDBStatusFailed
			_ = s.store.UpsertMovie(movie)
			result.Movie = movie
			result.TMDBStatus = movie.TMDBStatus
			result.FaceBankStatus = movie.FaceBankStatus
			result.Message = "TMDB 元数据同步失败：" + err.Error()
			return result, nil
		}
	}

	result.Movie = movie
	result.TMDBStatus = movie.TMDBStatus
	result.CastCount = uniqueCastPeople(s.store.GetMovieCast(movie.ID))
	if !s.identityRequired {
		if statusChanged {
			if err := s.store.UpsertMovie(movie); err != nil {
				return result, err
			}
		}
		result.FaceBankStatus = "not_configured"
		result.Message = "IMDb/TMDB 元数据已准备；Identity 未启用，视频仍可按普通语义流程入库"
		result.Ready = true
		return result, nil
	}

	ready, missingIDs, missingNames, err := s.ensureFaceBank(ctx, movie)
	if err != nil {
		return result, err
	}
	if updated, ok := s.store.GetMovie(movie.ID); ok {
		result.Movie = updated
		movie = updated
	}
	result.ReadyPersonCount = ready
	result.MissingPersonIDs = missingIDs
	result.MissingPersonNames = missingNames
	result.FaceBankStatus = movie.FaceBankStatus
	// A movie can be processed with a partial face bank. Actors without a
	// complete reference set are omitted from the actor picker and simply have
	// no usable reference during scene matching; they must not block semantic
	// video acquisition.
	tmdbReady := movie.TMDBStatus == TMDBStatusComplete || movie.TMDBStatus == TMDBStatusPartial
	faceBankReady := movie.FaceBankStatus == FaceBankStatusComplete || movie.FaceBankStatus == FaceBankStatusPartial
	result.Ready = tmdbReady && faceBankReady
	if result.Ready {
		if len(missingNames) > 0 {
			result.Message = fmt.Sprintf("影片元数据已准备；%d 位演员 reference 不足 %d 张，已跳过这些演员", len(missingNames), s.minReferences)
		} else {
			result.Message = "IMDb、TMDB 和演员人脸向量库均已准备完成"
		}
	} else if len(missingNames) > 0 {
		result.Message = fmt.Sprintf("仍有 %d 位演员的人脸 reference 不足 %d 张", len(missingNames), s.minReferences)
	}
	return result, nil
}

func (s *MoviePreparationService) resolveMovie(request MoviePreparationRequest) (model.Movie, bool) {
	if strings.TrimSpace(request.MovieID) != "" {
		if movie, ok := s.store.GetMovie(request.MovieID); ok {
			return movie, true
		}
	}
	media := model.Media{MovieID: request.MovieID, Title: request.Title, Year: request.Year}
	if strings.TrimSpace(request.FileName) != "" {
		if strings.TrimSpace(media.Title) == "" {
			media.Title = request.FileName
		} else {
			media.Metadata = map[string]any{"remote_name": request.FileName}
		}
	}
	if strings.TrimSpace(media.Title) == "" {
		return model.Movie{}, false
	}
	return ResolveMovieForMedia(s.store, media)
}

func resolveRequest(request MoviePreparationRequest) metadata.Resolution {
	title := strings.TrimSpace(request.Title)
	year := request.Year
	if title == "" {
		return metadata.ResolveFilename(request.FileName)
	}
	return metadata.Resolution{Title: title, Year: year}
}

func (s *MoviePreparationService) syncTMDB(ctx context.Context, movie *model.Movie) error {
	if s.tmdb == nil || !s.tmdb.Configured() {
		return fmt.Errorf("TMDB_API_KEY 未配置")
	}
	if strings.TrimSpace(movie.IMDbID) == "" {
		return fmt.Errorf("影片没有 IMDb ID")
	}
	result, err := s.tmdb.SyncMovieByIMDbIDForNames(ctx, movie.IMDbID, movie.Title, movie.Year, s.authoritativeCastNames(movie.ID))
	if err != nil {
		return err
	}
	if err := s.ApplyTMDBResult(movie, result); err != nil {
		return err
	}
	if len(result.Warnings) == 0 {
		movie.TMDBStatus = TMDBStatusComplete
	} else {
		movie.TMDBStatus = TMDBStatusPartial
	}
	movie.Metadata = mergePreparationMetadata(movie.Metadata, map[string]any{
		"tmdb_sync_complete": movie.TMDBStatus == TMDBStatusComplete,
		"tmdb_full_cast":     true,
		"tmdb_profile_count": len(result.ProfileImage),
	})
	return s.store.UpsertMovie(*movie)
}

func (s *MoviePreparationService) authoritativeCastNames(movieID string) []string {
	cast := s.store.GetMovieCast(movieID)
	result := make([]string, 0, len(cast))
	seen := make(map[string]struct{}, len(cast))
	for _, item := range cast {
		person, ok := s.store.GetPerson(item.PersonID)
		if !ok || strings.TrimSpace(person.IMDbID) == "" {
			continue
		}
		name := normalize(person.Name)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, person.Name)
	}
	return result
}

// ApplyTMDBResult merges one TMDB response into an existing IMDb movie. IMDb
// actor/actress cast remains authoritative: TMDB can attach an English TMDB
// ID and profile images to an existing IMDb person, but cannot create a
// Person or replace the IMDb movie-cast relationship. It is exported so the
// explicit metadata-sync endpoint and the worker preparation workflow share
// exactly the same de-duplication rules.
func (s *MoviePreparationService) ApplyTMDBResult(movie *model.Movie, result metadata.TMDBSyncResult) error {
	movie.Metadata = mergePreparationMetadata(movie.Metadata, result.Movie.Metadata)
	if result.Movie.TMDBID > 0 {
		movie.TMDBID = result.Movie.TMDBID
	}
	if result.Movie.Title != "" {
		movie.Title = result.Movie.Title
	}
	if result.Movie.OriginalTitle != "" {
		movie.OriginalTitle = result.Movie.OriginalTitle
	}
	if result.Movie.Year != nil {
		movie.Year = result.Movie.Year
	}
	if result.Movie.Overview != "" {
		movie.Overview = result.Movie.Overview
	}
	if result.Movie.PosterURL != "" {
		movie.PosterURL = result.Movie.PosterURL
	}

	localCast := s.store.GetMovieCast(movie.ID)
	localByName := make(map[string]string)
	for _, item := range localCast {
		if item.Source != "imdb_principals" {
			continue
		}
		person, ok := s.store.GetPerson(item.PersonID)
		if ok && strings.TrimSpace(person.IMDbID) != "" && normalize(person.Name) != "" {
			name := normalize(person.Name)
			if existingID, exists := localByName[name]; !exists {
				localByName[name] = person.ID
			} else if existingID != person.ID {
				// Two IMDb identities share a display name; never let a TMDB
				// name-only fallback arbitrarily attach to one of them.
				localByName[name] = ""
			}
		}
	}
	canonicalByTMDB := make(map[int]string, len(result.Persons))
	canonicalPersons := make([]model.Person, 0, len(result.Persons))
	for _, incoming := range result.Persons {
		if incoming.TMDBID > 0 {
			if _, exists := canonicalByTMDB[incoming.TMDBID]; exists {
				continue
			}
		}
		canonical, found := s.canonicalTMDBPerson(incoming, localByName)
		if !found {
			// IMDb title.principals is the only authoritative Person/cast source.
			// TMDB's expanded credits can contain extras that IMDb did not list;
			// they must not become searchable people or face-bank candidates.
			continue
		}
		canonicalPersons = append(canonicalPersons, canonical)
		if incoming.TMDBID > 0 {
			canonicalByTMDB[incoming.TMDBID] = canonical.ID
		}
	}
	if err := upsertPersons(s.store, canonicalPersons); err != nil {
		return err
	}

	canonicalImages := make([]model.PersonImage, 0, len(result.ProfileImage))
	seenImageIDs := make(map[string]struct{}, len(result.ProfileImage))
	for _, image := range result.ProfileImage {
		person := s.personForResult(result.Persons, image.PersonID)
		personID := canonicalByTMDB[person.TMDBID]
		if personID == "" {
			continue
		}
		if _, exists := seenImageIDs[image.ID]; exists {
			continue
		}
		seenImageIDs[image.ID] = struct{}{}
		image.PersonID = personID
		canonicalImages = append(canonicalImages, image)
	}
	if err := upsertPersonImages(s.store, canonicalImages); err != nil {
		return err
	}
	return nil
}

func upsertPersons(store Store, persons []model.Person) error {
	if len(persons) == 0 {
		return nil
	}
	if batch, ok := store.(PersonBatchStore); ok {
		return batch.UpsertPersons(persons)
	}
	for _, person := range persons {
		if err := store.UpsertPerson(person); err != nil {
			return err
		}
	}
	return nil
}

func upsertPersonImages(store Store, images []model.PersonImage) error {
	if len(images) == 0 {
		return nil
	}
	if batch, ok := store.(PersonImageBatchStore); ok {
		return batch.UpsertPersonImages(images)
	}
	for _, image := range images {
		if err := store.UpsertPersonImage(image); err != nil {
			return err
		}
	}
	return nil
}

func (s *MoviePreparationService) canonicalTMDBPerson(incoming model.Person, localByName map[string]string) (model.Person, bool) {
	var existing model.Person
	found := false
	if catalog, ok := s.store.(PersonCatalog); ok {
		if incoming.IMDbID != "" {
			if candidate, candidateFound := catalog.FindPersonByIMDbID(incoming.IMDbID); candidateFound && strings.TrimSpace(candidate.IMDbID) != "" {
				existing = candidate
				found = true
			}
		}
		if !found && incoming.TMDBID > 0 {
			if candidate, candidateFound := catalog.FindPersonByTMDBID(incoming.TMDBID); candidateFound && strings.TrimSpace(candidate.IMDbID) != "" {
				existing = candidate
				found = true
			}
		}
	}
	if !found {
		if existingID := localByName[normalize(incoming.Name)]; existingID != "" {
			if candidate, candidateFound := s.store.GetPerson(existingID); candidateFound && strings.TrimSpace(candidate.IMDbID) != "" {
				existing = candidate
				found = true
			}
		}
	}
	if !found {
		return model.Person{}, false
	}
	merged := mergePerson(existing, incoming)
	// IMDb owns the canonical identity and display name. TMDB only contributes
	// its foreign key and image provenance.
	merged.ID = existing.ID
	merged.IMDbID = existing.IMDbID
	merged.Name = existing.Name
	merged.NormalizedName = existing.NormalizedName
	return merged, true
}

func (s *MoviePreparationService) ensureFaceBank(ctx context.Context, movie model.Movie) (int, []string, []string, error) {
	cast := s.store.GetMovieCast(movie.ID)
	personIDs := make([]string, 0, len(cast))
	seen := make(map[string]struct{}, len(cast))
	for _, item := range cast {
		if strings.TrimSpace(item.PersonID) == "" {
			continue
		}
		if _, exists := seen[item.PersonID]; exists {
			continue
		}
		seen[item.PersonID] = struct{}{}
		personIDs = append(personIDs, item.PersonID)
	}
	sort.Strings(personIDs)
	if len(personIDs) == 0 {
		movie.FaceBankStatus = FaceBankStatusPartial
		_ = s.store.UpsertMovie(movie)
		return 0, nil, nil, nil
	}
	if s.references == nil {
		movie.FaceBankStatus = FaceBankStatusUnavailable
		_ = s.store.UpsertMovie(movie)
		return 0, personIDs, personNames(s.store, personIDs), nil
	}

	images := make([]model.PersonImage, 0)
	for _, personID := range personIDs {
		vectorIDs := make(map[string]struct{})
		vectors := s.store.ListFaceVectors(personID)
		for _, vector := range vectors {
			vectorIDs[vector.ID] = struct{}{}
			if vector.ImageID != "" {
				vectorIDs[vector.ImageID] = struct{}{}
			}
		}
		remaining := s.maxReferences - len(vectors)
		if remaining <= 0 {
			continue
		}
		selected := SelectDiverseReferenceImages(s.store.ListPersonImages(personID), vectorIDs, remaining)
		if len(selected) == 0 {
			continue
		}
		// Ingestion records failures per image and continues with the other
		// profiles. This is important when one TMDB portrait is corrupt while
		// portraits from another period of the actor's career are usable.
		images = append(images, selected...)
	}
	if len(images) > 0 {
		_ = s.references.Ingest(ctx, images)
	}

	ready := 0
	missingIDs := make([]string, 0)
	missingNames := make([]string, 0)
	for _, personID := range personIDs {
		if len(s.store.ListFaceVectors(personID)) >= s.minReferences {
			ready++
			continue
		}
		missingIDs = append(missingIDs, personID)
		if person, ok := s.store.GetPerson(personID); ok {
			missingNames = append(missingNames, person.Name)
		} else {
			missingNames = append(missingNames, personID)
		}
	}
	if ready == len(personIDs) {
		movie.FaceBankStatus = FaceBankStatusComplete
	} else {
		movie.FaceBankStatus = FaceBankStatusPartial
	}
	movie.Metadata = mergePreparationMetadata(movie.Metadata, map[string]any{
		"face_bank_complete":     movie.FaceBankStatus == FaceBankStatusComplete,
		"face_bank_ready_people": ready,
	})
	if err := s.store.UpsertMovie(movie); err != nil {
		return ready, missingIDs, missingNames, err
	}
	return ready, missingIDs, missingNames, nil
}

// SelectDiverseReferenceImages spreads the selected profiles across TMDB's
// stable image ordering instead of taking only the first popular portraits.
// TMDB does not publish an age label for a profile, so this is a deterministic
// age-diversity proxy; the stored SourceIndex keeps the choice stable across
// movies and retries.
func SelectDiverseReferenceImages(images []model.PersonImage, vectors map[string]struct{}, limit int) []model.PersonImage {
	if limit <= 0 {
		return nil
	}
	candidates := make([]model.PersonImage, 0, len(images))
	for _, image := range images {
		if strings.TrimSpace(image.SourceURL) == "" && strings.TrimSpace(image.LocalPath) == "" {
			continue
		}
		if image.Status == "rejected" {
			continue
		}
		if _, exists := vectors[image.ID]; exists {
			continue
		}
		candidates = append(candidates, image)
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].SourceIndex != candidates[right].SourceIndex {
			return candidates[left].SourceIndex < candidates[right].SourceIndex
		}
		return candidates[left].ID < candidates[right].ID
	})
	if len(candidates) <= limit {
		return candidates
	}
	result := make([]model.PersonImage, 0, limit)
	seen := make(map[string]struct{}, limit)
	for index := 0; index < limit; index++ {
		candidateIndex := index * (len(candidates) - 1) / maxInt(1, limit-1)
		if _, exists := seen[candidates[candidateIndex].ID]; exists {
			continue
		}
		seen[candidates[candidateIndex].ID] = struct{}{}
		result = append(result, candidates[candidateIndex])
	}
	return result
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func uniqueCastPeople(cast []model.MovieCast) int {
	seen := make(map[string]struct{}, len(cast))
	for _, item := range cast {
		if item.PersonID != "" {
			seen[item.PersonID] = struct{}{}
		}
	}
	return len(seen)
}

func personNames(store Store, ids []string) []string {
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		if person, ok := store.GetPerson(id); ok {
			result = append(result, person.Name)
		} else {
			result = append(result, id)
		}
	}
	return result
}

func (s *MoviePreparationService) personForResult(persons []model.Person, candidate string) model.Person {
	for _, person := range persons {
		if person.ID == candidate {
			return person
		}
	}
	return model.Person{}
}

func mergePreparationMetadata(existing, incoming map[string]any) map[string]any {
	return mergeMetadata(existing, incoming)
}
