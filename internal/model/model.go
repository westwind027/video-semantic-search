package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

type Media struct {
	MediaID       string         `json:"media_id,omitempty"`
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
	PreviewTime  float64 `json:"preview_time,omitempty"`
	Caption      string  `json:"caption,omitempty"`
	Subtitle     string  `json:"subtitle,omitempty"`
	QualityScore float32 `json:"quality_score,omitempty"`
}

type SearchRequest struct {
	Query    string   `json:"query"`
	Type     string   `json:"type,omitempty"`
	Year     *int     `json:"year,omitempty"`
	Language string   `json:"language,omitempty"`
	Limit    int      `json:"limit,omitempty"`
	MinScore *float32 `json:"min_score,omitempty"`
	Mode     string   `json:"mode,omitempty"`
}

type SceneResult struct {
	SceneID     string  `json:"scene_id"`
	Start       float64 `json:"start"`
	End         float64 `json:"end"`
	Score       float32 `json:"score"`
	Preview     string  `json:"preview,omitempty"`
	PreviewTime float64 `json:"preview_time,omitempty"`
	Caption     string  `json:"caption,omitempty"`
	Subtitle    string  `json:"subtitle,omitempty"`
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
