package acquisition

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"video-semantic-search/internal/httpclient"
	"video-semantic-search/internal/model"
	"video-semantic-search/internal/source"
)

type Request struct {
	LocalPath         string  `json:"local_path,omitempty"`
	Source            string  `json:"source,omitempty"`
	DriveID           string  `json:"drive_id,omitempty"`
	FileID            string  `json:"file_id,omitempty"`
	SourceName        string  `json:"source_name,omitempty"`
	SourceFingerprint string  `json:"source_fingerprint,omitempty"`
	Title             string  `json:"title,omitempty"`
	Type              string  `json:"type,omitempty"`
	SampleInterval    float64 `json:"sample_interval,omitempty"`
	SceneThreshold    float64 `json:"scene_threshold,omitempty"`
	FastMode          bool    `json:"fast_mode,omitempty"`
}

func (r Request) IsRemote() bool {
	return strings.EqualFold(strings.TrimSpace(r.Source), "alipan")
}

func (r Request) Validate() (string, error) {
	if r.IsRemote() {
		if strings.TrimSpace(r.DriveID) == "" {
			return "", fmt.Errorf("drive_id is required for remote source")
		}
		if strings.TrimSpace(r.FileID) == "" {
			return "", fmt.Errorf("file_id is required for remote source")
		}
		return "", nil
	}
	if strings.TrimSpace(r.Source) != "" && !strings.EqualFold(strings.TrimSpace(r.Source), "local") {
		return "", fmt.Errorf("unsupported source %q", r.Source)
	}
	if strings.TrimSpace(r.LocalPath) == "" {
		return "", fmt.Errorf("local_path is required")
	}
	path, err := filepath.Abs(NormalizeLocalPath(r.LocalPath))
	if err != nil {
		return "", fmt.Errorf("resolve local_path: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat local_path: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("local_path must be a regular file")
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("local_path is empty")
	}
	return path, nil
}

type Stage string

const (
	StageQueued     Stage = "queued"
	StageParsing    Stage = "parsing"
	StageDetecting  Stage = "detecting_scenes"
	StageExtracting Stage = "extracting_frames"
	StageEmbedding  Stage = "embedding"
	StageCompleted  Stage = "completed"
	StageFailed     Stage = "failed"
	StageCanceled   Stage = "canceled"
)

type Progress struct {
	Stage   Stage
	Percent float32
	Message string
}

type ProgressFunc func(Progress)

// Processor is the seam for future storyboard, HLS, TransNetV2, or remote
// source adapters. Callers only receive a normalized Media with local frames.
type Processor interface {
	Process(context.Context, string, Request, ProgressFunc) (model.Media, error)
}

type VideoProcessor struct {
	FFprobePath         string
	FFmpegPath          string
	FrameRoot           string
	MaxScenes           int
	SceneWorkers        int
	SceneOverlap        float64
	SceneSampleFPS      float64
	SceneDetectionWidth int
	FrameWorkers        int
	RemoteFrameWorkers  int
	FrameWidth          int
	FrameTimeout        time.Duration
	HWAccel             string
	FastMode            bool
	FastMaxScenes       int
	SceneRefineWindows  bool
	// RemoteChunkSize is the aligned read unit used by sequential readers such
	// as the FFmpeg range proxy. Container metadata and keyframe samples never
	// use this grid; they request exact byte intervals.
	RemoteChunkSize int64
	// RemoteCacheBytes bounds the shared range cache of one acquisition, so a
	// long movie cannot pin its media data in memory.
	RemoteCacheBytes int64
	// RemoteMoovWorkers and RemoteMoovChunkSize control the first-run MP4
	// metadata fetch. A large moov is split into exact ranges and downloaded
	// with bounded concurrency; these settings do not affect keyframe ranges.
	RemoteMoovWorkers   int
	RemoteMoovChunkSize int64
	// RemoteIndexCacheDir stores compact parsed MP4 indexes keyed by the remote
	// content identity. It never stores the original video or moov bytes.
	RemoteIndexCacheDir string
	RemoteResolver      source.Resolver

	hardwareMu   sync.RWMutex
	cudaDisabled bool
}

func NewVideoProcessor(frameRoot string) *VideoProcessor {
	return &VideoProcessor{
		FFprobePath:         "ffprobe",
		FFmpegPath:          "ffmpeg",
		FrameRoot:           frameRoot,
		MaxScenes:           240,
		SceneWorkers:        4,
		SceneOverlap:        1,
		SceneSampleFPS:      2,
		SceneDetectionWidth: 640,
		FrameWorkers:        4,
		RemoteFrameWorkers:  8,
		FrameWidth:          640,
		FrameTimeout:        45 * time.Second,
		HWAccel:             "auto",
		FastMaxScenes:       32,
		RemoteChunkSize:     source.DefaultChunkSize,
		RemoteCacheBytes:    source.DefaultMaxCacheBytes,
		RemoteMoovWorkers:   defaultMP4MoovWorkers,
		RemoteMoovChunkSize: defaultMP4MoovChunkSize,
		RemoteIndexCacheDir: filepath.Join(frameRoot, ".container-index-cache"),
	}
}

type probeResult struct {
	Format struct {
		Duration   string `json:"duration"`
		FormatName string `json:"format_name"`
	} `json:"format"`
	Streams []struct {
		CodecType   string `json:"codec_type"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		CodecName   string `json:"codec_name"`
		PixelFormat string `json:"pix_fmt"`
	} `json:"streams"`
}

type videoInfo struct {
	Duration    float64
	FormatName  string
	Width       int
	Height      int
	Codec       string
	PixelFormat string
}

var ptsTimePattern = regexp.MustCompile(`pts_time:([0-9]+(?:\.[0-9]+)?)`)

func (p *VideoProcessor) Process(ctx context.Context, mediaID string, request Request, report ProgressFunc) (model.Media, error) {
	path, err := request.Validate()
	if err != nil {
		return model.Media{}, err
	}
	var remote source.Video
	var remoteReader *source.RangeReader
	var remoteProxy *source.RangeProxy
	var remoteIndex containerIndex
	remoteIndexCacheState := "disabled"
	if request.IsRemote() {
		if p.RemoteResolver == nil {
			return model.Media{}, fmt.Errorf("remote source resolver is not configured")
		}
		remote, err = p.RemoteResolver.ResolveVideo(ctx, request.DriveID, request.FileID)
		if err != nil {
			return model.Media{}, fmt.Errorf("resolve remote video: %w", err)
		}
		path = remote.URL
		if strings.TrimSpace(path) == "" {
			return model.Media{}, fmt.Errorf("remote video returned an empty stream URL")
		}
		remoteReader = source.NewHTTPRangeReader(remote.URL, remote.Size, nil)
		if p.RemoteChunkSize > 0 {
			remoteReader.ChunkSize = p.RemoteChunkSize
		}
		if p.RemoteCacheBytes > 0 {
			remoteReader.SetMaxCacheBytes(p.RemoteCacheBytes)
		}
		remoteReader.SetURLRefresher(func(refreshContext context.Context) (string, error) {
			fresh, refreshErr := p.RemoteResolver.ResolveVideo(refreshContext, request.DriveID, request.FileID)
			if refreshErr != nil {
				return "", refreshErr
			}
			return fresh.URL, nil
		})
		remoteProxy, err = source.NewRangeProxy(remoteReader)
		if err != nil {
			return model.Media{}, fmt.Errorf("create remote range source: %w", err)
		}
		defer remoteProxy.Close()
		path = remoteProxy.URL()
	}
	if report == nil {
		report = func(Progress) {}
	}
	report(Progress{Stage: StageParsing, Percent: 0.05, Message: "正在解析视频元数据"})
	var info videoInfo
	if remoteReader != nil {
		// The container is identified from its first bytes rather than from the
		// file name: a cloud drive name is user data, so its extension is often
		// missing, wrong, or changed by a rename. Only container metadata is
		// downloaded here, never the media data it describes.
		report(Progress{Stage: StageParsing, Percent: 0.12, Message: "正在读取容器索引（仅元数据，不下载媒体数据）"})
		cacheKey := remoteIndexCacheKey(request, remote)
		if cacheKey != "" && p.RemoteIndexCacheDir != "" {
			cached, hit, cacheErr := loadMP4IndexCache(p.RemoteIndexCacheDir, cacheKey, remote.Size, remoteReader)
			if cacheErr == nil && hit {
				candidate := cached.info()
				if candidate.Duration > 0 && candidate.Width > 0 && candidate.Height > 0 {
					remoteIndex = cached
					info = candidate
					remoteIndexCacheState = "hit"
				}
			} else {
				remoteIndexCacheState = "miss"
			}
		}
		if remoteIndex == nil {
			indexed, indexErr := buildContainerIndexWithOptions(ctx, remoteReader, firstNonEmpty(remote.Name, request.SourceName), containerIndexOptions{
				MP4MoovChunkSize: p.RemoteMoovChunkSize,
				MP4MoovWorkers:   p.RemoteMoovWorkers,
			})
			if indexErr == nil {
				candidate := indexed.info()
				if candidate.Duration > 0 && candidate.Width > 0 && candidate.Height > 0 {
					remoteIndex = indexed
					info = candidate
					if index, isMP4 := indexed.(*mp4Index); isMP4 && cacheKey != "" && p.RemoteIndexCacheDir != "" {
						if cacheErr := saveMP4IndexCache(p.RemoteIndexCacheDir, cacheKey, remote.Size, index); cacheErr == nil {
							remoteIndexCacheState = "stored"
						}
					}
				} else {
					indexErr = fmt.Errorf("容器索引元数据不完整")
				}
			}
			if indexErr != nil {
				// The generic FFmpeg range proxy remains the fallback for
				// fragmented, unusual, or damaged containers.
				report(Progress{Stage: StageParsing, Percent: 0.14, Message: fmt.Sprintf("容器索引不可用（%v），切换为 FFmpeg 通用远程读取", indexErr)})
			}
		}
	}
	if remoteIndex == nil {
		info, err = p.probe(ctx, path)
		if err != nil {
			return model.Media{}, err
		}
	}

	threshold := request.SceneThreshold
	if threshold == 0 {
		threshold = 0.35
	}
	if threshold <= 0 || threshold >= 1 {
		return model.Media{}, fmt.Errorf("scene_threshold must be between 0 and 1")
	}
	fastMode := p.FastMode || request.FastMode
	if remoteIndex != nil && !fastMode {
		// A full scene scan would sequentially decode the whole remote movie.
		// Indexed containers use sparse keyframe-anchored samples instead.
		fastMode = true
	}
	var boundaries []float64
	if fastMode {
		sceneLimit := p.fastSceneLimit()
		samplingLabel := "快速采样"
		if remoteIndex != nil && !request.FastMode && !p.FastMode {
			// An indexed remote container can use its keyframe table instead of
			// the low-cost uniform fast sampler. Keep this path distinct so it
			// can collect more keyframes without changing the interactive fast
			// mode's 32-frame guard.
			sceneLimit = p.keyframeSceneLimit()
			samplingLabel = "关键帧采样"
		}
		sampleInterval := fastSampleInterval(request.SampleInterval)
		report(Progress{Stage: StageDetecting, Percent: 0.18, Message: fmt.Sprintf("%s模式：按 %.1f 秒间隔，最多 %d 个画面，跳过静态切镜扫描", samplingLabel, sampleInterval, sceneLimit)})
		boundaries = fastSceneBoundaries(info.Duration, request.SampleInterval, sceneLimit)
		report(Progress{Stage: StageDetecting, Percent: 0.25, Message: fmt.Sprintf("快速采样完成，共 %d 个镜头", len(boundaries)+1)})
	} else {
		report(Progress{Stage: StageDetecting, Percent: 0.18, Message: "正在用关键帧快速检测镜头切换"})
		boundaries, err = p.detectBoundaries(ctx, path, info.Duration, threshold, info.PixelFormat, func(completed, total, workers int) {
			report(Progress{
				Stage:   StageDetecting,
				Percent: 0.18 + 0.07*float32(completed)/float32(total),
				Message: fmt.Sprintf("已完成 %d/%d 个分段的镜头检测（并行 %d 路）", completed, total, workers),
			})
		})
		if err != nil {
			return model.Media{}, err
		}
		if len(boundaries) == 0 && request.SampleInterval > 0 && info.Duration > request.SampleInterval {
			for timestamp := request.SampleInterval; timestamp < info.Duration; timestamp += request.SampleInterval {
				boundaries = append(boundaries, timestamp)
			}
		}
	}
	if p.MaxScenes > 0 && len(boundaries)+1 > p.MaxScenes {
		boundaries = boundaries[:p.MaxScenes-1]
	}
	scenes := buildScenes(mediaID, info.Duration, boundaries)
	if len(scenes) == 0 {
		return model.Media{}, fmt.Errorf("video produced no scenes")
	}

	frameDir := filepath.Join(p.FrameRoot, mediaID)
	if err := os.MkdirAll(frameDir, 0o755); err != nil {
		return model.Media{}, fmt.Errorf("create frame directory: %w", err)
	}
	frameWidth := p.FrameWidth
	if fastMode && frameWidth <= 0 {
		frameWidth = 640
	}
	frameWorkerCount := p.FrameWorkers
	if remoteIndex != nil && p.RemoteFrameWorkers > frameWorkerCount {
		frameWorkerCount = p.RemoteFrameWorkers
	}
	report(Progress{Stage: StageExtracting, Percent: 0.25, Message: fmt.Sprintf("检测到 %d 个镜头，正在提取画面", len(scenes))})
	frameSummary, err := p.extractFramesWithFailures(ctx, path, mediaID, frameDir, scenes, info.PixelFormat, frameWidth, remoteIndex, report)
	if err != nil {
		if ctx.Err() != nil {
			_ = os.RemoveAll(frameDir)
		}
		return model.Media{}, err
	}

	title := strings.TrimSpace(request.Title)
	if title == "" {
		if request.SourceName != "" {
			title = strings.TrimSuffix(request.SourceName, filepath.Ext(request.SourceName))
		} else {
			title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}
	}
	typeName := strings.TrimSpace(request.Type)
	if typeName == "" {
		typeName = "movie"
	}
	duration := info.Duration
	frameSource := map[bool]string{true: "uniform_fast_sample", false: "ffmpeg_scene_detect"}[fastMode]
	if remoteIndex != nil {
		frameSource = "remote_" + info.FormatName + "_keyframe_sample"
	}
	metadata := map[string]any{
		"source":                   "local_file",
		"local_path":               path,
		"no_original_copy":         true,
		"frame_source":             frameSource,
		"format":                   info.FormatName,
		"video_codec":              info.Codec,
		"pixel_format":             info.PixelFormat,
		"width":                    info.Width,
		"height":                   info.Height,
		"scene_threshold":          threshold,
		"scene_count":              len(scenes),
		"scene_workers":            p.SceneWorkers,
		"scene_overlap":            p.SceneOverlap,
		"scene_sample_fps":         p.SceneSampleFPS,
		"scene_detection_width":    p.SceneDetectionWidth,
		"scene_refine_windows":     p.SceneRefineWindows,
		"frame_workers":            frameWorkerCount,
		"frame_width":              frameWidth,
		"frame_hwaccel":            p.HWAccel,
		"fast_mode":                fastMode,
		"fast_max_scenes":          p.fastSceneLimit(),
		"scene_max_scenes":         p.keyframeSceneLimit(),
		"frame_extracted_count":    len(scenes) - len(frameSummary.Failures),
		"frame_extraction_methods": frameSummary.Methods,
	}
	if len(frameSummary.Sources) > 0 {
		metadata["frame_extraction_sources"] = frameSummary.Sources
	}
	if len(frameSummary.Failures) > 0 {
		warnings := make([]map[string]any, 0, len(frameSummary.Failures))
		for _, failure := range frameSummary.Failures {
			warnings = append(warnings, map[string]any{
				"scene_index": failure.Index,
				"timestamp":   failure.Timestamp,
				"error":       failure.Error,
			})
		}
		metadata["frame_extraction_warnings"] = warnings
	}
	if remoteReader != nil {
		stats := remoteReader.Stats()
		ratio := 0.0
		if stats.Size > 0 {
			ratio = float64(stats.BytesDownloaded) / float64(stats.Size)
		}
		metadata["remote_range"] = map[string]any{
			"requests":         stats.Requests,
			"bytes_downloaded": stats.BytesDownloaded,
			"cached_blocks":    stats.CachedBlocks,
			"cached_bytes":     stats.CachedBytes,
			"size":             stats.Size,
			// Share of the remote file that was actually downloaded. A sparse
			// index keeps this far below one; it grows towards one only when the
			// FFmpeg fallback streams the movie.
			"download_ratio": ratio,
		}
		metadata["remote_access_mode"] = "http_range_cache"
		if remoteIndex != nil {
			metadata["remote_container_index"] = remoteIndex.summary()
			metadata["remote_container_index_cache"] = remoteIndexCacheState
		}
	}
	if request.IsRemote() {
		metadata["source"] = "aliyun_drive"
		metadata["remote_drive_id"] = request.DriveID
		metadata["remote_file_id"] = request.FileID
		metadata["remote_name"] = firstNonEmpty(request.SourceName, remote.Name)
		metadata["remote_size"] = remote.Size
		metadata["remote_fingerprint"] = firstNonEmpty(request.SourceFingerprint, remote.Fingerprint)
		metadata["remote_metadata"] = remote.Metadata
		delete(metadata, "local_path")
	}
	media := model.Media{
		MediaID:  mediaID,
		Type:     typeName,
		Title:    title,
		Duration: &duration,
		Metadata: metadata,
		Scenes:   scenes,
	}
	return media, nil
}

type frameExtractionFailure struct {
	Index     int
	Timestamp float64
	Error     string
}

type frameExtractionSummary struct {
	Failures []frameExtractionFailure
	Methods  map[string]int
	Sources  []frameExtractionSource
}

// frameExtractionSource records the exact container sample used for a preview.
// It is intentionally persisted as acquisition metadata: when a remote file
// produces repeated pictures, we can distinguish a bad sample index from a
// bad range response without downloading the whole file again.
type frameExtractionSource struct {
	Index           int     `json:"scene_index"`
	Timestamp       float64 `json:"timestamp"`
	Method          string  `json:"method"`
	SampleNumber    uint64  `json:"sample_number,omitempty"`
	SampleTimestamp float64 `json:"sample_timestamp,omitempty"`
	Offset          int64   `json:"offset,omitempty"`
	Size            int64   `json:"size,omitempty"`
	SampleDigest    string  `json:"sample_digest,omitempty"`
}

func (p *VideoProcessor) extractFrames(ctx context.Context, videoPath, mediaID, frameDir string, scenes []model.Scene, pixelFormat string, frameWidth int, accessIndex containerIndex, report ProgressFunc) error {
	_, err := p.extractFramesWithFailures(ctx, videoPath, mediaID, frameDir, scenes, pixelFormat, frameWidth, accessIndex, report)
	return err
}

func (p *VideoProcessor) extractFramesWithFailures(ctx context.Context, videoPath, mediaID, frameDir string, scenes []model.Scene, pixelFormat string, frameWidth int, accessIndex containerIndex, report ProgressFunc) (frameExtractionSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if report == nil {
		report = func(Progress) {}
	}
	if len(scenes) == 0 {
		return frameExtractionSummary{}, fmt.Errorf("video produced no scenes")
	}
	workerCount := p.FrameWorkers
	if accessIndex != nil && p.RemoteFrameWorkers > workerCount {
		workerCount = p.RemoteFrameWorkers
	}
	if workerCount <= 0 {
		workerCount = 1
	}
	if workerCount > len(scenes) {
		workerCount = len(scenes)
	}
	workContext := ctx
	type frameJob struct{ index int }
	jobs := make(chan frameJob)
	var workers sync.WaitGroup
	var resultMu sync.Mutex
	processed := 0
	succeeded := 0
	failures := make([]frameExtractionFailure, 0)
	methods := make(map[string]int)
	sources := make([]frameExtractionSource, 0)
	makeSummary := func() frameExtractionSummary {
		sort.Slice(sources, func(left, right int) bool { return sources[left].Index < sources[right].Index })
		return frameExtractionSummary{Failures: failures, Methods: methods, Sources: sources}
	}

	worker := func() {
		defer workers.Done()
		for job := range jobs {
			index := job.index
			timestamp := (scenes[index].Start + scenes[index].End) / 2
			filename := fmt.Sprintf("frame-%06d.jpg", index+1)
			framePath := filepath.Join(frameDir, filename)
			method, sourceInfo, err := p.extractFrameWithDetails(workContext, videoPath, timestamp, framePath, pixelFormat, frameWidth, accessIndex)
			if err != nil {
				resultMu.Lock()
				failures = append(failures, frameExtractionFailure{Index: index, Timestamp: timestamp, Error: err.Error()})
				processed++
				processedCount := processed
				failureCount := len(failures)
				resultMu.Unlock()
				report(Progress{
					Stage:   StageExtracting,
					Percent: 0.25 + 0.60*float32(processedCount)/float32(len(scenes)),
					Message: fmt.Sprintf("已处理 %d/%d 个画面，跳过 %d 个无法解码画面（并行 %d 路）", processedCount, len(scenes), failureCount, workerCount),
				})
				continue
			}
			scenes[index].PreviewPath = framePath
			scenes[index].Preview = fmt.Sprintf("/v1/media/%s/frames/%s", mediaID, filename)
			scenes[index].Caption = fmt.Sprintf("视频镜头，时间 %s", formatTimestamp(timestamp))
			resultMu.Lock()
			processed++
			succeeded++
			processedCount := processed
			succeededCount := succeeded
			failureCount := len(failures)
			methods[method]++
			if sourceInfo.SampleNumber != 0 {
				sourceInfo.Index = index
				sourceInfo.Timestamp = timestamp
				sources = append(sources, sourceInfo)
			}
			resultMu.Unlock()
			report(Progress{
				Stage:   StageExtracting,
				Percent: 0.25 + 0.60*float32(processedCount)/float32(len(scenes)),
				Message: fmt.Sprintf("已提取 %d/%d 个画面，跳过 %d 个（并行 %d 路）", succeededCount, len(scenes), failureCount, workerCount),
			})
		}
	}

	workers.Add(workerCount)
	for index := 0; index < workerCount; index++ {
		go worker()
	}
	for index := range scenes {
		select {
		case jobs <- frameJob{index: index}:
		case <-workContext.Done():
			break
		}
		if workContext.Err() != nil {
			break
		}
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return makeSummary(), fmt.Errorf("extract frames: %w", err)
	}
	if succeeded == 0 {
		if len(failures) > 0 {
			return makeSummary(), fmt.Errorf("extract frames: no usable frame (%s)", failures[0].Error)
		}
		return makeSummary(), fmt.Errorf("extract frames: no usable frame")
	}
	return makeSummary(), nil
}

func (p *VideoProcessor) probe(ctx context.Context, path string) (videoInfo, error) {
	command := exec.CommandContext(ctx, p.FFprobePath, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	command.Env = httpclient.WithoutProxyEnvironment()
	output, err := command.Output()
	if err != nil {
		return videoInfo{}, commandFailure("ffprobe", output, err)
	}
	var decoded probeResult
	if err := json.Unmarshal(output, &decoded); err != nil {
		return videoInfo{}, fmt.Errorf("decode ffprobe output: %w", err)
	}
	duration, err := strconv.ParseFloat(decoded.Format.Duration, 64)
	if err != nil || duration <= 0 {
		return videoInfo{}, fmt.Errorf("video duration is unavailable")
	}
	info := videoInfo{Duration: duration, FormatName: decoded.Format.FormatName}
	for _, stream := range decoded.Streams {
		if stream.CodecType == "video" {
			info.Width = stream.Width
			info.Height = stream.Height
			info.Codec = stream.CodecName
			break
		}
	}
	if info.Width <= 0 || info.Height <= 0 {
		return videoInfo{}, fmt.Errorf("video stream dimensions are unavailable")
	}
	for _, stream := range decoded.Streams {
		if stream.CodecType == "video" {
			info.PixelFormat = stream.PixelFormat
			break
		}
	}
	return info, nil
}

// sceneSegment is a half-open ownership interval. The FFmpeg decode interval
// is wider than this interval, but a boundary is retained only when it belongs
// to the ownership interval. This makes overlap useful for context without
// producing duplicate cuts at segment boundaries.
type sceneSegment struct {
	Index       int
	Start       float64
	End         float64
	DecodeStart float64
	DecodeEnd   float64
}

func (p *VideoProcessor) detectBoundaries(ctx context.Context, path string, duration, threshold float64, pixelFormat string, report func(completed, total, workers int)) ([]float64, error) {
	workerCount := p.SceneWorkers
	if workerCount <= 0 {
		workerCount = 1
	}
	if report == nil {
		report = func(int, int, int) {}
	}
	if duration <= 0 {
		return nil, fmt.Errorf("video duration is unavailable")
	}

	// A full-resolution scene filter still decodes every frame in a long movie.
	// First inspect only codec keyframes. They are sparse enough to make a good
	// set of candidate windows, while the second pass keeps the original scene
	// threshold and examines every frame only inside those windows.
	candidateThreshold := sceneCandidateThreshold(threshold)
	candidates, err := p.detectKeyframeCandidates(ctx, path, duration, candidateThreshold, pixelFormat)
	if err != nil {
		return nil, err
	}
	if !p.SceneRefineWindows {
		report(1, 1, workerCount)
		return mergeSceneBoundaries(candidates, duration), nil
	}
	windows := sceneRefinementWindows(candidates, duration)
	if len(windows) == 0 {
		report(1, 1, workerCount)
		return nil, nil
	}

	workContext, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		boundaries []float64
		err        error
	}
	jobs := make(chan sceneSegment)
	results := make(chan result, len(windows))
	if workerCount > len(windows) {
		workerCount = len(windows)
	}
	var workers sync.WaitGroup
	worker := func() {
		defer workers.Done()
		for window := range jobs {
			boundaries, windowErr := p.detectSceneSegment(workContext, path, threshold, pixelFormat, window)
			select {
			case results <- result{boundaries: boundaries, err: windowErr}:
			case <-workContext.Done():
				return
			}
			if windowErr != nil {
				cancel()
				return
			}
		}
	}

	workers.Add(workerCount)
	for index := 0; index < workerCount; index++ {
		go worker()
	}
	go func() {
		defer close(jobs)
		for _, window := range windows {
			select {
			case jobs <- window:
			case <-workContext.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	allBoundaries := make([]float64, 0)
	completed := 0
	var firstErr error
	total := len(windows) + 1
	report(1, total, workerCount)
	for item := range results {
		if item.err != nil {
			if firstErr == nil {
				firstErr = item.err
			}
			continue
		}
		allBoundaries = append(allBoundaries, item.boundaries...)
		completed++
		report(completed+1, total, workerCount)
	}
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("detect scene boundaries: %w", err)
	}
	return mergeSceneBoundaries(allBoundaries, duration), nil
}

func (p *VideoProcessor) detectKeyframeCandidates(ctx context.Context, path string, duration, threshold float64, pixelFormat string) ([]float64, error) {
	segment := sceneSegment{Index: 0, Start: 0, End: duration, DecodeStart: 0, DecodeEnd: duration}
	mode := strings.ToLower(strings.TrimSpace(p.HWAccel))
	if mode == "" {
		mode = "none"
	}
	if mode != "none" && mode != "auto" && mode != "cuda" {
		return nil, fmt.Errorf("unsupported ffmpeg hardware acceleration mode %q", mode)
	}
	if mode == "auto" || mode == "cuda" {
		if candidates, err := p.detectKeyframeCandidatesWithMode(ctx, path, threshold, pixelFormat, segment, true); err == nil {
			return candidates, nil
		} else if mode == "cuda" {
			return nil, err
		}
	}
	return p.detectKeyframeCandidatesWithMode(ctx, path, threshold, pixelFormat, segment, false)
}

func (p *VideoProcessor) detectKeyframeCandidatesWithMode(ctx context.Context, path string, threshold float64, pixelFormat string, segment sceneSegment, cuda bool) ([]float64, error) {
	filter := p.sceneDetectionFilter(threshold, pixelFormat, cuda, false)
	args := []string{"-hide_banner", "-loglevel", "info", "-skip_frame", "nokey"}
	if cuda {
		args = append(args, "-hwaccel", "cuda", "-hwaccel_output_format", "cuda")
	}
	args = append(args,
		"-threads", "1",
		"-ss", "0",
		"-i", path,
		"-t", strconv.FormatFloat(segment.DecodeEnd, 'f', 3, 64),
		"-vf", filter,
		"-an", "-f", "null", "-",
	)
	command := exec.CommandContext(ctx, p.FFmpegPath, args...)
	command.Env = httpclient.WithoutProxyEnvironment()
	var stderr bytes.Buffer
	command.Stdout = io.Discard
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		mode := "cpu"
		if cuda {
			mode = "cuda"
		}
		return nil, commandFailure(fmt.Sprintf("ffmpeg %s keyframe scan", mode), stderr.Bytes(), err)
	}
	values := ptsTimePattern.FindAllStringSubmatch(stderr.String(), -1)
	candidates := make([]float64, 0, len(values))
	for _, value := range values {
		timestamp, err := strconv.ParseFloat(value[1], 64)
		if err != nil || math.IsNaN(timestamp) || timestamp <= 0.25 || timestamp >= segment.DecodeEnd-0.05 {
			continue
		}
		candidates = append(candidates, timestamp)
	}
	return candidates, nil
}

func sceneCandidateThreshold(threshold float64) float64 {
	threshold *= 0.3
	if threshold < 0.08 {
		return 0.08
	}
	if threshold > 0.2 {
		return 0.2
	}
	return threshold
}

func sceneRefinementWindows(candidates []float64, duration float64) []sceneSegment {
	if len(candidates) == 0 || duration <= 0 {
		return nil
	}
	sort.Float64s(candidates)
	const lookback = 10.0
	const lookahead = 1.0
	windows := make([]sceneSegment, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate <= 0.25 || candidate >= duration-0.05 {
			continue
		}
		start := math.Max(0, candidate-lookback)
		end := math.Min(duration, candidate+lookahead)
		if len(windows) > 0 && start <= windows[len(windows)-1].End+0.25 {
			if end > windows[len(windows)-1].End {
				windows[len(windows)-1].End = end
				windows[len(windows)-1].DecodeEnd = end
			}
			continue
		}
		windows = append(windows, sceneSegment{Index: len(windows), Start: start, End: end, DecodeStart: start, DecodeEnd: end})
	}
	return windows
}

func (p *VideoProcessor) detectSceneSegment(ctx context.Context, path string, threshold float64, pixelFormat string, segment sceneSegment) ([]float64, error) {
	mode := strings.ToLower(strings.TrimSpace(p.HWAccel))
	if mode == "" {
		mode = "none"
	}
	if mode != "none" && mode != "auto" && mode != "cuda" {
		return nil, fmt.Errorf("unsupported ffmpeg hardware acceleration mode %q", mode)
	}
	if mode == "auto" || mode == "cuda" {
		if boundaries, err := p.detectSceneSegmentWithMode(ctx, path, threshold, pixelFormat, segment, true); err == nil {
			return boundaries, nil
		} else if mode == "cuda" {
			return nil, err
		}
	}
	return p.detectSceneSegmentWithMode(ctx, path, threshold, pixelFormat, segment, false)
}

func (p *VideoProcessor) detectSceneSegmentWithMode(ctx context.Context, path string, threshold float64, pixelFormat string, segment sceneSegment, cuda bool) ([]float64, error) {
	filter := p.sceneDetectionFilter(threshold, pixelFormat, cuda, true)
	segmentDuration := segment.DecodeEnd - segment.DecodeStart
	args := []string{"-hide_banner", "-loglevel", "info"}
	if cuda {
		args = append(args, "-hwaccel", "cuda", "-hwaccel_output_format", "cuda")
	}
	args = append(args,
		"-threads", "1",
		"-ss", strconv.FormatFloat(segment.DecodeStart, 'f', 3, 64),
		"-i", path,
		"-t", strconv.FormatFloat(segmentDuration, 'f', 3, 64),
		"-vf", filter,
		"-an", "-f", "null", "-",
	)
	command := exec.CommandContext(ctx, p.FFmpegPath, args...)
	command.Env = httpclient.WithoutProxyEnvironment()
	var stderr bytes.Buffer
	command.Stdout = io.Discard
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		mode := "cpu"
		if cuda {
			mode = "cuda"
		}
		return nil, commandFailure(fmt.Sprintf("ffmpeg %s scene detection segment %d", mode, segment.Index), stderr.Bytes(), err)
	}
	values := ptsTimePattern.FindAllStringSubmatch(stderr.String(), -1)
	boundaries := make([]float64, 0, len(values))
	for _, value := range values {
		timestamp, err := strconv.ParseFloat(value[1], 64)
		if err != nil || math.IsNaN(timestamp) {
			continue
		}
		// With -ss before -i, showinfo reports timestamps relative to the
		// decoded segment. Convert back before applying global ownership.
		timestamp += segment.DecodeStart
		if !sceneBoundaryBelongs(segment, timestamp) {
			continue
		}
		boundaries = append(boundaries, timestamp)
	}
	return boundaries, nil
}

func splitSceneSegments(duration float64, workers int, overlap float64) []sceneSegment {
	if duration <= 0 || workers <= 0 {
		return nil
	}
	segments := make([]sceneSegment, 0, workers)
	if overlap < 0 {
		overlap = 0
	}
	for index := 0; index < workers; index++ {
		start := duration * float64(index) / float64(workers)
		end := duration * float64(index+1) / float64(workers)
		if end <= start {
			continue
		}
		segments = append(segments, sceneSegment{Index: index, Start: start, End: end, DecodeStart: start, DecodeEnd: end})
	}
	for index := range segments {
		if index > 0 {
			segments[index].DecodeStart = math.Max(0, segments[index].Start-overlap)
		}
		if index+1 < len(segments) {
			segments[index].DecodeEnd = math.Min(duration, segments[index].End+overlap)
		}
	}
	return segments
}

func sceneBoundaryBelongs(segment sceneSegment, timestamp float64) bool {
	return timestamp >= segment.Start && timestamp < segment.End
}

func mergeSceneBoundaries(boundaries []float64, duration float64) []float64 {
	sort.Float64s(boundaries)
	deduplicated := make([]float64, 0, len(boundaries))
	for _, timestamp := range boundaries {
		if timestamp <= 0.25 || timestamp >= duration-0.05 || (len(deduplicated) > 0 && timestamp-deduplicated[len(deduplicated)-1] < 0.5) {
			continue
		}
		deduplicated = append(deduplicated, timestamp)
	}
	return deduplicated
}

func (p *VideoProcessor) extractFrame(ctx context.Context, videoPath string, timestamp float64, framePath, pixelFormat string, frameWidth int, accessIndex containerIndex) error {
	_, err := p.extractFrameWithMethod(ctx, videoPath, timestamp, framePath, pixelFormat, frameWidth, accessIndex)
	return err
}

func (p *VideoProcessor) extractFrameWithMethod(ctx context.Context, videoPath string, timestamp float64, framePath, pixelFormat string, frameWidth int, accessIndex containerIndex) (string, error) {
	method, _, err := p.extractFrameWithDetails(ctx, videoPath, timestamp, framePath, pixelFormat, frameWidth, accessIndex)
	return method, err
}

func (p *VideoProcessor) extractFrameWithDetails(ctx context.Context, videoPath string, timestamp float64, framePath, pixelFormat string, frameWidth int, accessIndex containerIndex) (string, frameExtractionSource, error) {
	var indexedErr error
	if accessIndex != nil {
		if handled, sourceInfo, err := p.extractFrameFromIndexedSampleWithSource(ctx, accessIndex, timestamp, framePath, pixelFormat, frameWidth); handled {
			if err == nil {
				sourceInfo.Method = "indexed_sample"
				return "indexed_sample", sourceInfo, nil
			}
			indexedErr = err
			if ctx.Err() != nil {
				return "", sourceInfo, ctx.Err()
			}
		}
	}

	// A keyframe is already a complete decodable picture. For indexed remote
	// containers, decode that picture directly instead of seeking to an I-frame
	// and decoding an arbitrary number of frames up to the scene midpoint.
	// This is both cheaper over HTTP ranges and avoids failures in a damaged
	// or unusual GOP after an otherwise valid sync sample.
	if accessIndex != nil {
		anchor := accessIndex.anchor(timestamp)
		if err := p.extractFrameAtWithSeek(ctx, videoPath, anchor, anchor, framePath, pixelFormat, frameWidth); err == nil {
			return "indexed_proxy", frameExtractionSource{}, nil
		} else if ctx.Err() != nil {
			return "", frameExtractionSource{}, ctx.Err()
		}
	}

	// Seeking exactly at a cut can land on a timestamp for which the decoder
	// cannot emit a picture. Try a few nearby points before treating the file as
	// unusable; this is common with variable-frame-rate and damaged indexes.
	var lastErr error
	allAttemptsProducedNoFrame := true
	for _, offset := range []float64{0, -0.75, 0.75, -2, 2} {
		candidate := timestamp + offset
		if candidate < 0 {
			continue
		}
		seekTimestamp := candidate
		if err := p.extractFrameAtWithSeek(ctx, videoPath, candidate, seekTimestamp, framePath, pixelFormat, frameWidth); err == nil {
			return "proxy_exact", frameExtractionSource{}, nil
		} else {
			lastErr = err
			if ctx.Err() != nil {
				return "", frameExtractionSource{}, ctx.Err()
			}
			if strings.Contains(err.Error(), "画面提取超时") {
				return "", frameExtractionSource{}, fmt.Errorf("视频画面提取超时：时间 %.3fs 附近无法解码：%w", timestamp, err)
			}
			if !strings.Contains(err.Error(), "produced no frame") {
				allAttemptsProducedNoFrame = false
			}
			if isMediaCorruptionError(err.Error()) {
				return "", frameExtractionSource{}, err
			}
		}
	}
	if !allAttemptsProducedNoFrame && lastErr != nil {
		return "", frameExtractionSource{}, fmt.Errorf("视频画面提取失败：时间 %.3fs 附近无法解码：%w", timestamp, lastErr)
	}
	if indexedErr != nil {
		return "", frameExtractionSource{}, fmt.Errorf("视频局部画面无法解码：时间 %.3fs 附近的 I 帧无法读取：%w", timestamp, indexedErr)
	}
	return "", frameExtractionSource{}, fmt.Errorf("视频局部画面无法解码：时间 %.3fs 附近没有可用画面", timestamp)
}

func (p *VideoProcessor) extractFrameFromIndexedSample(ctx context.Context, index containerIndex, timestamp float64, framePath, pixelFormat string, frameWidth int) (bool, error) {
	handled, _, err := p.extractFrameFromIndexedSampleWithSource(ctx, index, timestamp, framePath, pixelFormat, frameWidth)
	return handled, err
}

func (p *VideoProcessor) extractFrameFromIndexedSampleWithSource(ctx context.Context, index containerIndex, timestamp float64, framePath, pixelFormat string, frameWidth int) (bool, frameExtractionSource, error) {
	// An index without a supported elementary demuxer is still useful as a seek
	// anchor, so this reports "not handled" instead of failing.
	if index == nil || index.demuxer() == "" {
		return false, frameExtractionSource{}, nil
	}
	sample, payload, err := index.readKeyframe(ctx, timestamp)
	if err != nil {
		return true, frameExtractionSource{}, err
	}
	sourceInfo := frameExtractionSource{
		SampleNumber:    sample.Index,
		SampleTimestamp: sample.Timestamp,
		Offset:          sample.Offset,
		Size:            sample.Size,
		SampleDigest:    sampleDigest(payload),
	}
	return true, sourceInfo, p.extractElementaryFrame(ctx, index.demuxer(), payload, framePath, pixelFormat, frameWidth)
}

func (p *VideoProcessor) extractElementaryFrame(ctx context.Context, demuxer string, sample []byte, framePath, pixelFormat string, frameWidth int) error {
	attemptContext := ctx
	cancel := func() {}
	if p.FrameTimeout > 0 {
		attemptContext, cancel = context.WithTimeout(ctx, p.FrameTimeout)
	}
	defer cancel()
	mode := strings.ToLower(strings.TrimSpace(p.HWAccel))
	if mode == "" {
		mode = "none"
	}
	if mode != "none" && mode != "auto" && mode != "cuda" {
		return fmt.Errorf("unsupported ffmpeg hardware acceleration mode %q", mode)
	}
	tryCUDA := mode == "cuda" || (mode == "auto" && p.cudaAvailable())
	if tryCUDA {
		if err := p.extractElementaryFrameWithMode(attemptContext, demuxer, sample, framePath, pixelFormat, frameWidth, true); err == nil {
			return nil
		} else if mode == "cuda" {
			if attemptContext.Err() == context.DeadlineExceeded && ctx.Err() == nil {
				return fmt.Errorf("画面提取超时（超过 %s）", p.FrameTimeout)
			}
			return err
		} else if attemptContext.Err() == context.DeadlineExceeded {
			if ctx.Err() == nil {
				return fmt.Errorf("画面提取超时（超过 %s）", p.FrameTimeout)
			}
			return ctx.Err()
		} else if isCUDAUnavailableError(err.Error()) {
			p.disableCUDA()
		}
	}
	err := p.extractElementaryFrameWithMode(attemptContext, demuxer, sample, framePath, pixelFormat, frameWidth, false)
	if attemptContext.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return fmt.Errorf("画面提取超时（超过 %s）", p.FrameTimeout)
	}
	return err
}

// extractElementaryFrameWithMode decodes one self-contained elementary stream.
// demuxer is the ffmpeg -f input format that matches the payload: h264 and
// hevc for Annex-B streams, ivf for a single VP8/VP9/AV1 frame.
func (p *VideoProcessor) extractElementaryFrameWithMode(ctx context.Context, demuxer string, sample []byte, framePath, pixelFormat string, frameWidth int, cuda bool) error {
	_ = os.Remove(framePath)
	args := []string{"-hide_banner", "-loglevel", "error"}
	if cuda {
		args = append(args, "-hwaccel", "cuda", "-hwaccel_output_format", "cuda")
	}
	args = append(args, "-f", demuxer, "-i", "pipe:0", "-frames:v", "1")
	filters := make([]string, 0, 3)
	if cuda {
		filters = append(filters, "hwdownload", "format="+cudaDownloadFormat(pixelFormat))
	}
	if frameWidth > 0 {
		filters = append(filters, fmt.Sprintf("scale=%d:-2", frameWidth))
	}
	if len(filters) > 0 {
		args = append(args, "-vf", strings.Join(filters, ","))
	}
	args = append(args, "-q:v", "2", "-y", framePath)
	command := exec.CommandContext(ctx, p.FFmpegPath, args...)
	command.Env = httpclient.WithoutProxyEnvironment()
	command.Stdin = bytes.NewReader(sample)
	output, err := command.CombinedOutput()
	if err != nil {
		_ = os.Remove(framePath)
		mode := "cpu"
		if cuda {
			mode = "cuda"
		}
		return commandFailure("ffmpeg "+mode+" elementary frame extraction", output, err)
	}
	if info, err := os.Stat(framePath); err != nil || info.Size() == 0 {
		_ = os.Remove(framePath)
		mode := "cpu"
		if cuda {
			mode = "cuda"
		}
		return fmt.Errorf("ffmpeg %s produced no frame", mode)
	}
	return nil
}

func (p *VideoProcessor) extractFrameAt(ctx context.Context, videoPath string, timestamp float64, framePath, pixelFormat string, frameWidth int) error {
	return p.extractFrameAtWithSeek(ctx, videoPath, timestamp, timestamp, framePath, pixelFormat, frameWidth)
}

func (p *VideoProcessor) extractFrameAtWithSeek(ctx context.Context, videoPath string, timestamp, seekTimestamp float64, framePath, pixelFormat string, frameWidth int) error {
	attemptContext := ctx
	cancel := func() {}
	if p.FrameTimeout > 0 {
		attemptContext, cancel = context.WithTimeout(ctx, p.FrameTimeout)
	}
	defer cancel()
	mode := strings.ToLower(strings.TrimSpace(p.HWAccel))
	if mode == "" {
		mode = "none"
	}
	if mode != "none" && mode != "auto" && mode != "cuda" {
		return fmt.Errorf("unsupported ffmpeg hardware acceleration mode %q", mode)
	}
	seekTimestamp = math.Max(0, math.Min(timestamp, seekTimestamp))
	outputOffset := math.Max(0, timestamp-seekTimestamp)
	formatted := strconv.FormatFloat(seekTimestamp, 'f', 3, 64)
	tryCUDA := mode == "cuda" || (mode == "auto" && p.cudaAvailable())
	if tryCUDA {
		if err := p.extractFrameWithMode(attemptContext, videoPath, formatted, outputOffset, framePath, pixelFormat, frameWidth, true); err == nil {
			return nil
		} else if mode == "cuda" {
			if attemptContext.Err() == context.DeadlineExceeded && ctx.Err() == nil {
				return fmt.Errorf("画面提取超时（超过 %s）", p.FrameTimeout)
			}
			return err
		} else if attemptContext.Err() == context.DeadlineExceeded {
			if ctx.Err() == nil {
				return fmt.Errorf("画面提取超时（超过 %s）", p.FrameTimeout)
			}
			return ctx.Err()
		} else if isCUDAUnavailableError(err.Error()) {
			p.disableCUDA()
		}
	}
	err := p.extractFrameWithMode(attemptContext, videoPath, formatted, outputOffset, framePath, pixelFormat, frameWidth, false)
	if attemptContext.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return fmt.Errorf("画面提取超时（超过 %s）", p.FrameTimeout)
	}
	return err
}

func (p *VideoProcessor) extractFrameWithMode(ctx context.Context, videoPath, seekTimestamp string, outputOffset float64, framePath, pixelFormat string, frameWidth int, cuda bool) error {
	_ = os.Remove(framePath)
	args := []string{"-hide_banner", "-loglevel", "error"}
	if cuda {
		args = append(args, "-hwaccel", "cuda", "-hwaccel_output_format", "cuda")
	}
	args = append(args, "-ss", seekTimestamp, "-i", videoPath)
	if outputOffset > 0.001 {
		args = append(args, "-ss", strconv.FormatFloat(outputOffset, 'f', 3, 64))
	}
	args = append(args, "-frames:v", "1")
	filters := make([]string, 0, 3)
	if cuda {
		filters = append(filters, "hwdownload", "format="+cudaDownloadFormat(pixelFormat))
	}
	if frameWidth > 0 {
		filters = append(filters, fmt.Sprintf("scale=%d:-2", frameWidth))
	}
	if len(filters) > 0 {
		args = append(args, "-vf", strings.Join(filters, ","))
	}
	args = append(args, "-q:v", "2", "-y", framePath)
	command := exec.CommandContext(ctx, p.FFmpegPath, args...)
	command.Env = httpclient.WithoutProxyEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		_ = os.Remove(framePath)
		mode := "cpu"
		if cuda {
			mode = "cuda"
		}
		return commandFailure("ffmpeg "+mode+" frame extraction", output, err)
	}
	if info, err := os.Stat(framePath); err != nil || info.Size() == 0 {
		_ = os.Remove(framePath)
		mode := "cpu"
		if cuda {
			mode = "cuda"
		}
		return fmt.Errorf("ffmpeg %s produced no frame", mode)
	}
	return nil
}

func (p *VideoProcessor) cudaAvailable() bool {
	p.hardwareMu.RLock()
	defer p.hardwareMu.RUnlock()
	return !p.cudaDisabled
}

func (p *VideoProcessor) disableCUDA() {
	p.hardwareMu.Lock()
	p.cudaDisabled = true
	p.hardwareMu.Unlock()
}

func isCUDAUnavailableError(detail string) bool {
	detail = strings.ToLower(detail)
	for _, marker := range []string{
		"cannot load libcuda",
		"libcuda.so",
		"cuda is not available",
		"no cuda device",
		"no device",
		"device not found",
		"failed to initialize cuda",
		"failed to initialise cuda",
		"cuda error: 100",
	} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

const defaultFastSampleInterval = 30

func fastSampleInterval(interval float64) float64 {
	if interval <= 0 {
		return defaultFastSampleInterval
	}
	return interval
}

func (p *VideoProcessor) fastSceneLimit() int {
	if p.FastMaxScenes > 0 {
		return p.FastMaxScenes
	}
	return 32
}

func (p *VideoProcessor) keyframeSceneLimit() int {
	if p.MaxScenes > 0 {
		return p.MaxScenes
	}
	return 240
}

func fastSceneBoundaries(duration, interval float64, maxScenes int) []float64 {
	if duration <= 0 {
		return nil
	}
	interval = fastSampleInterval(interval)
	sceneCount := int(math.Ceil(duration / interval))
	if sceneCount < 1 {
		sceneCount = 1
	}
	if maxScenes > 0 && sceneCount > maxScenes {
		sceneCount = maxScenes
	}
	if sceneCount <= 1 {
		return nil
	}
	step := duration / float64(sceneCount)
	boundaries := make([]float64, 0, sceneCount-1)
	for index := 1; index < sceneCount; index++ {
		boundaries = append(boundaries, step*float64(index))
	}
	return boundaries
}

func (p *VideoProcessor) sceneDetectionFilter(threshold float64, pixelFormat string, cuda, sample bool) string {
	filters := make([]string, 0, 5)
	if cuda {
		if p.SceneDetectionWidth > 0 {
			filters = append(filters, fmt.Sprintf("scale_cuda=%d:-2", p.SceneDetectionWidth))
		}
		filters = append(filters, "hwdownload", "format="+cudaDownloadFormat(pixelFormat))
	} else if p.SceneDetectionWidth > 0 {
		filters = append(filters, fmt.Sprintf("scale=%d:-2", p.SceneDetectionWidth))
	}
	if sample && p.SceneSampleFPS > 0 {
		filters = append(filters, "fps="+strconv.FormatFloat(p.SceneSampleFPS, 'f', -1, 64))
	}
	filters = append(filters, fmt.Sprintf("select=gt(scene\\,%0.3f)", threshold), "showinfo")
	return strings.Join(filters, ",")
}

func cudaDownloadFormat(pixelFormat string) string {
	switch strings.ToLower(strings.TrimSpace(pixelFormat)) {
	case "yuv420p10le", "p010le", "yuv422p10le", "yuv444p10le":
		return "p010le"
	case "yuv420p", "yuvj420p", "nv12":
		return "nv12"
	default:
		return "nv12"
	}
}

func buildScenes(mediaID string, duration float64, boundaries []float64) []model.Scene {
	starts := []float64{0}
	for _, boundary := range boundaries {
		if boundary > starts[len(starts)-1]+0.5 && boundary < duration-0.05 {
			starts = append(starts, boundary)
		}
	}
	scenes := make([]model.Scene, 0, len(starts))
	for index, start := range starts {
		end := duration
		if index+1 < len(starts) {
			end = starts[index+1]
		}
		if end <= start {
			continue
		}
		scenes = append(scenes, model.Scene{AssetID: mediaID, Start: start, End: end, QualityScore: 1})
	}
	return scenes
}

func formatTimestamp(seconds float64) string {
	duration := time.Duration(seconds * float64(time.Second))
	hours := int(duration / time.Hour)
	minutes := int(duration/time.Minute) % 60
	wholeSeconds := int(duration/time.Second) % 60
	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, wholeSeconds)
}

func commandFailure(name string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if len(detail) > 1000 {
		detail = detail[len(detail)-1000:]
	}
	if strings.HasPrefix(name, "ffprobe") || isMediaCorruptionError(detail) {
		reason := "无法解析媒体文件"
		if isMediaCorruptionError(detail) {
			reason = "检测到损坏或不完整的视频码流"
		}
		return fmt.Errorf("文件损坏或无法解码（%s）：%s", name, reason)
	}
	if detail == "" {
		return fmt.Errorf("%s: %w", name, err)
	}
	return fmt.Errorf("%s: %w: %s", name, err, detail)
}

func isMediaCorruptionError(detail string) bool {
	detail = strings.ToLower(detail)
	for _, marker := range []string{
		"invalid nal", "error while decoding", "invalid data found",
		"conversion failed", "moov atom not found", "ebml header parsing failed",
		"unexpected eof", "end of file", "file ended prematurely", "corrupt",
		"truncated", "could not find codec parameters",
	} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
