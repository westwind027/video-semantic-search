package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

type Media struct {
	MediaID       string         `json:"media_id,omitempty"`
	MovieID       string         `json:"movie_id,omitempty"`
	Type          string         `json:"type"`
	Title         string         `json:"title"`
	OriginalTitle string         `json:"original_title,omitempty"`
	Year          *int           `json:"year,omitempty"`
	Duration      *float64       `json:"duration,omitempty"`
	Language      []string       `json:"language,omitempty"`
	Description   string         `json:"description,omitempty"`
	Tags          []string       `json:"tags,omitempty"`
	SourceURL     string         `json:"source_url,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	Scenes        []Scene        `json:"scenes"`
}

type Scene struct {
	SceneID     string  `json:"scene_id,omitempty"`
	AssetID     string  `json:"asset_id,omitempty"`
	Start       float64 `json:"start"`
	End         float64 `json:"end"`
	Preview     string  `json:"preview,omitempty"`
	PreviewPath string  `json:"preview_path,omitempty"`
	// PreviewTime is the exact timestamp of the extracted preview frame. For
	// container-indexed remote videos this is the keyframe actually decoded,
	// which can be earlier than the scene boundary the frame was sampled at.
	PreviewTime  float64  `json:"preview_time,omitempty"`
	Caption      string   `json:"caption,omitempty"`
	Subtitle     string   `json:"subtitle,omitempty"`
	QualityScore float32  `json:"quality_score,omitempty"`
	PersonIDs    []string `json:"person_ids,omitempty"`
}

// ScenePerson records a face match independently from the scene vector. The
// scene keeps the compact PersonIDs list for filtering, while this record is
// useful for audit/debug output and later threshold recalibration.
type ScenePerson struct {
	SceneID    string  `json:"scene_id"`
	PersonID   string  `json:"person_id"`
	Confidence float32 `json:"confidence"`
	MatchCount int     `json:"match_count"`
	BestScore  float32 `json:"best_score"`
}

type Movie struct {
	ID              string         `json:"id"`
	IMDbID          string         `json:"imdb_id,omitempty"`
	TMDBID          int            `json:"tmdb_id,omitempty"`
	TMDBStatus      string         `json:"tmdb_status,omitempty"`
	FaceBankStatus  string         `json:"face_bank_status,omitempty"`
	Title           string         `json:"title"`
	OriginalTitle   string         `json:"original_title,omitempty"`
	Year            *int           `json:"year,omitempty"`
	StartYear       *int           `json:"start_year,omitempty"`
	EndYear         *int           `json:"end_year,omitempty"`
	TitleType       string         `json:"title_type,omitempty"`
	IsAdult         bool           `json:"is_adult,omitempty"`
	RuntimeMinutes  int            `json:"runtime_minutes,omitempty"`
	Genres          []string       `json:"genres,omitempty"`
	AlternateTitles []string       `json:"alternate_titles,omitempty"`
	Directors       []string       `json:"directors,omitempty"`
	Writers         []string       `json:"writers,omitempty"`
	AverageRating   float32        `json:"average_rating,omitempty"`
	VoteCount       int64          `json:"vote_count,omitempty"`
	Overview        string         `json:"overview,omitempty"`
	PosterURL       string         `json:"poster_url,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

type Person struct {
	ID                 string         `json:"id"`
	IMDbID             string         `json:"imdb_id,omitempty"`
	TMDBID             int            `json:"tmdb_id,omitempty"`
	Name               string         `json:"name"`
	NormalizedName     string         `json:"normalized_name"`
	BirthYear          *int           `json:"birth_year,omitempty"`
	DeathYear          *int           `json:"death_year,omitempty"`
	PrimaryProfessions []string       `json:"primary_professions,omitempty"`
	KnownForTitles     []string       `json:"known_for_titles,omitempty"`
	Aliases            []string       `json:"aliases,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
}

type MovieCast struct {
	MovieID       string `json:"movie_id"`
	PersonID      string `json:"person_id"`
	IMDbNameID    string `json:"imdb_name_id,omitempty"`
	Category      string `json:"category,omitempty"`
	CharacterName string `json:"character_name,omitempty"`
	BillingOrder  int    `json:"billing_order,omitempty"`
	Source        string `json:"source,omitempty"`
}

type PersonImage struct {
	ID           string  `json:"id"`
	PersonID     string  `json:"person_id"`
	SourceIndex  int     `json:"source_index,omitempty"`
	Source       string  `json:"source,omitempty"`
	SourceURL    string  `json:"source_url,omitempty"`
	LocalPath    string  `json:"local_path,omitempty"`
	Width        int     `json:"width,omitempty"`
	Height       int     `json:"height,omitempty"`
	VoteAverage  float32 `json:"vote_average,omitempty"`
	QualityScore float32 `json:"quality_score,omitempty"`
	FaceCount    int     `json:"face_count,omitempty"`
	Status       string  `json:"status,omitempty"`
}

type FaceVector struct {
	ID       string    `json:"id"`
	PersonID string    `json:"person_id"`
	ImageID  string    `json:"image_id"`
	Quality  float32   `json:"quality,omitempty"`
	Model    string    `json:"model,omitempty"`
	Vector   []float32 `json:"vector"`
}

type SearchRequest struct {
	Query string `json:"query"`
	// MediaID narrows the search to one media record from the indexed library.
	MediaID  string   `json:"media_id,omitempty"`
	Type     string   `json:"type,omitempty"`
	Year     *int     `json:"year,omitempty"`
	Language string   `json:"language,omitempty"`
	Limit    int      `json:"limit,omitempty"`
	MinScore *float32 `json:"min_score,omitempty"`
	Mode     string   `json:"mode,omitempty"`
	PersonID string   `json:"person_id,omitempty"`
	Person   string   `json:"person,omitempty"`
	// PersonIDs is a multi-select actor filter. The index applies OR semantics:
	// a scene matches when it contains at least one selected actor.
	PersonIDs []string `json:"person_ids,omitempty"`
}

type SceneResult struct {
	SceneID     string   `json:"scene_id"`
	Start       float64  `json:"start"`
	End         float64  `json:"end"`
	Score       float32  `json:"score"`
	Preview     string   `json:"preview,omitempty"`
	PreviewTime float64  `json:"preview_time,omitempty"`
	Caption     string   `json:"caption,omitempty"`
	Subtitle    string   `json:"subtitle,omitempty"`
	PersonIDs   []string `json:"person_ids,omitempty"`
}

type SearchResult struct {
	MediaID       string        `json:"media_id"`
	Title         string        `json:"title"`
	Type          string        `json:"type"`
	Year          *int          `json:"year,omitempty"`
	SourceURL     string        `json:"source_url,omitempty"`
	Score         float32       `json:"score"`
	Scene         SceneResult   `json:"scene"`
	MatchedScenes []SceneResult `json:"matched_scenes,omitempty"`
	// True pixel geometry of the source file from the index metadata. The
	// browser cannot be trusted for the display ratio: broken SAR/DAR flags
	// (and Chromium's SAR-aware videoWidth) stretch such files to 16:9.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

type SearchResponse struct {
	Query   string         `json:"query"`
	Results []SearchResult `json:"results"`
}

type IngestResponse struct {
	MediaID            string `json:"media_id"`
	SceneCount         int    `json:"scene_count"`
	EmbeddingModel     string `json:"embedding_model"`
	EmbeddingDimension int    `json:"embedding_dimension"`
}

type MediaSummary struct {
	MediaID       string         `json:"media_id"`
	MovieID       string         `json:"movie_id,omitempty"`
	Type          string         `json:"type"`
	Title         string         `json:"title"`
	OriginalTitle string         `json:"original_title,omitempty"`
	Year          *int           `json:"year,omitempty"`
	Duration      *float64       `json:"duration,omitempty"`
	Language      []string       `json:"language,omitempty"`
	Description   string         `json:"description,omitempty"`
	Tags          []string       `json:"tags,omitempty"`
	SourceURL     string         `json:"source_url,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	SceneCount    int            `json:"scene_count"`
}

type HealthResponse struct {
	Status             string `json:"status"`
	MediaCount         int    `json:"media_count"`
	SceneCount         int    `json:"scene_count"`
	EmbeddingStatus    string `json:"embedding_status"`
	EmbeddingModel     string `json:"embedding_model,omitempty"`
	EmbeddingDimension int    `json:"embedding_dimension,omitempty"`
	IdentityStatus     string `json:"identity_status,omitempty"`
	IdentityModel      string `json:"identity_model,omitempty"`
	IdentityDimension  int    `json:"identity_dimension,omitempty"`
}

func (m *Media) Validate() error {
	if strings.TrimSpace(m.Type) == "" {
		m.Type = "movie"
	}
	if strings.TrimSpace(m.Title) == "" {
		return fmt.Errorf("title is required")
	}
	if len(m.Scenes) == 0 {
		return fmt.Errorf("at least one scene is required")
	}
	if m.Year != nil && (*m.Year < 1888 || *m.Year > 3000) {
		return fmt.Errorf("year must be between 1888 and 3000")
	}
	if m.Duration != nil && *m.Duration < 0 {
		return fmt.Errorf("duration cannot be negative")
	}
	for index := range m.Scenes {
		scene := &m.Scenes[index]
		if scene.Start < 0 || scene.End <= scene.Start {
			return fmt.Errorf("scene %d must have 0 <= start < end", index)
		}
		if scene.QualityScore == 0 {
			scene.QualityScore = 1
		}
		if scene.QualityScore < 0 || scene.QualityScore > 1 {
			return fmt.Errorf("scene %d quality_score must be between 0 and 1", index)
		}
	}
	return nil
}

func NewMediaID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}
