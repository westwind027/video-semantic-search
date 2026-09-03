package acquisition

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/ivf"
)

// Matroska and WebM element IDs. Only the ones needed to turn container
// metadata into a keyframe index are listed; everything else is skipped by its
// declared size.
const (
	ebmlHeaderID           = 0x1A45DFA3
	segmentID              = 0x18538067
	voidID                 = 0xEC
	seekHeadID             = 0x114D9B74
	seekID                 = 0x4DBB
	seekElementIDID        = 0x53AB
	seekPositionID         = 0x53AC
	infoID                 = 0x1549A966
	timestampScaleID       = 0x2AD7B1
	infoDurationID         = 0x4489
	tracksID               = 0x1654AE6B
	trackEntryID           = 0xAE
	trackNumberID          = 0xD7
	trackTypeID            = 0x83
	trackFlagDefaultID     = 0x88
	trackCodecIDID         = 0x86
	trackCodecPrivateID    = 0x63A2
	trackDefaultDurationID = 0x23E383
	trackVideoID           = 0xE0
	videoPixelWidthID      = 0xB0
	videoPixelHeightID     = 0xBA
	cuesID                 = 0x1C53BB6B
	cuePointID             = 0xBB
	cueTimeID              = 0xB3
	cueTrackPositionsID    = 0xB7
	cueTrackID             = 0xF7
	cueClusterPositionID   = 0xF1
	cueRelativePositionID  = 0xF0
	clusterID              = 0x1F43B675
	clusterTimestampID     = 0xE7
	blockGroupID           = 0xA0
	blockID                = 0xA1
	blockReferenceID       = 0xFB
	simpleBlockID          = 0xA3
	attachmentsID          = 0x1941A469
)

const (
	// ebmlHeaderMaxBytes is the longest element header: a four-byte ID plus an
	// eight-byte data size.
	ebmlHeaderMaxBytes = 12
	// matroskaHeaderWindow is one request that normally covers SeekHead, Info,
	// Tracks and the Cues element of a muxer-optimised file, so building the
	// index costs a handful of requests instead of one per element.
	matroskaHeaderWindow = 64 << 10
	// matroskaMetadataBytes bounds one fetched metadata element. Cues of a long
	// movie stay far below this, and the limit turns a corrupt size field into
	// an error instead of an allocation.
	matroskaMetadataBytes = 64 << 20
	// matroskaClusterProbeBytes covers a cluster header, its timestamp element
	// and normally the header of the first block. Reading them together is what
	// keeps a cued keyframe at two round trips instead of three: the block
	// offset is known from the cue, but its length is not.
	matroskaClusterProbeBytes = 128
	// matroskaBlockProbeBytes covers a block element header plus the block's
	// own header (track number, signed 16-bit timecode, flags).
	matroskaBlockProbeBytes = 32
	// matroskaSeekProbeBytes resolves one SeekHead entry. Only the element
	// header is needed, but a slightly wider read lets a small Cues element
	// arrive in the same request as its own header.
	matroskaSeekProbeBytes = 4 << 10
	// matroskaClusterScanBytes bounds the search for a keyframe inside a cluster
	// that did not record a relative block position.
	matroskaClusterScanBytes = 1 << 20
	// matroskaScannerHops bounds how many range requests the index build may
	// issue, so a damaged file cannot turn into an endless walk.
	matroskaScannerHops = 64
)

// matroskaCue is one entry of the Cues element: the position of a keyframe on
// the timeline plus enough information to find its bytes without reading the
// media data in between.
type matroskaCue struct {
	Timestamp float64
	Time      uint64
	Track     uint64
	// Cluster is the cluster offset relative to the segment data start.
	Cluster int64
	// Relative is the block offset inside the cluster data, or -1 when the
	// muxer omitted it.
	Relative int64
}

type matroskaClusterHead struct {
	offset    int64
	dataStart int64
	end       int64
	timestamp uint64
}

type matroskaIndex struct {
	Cues            []matroskaCue
	SegmentData     int64
	Duration        float64
	Width           int
	Height          int
	Codec           string
	CodecID         string
	TrackNumber     uint64
	TimestampScale  uint64
	DefaultDuration uint64
	Config          [][]byte
	NALLength       int
	AV1Header       []byte
	Fetcher         rangeFetcher
	CuesBytes       int64

	mu       sync.Mutex
	clusters map[int64]matroskaClusterHead
}

// buildMatroskaIndex reads only Matroska/WebM metadata. SeekHead gives the
// exact position of Info, Tracks and Cues, so the index is built from three
// small requests no matter how large the media data is, and every keyframe byte
// range is resolved lazily at extraction time.
func buildMatroskaIndex(ctx context.Context, fetcher rangeFetcher) (*matroskaIndex, error) {
	if fetcher == nil {
		return nil, fmt.Errorf("Matroska index fetcher is nil")
	}
	size := fetcher.Size()
	if size <= 0 {
		return nil, fmt.Errorf("Matroska size is unavailable")
	}
	scanner := newEBMLScanner(fetcher, matroskaHeaderWindow, matroskaScannerHops)
	header, err := scanner.elementHeader(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("read EBML header: %w", err)
	}
	if header.ID != ebmlHeaderID {
		return nil, fmt.Errorf("file does not start with an EBML header")
	}
	if header.Unknown {
		return nil, fmt.Errorf("EBML header has an unknown size")
	}
	segment, err := scanner.elementHeader(ctx, header.End())
	if err != nil {
		return nil, fmt.Errorf("read EBML segment: %w", err)
	}
	if segment.ID != segmentID {
		return nil, fmt.Errorf("EBML segment is missing")
	}
	segmentEnd := size
	if !segment.Unknown && segment.End() < segmentEnd {
		segmentEnd = segment.End()
	}

	index := &matroskaIndex{
		SegmentData:    segment.DataStart,
		TimestampScale: 1000000,
		Fetcher:        fetcher,
		NALLength:      4,
		clusters:       make(map[int64]matroskaClusterHead),
	}
	elements, err := scanSegmentHead(ctx, scanner, segment, segmentEnd)
	if err != nil {
		return nil, err
	}
	if err := index.applySeekHead(ctx, fetcher, &elements); err != nil {
		return nil, err
	}
	if err := index.loadInfo(ctx, fetcher, elements); err != nil {
		return nil, err
	}
	if err := index.loadTracks(ctx, fetcher, elements); err != nil {
		return nil, err
	}
	if err := index.loadCues(ctx, fetcher, elements); err != nil {
		return nil, err
	}
	return index, nil
}

