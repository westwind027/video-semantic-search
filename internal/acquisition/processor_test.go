package acquisition

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"video-semantic-search/internal/model"
	"video-semantic-search/internal/source"
)

func TestRequestValidateResolvesRegularFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "movie.mp4")
	if err := os.WriteFile(file, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := (Request{LocalPath: file}).Validate()
	if err != nil {
		t.Fatal(err)
	}
	if resolved != file {
		t.Fatalf("resolved path = %q, want %q", resolved, file)
	}
}

func TestRequestValidateRemoteDoesNotRequireLocalPath(t *testing.T) {
	path, err := (Request{Source: "alipan", DriveID: "drive-1", FileID: "file-1"}).Validate()
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Fatalf("remote path = %q, want empty path before resolving signed URL", path)
	}
	if _, err := (Request{Source: "alipan", DriveID: "drive-1"}).Validate(); err == nil {
		t.Fatal("remote request without file_id should fail validation")
	}
}

func TestBuildScenesFromBoundaries(t *testing.T) {
	scenes := buildScenes("media", 10, []float64{2, 2.1, 7})
	if len(scenes) != 3 {
		t.Fatalf("scene count = %d, want 3", len(scenes))
	}
	if scenes[0].Start != 0 || scenes[0].End != 2 || scenes[1].Start != 2 || scenes[1].End != 7 || scenes[2].Start != 7 || scenes[2].End != 10 {
		t.Fatalf("scenes = %+v", scenes)
	}
}

func TestSplitSceneSegmentsUsesHalfOpenOwnership(t *testing.T) {
	segments := splitSceneSegments(12, 3, 1)
	if len(segments) != 3 {
		t.Fatalf("segment count = %d, want 3", len(segments))
	}
	if segments[0].Start != 0 || segments[0].End != 4 || segments[1].Start != 4 || segments[1].End != 8 || segments[2].Start != 8 || segments[2].End != 12 {
		t.Fatalf("segments = %+v", segments)
	}
	if segments[0].DecodeStart != 0 || segments[0].DecodeEnd != 5 || segments[1].DecodeStart != 3 || segments[1].DecodeEnd != 9 || segments[2].DecodeStart != 7 || segments[2].DecodeEnd != 12 {
		t.Fatalf("decode intervals = %+v", segments)
	}
	if !sceneBoundaryBelongs(segments[0], 3.999) || sceneBoundaryBelongs(segments[0], 4) {
		t.Fatal("segment 0 must own [0, 4)")
	}
	if !sceneBoundaryBelongs(segments[1], 4) || sceneBoundaryBelongs(segments[1], 8) {
		t.Fatal("segment 1 must own [4, 8)")
	}
	if !sceneBoundaryBelongs(segments[2], 8) || sceneBoundaryBelongs(segments[2], 12) {
		t.Fatal("segment 2 must own [8, 12)")
	}
}

func TestMergeSceneBoundariesSortsAndFilters(t *testing.T) {
	merged := mergeSceneBoundaries([]float64{8, 2, 2.2, 2.6, 0.1, 9.98}, 10)
	if len(merged) != 3 {
		t.Fatalf("merged boundaries = %v, want 3 values", merged)
	}
	if merged[0] != 2 || merged[1] != 2.6 || merged[2] != 8 {
		t.Fatalf("merged boundaries = %v", merged)
	}
}

func TestFastSceneBoundariesRespectsIntervalAndLimit(t *testing.T) {
	boundaries := fastSceneBoundaries(7001, 30, 32)
	if len(boundaries) != 31 {
		t.Fatalf("boundary count = %d, want 31", len(boundaries))
	}
	if boundaries[0] <= 0 || boundaries[len(boundaries)-1] >= 7001 {
		t.Fatalf("boundaries must stay inside duration: %v", boundaries)
	}

	short := fastSceneBoundaries(10, 3, 32)
	if len(short) != 3 {
		t.Fatalf("short boundary count = %d, want 3", len(short))
	}
	if got := len(fastSceneBoundaries(10, 0, 4)); got != 3 {
		t.Fatalf("zero-interval boundary count = %d, want 3", got)
	}
}

func TestCudaDownloadFormatMatchesVideoDepth(t *testing.T) {
	if got := cudaDownloadFormat("yuv420p10le"); got != "p010le" {
		t.Fatalf("10-bit format = %q, want p010le", got)
	}
	if got := cudaDownloadFormat("yuv420p"); got != "nv12" {
		t.Fatalf("8-bit format = %q, want nv12", got)
	}
}

