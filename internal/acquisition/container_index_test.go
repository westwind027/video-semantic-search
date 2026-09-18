package acquisition

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"video-semantic-search/internal/source"
)

func TestEBMLElementIDKeepsItsLengthMarker(t *testing.T) {
	cases := []struct {
		name       string
		data       []byte
		wantID     uint32
		wantLength int
		wantOK     bool
	}{
		{"one byte SimpleBlock", []byte{0xA3}, 0xA3, 1, true},
		{"two byte Seek", []byte{0x4D, 0xBB}, 0x4DBB, 2, true},
		{"three byte TimestampScale", []byte{0x2A, 0xD7, 0xB1}, 0x2AD7B1, 3, true},
		{"four byte EBML header", []byte{0x1A, 0x45, 0xDF, 0xA3}, 0x1A45DFA3, 4, true},
		{"four byte Cues", []byte{0x1C, 0x53, 0xBB, 0x6B}, 0x1C53BB6B, 4, true},
		{"zero byte is reserved", []byte{0x00, 0x01}, 0, 0, false},
		{"empty", nil, 0, 0, false},
		{"no marker in four bytes", []byte{0x08, 0x08, 0x08, 0x08}, 0, 0, false},
	}
	for _, testCase := range cases {
		id, length, ok := ebmlID(testCase.data)
		if id != testCase.wantID || length != testCase.wantLength || ok != testCase.wantOK {
			t.Fatalf("ebmlID(%s) = %#x, %d, %t; want %#x, %d, %t",
				testCase.name, id, length, ok, testCase.wantID, testCase.wantLength, testCase.wantOK)
		}
	}
}

func TestEBMLPayloadIDKeepsLeadingZeroBytes(t *testing.T) {
	// SeekID stores an ID as element payload, so its width is the payload
	// length and a leading zero byte is significant.
	if got := ebmlIDValue([]byte{0x1C, 0x53, 0xBB, 0x6B}); got != 0x1C53BB6B {
		t.Fatalf("ebmlIDValue = %#x", got)
	}
	if got := ebmlIDValue([]byte{0x00, 0xA3}); got != 0xA3 {
		t.Fatalf("ebmlIDValue with a leading zero = %#x", got)
	}
	if got := ebmlIDValue([]byte{1, 2, 3, 4, 5}); got != 0 {
		t.Fatalf("ebmlIDValue of five bytes = %#x, want 0", got)
	}
}

func TestEBMLSizeStripsMarkerAndDetectsUnknownSize(t *testing.T) {
	cases := []struct {
		name        string
		data        []byte
		wantSize    int64
		wantLength  int
		wantUnknown bool
		wantOK      bool
	}{
		{"one byte", []byte{0x81}, 1, 1, false, true},
		{"one byte zero", []byte{0x80}, 0, 1, false, true},
		{"one byte unknown", []byte{0xFF}, 0, 1, true, true},
		{"two byte", []byte{0x40, 0x2A}, 42, 2, false, true},
		{"two byte unknown", []byte{0x7F, 0xFF}, 0, 2, true, true},
		{"three byte unknown", []byte{0x3F, 0xFF, 0xFF}, 0, 3, true, true},
		{"four byte", []byte{0x10, 0x00, 0x00, 0x07}, 7, 4, false, true},
		{"eight byte", []byte{0x01, 0, 0, 0, 0, 0, 0, 0x05}, 5, 8, false, true},
		{"zero byte is reserved", []byte{0x00}, 0, 0, false, false},
		{"empty", nil, 0, 0, false, false},
	}
	for _, testCase := range cases {
		size, length, unknown, ok := ebmlSize(testCase.data)
		if size != testCase.wantSize || length != testCase.wantLength || unknown != testCase.wantUnknown || ok != testCase.wantOK {
			t.Fatalf("ebmlSize(%s) = %d, %d, %t, %t; want %d, %d, %t, %t",
				testCase.name, size, length, unknown, ok,
				testCase.wantSize, testCase.wantLength, testCase.wantUnknown, testCase.wantOK)
		}
	}
}

