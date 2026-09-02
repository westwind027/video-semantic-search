package acquisition

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
)

// mp4Keyframe is the small part of an MP4 sample table that the acquisition
// pipeline needs. Offset and Size identify the encoded sample; Timestamp is
// the decode-time position used as a safe FFmpeg seek anchor.
type mp4Keyframe struct {
	SampleNumber uint32
	Timestamp    float64
	Offset       int64
	Size         int64
}

type mp4Index struct {
	Keyframes []mp4Keyframe
	Duration  float64
	Width     int
	Height    int
	Codec     string
	Reader    io.ReaderAt
	Config    [][]byte
	NALLength int
}

// buildMP4Index reads only the MP4 box metadata. Lazy mdat decoding makes the
// parser seek over media payloads without reading them, so a RangeReader only
// fetches ftyp/moov and the sample tables needed to build this index.
func buildMP4Index(reader io.ReadSeeker) (*mp4Index, error) {
	if reader == nil {
		return nil, fmt.Errorf("MP4 index reader is nil")
	}
	if prefetcher, ok := reader.(interface {
		SetPrefetchChunks(int)
		WaitPrefetch()
	}); ok {
		// moov/stbl is parsed sequentially, often after a large mdat seek.
		// Fetch a small look-ahead window concurrently, then wait before
		// random sample extraction so speculative requests cannot leak into
		// the next stage.
		prefetcher.SetPrefetchChunks(4)
		defer func() {
			prefetcher.SetPrefetchChunks(0)
			prefetcher.WaitPrefetch()
		}()
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind MP4 reader: %w", err)
	}
	file, err := mp4.DecodeFile(reader, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		return nil, fmt.Errorf("parse MP4 boxes: %w", err)
	}
	if file.Moov == nil {
		return nil, fmt.Errorf("MP4 moov box is missing")
	}
	readerAt, _ := reader.(io.ReaderAt)

	for _, track := range file.Moov.Traks {
		if track == nil || track.Tkhd == nil || track.Mdia == nil || track.Mdia.Hdlr == nil || track.Mdia.Hdlr.HandlerType != "vide" {
			continue
		}
		if track.Mdia.Mdhd == nil || track.Mdia.Mdhd.Timescale == 0 || track.Mdia.Minf == nil || track.Mdia.Minf.Stbl == nil {
			continue
		}
		stbl := track.Mdia.Minf.Stbl
		if stbl.Stts == nil || stbl.Stsc == nil || stbl.Stsz == nil || (stbl.Stco == nil && stbl.Co64 == nil) {
			continue
		}
		if file.Moov.Mvex != nil {
			return nil, fmt.Errorf("fragmented MP4 is not a progressive sample table")
		}

		samples := make([]uint32, 0)
		if stbl.Stss != nil {
			samples = append(samples, stbl.Stss.SampleNumber...)
		} else {
			for sample := uint32(1); sample <= stbl.Stsz.GetNrSamples(); sample++ {
				samples = append(samples, sample)
			}
		}
		if len(samples) == 0 {
			return nil, fmt.Errorf("video track has no samples")
		}

		result := &mp4Index{
			Keyframes: make([]mp4Keyframe, 0, len(samples)),
			Duration:  float64(track.Mdia.Mdhd.Duration) / float64(track.Mdia.Mdhd.Timescale),
			Width:     int(track.Tkhd.Width >> 16),
			Height:    int(track.Tkhd.Height >> 16),
			Codec:     mp4CodecName(stbl.Stsd),
			Reader:    readerAt,
			NALLength: 4,
		}
		result.Config = mp4CodecConfig(stbl.Stsd, &result.NALLength)
		if result.Duration <= 0 && file.Moov.Mvhd != nil && file.Moov.Mvhd.Timescale > 0 {
			result.Duration = float64(file.Moov.Mvhd.Duration) / float64(file.Moov.Mvhd.Timescale)
		}
		for _, sampleNumber := range samples {
			if sampleNumber == 0 || sampleNumber > stbl.Stsz.GetNrSamples() {
				continue
			}
			ranges, err := track.GetRangesForSampleInterval(sampleNumber, sampleNumber)
			if err != nil {
				return nil, fmt.Errorf("read MP4 sample %d range: %w", sampleNumber, err)
			}
			if len(ranges) != 1 {
				return nil, fmt.Errorf("read MP4 sample %d range: expected one range, got %d", sampleNumber, len(ranges))
			}
			// GetTimeCode accumulates into a uint32 inside mp4ff. That wraps on
			// long videos whose media timescale is high (this is common for
			// remote Blu-ray encodes), which then makes the sorted keyframe list
			// point many later timestamps at the same sample. GetDecodeTime uses
			// uint64 and preserves the complete timeline.
			decodeTime, _ := stbl.Stts.GetDecodeTime(sampleNumber)
			timestamp := float64(decodeTime) / float64(track.Mdia.Mdhd.Timescale)
			result.Keyframes = append(result.Keyframes, mp4Keyframe{
				SampleNumber: sampleNumber,
				Timestamp:    timestamp,
				Offset:       int64(ranges[0].Offset),
				Size:         int64(stbl.Stsz.GetSampleSize(int(sampleNumber))),
			})
		}
		if len(result.Keyframes) == 0 {
			return nil, fmt.Errorf("video track has no usable keyframe ranges")
		}
		sort.Slice(result.Keyframes, func(left, right int) bool {
			if result.Keyframes[left].Timestamp == result.Keyframes[right].Timestamp {
				return result.Keyframes[left].SampleNumber < result.Keyframes[right].SampleNumber
			}
			return result.Keyframes[left].Timestamp < result.Keyframes[right].Timestamp
		})
		return result, nil
	}
	return nil, fmt.Errorf("MP4 has no usable video track")
}

