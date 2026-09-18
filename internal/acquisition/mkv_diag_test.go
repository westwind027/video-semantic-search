package acquisition

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"video-semantic-search/internal/alipan"
	"video-semantic-search/internal/source"
)

// TestDiagMatroskaSamples dumps the codec-private bytes and one raw keyframe
// block of a remote Matroska file so storage-format problems (length-prefixed
// NALs vs Annex-B) can be diagnosed offline. Skipped unless DIAG_MKV_FILE_ID is
// set; requires the alipan profile in the usual environment.
func TestDiagMatroskaSamples(t *testing.T) {
	fileID := os.Getenv("DIAG_MKV_FILE_ID")
	if fileID == "" {
		t.Skip("DIAG_MKV_FILE_ID not set")
	}
	driveID := os.Getenv("DIAG_MKV_DRIVE_ID")
	connector := alipan.NewManager(alipan.ConfigFromEnv())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	video, err := connector.ResolveVideo(ctx, driveID, fileID)
	if err != nil {
		t.Fatalf("resolve video: %v", err)
	}
	t.Logf("remote size=%d", video.Size)
	reader := source.NewHTTPRangeReader(video.URL, video.Size, nil)
	reader.SetURLRefresher(func(refreshContext context.Context) (string, error) {
		fresh, refreshErr := connector.ResolveVideo(refreshContext, driveID, fileID)
		if refreshErr != nil {
			return "", refreshErr
		}
		return fresh.URL, nil
	})

	index, err := buildMatroskaIndex(ctx, reader)
	if err != nil {
		t.Fatalf("build matroska index: %v", err)
	}
	t.Logf("codec=%s codecID=%s track=%d width=%d height=%d cues=%d nalLength=%d configNALUs=%d",
		index.Codec, index.CodecID, index.TrackNumber, index.Width, index.Height,
		len(index.Cues), index.NALLength, len(index.Config))
	for i, nalu := range index.Config {
		if i > 3 {
			break
		}
		head := nalu
		if len(head) > 12 {
			head = head[:12]
		}
		t.Logf("config[%d] len=%d head=%s", i, len(nalu), hex.EncodeToString(head))
	}

	timestamp := 30.0
	if raw := os.Getenv("DIAG_MKV_TS"); raw != "" {
		fmt.Sscanf(raw, "%f", &timestamp)
	}
	cue, ok := index.cueAtOrBefore(timestamp)
	if !ok {
		t.Fatalf("no cue at or before %.3f", timestamp)
	}
	t.Logf("cue ts=%.3f cluster=%d relative=%d", cue.Timestamp, cue.Cluster, cue.Relative)
	if cluster, clusterErr := index.clusterHead(ctx, cue.Cluster); clusterErr == nil {
		t.Logf("cluster offset=%d data_start=%d end=%d payload_bytes=%d", cluster.offset, cluster.dataStart, cluster.end, cluster.end-cluster.dataStart)
	}
	sample, payload, err := index.resolveCue(ctx, cue)
	if err != nil {
		t.Fatalf("resolve cue: %v", err)
	}
	head := payload
	if len(head) > 96 {
		head = head[:96]
	}
	t.Logf("keyframe offset=%d size=%d", sample.Offset, sample.Size)
	for start := 0; start < len(head); start += 16 {
		end := start + 16
		if end > len(head) {
			end = len(head)
		}
		t.Logf("  %04d: %s", start, hex.EncodeToString(head[start:end]))
	}
	t.Logf("SPS at %d, PPS at %d", bytes.Index(payload, index.Config[0]), bytes.Index(payload, index.Config[1]))

	// Dump the block element header and the bytes right before the payload to
	// verify dataStart/size alignment (a shifted dataStart breaks every NAL).
	if pre, err := index.Fetcher.FetchRange(ctx, sample.Offset-10, 10); err == nil {
		t.Logf("bytes[-10..-1]: %s", hex.EncodeToString(pre))
	} else {
		t.Logf("pre-fetch failed: %v", err)
	}
	if element, err := index.Fetcher.FetchRange(ctx, sample.Offset, 8); err == nil {
		t.Logf("payload[0..7]: %s", hex.EncodeToString(element))
	}

	// Walk the payload with each candidate NAL length and log the first few
	// sizes so a broken layout is visible instead of failing silently.
	for _, length := range []int{4, 3, 2, 1} {
		offset := 0
		sizes := []uint32{}
		failed := ""
		for len(payload)-offset >= length && len(sizes) < 5 {
			var size uint32
			for index := 0; index < length; index++ {
				size = size<<8 | uint32(payload[offset+index])
			}
			offset += length
			if uint64(size) > uint64(len(payload)-offset) {
				failed = fmt.Sprintf("NAL %d: size %d exceeds remainder %d", len(sizes), size, len(payload)-offset)
				break
			}
			sizes = append(sizes, size)
			offset += int(size)
		}
		t.Logf("nalLength=%d sizes=%v done=%t %s", length, sizes, failed == "", failed)
	}

	converted, err := index.elementary(payload)
	t.Logf("elementary (annex-b): len=%d err=%v", len(converted), err)
	if err == nil {
		convHead := converted
		if len(convHead) > 24 {
			convHead = convHead[:24]
		}
		t.Logf("converted head=%s nalLength(now)=%d", hex.EncodeToString(convHead), index.NALLength)
	}
}

