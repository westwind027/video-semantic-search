package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"video-semantic-search/internal/embedding"
	"video-semantic-search/internal/model"
	"video-semantic-search/internal/search"
	"video-semantic-search/internal/store"
)

func main() {
	file := flag.String("file", "", "JSONL file containing media records")
	flag.Parse()
	if *file == "" {
		log.Fatal("-file is required")
	}
	indexPath := envOrDefault("VIDEO_SEARCH_INDEX", "data/index.json")
	endpoint := envOrDefault("EMBEDDING_ENDPOINT", "http://127.0.0.1:7001")
	indexStore, err := store.NewFileStore(indexPath)
	if err != nil {
		log.Fatal(err)
	}
	defer indexStore.Close()
	embedder := embedding.NewPythonClient(endpoint, 30*time.Second)
	engine := search.NewEngine(indexStore, embedder, os.Getenv("VIDEO_SEARCH_USE_IMAGE_EMBEDDING") == "true")
	engine.ImageProfile = imageProfileOrDefault()

	input, err := os.Open(*file)
	if err != nil {
		log.Fatal(err)
	}
	defer input.Close()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	count := 0
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var media model.Media
		if err := json.Unmarshal(scanner.Bytes(), &media); err != nil {
			log.Fatalf("line %d: invalid JSON: %v", lineNumber, err)
		}
		if err := media.Validate(); err != nil {
			log.Fatalf("line %d: invalid media: %v", lineNumber, err)
		}
		if media.MediaID == "" {
			media.MediaID, err = model.NewMediaID()
			if err != nil {
				log.Fatal(err)
			}
		}
		result, err := engine.Index(context.Background(), media)
		if err != nil {
			log.Fatalf("line %d: index media %s: %v", lineNumber, media.MediaID, err)
		}
		fmt.Printf("indexed %s: %d scenes (%s/%d)\n", media.MediaID, result.SceneCount, result.Model, result.Dimension)
		count++
	}
	if err := scanner.Err(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("indexed media records: %d\n", count)
}

func imageProfileOrDefault() embedding.ImageProfile {
	profile := embedding.ImageProfile(envOrDefault("VIDEO_IMAGE_PROFILE", string(embedding.ImageProfileOriginal)))
	if profile != embedding.ImageProfileOriginal && profile != embedding.ImageProfileCompressed {
		log.Printf("invalid VIDEO_IMAGE_PROFILE=%q, using %q", profile, embedding.ImageProfileOriginal)
		return embedding.ImageProfileOriginal
	}
	return profile
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
