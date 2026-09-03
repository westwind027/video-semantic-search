package acquisition

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

// rangeFetcher is the byte-access contract every container indexer uses.
// *source.RangeReader satisfies it for cloud drive files and fileRangeFetcher
// adapts a local file, so the indexers depend on neither HTTP nor FFmpeg and
// can be exercised against a small in-memory payload.
type rangeFetcher interface {
	// FetchRange returns exactly the bytes of [offset, offset+length), clamped
	// to the file end. Implementations must not widen the request.
	FetchRange(ctx context.Context, offset, length int64) ([]byte, error)
	Size() int64
}

type containerIndexOptions struct {
	MP4MoovChunkSize int64
	MP4MoovWorkers   int
}

// keyframeSample is one encoded picture located in the container. Offset and
// Size are the exact bytes that have to be downloaded to decode it; Timestamp
// is its position on the container timeline and is used as a safe FFmpeg seek
// anchor when direct decoding fails.
type keyframeSample struct {
	Index     uint64
	Timestamp float64
	Offset    int64
	Size      int64
}

// containerIndex answers "give me a decodable picture at or before t" using
// only container metadata plus that single picture. Media data around the
// keyframe is never downloaded, which is the whole point of indexing a remote
// file instead of streaming it through FFmpeg.
type containerIndex interface {
	info() videoInfo
	anchor(timestamp float64) float64
	readKeyframe(ctx context.Context, timestamp float64) (keyframeSample, []byte, error)
	// demuxer names the ffmpeg -f input format of the payload returned by
	// readKeyframe: h264 and hevc for Annex-B streams, ivf for VP8/VP9/AV1.
	demuxer() string
	summary() map[string]any
}

const (
	// containerProbeBytes covers the magic of every supported container plus
	// the first top-level element header.
	containerProbeBytes = 32
	// maxKeyframeBytes rejects absurd sample sizes coming from a damaged index
	// before any allocation happens.
	maxKeyframeBytes = 64 << 20
	// maxMetadataBytes bounds one container metadata structure (MP4 moov,
	// Matroska Cues). A movie never needs more than this to build an index and
	// the limit turns a corrupt length field into an error instead of an
	// out-of-memory condition.
	maxMetadataBytes = 256 << 20
)

// matroskaMagic is the EBML header element ID that starts every Matroska and
// WebM file.
var matroskaMagic = []byte{0x1A, 0x45, 0xDF, 0xA3}

// sniffContainer identifies a container from its first bytes. A cloud drive
// file name is user data: extensions are missing, wrong, or changed by a
// rename, so the decision has to come from the payload itself.
func sniffContainer(header []byte) string {
	if len(header) >= 8 && string(header[4:8]) == "ftyp" {
		return "mp4"
	}
	if bytes.HasPrefix(header, matroskaMagic) {
		return "matroska"
	}
	return ""
}

// buildContainerIndex parses whichever supported container the payload starts
// with. It fails for unsupported, fragmented, or index-less containers so the
// caller can fall back to the generic FFmpeg range proxy.
func buildContainerIndex(ctx context.Context, fetcher rangeFetcher, name string) (containerIndex, error) {
	return buildContainerIndexWithOptions(ctx, fetcher, name, containerIndexOptions{})
}

func buildContainerIndexWithOptions(ctx context.Context, fetcher rangeFetcher, name string, options containerIndexOptions) (containerIndex, error) {
	if fetcher == nil {
		return nil, fmt.Errorf("container index fetcher is nil")
	}
	size := fetcher.Size()
	if size <= 0 {
		return nil, fmt.Errorf("container size is unavailable")
	}
	probe := int64(containerProbeBytes)
	if probe > size {
		probe = size
	}
	header, err := fetcher.FetchRange(ctx, 0, probe)
	if err != nil {
		return nil, fmt.Errorf("probe container header: %w", err)
	}
	switch format := sniffContainer(header); format {
	case "mp4":
		return buildMP4IndexWithOptions(ctx, fetcher, mp4MoovFetchOptions{
			ChunkSize: options.MP4MoovChunkSize,
			Workers:   options.MP4MoovWorkers,
		})
	case "matroska":
		return buildMatroskaIndex(ctx, fetcher)
	default:
		return nil, fmt.Errorf("container of %s is not indexed by byte sniffing", firstNonEmpty(name, "the remote file"))
	}
}

// anchorFor returns the timestamp of the last keyframe at or before want. It is
// the position FFmpeg can seek to without decoding from the previous cut.
func anchorFor(keyframes []keyframeSample, want float64) float64 {
	if len(keyframes) == 0 || want <= keyframes[0].Timestamp {
		return 0
	}
	position := sort.Search(len(keyframes), func(index int) bool {
		return keyframes[index].Timestamp > want
	})
	if position == 0 {
		return keyframes[0].Timestamp
	}
	return keyframes[position-1].Timestamp
}

// keyframeAtOrBefore returns the keyframe that has to be decoded to render the
// picture shown at want.
func keyframeAtOrBefore(keyframes []keyframeSample, want float64) (keyframeSample, bool) {
	if len(keyframes) == 0 {
		return keyframeSample{}, false
	}
	position := sort.Search(len(keyframes), func(index int) bool {
		return keyframes[index].Timestamp > want
	})
	if position == 0 {
		return keyframes[0], true
	}
	return keyframes[position-1], true
}

// sampleDigest fingerprints the encoded bytes of one keyframe. Acquisition
// stores it so a repeated frame caused by a wrong index entry is visible in
// the metadata instead of silently producing identical pictures.
func sampleDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:8])
}

// metadataSummary is the part of an index summary that is identical for every
// container, so format-specific indexers only add their own fields.
func metadataSummary(format string, info videoInfo, keyframes []keyframeSample) map[string]any {
	var firstOffset, lastEnd int64
	if len(keyframes) > 0 {
		firstOffset = keyframes[0].Offset
		last := keyframes[len(keyframes)-1]
		lastEnd = last.Offset + last.Size
	}
	return map[string]any{
		"container":     format,
		"keyframes":     len(keyframes),
		"duration":      info.Duration,
		"width":         info.Width,
		"height":        info.Height,
		"codec":         info.Codec,
		"first_offset":  firstOffset,
		"last_end":      lastEnd,
		"indexed_at":    time.Now().UTC().Format(time.RFC3339),
		"media_payload": "keyframe_only",
	}
}

// fileRangeFetcher adapts a local file to rangeFetcher. Local acquisition keeps
// using FFmpeg's exact seeking, so this adapter exists for tests and for
// debugging an indexer against a downloaded sample.
type fileRangeFetcher struct {
	handle *os.File
	size   int64
}

func newFileRangeFetcher(path string) (*fileRangeFetcher, error) {
	handle, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	stat, err := handle.Stat()
	if err != nil {
		_ = handle.Close()
		return nil, err
	}
	return &fileRangeFetcher{handle: handle, size: stat.Size()}, nil
}

func (f *fileRangeFetcher) Size() int64 { return f.size }

func (f *fileRangeFetcher) Close() error { return f.handle.Close() }

func (f *fileRangeFetcher) FetchRange(ctx context.Context, offset, length int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, fmt.Errorf("negative read offset %d", offset)
	}
	if length <= 0 {
		return nil, fmt.Errorf("non-positive range length %d", length)
	}
	if offset >= f.size {
		return nil, io.EOF
	}
	if length > f.size-offset {
		length = f.size - offset
	}
	data := make([]byte, length)
	if _, err := f.handle.ReadAt(data, offset); err != nil && err != io.EOF {
		return nil, err
	}
	return data, nil
}
