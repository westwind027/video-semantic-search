package identity

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"video-semantic-search/internal/model"
	"video-semantic-search/internal/store"
)

type Config struct {
	MatchThreshold  float32
	MarginThreshold float32
	MinDetScore     float32
	ReferenceMax    int
	SceneBatchSize  int
}

func ConfigFromEnv() Config {
	config := Config{MatchThreshold: 0.45, MarginThreshold: 0.05, MinDetScore: 0.60, ReferenceMax: 10, SceneBatchSize: 8}
	config.MatchThreshold = envFloat("FACE_MATCH_THRESHOLD", config.MatchThreshold)
	config.MarginThreshold = envFloat("FACE_MARGIN_THRESHOLD", config.MarginThreshold)
	config.MinDetScore = envFloat("FACE_MIN_DET_SCORE", config.MinDetScore)
	config.ReferenceMax = envInt("FACE_REFERENCE_MAX_PER_PERSON", config.ReferenceMax)
	config.SceneBatchSize = envInt("IDENTITY_SCENE_BATCH_SIZE", config.SceneBatchSize)
	if config.ReferenceMax < 1 {
		config.ReferenceMax = 10
	}
	if config.SceneBatchSize < 1 {
		config.SceneBatchSize = 8
	}
	return config
}

type Tagger interface {
	TagMedia(context.Context, model.Media) (model.Media, error)
	RebuildMedia(context.Context, model.Media) (model.Media, error)
}

// ProgressFunc reports completed scene annotations during an identity-only
// rebuild. It is intentionally an optional interface so existing tagger
// implementations remain compatible with the base Tagger contract.
type ProgressFunc func(done, total int)

// ProgressTagger is implemented by taggers that can expose scene-level
// progress for long-running rebuilds.
type ProgressTagger interface {
	RebuildMediaWithProgress(context.Context, model.Media, ProgressFunc) (model.Media, error)
}

type TaggerService struct {
	store           Store
	client          Client
	config          Config
	referenceLoader ReferenceLoader
}

func NewTagger(identityStore Store, client Client, config Config) *TaggerService {
	return &TaggerService{store: identityStore, client: client, config: config}
}

// Client exposes the face inference seam to the reference-image ingestion
// handler without exposing TaggerService's internal storage fields.
func (t *TaggerService) Client() Client {
	if t == nil {
		return nil
	}
	return t.client
}

// SetReferenceLoader attaches the optional lazy metadata/reference module.
// Keeping it out of NewTagger preserves the small constructor used by the
// existing API and tests.
func (t *TaggerService) SetReferenceLoader(loader ReferenceLoader) {
	if t != nil {
		t.referenceLoader = loader
	}
}

// TagMedia runs face recognition on each existing scene preview frame. It is
// deliberately independent from WeMM: a face tag must never alter a visual
// embedding or its score.
func (t *TaggerService) TagMedia(ctx context.Context, media model.Media) (model.Media, error) {
	return t.tagMedia(ctx, media, nil)
}

// RebuildMediaWithProgress is the observable variant used by the task
// manager. The normal Tagger interface remains unchanged for callers that do
// not need progress updates.
func (t *TaggerService) RebuildMediaWithProgress(ctx context.Context, media model.Media, progress ProgressFunc) (model.Media, error) {
	return t.tagMedia(ctx, media, progress)
}

