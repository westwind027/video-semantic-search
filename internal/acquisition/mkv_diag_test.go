package acquisition

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
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