// TestDiagMatroskaConcurrentSamples replays the remote-frame worker pattern
// against a handful of cues. It is intentionally opt-in because it exercises a
// real cloud-drive download URL; unlike the single-sample diagnostic above it
// can expose upstream range throttling caused by concurrent frame extraction.
func TestDiagMatroskaConcurrentSamples(t *testing.T) {
	fileID := os.Getenv("DIAG_MKV_FILE_ID")
	if fileID == "" || os.Getenv("DIAG_MKV_CONCURRENT") == "" {
		t.Skip("DIAG_MKV_FILE_ID and DIAG_MKV_CONCURRENT are required")
	}
	driveID := os.Getenv("DIAG_MKV_DRIVE_ID")
	connector := alipan.NewManager(alipan.ConfigFromEnv())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	video, err := connector.ResolveVideo(ctx, driveID, fileID)
	if err != nil {
		t.Fatalf("resolve video: %v", err)
	}
	reader := source.NewHTTPRangeReader(video.URL, video.Size, nil)
	reader.SetURLRefresher(func(refreshContext context.Context) (string, error) {
		fresh, refreshErr := connector.ResolveVideo(refreshContext, driveID, fileID)
		if refreshErr != nil {
			return "", refreshErr
		}
		return fresh.URL, nil
	})
	index, err := buildMatroskaIndex(ctx, reader)
	if err != nil {
		t.Fatalf("build matroska index: %v", err)
	}
	raw := os.Getenv("DIAG_MKV_TIMESTAMPS")
	if raw == "" {
		raw = "2865.294,3363.606,4111.074,4360.230,4609.386,5107.698,5606.010,6104.322"
	}
	parts := strings.Split(raw, ",")
	if os.Getenv("DIAG_MKV_ALL") != "" {
		duration := index.Duration
		parts = make([]string, 32)
		for sampleIndex := range parts {
			parts[sampleIndex] = strconv.FormatFloat(indexDurationSample(sampleIndex, 32, duration), 'f', 3, 64)
		}
	}
	results := make(chan error, len(parts))
	for _, part := range parts {
		timestamp, parseErr := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if parseErr != nil {
			t.Fatalf("parse timestamp %q: %v", part, parseErr)
		}
		if cue, ok := index.cueAtOrBefore(timestamp); ok && cue.Relative < 0 {
			if cluster, clusterErr := index.clusterHead(ctx, cue.Cluster); clusterErr == nil {
				t.Logf("timestamp=%.3f cluster=%d payload_bytes=%d", timestamp, cluster.offset, cluster.end-cluster.dataStart)
			}
		}
		go func(timestamp float64) {
			_, _, readErr := index.readKeyframe(ctx, timestamp)
			results <- readErr
		}(timestamp)
	}
	var failures []string
	for range parts {
		if readErr := <-results; readErr != nil {
			failures = append(failures, readErr.Error())
		}
	}
	t.Logf("concurrent samples=%d failures=%d stats=%+v", len(parts), len(failures), reader.Stats())
	for _, failure := range failures {
		t.Logf("failure: %s", failure)
	}
	if len(failures) > 0 {
		t.Fatalf("concurrent Matroska samples failed: %d/%d", len(failures), len(parts))
	}
}

func indexDurationSample(index, count int, duration float64) float64 {
	return duration * (float64(index) + 0.5) / float64(count)
}
