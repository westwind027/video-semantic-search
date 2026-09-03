package acquisition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"video-semantic-search/internal/source"
)

const mp4IndexCacheVersion = 1

// mp4IndexCacheRecord contains only the parsed MP4 index. Keeping the sample
// table instead of the moov bytes makes the cache small and means a cache hit
// still downloads only the individual keyframes needed by this acquisition.
type mp4IndexCacheRecord struct {
	Version   int              `json:"version"`
	FileSize  int64            `json:"file_size"`
	Keyframes []keyframeSample `json:"keyframes"`
	Duration  float64          `json:"duration"`
	Width     int              `json:"width"`
	Height    int              `json:"height"`
	Codec     string           `json:"codec"`
	Config    [][]byte         `json:"config,omitempty"`
	NALLength int              `json:"nal_length"`
	MoovBytes int64            `json:"moov_bytes"`
}

// remoteIndexCacheKey returns a stable identity for a remote object. The
// provider content hash is preferred; DriveID/FileID remains a safe fallback
// for connectors that do not expose a content hash. Size is included so a
// stale provider identity cannot rebind an index to a different object.
func remoteIndexCacheKey(request Request, remote source.Video) string {
	if !request.IsRemote() || remote.Size <= 0 {
		return ""
	}
	identity := strings.TrimSpace(remote.Fingerprint)
	if identity == "" {
		identity = strings.TrimSpace(request.SourceFingerprint)
	}
	if identity == "" {
		identity = fmt.Sprintf("alipan:%s:%s", strings.TrimSpace(request.DriveID), strings.TrimSpace(request.FileID))
	}
	if identity == "" || identity == "alipan::" {
		return ""
	}
	return fmt.Sprintf("%s:%d", identity, remote.Size)
}

func mp4IndexCachePath(directory, key string) string {
	digest := sha256.Sum256([]byte(key))
	return filepath.Join(directory, hex.EncodeToString(digest[:])+".json")
}

func saveMP4IndexCache(directory, key string, fileSize int64, index *mp4Index) error {
	if strings.TrimSpace(directory) == "" || strings.TrimSpace(key) == "" || fileSize <= 0 || index == nil {
		return nil
	}
	if len(index.Keyframes) == 0 || index.Duration <= 0 || index.Width <= 0 || index.Height <= 0 {
		return fmt.Errorf("MP4 index is incomplete and cannot be cached")
	}
	record := mp4IndexCacheRecord{
		Version:   mp4IndexCacheVersion,
		FileSize:  fileSize,
		Keyframes: append([]keyframeSample(nil), index.Keyframes...),
		Duration:  index.Duration,
		Width:     index.Width,
		Height:    index.Height,
		Codec:     index.Codec,
		Config:    cloneByteSlices(index.Config),
		NALLength: index.NALLength,
		MoovBytes: index.MoovBytes,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode MP4 index cache: %w", err)
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create MP4 index cache directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".mp4-index-*.tmp")
	if err != nil {
		return fmt.Errorf("create MP4 index cache temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write MP4 index cache: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync MP4 index cache: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close MP4 index cache: %w", err)
	}
	if err := os.Rename(temporaryName, mp4IndexCachePath(directory, key)); err != nil {
		return fmt.Errorf("replace MP4 index cache: %w", err)
	}
	return nil
}

func loadMP4IndexCache(directory, key string, fileSize int64, fetcher rangeFetcher) (*mp4Index, bool, error) {
	if strings.TrimSpace(directory) == "" || strings.TrimSpace(key) == "" || fileSize <= 0 || fetcher == nil {
		return nil, false, nil
	}
	data, err := os.ReadFile(mp4IndexCachePath(directory, key))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read MP4 index cache: %w", err)
	}
	var record mp4IndexCacheRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, false, fmt.Errorf("decode MP4 index cache: %w", err)
	}
	if record.Version != mp4IndexCacheVersion {
		return nil, false, nil
	}
	if record.FileSize != fileSize || fetcher.Size() != fileSize || record.Duration <= 0 || record.Width <= 0 || record.Height <= 0 || len(record.Keyframes) == 0 {
		return nil, false, nil
	}
	for _, sample := range record.Keyframes {
		if sample.Offset < 0 || sample.Size <= 0 || sample.Size > maxKeyframeBytes || sample.Offset > fileSize-sample.Size {
			return nil, false, nil
		}
	}
	return &mp4Index{
		Keyframes: append([]keyframeSample(nil), record.Keyframes...),
		Duration:  record.Duration,
		Width:     record.Width,
		Height:    record.Height,
		Codec:     record.Codec,
		Fetcher:   fetcher,
		Config:    cloneByteSlices(record.Config),
		NALLength: record.NALLength,
		MoovBytes: record.MoovBytes,
	}, true, nil
}

func cloneByteSlices(input [][]byte) [][]byte {
	if len(input) == 0 {
		return nil
	}
	result := make([][]byte, len(input))
	for index, item := range input {
		result[index] = append([]byte(nil), item...)
	}
	return result
}