func TestEBMLScalarsDecode(t *testing.T) {
	if value, ok := ebmlUint([]byte{0x01, 0x00}); !ok || value != 256 {
		t.Fatalf("ebmlUint = %d, %t", value, ok)
	}
	if value, ok := ebmlUint(nil); ok || value != 0 {
		t.Fatalf("empty ebmlUint = %d, %t", value, ok)
	}
	if value, ok := ebmlUint(make([]byte, 9)); ok || value != 0 {
		t.Fatalf("nine byte ebmlUint = %d, %t", value, ok)
	}
	if value, ok := ebmlFloat([]byte{0x40, 0x49, 0x0F, 0xDB}); !ok || math.Abs(value-3.1415927) > 1e-5 {
		t.Fatalf("float32 ebmlFloat = %v, %t", value, ok)
	}
	if value, ok := ebmlFloat([]byte{0x40, 0x09, 0x21, 0xFB, 0x54, 0x44, 0x2D, 0x18}); !ok || math.Abs(value-math.Pi) > 1e-12 {
		t.Fatalf("float64 ebmlFloat = %v, %t", value, ok)
	}
	if value, ok := ebmlFloat([]byte{0x01, 0x02, 0x03}); ok || value != 0 {
		t.Fatalf("three byte ebmlFloat = %v, %t", value, ok)
	}
}

// ebmlBytes renders one element header plus payload, which keeps the fixture
// builders below readable.
func ebmlBytes(id []byte, size int, payload []byte) []byte {
	header := append(append([]byte{}, id...), byte(0x80|size))
	return append(header, payload[:size]...)
}