func TestSceneCandidateThresholdKeepsAUsefulSearchBand(t *testing.T) {
	if got := sceneCandidateThreshold(0.35); got != 0.105 {
		t.Fatalf("candidate threshold = %v, want 0.105", got)
	}
	if got := sceneCandidateThreshold(0.01); got != 0.08 {
		t.Fatalf("candidate threshold floor = %v, want 0.08", got)
	}
	if got := sceneCandidateThreshold(0.99); got != 0.2 {
		t.Fatalf("candidate threshold ceiling = %v, want 0.2", got)
	}
}

func TestSceneRefinementWindowsMergeNearbyCandidates(t *testing.T) {
	windows := sceneRefinementWindows([]float64{8, 16, 50}, 60)
	if len(windows) != 2 {
		t.Fatalf("window count = %d, want 2", len(windows))
	}
	if windows[0].Start != 0 || windows[0].End != 17 {
		t.Fatalf("merged first window = %+v", windows[0])
	}
	if windows[1].Start != 40 || windows[1].End != 51 {
		t.Fatalf("second window = %+v", windows[1])
	}
}

func TestCommandFailureClassifiesUnusableMedia(t *testing.T) {
	corrupt := commandFailure("ffmpeg cpu keyframe scan", []byte("Invalid NAL unit size\nError while decoding stream"), errors.New("exit status 69"))
	if !strings.HasPrefix(corrupt.Error(), "文件损坏或无法解码") {
		t.Fatalf("corrupt media error = %q", corrupt)
	}
	probe := commandFailure("ffprobe", []byte("{}"), errors.New("exit status 1"))
	if !strings.HasPrefix(probe.Error(), "文件损坏或无法解码") {
		t.Fatalf("probe error = %q", probe)
	}
}

func TestMediaCorruptionMarkers(t *testing.T) {
	if !isMediaCorruptionError("ffmpeg: Invalid data found when processing input") {
		t.Fatal("invalid data should be classified as media corruption")
	}
	if isMediaCorruptionError("temporary CUDA initialization failure") {
		t.Fatal("CUDA initialization failure is not necessarily media corruption")
	}
}