// segmentElements records where the interesting level-1 elements live. Positions
// stay relative to the segment data start, exactly as SeekHead stores them.
type segmentElements struct {
	seekHead ebmlElement
	info     ebmlElement
	tracks   ebmlElement
	cues     ebmlElement
}

// scanSegmentHead walks the level-1 children of the segment and stops at the
// first element that carries media data. Only element headers are inspected, so
// skipping a cluster costs nothing; the metadata elements that follow it are
// found through SeekHead instead.
func scanSegmentHead(ctx context.Context, scanner *ebmlScanner, segment ebmlElement, segmentEnd int64) (segmentElements, error) {
	var elements segmentElements
	for position := segment.DataStart; position < segmentEnd; {
		element, err := scanner.elementHeader(ctx, position)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return elements, fmt.Errorf("read Matroska element at %d: %w", position, err)
		}
		switch element.ID {
		case seekHeadID:
			elements.seekHead = element
		case infoID:
			elements.info = element
		case tracksID:
			elements.tracks = element
		case cuesID:
			elements.cues = element
			return elements, nil
		case clusterID, attachmentsID:
			// Everything after this point is media data or unrelated payload.
			return elements, nil
		}
		if element.Unknown {
			return elements, nil
		}
		if element.Size < 0 {
			return elements, fmt.Errorf("Matroska element 0x%X at %d has an invalid size", element.ID, position)
		}
		position = element.End()
	}
	return elements, nil
}