func (t *TaggerService) tagMedia(ctx context.Context, media model.Media, progress ProgressFunc) (model.Media, error) {
	if t == nil || t.store == nil || t.client == nil {
		return media, nil
	}
	movie, cast, ok := t.resolveMovieCast(media)
	if !ok || len(cast) == 0 {
		return media, nil
	}
	if t.referenceLoader != nil {
		// Remote metadata is best effort. A TMDB outage must not make an
		// otherwise valid video acquisition fail; existing vectors are still
		// used below.
		_ = t.referenceLoader.EnsureMovieReferences(ctx, movie, cast)
	}
	refs := make(map[string][]model.FaceVector, len(cast))
	for _, item := range cast {
		if _, exists := refs[item.PersonID]; exists {
			continue
		}
		vectors := t.store.ListFaceVectors(item.PersonID)
		if t.config.ReferenceMax > 0 && len(vectors) > t.config.ReferenceMax {
			vectors = vectors[:t.config.ReferenceMax]
		}
		if len(vectors) > 0 {
			refs[item.PersonID] = vectors
		}
	}
	if len(refs) == 0 {
		return media, nil
	}
	result := media
	result.MovieID = movie.ID
	result.Metadata = copyMetadata(media.Metadata)
	if result.Metadata == nil {
		result.Metadata = map[string]any{}
	}
	result.Metadata["movie_id"] = movie.ID
	result.Metadata["identity_model"] = "insightface_buffalo_l"
	work := make([]sceneFaceWork, 0, len(result.Scenes))
	for index := range result.Scenes {
		if result.Scenes[index].SceneID == "" {
			result.Scenes[index].SceneID = store.StableSceneID(result.MediaID, result.Scenes[index])
		}
		path, available, err := scenePreviewPath(result.Scenes[index])
		if err != nil {
			return model.Media{}, fmt.Errorf("tag scene %d: %w", index, err)
		}
		if available {
			work = append(work, sceneFaceWork{index: index, path: path})
		}
	}
	peopleByScene := make([][]model.ScenePerson, len(result.Scenes))
	if batchClient, ok := t.client.(BatchClient); ok && len(work) > 0 {
		batchSize := t.config.SceneBatchSize
		if batchSize < 1 {
			batchSize = 8
		}
		for start := 0; start < len(work); start += batchSize {
			end := start + batchSize
			if end > len(work) {
				end = len(work)
			}
			paths := make([]string, end-start)
			for offset := range paths {
				paths[offset] = work[start+offset].path
			}
			embedded, err := batchClient.EmbedBatch(ctx, paths)
			if err != nil {
				return model.Media{}, fmt.Errorf("tag scenes %d-%d: %w", work[start].index, work[end-1].index, err)
			}
			if len(embedded) != len(paths) {
				return model.Media{}, fmt.Errorf("tag scenes %d-%d: embedding count %d does not match scene count %d", work[start].index, work[end-1].index, len(embedded), len(paths))
			}
			for offset, faceResult := range embedded {
				index := work[start+offset].index
				peopleByScene[index] = t.matchFaces(result.Scenes[index].SceneID, faceResult, refs)
			}
		}
	} else {
		for _, item := range work {
			faceResult, err := t.client.Embed(ctx, item.path)
			if err != nil {
				return model.Media{}, fmt.Errorf("tag scene %d: %w", item.index, err)
			}
			peopleByScene[item.index] = t.matchFaces(result.Scenes[item.index].SceneID, faceResult, refs)
		}
	}
	updates := make([]ScenePeopleUpdate, len(result.Scenes))
	for index := range result.Scenes {
		people := peopleByScene[index]
		result.Scenes[index].PersonIDs = personIDs(people)
		updates[index] = ScenePeopleUpdate{SceneID: result.Scenes[index].SceneID, People: people}
		if progress != nil {
			progress(index+1, len(result.Scenes))
		}
	}
	if batchStore, ok := t.store.(ScenePeopleBatchStore); ok {
		if err := batchStore.ReplaceScenePeopleBatch(result.MediaID, updates); err != nil {
			return model.Media{}, fmt.Errorf("persist scene people: %w", err)
		}
	} else {
		for index, update := range updates {
			if err := t.store.ReplaceScenePeople(result.MediaID, update.SceneID, update.People); err != nil {
				return model.Media{}, fmt.Errorf("persist scene %d people: %w", index, err)
			}
		}
	}
	return result, nil
}

// RebuildMedia is the vector-free management path. It updates the scene
// person list in the main index and the auditable match records, but never
// invokes the video processor or WeMM.
func (t *TaggerService) RebuildMedia(ctx context.Context, media model.Media) (model.Media, error) {
	return t.TagMedia(ctx, media)
}

