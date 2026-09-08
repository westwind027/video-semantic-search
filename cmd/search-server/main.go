package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"video-semantic-search/internal/acquisition"
	"video-semantic-search/internal/alipan"
	"video-semantic-search/internal/api"
	"video-semantic-search/internal/embedding"
	"video-semantic-search/internal/identity"
	"video-semantic-search/internal/metadata"
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
	// Image embedding is the intended default: WeMM maps text queries and
	// image documents into one shared space, while text-only scene captions
	// carry far weaker semantics. Opt out explicitly with =false.
	engine := search.NewEngine(indexStore, embedder, os.Getenv("VIDEO_SEARCH_USE_IMAGE_EMBEDDING") != "false")
	engine.ImageProfile = imageProfileOrDefault()
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
	processor.MaxScenes = intOrDefault("VIDEO_MAX_SCENES", processor.MaxScenes)
	processor.FastMaxScenes = intOrDefault("VIDEO_FAST_MAX_SCENES", processor.FastMaxScenes)
	processor.SceneRefineWindows = boolOrDefault("VIDEO_SCENE_REFINE_WINDOWS", processor.SceneRefineWindows)
	// Both values are in bytes. The chunk size only applies to sequential
	// readers such as the FFmpeg range proxy; container metadata and keyframe
	// samples are always requested as exact intervals.
	processor.RemoteChunkSize = int64OrDefault("VIDEO_REMOTE_CHUNK_SIZE", processor.RemoteChunkSize)
	processor.RemoteCacheBytes = int64OrDefault("VIDEO_REMOTE_CACHE_BYTES", processor.RemoteCacheBytes)
	processor.RemoteMoovWorkers = intOrDefault("VIDEO_REMOTE_MP4_MOOV_WORKERS", processor.RemoteMoovWorkers)
	processor.RemoteMoovChunkSize = int64OrDefault("VIDEO_REMOTE_MP4_MOOV_CHUNK_SIZE", processor.RemoteMoovChunkSize)
	processor.RemoteIndexCacheDir = envOrDefault("VIDEO_REMOTE_INDEX_CACHE_DIR", processor.RemoteIndexCacheDir)
	connector := alipan.NewManager(alipan.ConfigFromEnv())
	connector.StartAutoRefresh(context.Background())
	processor.RemoteResolver = connector
	taskFile := envOrDefault("VIDEO_SEARCH_TASK_FILE", "data/acquisition_tasks.json")
	jobs := acquisition.NewManagerWithTaskFile(processor, engine, taskFile)
	identityStore, err := identity.NewStoreFromEnv()
	if err != nil {
		log.Fatalf("open identity store: %v", err)
	}
	defer identityStore.Close()
	keepReferenceImages := boolOrDefault("IDENTITY_KEEP_REFERENCE_IMAGES", false)
	if !keepReferenceImages {
		if cleaner, ok := identityStore.(interface{ ClearRemoteLocalPaths() (int, error) }); ok {
			if cleared, err := cleaner.ClearRemoteLocalPaths(); err != nil {
				log.Printf("clear stale remote identity image paths: %v", err)
			} else if cleared > 0 {
				log.Printf("cleared %d stale remote identity image paths", cleared)
			}
		}
	}
	identityClient := identity.NewPythonClient(envOrDefault("IDENTITY_ENDPOINT", "http://127.0.0.1:7003"), durationOrDefault("IDENTITY_TIMEOUT", 2*time.Minute))
	tmdbClient := metadata.NewTMDBClientFromEnv()
	var identityTagger identity.Tagger
	identityEnabled := boolOrDefault("IDENTITY_ENABLED", false)
	var references *identity.ReferenceIngestor
	if identityEnabled {
		tagger := identity.NewTagger(identityStore, identityClient, identity.ConfigFromEnv())
		referenceConfig := identity.DefaultReferenceConfig()
		referenceConfig.DownloadRoot = envOrDefault("IDENTITY_REFERENCE_ROOT", referenceConfig.DownloadRoot)
		referenceConfig.ProxyURL = envOrDefault("IDENTITY_REFERENCE_PROXY_URL", os.Getenv("TMDB_PROXY_URL"))
		referenceConfig.Workers = intOrDefault("IDENTITY_REFERENCE_WORKERS", referenceConfig.Workers)
		referenceConfig.KeepLocalFiles = keepReferenceImages
		if tmdbClient.Configured() {
			references = identity.NewReferenceIngestor(identityStore, identityClient, referenceConfig)
		}
		if boolOrDefault("IDENTITY_LAZY_LOAD", true) && references != nil {
			maxImagesPerPerson := intOrDefault("IDENTITY_REFERENCE_MAX_PER_PERSON", 8)
			loader := identity.NewLazyReferenceLoader(identityStore, tmdbClient, references, maxImagesPerPerson)
			tagger.SetReferenceLoader(loader)
			log.Printf("identity lazy reference loading enabled: max_images_per_person=%d keep_images=%t", maxImagesPerPerson, referenceConfig.KeepLocalFiles)
		}
		identityTagger = tagger
		jobs.SetIdentityTagger(identityTagger)
	}
	server := api.NewServerWithAcquisitionAndAliyun(engine, indexStore, embedder, jobs, frameRoot, connector)
	server.ConfigureIdentity(identityStore, identityTagger)
	moviePreparer := identity.NewMoviePreparationService(identityStore, tmdbClient, references, intOrDefault("IDENTITY_MIN_REFERENCES", 5), intOrDefault("IDENTITY_REFERENCE_MAX_PER_PERSON", 8), identityEnabled)
	jobs.SetMoviePreparer(moviePreparer)
	server.ConfigureMoviePreparer(moviePreparer)

	address := envOrDefault("VIDEO_SEARCH_ADDR", ":8000")
	log.Printf("video semantic search listening on %s, index=%s, embedding=%s", address, indexPath, endpoint)
	if err := http.ListenAndServe(address, server.Handler()); err != nil {
		log.Fatal(err)
	}
}

func imageProfileOrDefault() embedding.ImageProfile {
	profile := embedding.ImageProfile(envOrDefault("VIDEO_IMAGE_PROFILE", string(embedding.ImageProfileOriginal)))
	if profile != embedding.ImageProfileOriginal && profile != embedding.ImageProfileCompressed {
		log.Printf("invalid VIDEO_IMAGE_PROFILE=%q, using %q", profile, embedding.ImageProfileOriginal)
		return embedding.ImageProfileOriginal
	}
	return profile
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

func int64OrDefault(name string, fallback int64) int64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
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
