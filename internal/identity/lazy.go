package identity

import (
	"context"
	"fmt"
	"strings"

	"video-semantic-search/internal/metadata"
	"video-semantic-search/internal/model"
)

// ReferenceLoader is the seam between scene tagging and the optional remote
// metadata/image pipeline. Tagging remains useful when the loader is absent or
// when TMDB is temporarily unavailable.
type ReferenceLoader interface {
	EnsureMovieReferences(context.Context, model.Movie, []model.MovieCast) error
}

// LazyReferenceLoader resolves only the actors in the movie currently being
// processed. TMDB profile images are transient inputs: ReferenceIngestor keeps
// the face vector and image provenance, then removes the downloaded file.
type LazyReferenceLoader struct {
	store        Store
	tmdb         *metadata.TMDBClient
	references   *ReferenceIngestor
	maxPerPerson int
}

func NewLazyReferenceLoader(store Store, tmdb *metadata.TMDBClient, references *ReferenceIngestor, maxPerPerson int) *LazyReferenceLoader {
	if maxPerPerson <= 0 {
		maxPerPerson = 8
	}
	return &LazyReferenceLoader{store: store, tmdb: tmdb, references: references, maxPerPerson: maxPerPerson}
}

// EnsureMovieReferences is idempotent. Existing vectors are reused globally;
// only missing references for this movie are fetched and embedded.
func (l *LazyReferenceLoader) EnsureMovieReferences(ctx context.Context, movie model.Movie, cast []model.MovieCast) error {
	if l == nil || l.store == nil || l.tmdb == nil || !l.tmdb.Configured() || l.references == nil || len(cast) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	batch, batching := l.store.(*FileStore)
	if batching {
		batch.BeginBatch()
		defer func() { _ = batch.CommitBatch() }()
	}
	if strings.TrimSpace(movie.IMDbID) != "" && movie.TMDBID == 0 && !hasMetadataFlag(movie.Metadata, "tmdb_metadata_loaded") {
		if enriched, err := l.tmdb.GetMovieMetadataByIMDbID(ctx, movie.IMDbID); err == nil {
			movie.TMDBID = enriched.TMDBID
			movie.Overview = enriched.Overview
			movie.PosterURL = enriched.PosterURL
			movie.Metadata = mergePersonMetadata(movie.Metadata, enriched.Metadata)
			_ = l.store.UpsertMovie(movie)
		}
	}

	images := make([]model.PersonImage, 0)
	imageSize := metadata.ProfileImageSize()
	seenPeople := make(map[string]struct{}, len(cast))
	for _, castMember := range cast {
		personID := strings.TrimSpace(castMember.PersonID)
		if personID == "" {
			continue
		}
		if _, seen := seenPeople[personID]; seen {
			continue
		}
		seenPeople[personID] = struct{}{}

		person, ok := l.store.GetPerson(personID)
		if !ok || strings.TrimSpace(person.IMDbID) == "" {
			continue
		}
		vectors := l.store.ListFaceVectors(personID)
		if len(vectors) >= l.maxPerPerson {
			continue
		}
		remaining := l.maxPerPerson - len(vectors)
		vectorIDSet := vectorIDs(vectors)
		storedImages := l.store.ListPersonImages(personID)
		storedCandidates := SelectDiverseReferenceImages(storedImages, vectorIDSet, remaining)
		if len(storedCandidates) >= remaining || hasMetadataFlag(person.Metadata, "tmdb_profile_images_loaded") {
			images = append(images, storedCandidates...)
			continue
		}

		tmdbPerson := model.Person{}
		if person.TMDBID > 0 {
			tmdbPerson = model.Person{ID: person.ID, TMDBID: person.TMDBID, Name: person.Name}
		} else {
			resolved, err := l.tmdb.FindPersonByIMDbID(ctx, person.IMDbID)
			if err != nil {
				continue
			}
			tmdbPerson = resolved
			person.TMDBID = resolved.TMDBID
			person.Metadata = mergePersonMetadata(person.Metadata, map[string]any{"source_tmdb": true})
			if err := l.store.UpsertPerson(person); err != nil {
				return err
			}
		}

		profiles, err := l.tmdb.GetPersonImages(ctx, tmdbPerson.TMDBID)
		if err != nil {
			continue
		}
		existingIDs := make(map[string]struct{}, len(storedImages))
		for _, item := range storedImages {
			existingIDs[item.ID] = struct{}{}
		}
		profileImages := make([]model.PersonImage, 0, len(profiles))
		for profileIndex, profile := range profiles {
			if strings.TrimSpace(profile.FilePath) == "" {
				continue
			}
			imageID := metadata.ProfileImageID(tmdbPerson.TMDBID, profile.FilePath)
			if _, exists := vectorIDSet[imageID]; exists {
				continue
			}
			if _, exists := existingIDs[imageID]; exists {
				continue
			}
			profileImages = append(profileImages, model.PersonImage{ID: imageID, PersonID: personID, SourceIndex: profileIndex, Source: "tmdb", SourceURL: metadata.ProfileImageURL(profile.FilePath, imageSize), Width: profile.Width, Height: profile.Height, VoteAverage: profile.VoteAverage, Status: "pending"})
		}
		if err := upsertPersonImages(l.store, profileImages); err != nil {
			return err
		}
		person.Metadata = mergePersonMetadata(person.Metadata, map[string]any{"tmdb_profile_images_loaded": true})
		if err := l.store.UpsertPerson(person); err != nil {
			return err
		}
		allImages := append(append([]model.PersonImage(nil), storedImages...), profileImages...)
		for _, image := range SelectDiverseReferenceImages(allImages, vectorIDSet, remaining) {
			images = append(images, image)
		}
	}

	results := l.references.Ingest(ctx, images)
	for _, result := range results {
		if result.Error != nil && result.Image.Status == "failed" {
			return fmt.Errorf("reference %s: %w", result.Image.ID, result.Error)
		}
	}
	return nil
}

func vectorIDs(vectors []model.FaceVector) map[string]struct{} {
	result := make(map[string]struct{}, len(vectors))
	for _, vector := range vectors {
		result[vector.ID] = struct{}{}
		if vector.ImageID != "" {
			result[vector.ImageID] = struct{}{}
		}
	}
	return result
}

func mergePersonMetadata(existing, incoming map[string]any) map[string]any {
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

func hasMetadataFlag(values map[string]any, key string) bool {
	value, ok := values[key]
	flag, ok := value.(bool)
	return ok && flag
}