func (t *TaggerService) resolveMovieCast(media model.Media) (model.Movie, []model.MovieCast, bool) {
	if movie, ok := ResolveMovieForMedia(t.store, media); ok {
		return movie, t.store.GetMovieCast(movie.ID), true
	}
	return model.Movie{}, nil, false
}

func (t *TaggerService) tagScene(ctx context.Context, scene model.Scene, refs map[string][]model.FaceVector) ([]model.ScenePerson, error) {
	path, available, err := scenePreviewPath(scene)
	if err != nil || !available {
		return nil, err
	}
	embedded, err := t.client.Embed(ctx, path)
	if err != nil {
		return nil, err
	}
	return t.matchFaces(scene.SceneID, embedded, refs), nil
}

type sceneFaceWork struct {
	index int
	path  string
}

func scenePreviewPath(scene model.Scene) (string, bool, error) {
	path := scene.PreviewPath
	if path == "" && !strings.HasPrefix(scene.Preview, "/v1/") {
		path = scene.Preview
	}
	if strings.TrimSpace(path) == "" {
		return "", false, nil
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "/v1/") {
		return "", false, nil
	}
	if _, err := os.Stat(path); err != nil {
		return "", false, fmt.Errorf("preview %q: %w", path, err)
	}
	return path, true, nil
}

func (t *TaggerService) matchFaces(sceneID string, embedded EmbedResult, refs map[string][]model.FaceVector) []model.ScenePerson {
	byPerson := map[string]model.ScenePerson{}
	for _, face := range embedded.Faces {
		if face.DetScore < t.config.MinDetScore || len(face.Embedding) == 0 {
			continue
		}
		bestPerson, bestScore, secondScore := "", float32(-1), float32(-1)
		for personID, vectors := range refs {
			score := aggregatePersonScore(face.Embedding, vectors)
			if score > bestScore {
				secondScore = bestScore
				bestPerson, bestScore = personID, score
			} else if score > secondScore {
				secondScore = score
			}
		}
		if bestPerson == "" || bestScore < t.config.MatchThreshold || bestScore-secondScore < t.config.MarginThreshold {
			continue
		}
		match := byPerson[bestPerson]
		match.SceneID = sceneID
		match.PersonID = bestPerson
		match.MatchCount++
		if bestScore > match.BestScore {
			match.BestScore = bestScore
		}
		match.Confidence = match.BestScore
		byPerson[bestPerson] = match
	}
	result := make([]model.ScenePerson, 0, len(byPerson))
	for _, match := range byPerson {
		result = append(result, match)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].BestScore > result[j].BestScore })
	return result
}

func aggregatePersonScore(query []float32, refs []model.FaceVector) float32 {
	scores := make([]float32, 0, len(refs))
	for _, ref := range refs {
		if score := cosine(query, ref.Vector); score > -1 {
			scores = append(scores, score)
		}
	}
	if len(scores) == 0 {
		return -1
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i] > scores[j] })
	if len(scores) > 3 {
		scores = scores[:3]
	}
	var total float32
	for _, score := range scores {
		total += score
	}
	return total / float32(len(scores))
}

func cosine(left, right []float32) float32 {
	if len(left) == 0 || len(left) != len(right) {
		return -1
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		dot += float64(left[index] * right[index])
		leftNorm += float64(left[index] * left[index])
		rightNorm += float64(right[index] * right[index])
	}
	if leftNorm == 0 || rightNorm == 0 {
		return -1
	}
	return float32(math.Max(-1, math.Min(1, dot/math.Sqrt(leftNorm*rightNorm))))
}

func personIDs(matches []model.ScenePerson) []string {
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		result = append(result, match.PersonID)
	}
	return result
}

func copyMetadata(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func envFloat(name string, fallback float32) float32 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 32)
	if err != nil || parsed < 0 || parsed > 1 {
		return fallback
	}
	return float32(parsed)
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