func (i *mp4Index) videoInfo() videoInfo {
	if i == nil {
		return videoInfo{}
	}
	return videoInfo{
		Duration:   i.Duration,
		FormatName: "mp4",
		Width:      i.Width,
		Height:     i.Height,
		Codec:      i.Codec,
	}
}

func (i *mp4Index) anchor(timestamp float64) float64 {
	if i == nil || len(i.Keyframes) == 0 || timestamp <= i.Keyframes[0].Timestamp {
		return 0
	}
	position := sort.Search(len(i.Keyframes), func(index int) bool {
		return i.Keyframes[index].Timestamp > timestamp
	})
	if position == 0 {
		return i.Keyframes[0].Timestamp
	}
	return i.Keyframes[position-1].Timestamp
}

func (i *mp4Index) keyframeAtOrBefore(timestamp float64) (mp4Keyframe, bool) {
	if i == nil || len(i.Keyframes) == 0 {
		return mp4Keyframe{}, false
	}
	position := sort.Search(len(i.Keyframes), func(index int) bool {
		return i.Keyframes[index].Timestamp > timestamp
	})
	if position == 0 {
		return i.Keyframes[0], true
	}
	return i.Keyframes[position-1], true
}

func (i *mp4Index) readKeyframe(ctx context.Context, timestamp float64) (mp4Keyframe, []byte, error) {
	if i == nil || i.Reader == nil {
		return mp4Keyframe{}, nil, fmt.Errorf("MP4 sample reader is unavailable")
	}
	if len(i.Config) == 0 || (i.Codec != "h264" && i.Codec != "hevc") {
		return mp4Keyframe{}, nil, fmt.Errorf("MP4 codec configuration is unavailable for %s", i.Codec)
	}
	sample, ok := i.keyframeAtOrBefore(timestamp)
	if !ok || sample.Offset < 0 || sample.Size <= 0 {
		return mp4Keyframe{}, nil, fmt.Errorf("MP4 keyframe sample is unavailable")
	}
	const maxKeyframeSize = 64 << 20
	if sample.Size > maxKeyframeSize {
		return mp4Keyframe{}, nil, fmt.Errorf("MP4 keyframe sample is too large: %d bytes", sample.Size)
	}
	data := make([]byte, sample.Size)
	if _, err := readAtContext(ctx, i.Reader, data, sample.Offset); err != nil {
		return mp4Keyframe{}, nil, fmt.Errorf("read MP4 keyframe sample at %d: %w", sample.Offset, err)
	}
	converted, err := mp4SampleToAnnexB(data, i.Config, i.NALLength)
	if err != nil {
		return mp4Keyframe{}, nil, fmt.Errorf("convert MP4 %s sample: %w", i.Codec, err)
	}
	return sample, converted, nil
}

func mp4SampleDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:8])
}

type contextReaderAt interface {
	ReadAtContext(context.Context, []byte, int64) (int, error)
}

func readAtContext(ctx context.Context, reader io.ReaderAt, data []byte, offset int64) (int, error) {
	if contextual, ok := reader.(contextReaderAt); ok {
		return contextual.ReadAtContext(ctx, data, offset)
	}
	return reader.ReadAt(data, offset)
}