// applySeekHead fills the gaps of the head scan with the positions the muxer
// wrote down. This is what makes a file whose Cues sit behind the media data
// just as cheap to index as a faststart one.
func (i *matroskaIndex) applySeekHead(ctx context.Context, fetcher rangeFetcher, elements *segmentElements) error {
	if !elements.seekHead.found() {
		return nil
	}
	payload, err := fetchElement(ctx, fetcher, elements.seekHead, "Matroska SeekHead")
	if err != nil {
		return err
	}
	positions := make(map[uint32]int64)
	if err := ebmlChildren(payload, func(id uint32, entry []byte) error {
		if id != seekID {
			return nil
		}
		var seekIDValue uint32
		var seekPosition int64 = -1
		if err := ebmlChildren(entry, func(childID uint32, child []byte) error {
			switch childID {
			case seekElementIDID:
				seekIDValue = ebmlIDValue(child)
			case seekPositionID:
				if value, ok := ebmlUint(child); ok {
					seekPosition = int64(value)
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if seekIDValue != 0 && seekPosition >= 0 {
			positions[seekIDValue] = seekPosition
		}
		return nil
	}); err != nil {
		return fmt.Errorf("parse Matroska SeekHead: %w", err)
	}
	// One scanner for every SeekHead entry, so metadata elements that sit next
	// to each other are read with a single request.
	scanner := newEBMLScanner(fetcher, matroskaSeekProbeBytes, 8)
	for id, position := range positions {
		// Only the metadata acquisition needs is resolved. SeekHead also lists
		// chapters, tags and attachments, and reading a header for each of them
		// would turn indexing into a tour of the whole file.
		switch id {
		case infoID:
			if elements.info.found() {
				continue
			}
		case tracksID:
			if elements.tracks.found() {
				continue
			}
		case cuesID:
			if elements.cues.found() {
				continue
			}
		default:
			continue
		}
		offset := i.SegmentData + position
		if offset < i.SegmentData || offset >= fetcher.Size() {
			continue
		}
		element, err := scanner.elementHeader(ctx, offset)
		if err != nil || element.ID != id {
			continue
		}
		switch id {
		case infoID:
			elements.info = element
		case tracksID:
			elements.tracks = element
		case cuesID:
			elements.cues = element
		}
	}
	return nil
}

func (i *matroskaIndex) loadInfo(ctx context.Context, fetcher rangeFetcher, elements segmentElements) error {
	if !elements.info.found() {
		return fmt.Errorf("Matroska Info element is missing")
	}
	payload, err := fetchElement(ctx, fetcher, elements.info, "Matroska Info")
	if err != nil {
		return err
	}
	var duration float64
	if err := ebmlChildren(payload, func(id uint32, child []byte) error {
		switch id {
		case timestampScaleID:
			if value, ok := ebmlUint(child); ok && value > 0 {
				i.TimestampScale = value
			}
		case infoDurationID:
			if value, ok := ebmlFloat(child); ok && value > 0 {
				duration = value
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("parse Matroska Info: %w", err)
	}
	i.Duration = duration * float64(i.TimestampScale) / float64(time.Second)
	return nil
}

func (i *matroskaIndex) loadTracks(ctx context.Context, fetcher rangeFetcher, elements segmentElements) error {
	if !elements.tracks.found() {
		return fmt.Errorf("Matroska Tracks element is missing")
	}
	payload, err := fetchElement(ctx, fetcher, elements.tracks, "Matroska Tracks")
	if err != nil {
		return err
	}
	tracks, err := parseMatroskaTracks(payload)
	if err != nil {
		return err
	}
	if len(tracks) == 0 {
		return fmt.Errorf("Matroska has no usable video track")
	}
	selected := tracks[0]
	for _, track := range tracks {
		if track.FlagDefault {
			selected = track
			break
		}
	}
	codec := matroskaCodecName(selected.CodecID)
	if codec == "" {
		return fmt.Errorf("Matroska codec %s is not supported for direct decoding", selected.CodecID)
	}
	config, nalLength, av1Header := matroskaCodecConfig(codec, selected.CodecPrivate)
	i.TrackNumber = selected.Number
	i.CodecID = selected.CodecID
	i.Codec = codec
	i.Width = selected.Width
	i.Height = selected.Height
	i.DefaultDuration = selected.DefaultDuration
	i.Config = config
	i.NALLength = nalLength
	i.AV1Header = av1Header
	return nil
}

func (i *matroskaIndex) loadCues(ctx context.Context, fetcher rangeFetcher, elements segmentElements) error {
	if !elements.cues.found() {
		return fmt.Errorf("Matroska Cues element is missing, so keyframes cannot be located without scanning the media data")
	}
	if elements.cues.Size > matroskaMetadataBytes {
		return fmt.Errorf("Matroska Cues element is too large: %d bytes", elements.cues.Size)
	}
	payload, err := fetchElement(ctx, fetcher, elements.cues, "Matroska Cues")
	if err != nil {
		return err
	}
	i.CuesBytes = int64(len(payload))
	cues, err := parseMatroskaCues(payload, i.TimestampScale)
	if err != nil {
		return err
	}
	if len(cues) == 0 {
		return fmt.Errorf("Matroska Cues element has no cue points")
	}
	if i.TrackNumber != 0 {
		video := make([]matroskaCue, 0, len(cues))
		for _, cue := range cues {
			if cue.Track == i.TrackNumber {
				video = append(video, cue)
			}
		}
		if len(video) > 0 {
			cues = video
		}
	}
	sort.Slice(cues, func(left, right int) bool {
		if cues[left].Time == cues[right].Time {
			return cues[left].Cluster < cues[right].Cluster
		}
		return cues[left].Time < cues[right].Time
	})
	// Muxers may repeat a position for several tracks; keep the first one so a
	// timestamp never resolves to two different blocks.
	deduplicated := make([]matroskaCue, 0, len(cues))
	for _, cue := range cues {
		if len(deduplicated) > 0 {
			last := deduplicated[len(deduplicated)-1]
			if last.Time == cue.Time && last.Cluster == cue.Cluster && last.Relative == cue.Relative {
				continue
			}
		}
		deduplicated = append(deduplicated, cue)
	}
	i.Cues = deduplicated
	return nil
}

type matroskaTrack struct {
	Number          uint64
	CodecID         string
	CodecPrivate    []byte
	Width           int
	Height          int
	DefaultDuration uint64
	FlagDefault     bool
}

func parseMatroskaTracks(data []byte) ([]matroskaTrack, error) {
	tracks := make([]matroskaTrack, 0, 2)
	err := ebmlChildren(data, func(id uint32, entry []byte) error {
		if id != trackEntryID {
			return nil
		}
		track := matroskaTrack{FlagDefault: true}
		trackType := uint64(0)
		if err := ebmlChildren(entry, func(childID uint32, child []byte) error {
			switch childID {
			case trackNumberID:
				track.Number, _ = ebmlUint(child)
			case trackTypeID:
				trackType, _ = ebmlUint(child)
			case trackCodecIDID:
				track.CodecID = strings.TrimSpace(string(child))
			case trackCodecPrivateID:
				track.CodecPrivate = append([]byte(nil), child...)
			case trackDefaultDurationID:
				track.DefaultDuration, _ = ebmlUint(child)
			case trackFlagDefaultID:
				if value, ok := ebmlUint(child); ok {
					track.FlagDefault = value != 0
				}
			case trackVideoID:
				return ebmlChildren(child, func(videoID uint32, videoChild []byte) error {
					switch videoID {
					case videoPixelWidthID:
						if value, ok := ebmlUint(videoChild); ok {
							track.Width = int(value)
						}
					case videoPixelHeightID:
						if value, ok := ebmlUint(videoChild); ok {
							track.Height = int(value)
						}
					}
					return nil
				})
			}
			return nil
		}); err != nil {
			return err
		}
		// Track type 1 is video. Audio and subtitle tracks are never indexed.
		if trackType == 1 && track.Number > 0 {
			tracks = append(tracks, track)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("parse Matroska Tracks: %w", err)
	}
	return tracks, nil
}

func parseMatroskaCues(data []byte, timestampScale uint64) ([]matroskaCue, error) {
	cues := make([]matroskaCue, 0, 256)
	scale := float64(timestampScale) / float64(time.Second)
	err := ebmlChildren(data, func(id uint32, point []byte) error {
		if id != cuePointID {
			return nil
		}
		var cueTime uint64
		var haveTime bool
		positions := make([]matroskaCue, 0, 2)
		if err := ebmlChildren(point, func(childID uint32, child []byte) error {
			switch childID {
			case cueTimeID:
				if value, ok := ebmlUint(child); ok {
					cueTime, haveTime = value, true
				}
			case cueTrackPositionsID:
				cue := matroskaCue{Relative: -1}
				if err := ebmlChildren(child, func(positionID uint32, positionChild []byte) error {
					switch positionID {
					case cueTrackID:
						cue.Track, _ = ebmlUint(positionChild)
					case cueClusterPositionID:
						if value, ok := ebmlUint(positionChild); ok {
							cue.Cluster = int64(value)
						}
					case cueRelativePositionID:
						if value, ok := ebmlUint(positionChild); ok {
							cue.Relative = int64(value)
						}
					}
					return nil
				}); err != nil {
					return err
				}
				if cue.Cluster >= 0 {
					positions = append(positions, cue)
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if !haveTime {
			return nil
		}
		for _, cue := range positions {
			cue.Time = cueTime
			cue.Timestamp = float64(cueTime) * scale
			cues = append(cues, cue)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("parse Matroska Cues: %w", err)
	}
	return cues, nil
}

func matroskaCodecName(codecID string) string {
	switch strings.ToUpper(strings.TrimSpace(codecID)) {
	case "V_MPEG4/ISO/AVC":
		return "h264"
	case "V_MPEGH/ISO/HEVC":
		return "hevc"
	case "V_VP8":
		return "vp8"
	case "V_VP9":
		return "vp9"
	case "V_AV1":
		return "av1"
	case "V_MPEG4/ISO/ASP":
		return "mpeg4"
	case "V_MPEG2":
		return "mpeg2video"
	default:
		return ""
	}
}

// matroskaCodecConfig turns the CodecPrivate element into the parameter sets a
// lone keyframe needs. Matroska stores the same decoder configuration records
// as MP4, so the conversion to Annex-B is shared with the MP4 indexer.
func matroskaCodecConfig(codec string, private []byte) (config [][]byte, nalLength int, av1Header []byte) {
	switch codec {
	case "h264":
		record, err := avc.DecodeAVCDecConfRec(private)
		if err != nil || (len(record.SPSnalus) == 0 && len(record.PPSnalus) == 0) {
			return nil, 0, nil
		}
		config = make([][]byte, 0, len(record.SPSnalus)+len(record.PPSnalus))
		for _, nalu := range record.SPSnalus {
			config = append(config, append([]byte(nil), nalu...))
		}
		for _, nalu := range record.PPSnalus {
			config = append(config, append([]byte(nil), nalu...))
		}
		// Matroska always uses four-byte NAL unit lengths for AVC.
		return config, 4, nil
	case "hevc":
		record, err := hevc.DecodeHEVCDecConfRec(private)
		if err != nil {
			return nil, 0, nil
		}
		config = make([][]byte, 0, 4)
		for _, array := range record.NaluArrays {
			for _, nalu := range array.Nalus {
				config = append(config, append([]byte(nil), nalu...))
			}
		}
		if len(config) == 0 {
			return nil, 0, nil
		}
		length := int(record.LengthSizeMinusOne) + 1
		if length < 1 || length > 4 {
			length = 4
		}
		return config, length, nil
	case "av1":
		// The codec private data starts with a four byte AV1 configuration
		// record; the OBU sequence header follows it.
		if len(private) > 4 {
			return nil, 0, append([]byte(nil), private[4:]...)
		}
	}
	return nil, 0, nil
}

func (i *matroskaIndex) info() videoInfo {
	if i == nil {
		return videoInfo{}
	}
	return videoInfo{
		Duration:   i.Duration,
		FormatName: "matroska",
		Width:      i.Width,
		Height:     i.Height,
		Codec:      i.Codec,
	}
}

func (i *matroskaIndex) anchor(timestamp float64) float64 {
	cue, ok := i.cueAtOrBefore(timestamp)
	if !ok {
		return 0
	}
	return cue.Timestamp
}

func (i *matroskaIndex) cueAtOrBefore(timestamp float64) (matroskaCue, bool) {
	if i == nil || len(i.Cues) == 0 {
		return matroskaCue{}, false
	}
	position := sort.Search(len(i.Cues), func(index int) bool {
		return i.Cues[index].Timestamp > timestamp
	})
	if position == 0 {
		return i.Cues[0], true
	}
	return i.Cues[position-1], true
}

// demuxer names the FFmpeg input format for the payload readKeyframe returns.
// H.264 and HEVC become Annex-B streams, and the VP family becomes a
// single-frame IVF file, which is the only container FFmpeg accepts from a pipe
// without knowing the rest of the timeline. An empty string means the caller
// has to fall back to the generic range proxy.
func (i *matroskaIndex) demuxer() string {
	if i == nil {
		return ""
	}
	switch i.Codec {
	case "h264", "hevc":
		if len(i.Config) == 0 {
			return ""
		}
		return i.Codec
	case "vp8", "vp9", "av1":
		return "ivf"
	default:
		return ""
	}
}

func (i *matroskaIndex) readKeyframe(ctx context.Context, timestamp float64) (keyframeSample, []byte, error) {
	if i == nil || i.Fetcher == nil {
		return keyframeSample{}, nil, fmt.Errorf("Matroska sample fetcher is unavailable")
	}
	if i.demuxer() == "" {
		return keyframeSample{}, nil, fmt.Errorf("Matroska codec %s cannot be decoded directly", i.CodecID)
	}
	cue, ok := i.cueAtOrBefore(timestamp)
	if !ok {
		return keyframeSample{}, nil, fmt.Errorf("Matroska cue point is unavailable")
	}
	sample, payload, err := i.resolveCue(ctx, cue)
	if err != nil {
		return keyframeSample{}, nil, err
	}
	converted, err := i.elementary(payload)
	if err != nil {
		return keyframeSample{}, nil, err
	}
	return sample, converted, nil
}

// resolveCue downloads only the bytes of the block a cue points at. The cluster
// header is cached because neighbouring cues usually share a cluster.
func (i *matroskaIndex) resolveCue(ctx context.Context, cue matroskaCue) (keyframeSample, []byte, error) {
	cluster, err := i.clusterHead(ctx, cue.Cluster)
	if err != nil {
		return keyframeSample{}, nil, err
	}
	blockOffset := int64(-1)
	if cue.Relative >= 0 {
		blockOffset = cluster.dataStart + cue.Relative
	} else {
		if blockOffset, err = i.scanClusterKeyframe(ctx, cluster); err != nil {
			return keyframeSample{}, nil, err
		}
	}
	probe, err := i.Fetcher.FetchRange(ctx, blockOffset, matroskaBlockProbeBytes)
	if err != nil {
		return keyframeSample{}, nil, fmt.Errorf("read Matroska block at %d: %w", blockOffset, err)
	}
	block, err := i.parseBlockElement(ctx, blockOffset, probe)
	if err != nil {
		return keyframeSample{}, nil, err
	}
	if block.lacing != 0 {
		// A laced block holds several frames behind an extra lace header, so its
		// payload is not one decodable picture.
		return keyframeSample{}, nil, fmt.Errorf("Matroska keyframe at %d is laced", block.dataStart)
	}
	if block.size > maxKeyframeBytes {
		return keyframeSample{}, nil, fmt.Errorf("Matroska keyframe is too large: %d bytes", block.size)
	}
	if block.size <= 0 {
		return keyframeSample{}, nil, fmt.Errorf("Matroska keyframe at %d is empty", block.dataStart)
	}
	payload, err := i.Fetcher.FetchRange(ctx, block.dataStart, block.size)
	if err != nil {
		return keyframeSample{}, nil, fmt.Errorf("read Matroska keyframe at %d: %w", block.dataStart, err)
	}
	sample := keyframeSample{
		Index:     uint64(cue.Time),
		Timestamp: cue.Timestamp,
		Offset:    block.dataStart,
		Size:      block.size,
	}
	return sample, payload, nil
}

// matroskaBlock is one decoded block header. dataStart and size describe the
// frame payload only, without the EBML header or the block header.
type matroskaBlock struct {
	track     uint64
	keyframe  bool
	lacing    int
	dataStart int64
	size      int64
}

// parseBlockElement reads the block a cue points at. Cues that reference a
// BlockGroup instead of a plain block need one more bounded request, which is
// rare because muxers write SimpleBlocks for video keyframes.
func (i *matroskaIndex) parseBlockElement(ctx context.Context, offset int64, probe []byte) (matroskaBlock, error) {
	element, err := ebmlElementFrom(probe, offset)
	if err != nil {
		return matroskaBlock{}, fmt.Errorf("read Matroska block at %d: %w", offset, err)
	}
	if element.ID == blockGroupID {
		return i.parseBlockGroup(ctx, offset, element)
	}
	if element.ID != simpleBlockID && element.ID != blockID {
		return matroskaBlock{}, fmt.Errorf("Matroska element 0x%X at %d is not a block", element.ID, offset)
	}
	bodyStart := element.DataStart - offset
	if bodyStart < 0 || bodyStart+4 > int64(len(probe)) {
		return matroskaBlock{}, fmt.Errorf("Matroska block header at %d is truncated", offset)
	}
	return parseBlockHeader(probe[bodyStart:], element.Size, element.ID == simpleBlockID, element.DataStart)
}

// parseBlockGroup resolves a cue that points at a BlockGroup. The group is read
// with one bounded request; only the header of the block inside it matters, so
// a large frame payload is never downloaded twice.
func (i *matroskaIndex) parseBlockGroup(ctx context.Context, offset int64, element ebmlElement) (matroskaBlock, error) {
	if element.Unknown || element.Size <= 0 {
		return matroskaBlock{}, fmt.Errorf("Matroska block group at %d has an invalid size", offset)
	}
	length := element.Size
	if length > matroskaHeaderWindow {
		length = matroskaHeaderWindow
	}
	group, err := i.Fetcher.FetchRange(ctx, element.DataStart, length)
	if err != nil {
		return matroskaBlock{}, fmt.Errorf("read Matroska block group at %d: %w", offset, err)
	}
	var reference, located bool
	var found matroskaBlock
	for childOffset := 0; childOffset < len(group); {
		id, idLength, ok := ebmlID(group[childOffset:])
		if !ok {
			break
		}
		size, sizeLength, unknown, ok := ebmlSize(group[childOffset+idLength:])
		if !ok || unknown || size < 0 {
			break
		}
		start := childOffset + idLength + sizeLength
		switch id {
		case blockReferenceID:
			// A reference block means the picture depends on earlier frames.
			reference = true
		case blockID:
			if size >= 4 && start+4 <= len(group) {
				block, err := parseBlockHeader(group[start:], size, false, element.DataStart+int64(start))
				if err == nil {
					found, located = block, true
				}
			}
		}
		if size > int64(len(group)) {
			break
		}
		childOffset = start + int(size)
	}
	if !located {
		return matroskaBlock{}, fmt.Errorf("Matroska block group at %d has no block", offset)
	}
	if reference {
		return matroskaBlock{}, fmt.Errorf("Matroska block at %d is not a keyframe", offset)
	}
	// Inside a BlockGroup the keyframe flag is not meaningful; the absence of a
	// reference block above is what marks the picture as independent.
	found.keyframe = true
	return found, nil
}

// parseBlockHeader decodes the Matroska block header: a track number vint, a
// signed 16-bit timecode relative to the cluster, and one flags byte. data
// starts at the block payload and size is its declared length, which is usually
// longer than the probe that was downloaded.
func parseBlockHeader(data []byte, size int64, keyframeFlag bool, dataStart int64) (matroskaBlock, error) {
	track, trackLength, _, ok := ebmlSize(data)
	if !ok || track <= 0 {
		return matroskaBlock{}, fmt.Errorf("Matroska block at %d has an invalid track number", dataStart)
	}
	header := int64(trackLength) + 3
	if header > int64(len(data)) {
		return matroskaBlock{}, fmt.Errorf("Matroska block header at %d is truncated", dataStart)
	}
	if size < header {
		return matroskaBlock{}, fmt.Errorf("Matroska block at %d has the invalid size %d", dataStart, size)
	}
	flags := data[trackLength+2]
	return matroskaBlock{
		track:     uint64(track),
		keyframe:  keyframeFlag && flags&0x80 != 0,
		lacing:    int(flags&0x06) >> 1,
		dataStart: dataStart + header,
		size:      size - header,
	}, nil
}

// clusterHead resolves one CueClusterPosition into absolute offsets. Header and
// timestamp element are read with a single small request and cached, since a
// cluster usually holds more than one cued keyframe.
func (i *matroskaIndex) clusterHead(ctx context.Context, clusterPosition int64) (matroskaClusterHead, error) {
	offset := i.SegmentData + clusterPosition
	if offset < i.SegmentData || offset >= i.Fetcher.Size() {
		return matroskaClusterHead{}, fmt.Errorf("Matroska cluster position %d is outside the segment", clusterPosition)
	}
	i.mu.Lock()
	if cached, ok := i.clusters[offset]; ok {
		i.mu.Unlock()
		return cached, nil
	}
	i.mu.Unlock()

	probe, err := i.Fetcher.FetchRange(ctx, offset, matroskaClusterProbeBytes)
	if err != nil {
		return matroskaClusterHead{}, fmt.Errorf("read Matroska cluster at %d: %w", offset, err)
	}
	element, err := ebmlElementFrom(probe, offset)
	if err != nil {
		return matroskaClusterHead{}, fmt.Errorf("parse Matroska cluster at %d: %w", offset, err)
	}
	if element.ID != clusterID {
		return matroskaClusterHead{}, fmt.Errorf("Matroska element 0x%X at %d is not a cluster", element.ID, offset)
	}
	head := matroskaClusterHead{offset: offset, dataStart: element.DataStart}
	if !element.Unknown {
		head.end = element.End()
	}
	if rest := probe[element.DataStart-offset:]; len(rest) > 0 {
		if childID, childLength, ok := ebmlID(rest); ok && childID == clusterTimestampID {
			if childSize, childSizeLength, _, ok := ebmlSize(rest[childLength:]); ok {
				start := childLength + childSizeLength
				if childSize >= 0 && int64(start)+childSize <= int64(len(rest)) {
					if value, ok := ebmlUint(rest[start : start+int(childSize)]); ok {
						head.timestamp = value
					}
				}
			}
		}
	}
	i.mu.Lock()
	i.clusters[offset] = head
	i.mu.Unlock()
	return head, nil
}

// scanClusterKeyframe finds the first video keyframe of a cluster whose cue did
// not record a relative block position. Only element headers are inspected, so
// the walk costs one bounded window per hop instead of the cluster itself.
func (i *matroskaIndex) scanClusterKeyframe(ctx context.Context, cluster matroskaClusterHead) (int64, error) {
	limit := cluster.dataStart + matroskaClusterScanBytes
	if cluster.end > 0 && cluster.end < limit {
		limit = cluster.end
	}
	for position := cluster.dataStart; position < limit; {
		windowLength := int64(matroskaHeaderWindow)
		if limit-position < windowLength {
			windowLength = limit - position
		}
		window, err := i.Fetcher.FetchRange(ctx, position, windowLength)
		if err != nil {
			return 0, fmt.Errorf("scan Matroska cluster at %d: %w", position, err)
		}
		var offset int64
		for offset < int64(len(window)) {
			element, err := ebmlElementFrom(window[offset:], position+offset)
			if err != nil {
				break
			}
			if element.ID == clusterID && position+offset > cluster.offset {
				return 0, fmt.Errorf("Matroska cluster at %d has no keyframe", cluster.offset)
			}
			if element.ID == simpleBlockID && element.Size > 0 {
				headerEnd := offset + element.HeaderLength
				if headerEnd+4 <= int64(len(window)) {
					block, err := parseBlockHeader(window[headerEnd:], element.Size, true, element.DataStart)
					if err == nil && block.keyframe && block.lacing == 0 && (i.TrackNumber == 0 || block.track == i.TrackNumber) {
						return position + offset, nil
					}
				}
			}
			if element.Unknown || element.Size < 0 {
				return 0, fmt.Errorf("Matroska cluster at %d contains an element with an unknown size", cluster.offset)
			}
			offset += element.HeaderLength + element.Size
		}
		if offset == 0 {
			return 0, fmt.Errorf("Matroska cluster at %d cannot be parsed at %d", cluster.offset, position)
		}
		position += offset
	}
	return 0, fmt.Errorf("Matroska cluster at %d has no keyframe inside %d bytes", cluster.offset, matroskaClusterScanBytes)
}

// elementary converts one Matroska block into a stream FFmpeg can decode on its
// own.
func (i *matroskaIndex) elementary(block []byte) ([]byte, error) {
	switch i.Codec {
	case "h264", "hevc":
		converted, err := mp4SampleToAnnexB(block, i.Config, i.NALLength)
		if err != nil {
			return nil, fmt.Errorf("convert Matroska %s block: %w", i.Codec, err)
		}
		return converted, nil
	case "vp8", "vp9", "av1":
		return i.wrapIVF(block)
	default:
		return nil, fmt.Errorf("Matroska codec %s cannot be decoded directly", i.CodecID)
	}
}

// wrapIVF puts a single VP8, VP9 or AV1 frame into the smallest container
// FFmpeg can read from a pipe. The AV1 sequence header from the codec private
// data is prepended so the frame stays decodable without its predecessors.
func (i *matroskaIndex) wrapIVF(frame []byte) ([]byte, error) {
	fourCC := map[string]string{"vp8": ivf.CodecVP8, "vp9": ivf.CodecVP9, "av1": ivf.CodecAV1}[i.Codec]
	if fourCC == "" {
		return nil, fmt.Errorf("Matroska codec %s has no IVF tag", i.Codec)
	}
	payload := frame
	if i.Codec == "av1" && len(i.AV1Header) > 0 {
		payload = make([]byte, 0, len(i.AV1Header)+len(frame))
		payload = append(payload, i.AV1Header...)
		payload = append(payload, frame...)
	}
	rate, scale := uint32(25), uint32(1)
	if i.DefaultDuration > 0 && i.DefaultDuration < uint64(time.Second) {
		rate = uint32(uint64(time.Second) / i.DefaultDuration)
		if rate == 0 {
			rate = 25
		}
	}
	buffer := &bytes.Buffer{}
	writer, err := ivf.NewWriter(buffer, ivf.FileHeader{
		FourCC:    fourCC,
		Width:     uint16(i.Width),
		Height:    uint16(i.Height),
		Rate:      rate,
		Scale:     scale,
		NumFrames: 1,
	})
	if err != nil {
		return nil, fmt.Errorf("write IVF header: %w", err)
	}
	if err := writer.WriteFrame(ivf.Frame{Timestamp: 0, Data: payload}); err != nil {
		return nil, fmt.Errorf("write IVF frame: %w", err)
	}
	return buffer.Bytes(), nil
}

func (i *matroskaIndex) summary() map[string]any {
	if i == nil {
		return nil
	}
	var first, last float64
	if len(i.Cues) > 0 {
		first = i.Cues[0].Timestamp
		last = i.Cues[len(i.Cues)-1].Timestamp
	}
	return map[string]any{
		"container":       "matroska",
		"keyframes":       len(i.Cues),
		"duration":        i.Duration,
		"width":           i.Width,
		"height":          i.Height,
		"codec":           i.Codec,
		"codec_id":        i.CodecID,
		"track_number":    i.TrackNumber,
		"timestamp_scale": i.TimestampScale,
		"first_keyframe":  first,
		"last_keyframe":   last,
		"cues_bytes":      i.CuesBytes,
		"segment_data":    i.SegmentData,
		"indexed_at":      time.Now().UTC().Format(time.RFC3339),
		"media_payload":   "keyframe_only",
	}
}

// ebmlElement is one parsed element header.
type ebmlElement struct {
	ID           uint32
	Offset       int64
	HeaderLength int64
	Size         int64
	DataStart    int64
	Unknown      bool
}

// End returns the offset just past the element, which is meaningless when the
// size is unknown.
func (e ebmlElement) End() int64 { return e.DataStart + e.Size }

func (e ebmlElement) found() bool { return e.ID != 0 }

// ebmlElementFrom parses the element header at the start of data, which begins
// at the absolute offset position.
func ebmlElementFrom(data []byte, position int64) (ebmlElement, error) {
	id, idLength, ok := ebmlID(data)
	if !ok {
		return ebmlElement{}, fmt.Errorf("invalid EBML element ID at %d", position)
	}
	size, sizeLength, unknown, ok := ebmlSize(data[idLength:])
	if !ok {
		return ebmlElement{}, fmt.Errorf("invalid EBML element size at %d", position)
	}
	return ebmlElement{
		ID:           id,
		Offset:       position,
		HeaderLength: int64(idLength + sizeLength),
		Size:         size,
		DataStart:    position + int64(idLength+sizeLength),
		Unknown:      unknown,
	}, nil
}

// ebmlScanner reads element headers through a cached window. Walking a
// container head element by element would otherwise cost one HTTP request per
// element, and on a cloud drive every request is a round trip.
type ebmlScanner struct {
	fetcher rangeFetcher
	window  int64
	maxHops int

	mu   sync.Mutex
	base int64
	data []byte
	hops int
}

func newEBMLScanner(fetcher rangeFetcher, window int64, maxHops int) *ebmlScanner {
	if window <= 0 {
		window = matroskaHeaderWindow
	}
	if maxHops <= 0 {
		maxHops = matroskaScannerHops
	}
	return &ebmlScanner{fetcher: fetcher, window: window, maxHops: maxHops}
}

func (s *ebmlScanner) elementHeader(ctx context.Context, position int64) (ebmlElement, error) {
	if position < 0 {
		return ebmlElement{}, fmt.Errorf("negative EBML offset %d", position)
	}
	size := s.fetcher.Size()
	if size > 0 && position >= size {
		return ebmlElement{}, io.EOF
	}
	length := int64(ebmlHeaderMaxBytes)
	if size > 0 && size-position < length {
		length = size - position
	}
	data, err := s.headerBytes(ctx, position, length)
	if err != nil {
		return ebmlElement{}, err
	}
	return ebmlElementFrom(data, position)
}

// headerBytes returns the length bytes at position, reading a whole window on a
// miss so the neighbouring element headers come for free. maxHops bounds the
// number of requests, which keeps a damaged element chain from scanning the
// whole file.
func (s *ebmlScanner) headerBytes(ctx context.Context, position, length int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cached := s.data; cached != nil && position >= s.base && position+length <= s.base+int64(len(cached)) {
		start := position - s.base
		return cached[start : start+length], nil
	}
	if s.hops >= s.maxHops {
		return nil, fmt.Errorf("EBML scan exceeded %d range requests", s.maxHops)
	}
	s.hops++
	window := s.window
	if window < length {
		window = length
	}
	if size := s.fetcher.Size(); size > 0 && position+window > size {
		window = size - position
	}
	data, err := s.fetcher.FetchRange(ctx, position, window)
	if err != nil {
		return nil, err
	}
	s.base = position
	s.data = data
	if int64(len(data)) < length {
		return nil, io.ErrUnexpectedEOF
	}
	return data[:length], nil
}

// ebmlChildren walks the children of an element payload that is already in
// memory. Unknown sizes are rejected because a bounded parent always declares
// bounded children.
func ebmlChildren(data []byte, visit func(id uint32, payload []byte) error) error {
	for offset := 0; offset < len(data); {
		id, idLength, ok := ebmlID(data[offset:])
		if !ok {
			return fmt.Errorf("invalid EBML element ID at byte %d", offset)
		}
		size, sizeLength, unknown, ok := ebmlSize(data[offset+idLength:])
		if !ok {
			return fmt.Errorf("invalid EBML element size at byte %d", offset)
		}
		if unknown {
			return fmt.Errorf("EBML element 0x%X has an unknown size inside a bounded parent", id)
		}
		start := offset + idLength + sizeLength
		if size < 0 || size > int64(len(data)-start) {
			return fmt.Errorf("EBML element 0x%X overruns its parent", id)
		}
		if err := visit(id, data[start:start+int(size)]); err != nil {
			return err
		}
		offset = start + int(size)
	}
	return nil
}

// ebmlID reads an element ID. The length marker stays part of the value, which
// is what makes 0x1A45DFA3 a four-byte ID instead of a truncated one.
func ebmlID(data []byte) (uint32, int, bool) {
	if len(data) == 0 || data[0] == 0 {
		return 0, 0, false
	}
	first := data[0]
	for length := 1; length <= 4 && length <= len(data); length++ {
		if first&(0x80>>uint(length-1)) == 0 {
			continue
		}
		var value uint32
		for index := 0; index < length; index++ {
			value = value<<8 | uint32(data[index])
		}
		return value, length, true
	}
	return 0, 0, false
}

// ebmlIDValue reads an ID that is stored as element payload, as SeekID does.
// Such IDs keep their leading zero bytes, so the width is the payload length.
func ebmlIDValue(data []byte) uint32 {
	if len(data) == 0 || len(data) > 4 {
		return 0
	}
	var value uint32
	for _, b := range data {
		value = value<<8 | uint32(b)
	}
	return value
}

// ebmlSize reads a data size. The length marker is stripped, and all data bits
// set to one means "unknown size", i.e. the element runs to the end of its
// parent.
func ebmlSize(data []byte) (int64, int, bool, bool) {
	if len(data) == 0 || data[0] == 0 {
		return 0, 0, false, false
	}
	first := data[0]
	for length := 1; length <= 8 && length <= len(data); length++ {
		marker := byte(0x80 >> uint(length-1))
		if first&marker == 0 {
			continue
		}
		value := uint64(first & (marker - 1))
		for index := 1; index < length; index++ {
			value = value<<8 | uint64(data[index])
		}
		if value == uint64(1)<<(7*uint(length))-1 {
			return 0, length, true, true
		}
		if value > uint64(math.MaxInt64) {
			return 0, 0, false, false
		}
		return int64(value), length, false, true
	}
	return 0, 0, false, false
}

func ebmlUint(data []byte) (uint64, bool) {
	if len(data) == 0 || len(data) > 8 {
		return 0, false
	}
	var value uint64
	for _, b := range data {
		value = value<<8 | uint64(b)
	}
	return value, true
}

func ebmlFloat(data []byte) (float64, bool) {
	switch len(data) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(data))), true
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(data)), true
	default:
		return 0, false
	}
}

// fetchElement downloads one whole metadata element with a single exact
// request.
func fetchElement(ctx context.Context, fetcher rangeFetcher, element ebmlElement, what string) ([]byte, error) {
	if element.Unknown {
		return nil, fmt.Errorf("%s has an unknown size", what)
	}
	if element.Size <= 0 {
		return nil, fmt.Errorf("%s is empty", what)
	}
	if element.Size > matroskaMetadataBytes {
		return nil, fmt.Errorf("%s is too large: %d bytes", what, element.Size)
	}
	return fetcher.FetchRange(ctx, element.DataStart, element.Size)
}