func TestEBMLElementFromLocatesThePayload(t *testing.T) {
	header := []byte{0xA3, 0x84, 1, 2, 3, 4}
	element, err := ebmlElementFrom(header, 100)
	if err != nil {
		t.Fatal(err)
	}
	if element.ID != 0xA3 || element.Offset != 100 || element.HeaderLength != 2 {
		t.Fatalf("element = %+v", element)
	}
	if element.Size != 4 || element.DataStart != 102 || element.End() != 106 || element.Unknown {
		t.Fatalf("element = %+v", element)
	}
	if _, err := ebmlElementFrom([]byte{0xA3}, 0); err == nil {
		t.Fatal("a truncated header should fail")
	}
	// An unknown size is legal for a streamed segment, so it is reported as a
	// flag rather than as an error; fetchElement is what refuses to read it.
	unknown, err := ebmlElementFrom([]byte{0xA3, 0xFF}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !unknown.Unknown || unknown.HeaderLength != 2 || unknown.DataStart != 9 {
		t.Fatalf("unknown size element = %+v", unknown)
	}
}

func TestEBMLChildrenWalksNestedPayloads(t *testing.T) {
	inner := ebmlBytes([]byte{0xB3}, 2, []byte{0x00, 0x0A})
	outer := ebmlBytes([]byte{0xF7}, len(inner), inner)
	outer = append(outer, ebmlBytes([]byte{0xF1}, 3, []byte{0x01, 0x02, 0x03})...)
	seen := make(map[uint32]uint64)
	if err := ebmlChildren(outer, func(id uint32, payload []byte) error {
		if id == 0xF7 {
			// A master element is visited with its raw children so the caller
			// can decide whether another level is worth parsing.
			if err := ebmlChildren(payload, func(nested uint32, nestedPayload []byte) error {
				value, _ := ebmlUint(nestedPayload)
				seen[nested] = value
				return nil
			}); err != nil {
				return err
			}
			return nil
		}
		value, _ := ebmlUint(payload)
		seen[id] = value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen[0xB3] != 10 || seen[0xF1] != 0x010203 {
		t.Fatalf("visited elements = %v", seen)
	}
	// A child that claims more bytes than its parent holds must be rejected
	// instead of slicing past the end.
	if err := ebmlChildren([]byte{0xB3, 0x8A, 1, 2}, func(uint32, []byte) error { return nil }); err == nil {
		t.Fatal("an overrunning child should fail")
	}
}

func TestMatroskaCodecNameMapsContainerCodecIDs(t *testing.T) {
	cases := map[string]string{
		"V_MPEG4/ISO/AVC":  "h264",
		"v_mpeg4/iso/avc":  "h264",
		"V_MPEGH/ISO/HEVC": "hevc",
		"V_VP8":            "vp8",
		"V_VP9":            "vp9",
		"V_AV1":            "av1",
		"V_MPEG4/ISO/ASP":  "mpeg4",
		"V_MPEG2":          "mpeg2video",
		"A_VORBIS":         "",
		"V_UNSUPPORTED":    "",
	}
	for codecID, want := range cases {
		if got := matroskaCodecName(codecID); got != want {
			t.Fatalf("matroskaCodecName(%q) = %q, want %q", codecID, got, want)
		}
	}
}

func TestBuildContainerIndexDispatchesOnSniffedBytes(t *testing.T) {
	fixtureDir := t.TempDir()
	cases := []struct {
		name          string
		fileName      string
		args          []string
		wantContainer string
	}{
		{
			name:     "mp4",
			fileName: "dispatch.mp4",
			args: []string{
				"-f", "lavfi", "-i", "testsrc=size=64x64:rate=4",
				"-t", "1", "-an", "-c:v", "libx264", "-preset", "ultrafast", "-g", "4",
				"-movflags", "+faststart",
			},
			wantContainer: "mp4",
		},
		{
			name:     "matroska",
			fileName: "dispatch.mkv",
			args: []string{
				"-f", "lavfi", "-i", "testsrc=size=64x64:rate=4",
				"-t", "1", "-an", "-c:v", "libx264", "-preset", "ultrafast", "-g", "4",
			},
			wantContainer: "matroska",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			videoPath := filepath.Join(fixtureDir, testCase.fileName)
			createVideoFixture(t, videoPath, testCase.args...)
			fetcher, err := newFileRangeFetcher(videoPath)
			if err != nil {
				t.Fatal(err)
			}
			defer fetcher.Close()
			// The name is deliberately misleading: the container has to be
			// recognised from the payload, because a cloud drive file name is
			// user data.
			index, err := buildContainerIndex(context.Background(), fetcher, "wrong-extension.avi")
			if err != nil {
				t.Fatal(err)
			}
			if info := index.info(); info.FormatName != testCase.wantContainer {
				t.Fatalf("format = %q, want %q", info.FormatName, testCase.wantContainer)
			}
			if index.demuxer() != "h264" {
				t.Fatalf("demuxer = %q, want h264", index.demuxer())
			}
			summary := index.summary()
			if summary["container"] != testCase.wantContainer || summary["media_payload"] != "keyframe_only" {
				t.Fatalf("summary = %v", summary)
			}
		})
	}
}

func TestBuildContainerIndexRejectsUnsupportedPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "movie.avi")
	if err := os.WriteFile(path, []byte("RIFF....AVI LIST....hdrl"), 0o600); err != nil {
		t.Fatal(err)
	}
	fetcher, err := newFileRangeFetcher(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fetcher.Close()
	if _, err := buildContainerIndex(context.Background(), fetcher, "movie.avi"); err == nil {
		t.Fatal("an unsupported container must be rejected so FFmpeg can take over")
	}
	if _, err := buildContainerIndex(context.Background(), nil, "movie.mp4"); err == nil {
		t.Fatal("a nil fetcher must be rejected")
	}
}

func TestRemoteMatroskaIndexDownloadsOnlyMetadata(t *testing.T) {
	fixtureDir := t.TempDir()
	cases := []struct {
		name      string
		fileName  string
		args      []string
		wantCodec string
	}{
		{
			name:     "matroska h264",
			fileName: "index.mkv",
			args: []string{
				"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=15",
				"-t", "12", "-an", "-c:v", "libx264", "-preset", "ultrafast", "-b:v", "600k", "-g", "30",
			},
			wantCodec: "h264",
		},
		{
			name:     "webm vp9",
			fileName: "index.webm",
			args: []string{
				"-f", "lavfi", "-i", "testsrc=size=320x240:rate=15",
				"-t", "24", "-an", "-c:v", "libvpx-vp9", "-b:v", "400k", "-g", "30", "-row-mt", "1",
			},
			wantCodec: "vp9",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			videoPath := filepath.Join(fixtureDir, testCase.fileName)
			createVideoFixture(t, videoPath, testCase.args...)
			payload, err := os.ReadFile(videoPath)
			if err != nil {
				t.Fatal(err)
			}
			server := newRemoteFixtureServer(t, testCase.fileName, payload)
			reader := source.NewHTTPRangeReader(server.URL, int64(len(payload)), server.Client())
			reader.ChunkSize = 1024

			index, err := buildMatroskaIndex(ctx, reader)
			if err != nil {
				t.Fatal(err)
			}
			if len(index.Cues) == 0 {
				t.Fatal("Matroska index has no cues")
			}
			if index.Codec != testCase.wantCodec {
				t.Fatalf("codec = %q, want %q", index.Codec, testCase.wantCodec)
			}
			if info := index.info(); info.FormatName != "matroska" || info.Duration <= 0 || info.Width <= 0 {
				t.Fatalf("index info = %+v", info)
			}
			stats := reader.Stats()
			// Building the index reads SeekHead, Info, Tracks and Cues only.
			// Cue positions are resolved to byte ranges lazily, so a file with
			// thousands of cues still costs two requests: the head window and
			// the Cues element wherever the muxer put it.
			limit := int64(matroskaHeaderWindow) + matroskaSeekProbeBytes + index.CuesBytes + 4096
			if stats.BytesDownloaded > limit {
				t.Fatalf("index downloaded %d bytes, want at most %d (cues are %d bytes)",
					stats.BytesDownloaded, limit, index.CuesBytes)
			}
			if stats.Requests > 4 {
				t.Fatalf("index issued %d range requests, want at most 4", stats.Requests)
			}

			// Reading one keyframe may only add that keyframe: the cluster head
			// and the block header are probed with a few dozen bytes each.
			before := reader.Stats()
			sample, data, err := index.readKeyframe(ctx, index.Duration/2)
			if err != nil {
				t.Fatalf("read keyframe: %v", err)
			}
			if len(data) == 0 {
				t.Fatalf("keyframe %+v returned an empty payload", sample)
			}
			// readKeyframe hands back an elementary stream rather than the raw
			// block: the container framing becomes Annex-B start codes or an IVF
			// wrapper, plus the decoder configuration. That adds bytes, but only
			// a bounded number of them.
			if sample.Size <= 0 || int64(len(data)) > sample.Size+4096 {
				t.Fatalf("keyframe %+v became %d bytes", sample, len(data))
			}
			if sample.Offset+sample.Size > int64(len(payload)) {
				t.Fatalf("keyframe %+v runs past the %d byte file", sample, len(payload))
			}
			after := reader.Stats()
			keyframeRequests := after.Requests - before.Requests
			extra := after.BytesDownloaded - before.BytesDownloaded
			if extra > sample.Size+256 {
				t.Fatalf("reading a %d byte keyframe downloaded %d extra bytes", sample.Size, extra)
			}
			// A second read of the same timestamp must hit the cache.
			before = reader.Stats()
			if _, _, err := index.readKeyframe(ctx, sample.Timestamp); err != nil {
				t.Fatal(err)
			}
			if extra := reader.Stats().BytesDownloaded - before.BytesDownloaded; extra != 0 {
				t.Fatalf("repeated keyframe read downloaded %d bytes, want 0", extra)
			}
			// Indexing and keyframe reading are reported separately so the numbers
			// line up with the two rows they have in the traffic table of
			// docs/handoff.md; a single cumulative total would not be comparable.
			t.Logf("indexed %d cues of a %d byte file with %d range requests and %d bytes (%.2f%%); one %d byte keyframe then cost %d requests and %d bytes",
				len(index.Cues), len(payload), stats.Requests, stats.BytesDownloaded,
				100*float64(stats.BytesDownloaded)/float64(len(payload)), sample.Size, keyframeRequests, extra)
		})
	}
}