func mp4CodecConfig(stsd *mp4.StsdBox, nalLength *int) [][]byte {
	if stsd == nil || len(stsd.Children) == 0 {
		return nil
	}
	entry, ok := stsd.Children[0].(*mp4.VisualSampleEntryBox)
	if !ok {
		return nil
	}
	if entry.HvcC != nil {
		if nalLength != nil {
			*nalLength = int(entry.HvcC.LengthSizeMinusOne) + 1
		}
		config := make([][]byte, 0)
		for _, array := range entry.HvcC.NaluArrays {
			for _, nalu := range array.Nalus {
				config = append(config, append([]byte(nil), nalu...))
			}
		}
		return config
	}
	if entry.AvcC != nil {
		config := make([][]byte, 0, len(entry.AvcC.SPSnalus)+len(entry.AvcC.PPSnalus))
		for _, nalu := range entry.AvcC.SPSnalus {
			config = append(config, append([]byte(nil), nalu...))
		}
		for _, nalu := range entry.AvcC.PPSnalus {
			config = append(config, append([]byte(nil), nalu...))
		}
		return config
	}
	return nil
}

func mp4SampleToAnnexB(sample []byte, config [][]byte, nalLength int) ([]byte, error) {
	if nalLength < 1 || nalLength > 4 {
		return nil, fmt.Errorf("unsupported NAL length size %d", nalLength)
	}
	capacity := len(sample) + len(config)*4
	for _, nalu := range config {
		capacity += len(nalu)
	}
	output := bytes.NewBuffer(make([]byte, 0, capacity))
	startCode := []byte{0, 0, 0, 1}
	for _, nalu := range config {
		if len(nalu) == 0 {
			continue
		}
		_, _ = output.Write(startCode)
		_, _ = output.Write(nalu)
	}
	for offset := 0; offset < len(sample); {
		if len(sample)-offset < nalLength {
			return nil, fmt.Errorf("truncated NAL length at byte %d", offset)
		}
		var size uint32
		for index := 0; index < nalLength; index++ {
			size = size<<8 | uint32(sample[offset+index])
		}
		offset += nalLength
		if size == 0 {
			continue
		}
		if uint64(size) > uint64(len(sample)-offset) {
			return nil, fmt.Errorf("NAL size %d exceeds sample remainder %d", size, len(sample)-offset)
		}
		_, _ = output.Write(startCode)
		_, _ = output.Write(sample[offset : offset+int(size)])
		offset += int(size)
	}
	return output.Bytes(), nil
}

func isMP4Format(name, format string) bool {
	for _, value := range strings.Split(strings.ToLower(format), ",") {
		switch strings.TrimSpace(value) {
		case "mov", "mp4", "m4a", "3gp", "3g2", "mj2", "ismv", "isma", "f4v":
			return true
		}
	}
	name = strings.ToLower(strings.TrimSpace(name))
	for _, suffix := range []string{".mp4", ".mov", ".m4v", ".3gp", ".3g2", ".m4a", ".mj2"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func (i *mp4Index) summary() map[string]any {
	if i == nil {
		return nil
	}
	var firstOffset, lastOffset int64
	if len(i.Keyframes) > 0 {
		firstOffset = i.Keyframes[0].Offset
		last := i.Keyframes[len(i.Keyframes)-1]
		lastOffset = last.Offset + last.Size
	}
	return map[string]any{
		"keyframes":     len(i.Keyframes),
		"duration":      i.Duration,
		"width":         i.Width,
		"height":        i.Height,
		"codec":         i.Codec,
		"first_offset":  firstOffset,
		"last_end":      lastOffset,
		"indexed_at":    time.Now().UTC().Format(time.RFC3339),
		"media_payload": "lazy",
	}
}

func mp4CodecName(stsd *mp4.StsdBox) string {
	if stsd == nil || len(stsd.Children) == 0 {
		return ""
	}
	switch stsd.Children[0].Type() {
	case "avc1", "avc3":
		return "h264"
	case "hvc1", "hev1", "dvh1", "dvhe":
		return "hevc"
	case "av01":
		return "av1"
	case "vp08":
		return "vp8"
	case "vp09":
		return "vp9"
	case "mp4v":
		return "mpeg4"
	case "mjpg", "jpeg":
		return "mjpeg"
	default:
		return stsd.Children[0].Type()
	}
}
