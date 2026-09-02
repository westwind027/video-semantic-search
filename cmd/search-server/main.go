package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"video-semantic-search/internal/acquisition"
	"video-semantic-search/internal/alipan"
	"video-semantic-search/internal/api"
	"video-semantic-search/internal/embedding"
	"video-semantic-search/internal/search"
	"video-semantic-search/internal/store"
)

func main() {
	indexPath := envOrDefault("VIDEO_SEARCH_INDEX", "data/index.json")
	endpoint := envOrDefault("EMBEDDING_ENDPOINT", "http://127.0.0.1:7001")
	timeout := durationOrDefault("EMBEDDING_TIMEOUT", 5*time.Minute)

	indexStore, err := store.NewFileStore(indexPath)
	if err != nil {
		log.Fatalf("open index store: %v", err)
	}
	defer indexStore.Close()

	embedder := embedding.NewPythonClient(endpoint, timeout)
	engine := search.NewEngine(indexStore, embedder, os.Getenv("VIDEO_SEARCH_USE_IMAGE_EMBEDDING") == "true")
	engine.ImageBatchSize = intOrDefault("VIDEO_IMAGE_BATCH_SIZE", engine.ImageBatchSize)
	frameRoot := envOrDefault("VIDEO_SEARCH_FRAME_DIR", "data/frames")
	processor := acquisition.NewVideoProcessor(frameRoot)
	processor.SceneWorkers = intOrDefault("VIDEO_SCENE_WORKERS", processor.SceneWorkers)
	processor.SceneOverlap = floatOrDefault("VIDEO_SCENE_OVERLAP", processor.SceneOverlap)
	processor.SceneSampleFPS = floatOrDefault("VIDEO_SCENE_SAMPLE_FPS", processor.SceneSampleFPS)
	processor.SceneDetectionWidth = intOrDefault("VIDEO_SCENE_DETECTION_WIDTH", processor.SceneDetectionWidth)
	processor.FrameWorkers = intOrDefault("VIDEO_FRAME_WORKERS", processor.FrameWorkers)
	processor.RemoteFrameWorkers = intOrDefault("VIDEO_REMOTE_FRAME_WORKERS", processor.RemoteFrameWorkers)
	processor.FrameWidth = intOrDefault("VIDEO_FRAME_WIDTH", processor.FrameWidth)
	processor.FrameTimeout = durationOrDefault("VIDEO_FRAME_TIMEOUT", processor.FrameTimeout)
	processor.HWAccel = envOrDefault("VIDEO_FFMPEG_HWACCEL", processor.HWAccel)
	processor.FastMode = boolOrDefault("VIDEO_FAST_MODE", processor.FastMode)
	processor.FastMaxScenes = intOrDefault("VIDEO_FAST_MAX_SCENES", processor.FastMaxScenes)
	processor.SceneRefineWindows = boolOrDefault("VIDEO_SCENE_REFINE_WINDOWS", processor.SceneRefineWindows)
	connector := alipan.NewManager(alipan.ConfigFromEnv())
	processor.RemoteResolver = connector
	taskFile := envOrDefault("VIDEO_SEARCH_TASK_FILE", "data/acquisition_tasks.json")
	jobs := acquisition.NewManagerWithTaskFile(processor, engine, taskFile)
	server := api.NewServerWithAcquisitionAndAliyun(engine, indexStore, embedder, jobs, frameRoot, connector)

	address := envOrDefault("VIDEO_SEARCH_ADDR", ":8000")
	log.Printf("video semantic search listening on %s, index=%s, embedding=%s", address, indexPath, endpoint)
	if err := http.ListenAndServe(address, server.Handler()); err != nil {
		log.Fatal(err)
	}
}

func boolOrDefault(name string, fallback bool) bool {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		log.Printf("invalid %s=%q, using %t", name, value, fallback)
		return fallback
	}
	return parsed
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func durationOrDefault(name string, fallback time.Duration) time.Duration {
	if value := os.Getenv(name); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			return parsed
		}
		log.Printf("invalid %s=%q, using %s", name, value, fallback)
	}
	return fallback
}

func intOrDefault(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		log.Printf("invalid %s=%q, using %d", name, value, fallback)
		return fallback
	}
	return parsed
}

func floatOrDefault(name string, fallback float64) float64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed < 0 {
		log.Printf("invalid %s=%q, using %g", name, value, fallback)
		return fallback
	}
	return parsed
}
