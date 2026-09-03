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
	"sync"
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

func TestFastSceneBoundariesRespectsModeCaps(t *testing.T) {
	boundaries := fastSceneBoundaries(7001, 30, 32)
	if len(boundaries) != 31 {
		t.Fatalf("fast boundary count = %d, want 31", len(boundaries))
	}
	if boundaries[0] <= 0 || boundaries[len(boundaries)-1] >= 7001 {
		t.Fatalf("boundaries must stay inside duration: %v", boundaries)
	}

	short := fastSceneBoundaries(10, 3, 320)
	if len(short) != 3 {
		t.Fatalf("short boundary count = %d, want 3", len(short))
	}
	if got := len(fastSceneBoundaries(10, 0, 32)); got != 0 {
		t.Fatalf("default-interval boundary count = %d, want 0", got)
	}
	if got := len(fastSceneBoundaries(10000, 1, 320)); got != 319 {
		t.Fatalf("keyframe boundary count = %d, want 319", got)
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
	index := &mp4Index{Keyframes: []keyframeSample{{Timestamp: 0}, {Timestamp: 4}}}
	framePath := filepath.Join(t.TempDir(), "frame.jpg")
	if err := processor.extractFrame(context.Background(), "unused.mp4", 5, framePath, "yuv420p", 64, index); err != nil {
		t.Fatalf("indexed I-frame extraction failed: %v", err)
	}
	if info, err := os.Stat(framePath); err != nil || info.Size() == 0 {
		t.Fatalf("indexed I-frame output is unavailable: %v", err)
	}
}

func TestMP4IndexAnchorUsesPreviousKeyframe(t *testing.T) {
	index := &mp4Index{Keyframes: []keyframeSample{
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
	if index.demuxer() != "" {
		t.Fatalf("index without codec configuration should not claim a demuxer, got %q", index.demuxer())
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

func TestSniffContainerIdentifiesFormatFromBytes(t *testing.T) {
	cases := []struct {
		name   string
		header []byte
		want   string
	}{
		{"mp4", []byte{0, 0, 0, 0x20, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, "mp4"},
		{"mov keeps the ftyp brand", []byte{0, 0, 0, 0x14, 'f', 't', 'y', 'p', 'q', 't', ' ', ' '}, "mp4"},
		{"matroska", append(append([]byte{}, matroskaMagic...), 0x93, 0x42, 0xF2, 0x01), "matroska"},
		{"avi", []byte{'R', 'I', 'F', 'F', 0, 0, 0, 0, 'A', 'V', 'I', ' '}, ""},
		{"mpeg transport stream", []byte{0x47, 0x40, 0x00, 0x10, 0, 0, 0, 0}, ""},
		{"truncated", []byte{0, 0, 'f', 't'}, ""},
	}
	for _, testCase := range cases {
		if got := sniffContainer(testCase.header); got != testCase.want {
			t.Fatalf("sniffContainer(%s) = %q, want %q", testCase.name, got, testCase.want)
		}
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
	fetcher, err := newFileRangeFetcher(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer fetcher.Close()
	index, err := buildMP4Index(context.Background(), fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Keyframes) < 2 {
		t.Fatalf("keyframes = %d, want at least 2", len(index.Keyframes))
	}
	if index.Keyframes[0].Offset <= 0 || index.Keyframes[0].Size <= 0 {
		t.Fatalf("first keyframe = %+v", index.Keyframes[0])
	}
	if info := index.info(); info.FormatName != "mp4" || info.Duration <= 0 || info.Width != 64 {
		t.Fatalf("index info = %+v", info)
	}
	// Every keyframe must describe bytes that really exist in the file, and the
	// first one must start with the MPEG-4 visual object header rather than
	// random media data.
	for _, keyframe := range index.Keyframes {
		if keyframe.Offset+keyframe.Size > fetcher.Size() {
			t.Fatalf("keyframe %+v runs past the %d byte file", keyframe, fetcher.Size())
		}
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
	fetcher, err := newFileRangeFetcher(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer fetcher.Close()
	index, err := buildMP4Index(context.Background(), fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if index.Codec != "hevc" || len(index.Config) == 0 {
		t.Fatalf("HEVC index configuration = codec %q, config %d", index.Codec, len(index.Config))
	}
	if index.demuxer() != "hevc" {
		t.Fatalf("HEVC demuxer = %q, want hevc", index.demuxer())
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

// createVideoFixture renders a synthetic clip with FFmpeg. It skips the test
// instead of failing when an encoder is missing, so a machine without libx265
// or libvpx still exercises the rest of the suite.
func createVideoFixture(t *testing.T, path string, args ...string) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	full := append([]string{"-hide_banner", "-loglevel", "error"}, args...)
	full = append(full, "-y", path)
	command := exec.Command(ffmpeg, full...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Skipf("create fixture %s: %v: %s", filepath.Base(path), err, output)
	}
}

// newRemoteFixtureServer publishes a local file over HTTP range requests the
// way a cloud drive does, so an indexer can be measured against real traffic.
func newRemoteFixtureServer(t *testing.T, name string, payload []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.ServeContent(response, request, name, time.Unix(0, 0), bytes.NewReader(payload))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestRemoteMP4IndexDownloadsOnlyMetadata(t *testing.T) {
	fixtureDir := t.TempDir()
	cases := []struct {
		name string
		args []string
	}{
		{
			// faststart keeps moov inside the first window, so the complete index
			// costs a single request.
			name: "faststart",
			args: []string{"-movflags", "+faststart"},
		},
		{
			// Without faststart moov sits behind the entire mdat box, which is
			// the layout that used to cost a sequential parse of the file.
			name: "moov after mdat",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			videoPath := filepath.Join(fixtureDir, "remote.mp4")
			args := append([]string{
				"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=15",
				"-t", "16", "-an", "-c:v", "libx264", "-preset", "ultrafast", "-b:v", "600k", "-g", "30",
			}, testCase.args...)
			createVideoFixture(t, videoPath, args...)
			payload, err := os.ReadFile(videoPath)
			if err != nil {
				t.Fatal(err)
			}
			server := newRemoteFixtureServer(t, "remote.mp4", payload)
			reader := source.NewHTTPRangeReader(server.URL, int64(len(payload)), server.Client())
			reader.ChunkSize = 1024

			index, err := buildMP4Index(context.Background(), reader)
			if err != nil {
				t.Fatal(err)
			}
			if len(index.Keyframes) == 0 {
				t.Fatal("remote MP4 index is empty")
			}
			stats := reader.Stats()
			// Metadata plus the probe window is all an index may cost; the box
			// header hops that skip mdat are bounded by the top-level box count.
			limit := index.MoovBytes + mp4ProbeWindow + 8*mp4BoxHeaderBytes
			if stats.BytesDownloaded > limit {
				t.Fatalf("downloaded %d bytes, want at most %d (moov is %d)", stats.BytesDownloaded, limit, index.MoovBytes)
			}
			if ratio := float64(stats.BytesDownloaded) / float64(len(payload)); ratio > 0.25 {
				t.Fatalf("index downloaded %.1f%% of the %d byte file", ratio*100, len(payload))
			}
			if stats.Requests > 4 {
				t.Fatalf("index issued %d range requests, want at most 4", stats.Requests)
			}
			// Reading one keyframe is the operation that used to pull a whole
			// aligned chunk: it must now cost exactly one request and exactly
			// the bytes of the sample.
			before := reader.Stats()
			sample, data, err := index.readKeyframe(context.Background(), index.Duration/2)
			if err != nil {
				t.Fatalf("read keyframe: %v", err)
			}
			after := reader.Stats()
			if after.Requests-before.Requests != 1 {
				t.Fatalf("keyframe read issued %d requests, want 1", after.Requests-before.Requests)
			}
			if downloaded := after.BytesDownloaded - before.BytesDownloaded; downloaded != sample.Size {
				t.Fatalf("keyframe read downloaded %d bytes for a %d byte sample", downloaded, sample.Size)
			}
			if len(data) == 0 {
				t.Fatalf("keyframe %+v produced no elementary payload", sample)
			}
			t.Logf("indexed a %d byte MP4 with %d range requests and %d bytes (%.2f%% of the file, moov is %d bytes); one %d byte keyframe cost 1 request",
				len(payload), stats.Requests, stats.BytesDownloaded,
				100*float64(stats.BytesDownloaded)/float64(len(payload)), index.MoovBytes, sample.Size)
		})
	}
}

type delayedRangeFetcher struct {
	payload []byte
	delay   time.Duration

	mu        sync.Mutex
	active    int
	maxActive int
	requests  int
}

func (f *delayedRangeFetcher) Size() int64 { return int64(len(f.payload)) }

func (f *delayedRangeFetcher) FetchRange(ctx context.Context, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 || offset+length > int64(len(f.payload)) {
		return nil, errors.New("invalid test range")
	}
	f.mu.Lock()
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.requests++
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return append([]byte(nil), f.payload[offset:offset+length]...), nil
}

func (f *delayedRangeFetcher) Stats() (requests, maxActive int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.maxActive
}

func TestFetchMP4MoovUsesBoundedParallelRangesAndReassembles(t *testing.T) {
	payload := bytes.Repeat([]byte("moov-index"), 500000)
	fetcher := &delayedRangeFetcher{payload: payload, delay: 25 * time.Millisecond}
	got, err := fetchMP4Moov(context.Background(), fetcher, 0, int64(len(payload)), mp4MoovFetchOptions{
		ChunkSize: 1 << 20,
		Workers:   4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("parallel moov fetch changed the byte sequence")
	}
	requests, maxActive := fetcher.Stats()
	if requests != 5 {
		t.Fatalf("moov requests = %d, want 5 chunks", requests)
	}
	if maxActive < 2 {
		t.Fatalf("moov fetch never overlapped requests; max concurrency = %d", maxActive)
	}
	if maxActive > 4 {
		t.Fatalf("moov fetch exceeded configured concurrency: %d", maxActive)
	}
}

func TestMP4IndexCacheRoundTripRebindsRemoteFetcher(t *testing.T) {
	cacheDir := t.TempDir()
	fetcher := &delayedRangeFetcher{payload: bytes.Repeat([]byte{'x'}, 1024)}
	original := &mp4Index{
		Keyframes: []keyframeSample{{Index: 1, Timestamp: 2.5, Offset: 128, Size: 64}},
		Duration:  10,
		Width:     1920,
		Height:    1080,
		Codec:     "hevc",
		Config:    [][]byte{{0x40, 0x01}},
		NALLength: 4,
		MoovBytes: 4096,
	}
	const key = "alipan:fingerprint-1:1024"
	if err := saveMP4IndexCache(cacheDir, key, fetcher.Size(), original); err != nil {
		t.Fatal(err)
	}
	loaded, hit, err := loadMP4IndexCache(cacheDir, key, fetcher.Size(), fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if !hit || loaded == nil {
		t.Fatalf("cache hit = %t, index = %+v", hit, loaded)
	}
	if loaded.Fetcher != fetcher || loaded.Codec != original.Codec || loaded.MoovBytes != original.MoovBytes {
		t.Fatalf("cached index was not rebound correctly: %+v", loaded)
	}
	if len(loaded.Keyframes) != 1 || loaded.Keyframes[0] != original.Keyframes[0] {
		t.Fatalf("cached keyframes = %+v, want %+v", loaded.Keyframes, original.Keyframes)
	}
}

func TestRemoteProcessReusesCachedMP4Index(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	fixtureDir := t.TempDir()
	videoPath := filepath.Join(fixtureDir, "cached.mp4")
	createVideoFixture(t, videoPath,
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=8",
		"-t", "4", "-an", "-c:v", "libx264", "-preset", "ultrafast", "-g", "8", "-movflags", "+faststart",
	)
	payload, err := os.ReadFile(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	backend := newRemoteFixtureServer(t, "cached.mp4", payload)
	processor := NewVideoProcessor(filepath.Join(fixtureDir, "frames"))
	processor.FFmpegPath = ffmpeg
	processor.HWAccel = "none"
	processor.FrameTimeout = 20 * time.Second
	processor.RemoteFrameWorkers = 1
	processor.RemoteIndexCacheDir = filepath.Join(fixtureDir, "index-cache")
	processor.RemoteResolver = staticVideoResolver{video: source.Video{
		URL:         backend.URL,
		Name:        "cached.mp4",
		Size:        int64(len(payload)),
		Fingerprint: "fixture-content-hash",
	}}
	request := Request{Source: "alipan", DriveID: "drive-1", FileID: "file-1", SampleInterval: 2}

	first, err := processor.Process(context.Background(), "cached-first", request, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := processor.Process(context.Background(), "cached-second", request, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstRange, _ := first.Metadata["remote_range"].(map[string]any)
	secondRange, _ := second.Metadata["remote_range"].(map[string]any)
	firstBytes, _ := firstRange["bytes_downloaded"].(int64)
	secondBytes, _ := secondRange["bytes_downloaded"].(int64)
	if first.Metadata["remote_container_index_cache"] != "stored" {
		t.Fatalf("first cache state = %v, want stored", first.Metadata["remote_container_index_cache"])
	}
	if second.Metadata["remote_container_index_cache"] != "hit" {
		t.Fatalf("second cache state = %v, want hit", second.Metadata["remote_container_index_cache"])
	}
	if secondBytes >= firstBytes {
		t.Fatalf("cached run downloaded %d bytes after first run downloaded %d", secondBytes, firstBytes)
	}
}

type staticVideoResolver struct {
	video source.Video
}

func (r staticVideoResolver) ResolveVideo(context.Context, string, string) (source.Video, error) {
	return r.video, nil
}

func TestRemoteProcessUsesContainerIndexBeforeFFprobe(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	cases := []struct {
		name           string
		fileName       string
		args           []string
		sampleInterval float64
		container      string
		wantSource     string
		// maxRatio is the share of the remote file the whole acquisition may
		// download. Fixtures are rendered large enough for the metadata windows
		// (32 KiB for MP4, 64 KiB for Matroska) not to dominate the ratio.
		maxRatio float64
	}{
		{
			name:           "mp4 h264",
			fileName:       "remote.mp4",
			sampleInterval: 8,
			args: []string{
				"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=15",
				"-t", "12", "-an", "-c:v", "libx264", "-preset", "ultrafast", "-b:v", "600k", "-g", "30",
				"-movflags", "+faststart",
			},
			container:  "mp4",
			wantSource: "remote_mp4_keyframe_sample",
			maxRatio:   0.25,
		},
		{
			name:           "matroska h264",
			fileName:       "remote.mkv",
			sampleInterval: 8,
			args: []string{
				"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=15",
				"-t", "12", "-an", "-c:v", "libx264", "-preset", "ultrafast", "-b:v", "600k", "-g", "30",
			},
			container:  "matroska",
			wantSource: "remote_matroska_keyframe_sample",
			maxRatio:   0.25,
		},
		{
			name:           "webm vp9",
			fileName:       "remote.webm",
			sampleInterval: 16,
			args: []string{
				"-f", "lavfi", "-i", "testsrc=size=320x240:rate=15",
				"-t", "24", "-an", "-c:v", "libvpx-vp9", "-b:v", "400k", "-g", "30", "-row-mt", "1",
			},
			container:  "matroska",
			wantSource: "remote_matroska_keyframe_sample",
			maxRatio:   0.35,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixtureDir := t.TempDir()
			videoPath := filepath.Join(fixtureDir, testCase.fileName)
			createVideoFixture(t, videoPath, testCase.args...)
			payload, err := os.ReadFile(videoPath)
			if err != nil {
				t.Fatal(err)
			}
			backend := newRemoteFixtureServer(t, testCase.fileName, payload)

			// ffprobe must never run: the container index already knows the
			// duration and geometry, and probing is what used to stream the
			// whole remote file.
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
			processor.FrameTimeout = 20 * time.Second
			processor.RemoteResolver = staticVideoResolver{video: source.Video{
				URL:  backend.URL,
				Name: testCase.fileName,
				Size: int64(len(payload)),
			}}

			media, err := processor.Process(context.Background(), "remote-process", Request{
				Source:         "alipan",
				DriveID:        "drive-1",
				FileID:         "file-1",
				SourceName:     testCase.fileName,
				SampleInterval: testCase.sampleInterval,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(media.Scenes) != 2 {
				t.Fatalf("scene count = %d, want 2", len(media.Scenes))
			}
			if media.Metadata["frame_source"] != testCase.wantSource {
				t.Fatalf("frame source = %v, want %q", media.Metadata["frame_source"], testCase.wantSource)
			}
			if media.Metadata["format"] != testCase.container {
				t.Fatalf("format = %v, want %q", media.Metadata["format"], testCase.container)
			}
			methods, _ := media.Metadata["frame_extraction_methods"].(map[string]int)
			if methods["indexed_sample"] != len(media.Scenes) {
				t.Fatalf("frame extraction methods = %v, want every scene from indexed_sample", methods)
			}
			indexSummary, ok := media.Metadata["remote_container_index"].(map[string]any)
			if !ok || indexSummary["container"] != testCase.container {
				t.Fatalf("remote_container_index = %v, want container %q", media.Metadata["remote_container_index"], testCase.container)
			}
			remoteRange, ok := media.Metadata["remote_range"].(map[string]any)
			if !ok {
				t.Fatalf("remote_range = %v", media.Metadata["remote_range"])
			}
			ratio, _ := remoteRange["download_ratio"].(float64)
			if ratio <= 0 || ratio > testCase.maxRatio {
				t.Fatalf("download_ratio = %v for a %d byte file, want at most %v: %v", ratio, len(payload), testCase.maxRatio, remoteRange)
			}
			t.Logf("acquired %d scenes from a %d byte %s file: %v", len(media.Scenes), len(payload), testCase.container, remoteRange)
			for _, scene := range media.Scenes {
				if scene.PreviewPath == "" {
					t.Fatalf("scene has no extracted preview: %+v", scene)
				}
				if info, err := os.Stat(scene.PreviewPath); err != nil || info.Size() == 0 {
					t.Fatalf("preview %q is unavailable: %v", scene.PreviewPath, err)
				}
			}
		})
	}
}