func TestExtractFrameFromIndexedMatroskaSample(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	fixtureDir := t.TempDir()
	cases := []struct {
		name        string
		fileName    string
		args        []string
		wantDemuxer string
	}{
		{
			name:     "h264 uses Annex-B",
			fileName: "sample.mkv",
			args: []string{
				"-f", "lavfi", "-i", "testsrc2=size=128x72:rate=8",
				"-t", "3", "-an", "-c:v", "libx264", "-preset", "ultrafast", "-g", "8",
			},
			wantDemuxer: "h264",
		},
		{
			name:     "vp9 is wrapped in IVF",
			fileName: "sample.webm",
			args: []string{
				"-f", "lavfi", "-i", "testsrc=size=128x72:rate=8",
				"-t", "3", "-an", "-c:v", "libvpx-vp9", "-b:v", "200k", "-g", "8", "-row-mt", "1",
			},
			wantDemuxer: "ivf",
		},
		{
			name:     "av1 is wrapped in IVF",
			fileName: "av1.mkv",
			args: []string{
				"-f", "lavfi", "-i", "testsrc=size=128x72:rate=8",
				"-t", "3", "-an", "-c:v", "libaom-av1", "-crf", "40", "-b:v", "0", "-cpu-used", "8", "-g", "8",
			},
			wantDemuxer: "ivf",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			videoPath := filepath.Join(fixtureDir, testCase.fileName)
			createVideoFixture(t, videoPath, testCase.args...)
			fetcher, err := newFileRangeFetcher(videoPath)
			if err != nil {
				t.Fatal(err)
			}
			defer fetcher.Close()
			index, err := buildMatroskaIndex(context.Background(), fetcher)
			if err != nil {
				t.Fatal(err)
			}
			if index.demuxer() != testCase.wantDemuxer {
				t.Fatalf("demuxer = %q, want %q", index.demuxer(), testCase.wantDemuxer)
			}
			processor := NewVideoProcessor(filepath.Join(fixtureDir, "frames"))
			processor.FFmpegPath = ffmpeg
			processor.HWAccel = "none"
			processor.FrameTimeout = 30 * time.Second
			for _, position := range []int{0, 1, len(index.Cues) - 1} {
				if position < 0 || position >= len(index.Cues) {
					continue
				}
				framePath := filepath.Join(fixtureDir, "frames", testCase.fileName+"-"+string(rune('a'+position))+".jpg")
				if err := os.MkdirAll(filepath.Dir(framePath), 0o755); err != nil {
					t.Fatal(err)
				}
				handled, sourceInfo, err := processor.extractFrameFromIndexedSampleWithSource(
					context.Background(), index, index.Cues[position].Timestamp, framePath, "yuv420p", 64)
				if !handled || err != nil {
					t.Fatalf("cue %d: handled %t, err %v", position, handled, err)
				}
				// Method is filled in by the caller; the source itself has to
				// name the exact container sample that produced the picture.
				if sourceInfo.Size <= 0 || sourceInfo.SampleDigest == "" {
					t.Fatalf("cue %d source = %+v", position, sourceInfo)
				}
				if sourceInfo.SampleTimestamp > index.Cues[position].Timestamp {
					t.Fatalf("cue %d used the sample at %v for the timestamp %v", position, sourceInfo.SampleTimestamp, index.Cues[position].Timestamp)
				}
				if info, err := os.Stat(framePath); err != nil || info.Size() == 0 {
					t.Fatalf("cue %d frame is unavailable: %v", position, err)
				}
			}
		})
	}
}