func TestExtractFrameAtHonorsTimeout(t *testing.T) {
	ffmpeg := filepath.Join(t.TempDir(), "fake-ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nsleep 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	processor := NewVideoProcessor(t.TempDir())
	processor.FFmpegPath = ffmpeg
	processor.HWAccel = "none"
	processor.FrameTimeout = 10 * time.Millisecond
	err := processor.extractFrameAt(context.Background(), "unused.mp4", 1, filepath.Join(t.TempDir(), "frame.jpg"), "yuv420p", 64)
	if err == nil || !strings.Contains(err.Error(), "画面提取超时") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestExtractFramesContinuesAfterOneFrameProducesNoPicture(t *testing.T) {
	ffmpeg := filepath.Join(t.TempDir(), "fake-ffmpeg")
	script := `#!/bin/sh
first_ss=""
last_arg=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-ss" ] && [ -z "$first_ss" ]; then
    first_ss="$2"
    shift 2
    continue
  fi
  last_arg="$1"
  shift
done
case "$first_ss" in
  10.500|9.750|11.250|8.500|12.500) exit 0 ;;
esac
printf 'jpeg' > "$last_arg"
`
	if err := os.WriteFile(ffmpeg, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	processor := NewVideoProcessor(filepath.Join(t.TempDir(), "frames"))
	processor.FFmpegPath = ffmpeg
	processor.HWAccel = "none"
	processor.FrameWorkers = 1
	processor.FrameTimeout = time.Second
	frameDir := filepath.Join(t.TempDir(), "frames")
	if err := os.MkdirAll(frameDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scenes := []model.Scene{
		{Start: 0, End: 1},
		{Start: 10, End: 11},
		{Start: 20, End: 21},
	}
	if err := processor.extractFrames(context.Background(), "unused.mp4", "media", frameDir, scenes, "yuv420p", 64, nil, func(Progress) {}); err != nil {
		t.Fatalf("one unusable frame must not fail the queue: %v", err)
	}
	if scenes[0].PreviewPath == "" || scenes[2].PreviewPath == "" {
		t.Fatalf("valid frames were not retained: %+v", scenes)
	}
	if scenes[1].PreviewPath != "" {
		t.Fatalf("unusable frame should remain without a preview: %+v", scenes[1])
	}
}

func TestExtractFrameUsesIndexedIFrameBeforeTargetDecode(t *testing.T) {
	ffmpeg := filepath.Join(t.TempDir(), "fake-ffmpeg")
	script := `#!/bin/sh
first_ss=""
ss_count=0
last_arg=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-ss" ]; then
    ss_count=$((ss_count + 1))
    if [ -z "$first_ss" ]; then first_ss="$2"; fi
    shift 2
    continue
  fi
  last_arg="$1"
  shift
done
if [ "$first_ss" = "4.000" ] && [ "$ss_count" -eq 1 ]; then
  printf 'jpeg' > "$last_arg"
fi
exit 0
`
	if err := os.WriteFile(ffmpeg, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	processor := NewVideoProcessor(t.TempDir())
	processor.FFmpegPath = ffmpeg
	processor.HWAccel = "none"
	processor.FrameTimeout = time.Second
	index := &mp4Index{Keyframes: []mp4Keyframe{{Timestamp: 0}, {Timestamp: 4}}}
	framePath := filepath.Join(t.TempDir(), "frame.jpg")
	if err := processor.extractFrame(context.Background(), "unused.mp4", 5, framePath, "yuv420p", 64, index); err != nil {
		t.Fatalf("indexed I-frame extraction failed: %v", err)
	}
	if info, err := os.Stat(framePath); err != nil || info.Size() == 0 {
		t.Fatalf("indexed I-frame output is unavailable: %v", err)
	}
}

func TestMP4IndexAnchorUsesPreviousKeyframe(t *testing.T) {
	index := &mp4Index{Keyframes: []mp4Keyframe{
		{Timestamp: 0},
		{Timestamp: 2.5},
		{Timestamp: 5},
	}}
	if got := index.anchor(0.1); got != 0 {
		t.Fatalf("anchor before first keyframe = %v", got)
	}
	if got := index.anchor(4.9); got != 2.5 {
		t.Fatalf("anchor = %v, want 2.5", got)
	}
	if got := index.anchor(8); got != 5 {
		t.Fatalf("anchor after last keyframe = %v, want 5", got)
	}
}

func TestMP4SampleToAnnexBAddsConfigurationAndConvertsNALLengths(t *testing.T) {
	config := [][]byte{{0x40, 0x01}, {0x42, 0x01}}
	sample := []byte{
		0, 0, 0, 2, 0x26, 0x01,
		0, 0, 0, 3, 0x02, 0x03, 0x04,
	}
	got, err := mp4SampleToAnnexB(sample, config, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0, 0, 0, 1, 0x40, 0x01,
		0, 0, 0, 1, 0x42, 0x01,
		0, 0, 0, 1, 0x26, 0x01,
		0, 0, 0, 1, 0x02, 0x03, 0x04,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Annex-B sample = %x, want %x", got, want)
	}
}

func TestMP4SampleToAnnexBRejectsTruncatedNAL(t *testing.T) {
	if _, err := mp4SampleToAnnexB([]byte{0, 0, 0}, nil, 4); err == nil {
		t.Fatal("truncated NAL length should fail")
	}
}

func TestIsMP4Format(t *testing.T) {
	if !isMP4Format("movie.MOV", "") || !isMP4Format("", "mov,mp4,m4a") {
		t.Fatal("MOV/MP4 should be recognized")
	}
	if isMP4Format("movie.mkv", "matroska,webm") {
		t.Fatal("Matroska should not be recognized as MP4")
	}
}

func TestBuildMP4IndexUsesLazyMediaData(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	videoPath := filepath.Join(t.TempDir(), "fixture.mp4")
	command := exec.Command(ffmpeg,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=blue:s=64x64:r=2",
		"-t", "2", "-an", "-c:v", "mpeg4", "-g", "2", "-movflags", "+faststart", "-y", videoPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create MP4 fixture: %v: %s", err, output)
	}
	file, err := os.Open(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	index, err := buildMP4Index(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Keyframes) < 2 {
		t.Fatalf("keyframes = %d, want at least 2", len(index.Keyframes))
	}
	if index.Keyframes[0].Offset <= 0 || index.Keyframes[0].Size <= 0 {
		t.Fatalf("first keyframe = %+v", index.Keyframes[0])
	}
}

func TestExtractFrameFromIndexedHEVCSample(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	fixtureDir := t.TempDir()
	videoPath := filepath.Join(fixtureDir, "hevc-fixture.mp4")
	command := exec.Command(ffmpeg,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=128x72:rate=4",
		"-t", "2", "-an", "-c:v", "libx265", "-preset", "ultrafast", "-g", "4", "-movflags", "+faststart", "-y", videoPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Skipf("HEVC encoder is unavailable: %v: %s", err, output)
	}
	file, err := os.Open(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	index, err := buildMP4Index(file)
	if err != nil {
		t.Fatal(err)
	}
	if index.Codec != "hevc" || len(index.Config) == 0 {
		t.Fatalf("HEVC index configuration = codec %q, config %d", index.Codec, len(index.Config))
	}
	processor := NewVideoProcessor(filepath.Join(fixtureDir, "frames"))
	processor.FFmpegPath = ffmpeg
	processor.HWAccel = "none"
	processor.FrameTimeout = 10 * time.Second
	framePath := filepath.Join(fixtureDir, "frames", "frame.jpg")
	if err := os.MkdirAll(filepath.Dir(framePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if handled, err := processor.extractFrameFromIndexedSample(context.Background(), index, 1, framePath, "yuv420p", 64); !handled || err != nil {
		t.Fatalf("indexed HEVC extraction = handled %t, err %v", handled, err)
	}
	if info, err := os.Stat(framePath); err != nil || info.Size() == 0 {
		t.Fatalf("indexed HEVC frame is unavailable: %v", err)
	}
}

func TestRemoteMP4IndexAndFFprobeUsePartialRanges(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	videoPath := filepath.Join(t.TempDir(), "remote-fixture.mp4")
	command := exec.Command(ffmpeg,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=96x64:rate=4",
		"-t", "4", "-an", "-c:v", "mpeg4", "-g", "4", "-movflags", "+faststart", "-y", videoPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create MP4 fixture: %v: %s", err, output)
	}
	payload, err := os.ReadFile(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.ServeContent(response, request, "remote-fixture.mp4", time.Unix(0, 0), bytes.NewReader(payload))
	}))
	defer backend.Close()

	reader := source.NewHTTPRangeReader(backend.URL, int64(len(payload)), backend.Client())
	reader.ChunkSize = 1024
	proxy, err := source.NewRangeProxy(reader)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	index, err := buildMP4Index(reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Keyframes) == 0 {
		t.Fatal("remote MP4 index is empty")
	}
	stats := reader.Stats()
	if stats.BytesDownloaded >= int64(len(payload)) {
		t.Fatalf("remote reader downloaded %d of %d bytes; expected partial read", stats.BytesDownloaded, len(payload))
	}
}

type staticVideoResolver struct {
	video source.Video
}

func (r staticVideoResolver) ResolveVideo(context.Context, string, string) (source.Video, error) {
	return r.video, nil
}

func TestRemoteMP4ProcessUsesIndexBeforeFFprobe(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	fixtureDir := t.TempDir()
	videoPath := filepath.Join(fixtureDir, "remote-process.mp4")
	command := exec.Command(ffmpeg,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=96x64:rate=4",
		"-t", "4", "-an", "-c:v", "mpeg4", "-g", "4", "-movflags", "+faststart", "-y", videoPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create MP4 fixture: %v: %s", err, output)
	}
	payload, err := os.ReadFile(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.ServeContent(response, request, "remote-process.mp4", time.Unix(0, 0), bytes.NewReader(payload))
	}))
	defer backend.Close()

	ffprobe := filepath.Join(fixtureDir, "ffprobe-must-not-run")
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	processor := NewVideoProcessor(filepath.Join(fixtureDir, "frames"))
	processor.FFmpegPath = ffmpeg
	processor.FFprobePath = ffprobe
	processor.HWAccel = "none"
	processor.FrameWorkers = 1
	processor.FrameWidth = 32
	processor.FrameTimeout = 10 * time.Second
	processor.FastMaxScenes = 2
	processor.RemoteResolver = staticVideoResolver{video: source.Video{
		URL:  backend.URL,
		Name: "remote-process.mp4",
		Size: int64(len(payload)),
	}}

	media, err := processor.Process(context.Background(), "remote-process", Request{
		Source:         "alipan",
		DriveID:        "drive-1",
		FileID:         "file-1",
		SourceName:     "remote-process.mp4",
		SampleInterval: 2,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(media.Scenes) != 2 {
		t.Fatalf("scene count = %d, want 2", len(media.Scenes))
	}
	if media.Metadata["frame_source"] != "remote_mp4_keyframe_sample" {
		t.Fatalf("frame source = %v", media.Metadata["frame_source"])
	}
	for _, scene := range media.Scenes {
		if scene.PreviewPath == "" {
			t.Fatalf("scene has no extracted preview: %+v", scene)
		}
		if info, err := os.Stat(scene.PreviewPath); err != nil || info.Size() == 0 {
			t.Fatalf("preview %q is unavailable: %v", scene.PreviewPath, err)
		}
	}
}