func TestMatroskaElementaryRepairsDroppedLengthPrefixByte(t *testing.T) {
	// Simulates the broken CMCT layout: a 4-byte AVCC block whose first NAL
	// length prefix lost one leading zero byte, so standard parsing reads an
	// absurd size while the remaining prefixes stay well-formed.
	sps := []byte{0x67, 0x64, 0x00, 0x29}
	pps := []byte{0x68, 0xe8, 0x01}
	idr := []byte{0x65, 0x01, 0x02, 0x03}
	block := []byte{}
	block = append(block, 0x00, 0x00, byte(len(sps))) // 3-byte (broken) prefix
	block = append(block, sps...)
	block = append(block, 0x00, 0x00, 0x00, byte(len(pps))) // standard 4-byte prefix
	block = append(block, pps...)
	block = append(block, 0x00, 0x00, 0x00, byte(len(idr))) // standard 4-byte prefix
	block = append(block, idr...)

	index := &matroskaIndex{Codec: "h264", CodecID: "V_MPEG4/ISO/AVC", NALLength: 4, Config: [][]byte{sps, pps}}
	got, err := index.elementary(block)
	if err != nil {
		t.Fatalf("elementary: %v", err)
	}
	// mp4SampleToAnnexB always prepends the configuration NALUs, so the
	// in-band SPS/PPS of the block appear twice; that is harmless for decode.
	want := []byte{}
	for _, nalu := range [][]byte{sps, pps, sps, pps, idr} {
		want = append(want, 0, 0, 0, 1)
		want = append(want, nalu...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("repaired sample = %x, want %x", got, want)
	}
}

func TestMatroskaClusterScanContinuesPastOneMiB(t *testing.T) {
	fillerSize := (1 << 20) + 128
	timestamp := []byte{0xE7, 0x81, 0x00}
	voidHeader := append([]byte{0xEC}, ebmlTestSize(fillerSize)...)
	voidElement := append(append([]byte{}, voidHeader...), make([]byte, fillerSize)...)
	blockBody := []byte{0x81, 0x00, 0x00, 0x80, 0x65}
	blockElement := append([]byte{0xA3}, ebmlTestSize(len(blockBody))...)
	blockElement = append(blockElement, blockBody...)
	clusterPayload := append(append(append([]byte{}, timestamp...), voidElement...), blockElement...)
	cluster := append([]byte{0x1F, 0x43, 0xB6, 0x75}, ebmlTestSize(len(clusterPayload))...)
	cluster = append(cluster, clusterPayload...)

	path := filepath.Join(t.TempDir(), "large-cluster.mkv")
	if err := os.WriteFile(path, cluster, 0o600); err != nil {
		t.Fatal(err)
	}
	fetcher, err := newFileRangeFetcher(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fetcher.Close()
	index := &matroskaIndex{Fetcher: fetcher, TrackNumber: 1}
	headLength := int64(4 + len(ebmlTestSize(len(clusterPayload))))
	head := matroskaClusterHead{
		offset:    0,
		dataStart: headLength,
		end:       int64(len(cluster)),
	}
	got, err := index.scanClusterKeyframe(context.Background(), head)
	if err != nil {
		t.Fatalf("scan cluster with a late keyframe: %v", err)
	}
	want := headLength + int64(len(timestamp)+len(voidElement))
	if got != want {
		t.Fatalf("keyframe element offset = %d, want %d", got, want)
	}
}

func ebmlTestSize(value int) []byte {
	if value < 0 {
		panic("negative EBML test size")
	}
	unsigned := uint64(value)
	for length := 1; length <= 8; length++ {
		max := (uint64(1) << uint(7*length)) - 2
		if unsigned > max {
			continue
		}
		encoded := make([]byte, length)
		remaining := unsigned
		for index := length - 1; index >= 0; index-- {
			encoded[index] = byte(remaining)
			remaining >>= 8
		}
		encoded[0] |= 1 << uint(8-length)
		return encoded
	}
	panic("EBML test size is too large")
}
