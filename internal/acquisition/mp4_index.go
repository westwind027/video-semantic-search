package acquisition

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/Eyevinn/mp4ff/mp4"
)

const (
	// mp4ProbeWindow is read once at the start of the file. The top-level boxes
	// that precede moov are ftyp, free/skip and possibly mdat, and only their
	// headers are inspected, so one small window covers the common faststart
	// layout without any additional request.
	mp4ProbeWindow = 32 << 10
	// mp4BoxHeaderBytes is the largest top-level box header: the 64-bit size
	// form is 8 bytes of type and size plus 8 bytes of largesize.
	mp4BoxHeaderBytes = 16
	// A large moov is still one logical byte sequence, but fetching it as
	// bounded pieces lets a high-latency cloud-drive connection use several
	// HTTP range streams. The limit is deliberately conservative because some
	// providers return 429 when too many ranges are opened at once.
	defaultMP4MoovChunkSize = 1 << 20
	defaultMP4MoovWorkers   = 4
	maxMP4MoovWorkers       = 16
)

type mp4MoovFetchOptions struct {
	ChunkSize int64
	Workers   int
}

// mp4Index is the part of an MP4 sample table that acquisition needs: where
// every sync sample lives, how long the movie is, and how to turn one sample
// back into an elementary stream FFmpeg can decode.
type mp4Index struct {
	Keyframes []keyframeSample
	Duration  float64
	Width     int
	Height    int
	Codec     string
	Fetcher   rangeFetcher
	Config    [][]byte
	NALLength int
	MoovBytes int64
}

// buildMP4Index reads only the MP4 box metadata. The moov box is located by
// walking top-level box headers, then downloaded as bounded exact ranges and
// parsed entirely in memory. Media data is never touched: a 2 GiB mdat before
// moov costs one 16 byte header read to skip.
func buildMP4Index(ctx context.Context, fetcher rangeFetcher) (*mp4Index, error) {
	return buildMP4IndexWithOptions(ctx, fetcher, mp4MoovFetchOptions{})
}

func buildMP4IndexWithOptions(ctx context.Context, fetcher rangeFetcher, options mp4MoovFetchOptions) (*mp4Index, error) {
	if fetcher == nil {
		return nil, fmt.Errorf("MP4 index fetcher is nil")
	}
	if fetcher.Size() <= 0 {
		return nil, fmt.Errorf("MP4 size is unavailable")
	}
	moovOffset, moovSize, err := locateMP4Moov(ctx, fetcher)
	if err != nil {
		return nil, err
	}
	if moovSize > maxMetadataBytes {
		return nil, fmt.Errorf("MP4 moov box is too large: %d bytes", moovSize)
	}
	moovData, err := fetchMP4Moov(ctx, fetcher, moovOffset, moovSize, options)
	if err != nil {
		return nil, fmt.Errorf("read MP4 moov box at %d: %w", moovOffset, err)
	}
	moov, err := decodeMP4Moov(moovData)
	if err != nil {
		return nil, err
	}
	if moov.Mvex != nil {
		// Fragmented files keep their sample tables in moof boxes scattered
		// through the media data, so there is no cheap progressive index.
		return nil, fmt.Errorf("fragmented MP4 has no progressive sample table")
	}
	for _, track := range moov.Traks {
		index, err := buildMP4TrackIndex(ctx, fetcher, moov, track, moovSize)
		if err != nil {
			return nil, err
		}
		if index != nil {
			return index, nil
		}
	}
	return nil, fmt.Errorf("MP4 has no usable video track")
}

// fetchMP4Moov downloads one complete moov box in ordered, bounded pieces.
// The returned byte sequence is identical to a single Range request, while
// independent pieces can make progress concurrently on remote providers that
// are slow for a large contiguous response.
func fetchMP4Moov(ctx context.Context, fetcher rangeFetcher, offset, size int64, options mp4MoovFetchOptions) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if fetcher == nil {
		return nil, fmt.Errorf("MP4 moov fetcher is nil")
	}
	if offset < 0 || size <= 0 {
		return nil, fmt.Errorf("invalid MP4 moov range %d-%d", offset, size)
	}
	if size > maxMetadataBytes {
		return nil, fmt.Errorf("MP4 moov box is too large: %d bytes", size)
	}
	chunkSize := options.ChunkSize
	if chunkSize <= 0 {
		chunkSize = defaultMP4MoovChunkSize
	}
	if chunkSize > maxMetadataBytes {
		chunkSize = maxMetadataBytes
	}
	workers := options.Workers
	if workers <= 0 {
		workers = defaultMP4MoovWorkers
	}
	if workers > maxMP4MoovWorkers {
		workers = maxMP4MoovWorkers
	}
	parts := int((size + chunkSize - 1) / chunkSize)
	if parts == 1 {
		data, err := fetcher.FetchRange(ctx, offset, size)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != size {
			return nil, fmt.Errorf("MP4 moov range returned %d bytes, expected %d", len(data), size)
		}
		return data, nil
	}
	if workers > parts {
		workers = parts
	}

	workContext, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	results := make([][]byte, parts)
	var wait sync.WaitGroup
	var errorMu sync.Mutex
	var firstErr error
	setError := func(err error) {
		if err == nil {
			return
		}
		errorMu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		errorMu.Unlock()
	}

	worker := func() {
		defer wait.Done()
		for {
			select {
			case <-workContext.Done():
				return
			case part, ok := <-jobs:
				if !ok {
					return
				}
				partOffset := offset + int64(part)*chunkSize
				partSize := chunkSize
				if remaining := size - int64(part)*chunkSize; remaining < partSize {
					partSize = remaining
				}
				data, err := fetcher.FetchRange(workContext, partOffset, partSize)
				if err != nil {
					setError(fmt.Errorf("read MP4 moov part %d at %d: %w", part, partOffset, err))
					return
				}
				if int64(len(data)) != partSize {
					setError(fmt.Errorf("MP4 moov part %d returned %d bytes, expected %d", part, len(data), partSize))
					return
				}
				results[part] = data
			}
		}
	}

	wait.Add(workers)
	for index := 0; index < workers; index++ {
		go worker()
	}
	for part := 0; part < parts; part++ {
		select {
		case jobs <- part:
		case <-workContext.Done():
			break
		}
		if workContext.Err() != nil {
			break
		}
	}
	close(jobs)
	wait.Wait()
	errorMu.Lock()
	err := firstErr
	errorMu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	assembled := make([]byte, 0, int(size))
	for part, data := range results {
		if data == nil {
			return nil, fmt.Errorf("MP4 moov part %d was not downloaded", part)
		}
		assembled = append(assembled, data...)
	}
	return assembled, nil
}

// buildMP4TrackIndex returns nil when the track is not a usable video track,
// which lets the caller keep looking at the remaining tracks.
func buildMP4TrackIndex(_ context.Context, fetcher rangeFetcher, moov *mp4.MoovBox, track *mp4.TrakBox, moovSize int64) (*mp4Index, error) {
	if track == nil || track.Tkhd == nil || track.Mdia == nil || track.Mdia.Hdlr == nil || track.Mdia.Hdlr.HandlerType != "vide" {
		return nil, nil
	}
	if track.Mdia.Mdhd == nil || track.Mdia.Mdhd.Timescale == 0 || track.Mdia.Minf == nil || track.Mdia.Minf.Stbl == nil {
		return nil, nil
	}
	stbl := track.Mdia.Minf.Stbl
	if stbl.Stts == nil || stbl.Stsc == nil || stbl.Stsz == nil || (stbl.Stco == nil && stbl.Co64 == nil) {
		return nil, nil
	}
	samples, err := mp4SyncSamples(stbl)
	if err != nil {
		return nil, err
	}
	index := &mp4Index{
		Keyframes: make([]keyframeSample, 0, len(samples)),
		Duration:  float64(track.Mdia.Mdhd.Duration) / float64(track.Mdia.Mdhd.Timescale),
		Width:     int(track.Tkhd.Width >> 16),
		Height:    int(track.Tkhd.Height >> 16),
		Codec:     mp4CodecName(stbl.Stsd),
		Fetcher:   fetcher,
		NALLength: 4,
		MoovBytes: moovSize,
	}
	index.Config = mp4CodecConfig(stbl.Stsd, &index.NALLength)
	if index.Duration <= 0 && moov.Mvhd != nil && moov.Mvhd.Timescale > 0 {
		index.Duration = float64(moov.Mvhd.Duration) / float64(moov.Mvhd.Timescale)
	}
	timestamps := mp4DecodeTimes(stbl.Stts, samples, track.Mdia.Mdhd.Timescale)
	for position, sampleNumber := range samples {
		ranges, err := track.GetRangesForSampleInterval(sampleNumber, sampleNumber)
		if err != nil {
			return nil, fmt.Errorf("read MP4 sample %d range: %w", sampleNumber, err)
		}
		if len(ranges) != 1 {
			return nil, fmt.Errorf("read MP4 sample %d range: expected one range, got %d", sampleNumber, len(ranges))
		}
		index.Keyframes = append(index.Keyframes, keyframeSample{
			Index:     uint64(sampleNumber),
			Timestamp: timestamps[position],
			Offset:    int64(ranges[0].Offset),
			Size:      int64(stbl.Stsz.GetSampleSize(int(sampleNumber))),
		})
	}
	if len(index.Keyframes) == 0 {
		return nil, fmt.Errorf("video track has no usable keyframe ranges")
	}
	sort.Slice(index.Keyframes, func(left, right int) bool {
		if index.Keyframes[left].Timestamp == index.Keyframes[right].Timestamp {
			return index.Keyframes[left].Index < index.Keyframes[right].Index
		}
		return index.Keyframes[left].Timestamp < index.Keyframes[right].Timestamp
	})
	return index, nil
}

// locateMP4Moov walks the top-level box chain and returns the offset and size
// of the moov box. Reading a full window once and then only 16 byte headers
// keeps the search cheap for both layouts: moov before mdat (faststart) and
// moov after a multi-gigabyte mdat.
func locateMP4Moov(ctx context.Context, fetcher rangeFetcher) (int64, int64, error) {
	size := fetcher.Size()
	windowLength := int64(mp4ProbeWindow)
	if windowLength > size {
		windowLength = size
	}
	window, err := fetcher.FetchRange(ctx, 0, windowLength)
	if err != nil {
		return 0, 0, fmt.Errorf("read MP4 top-level boxes: %w", err)
	}
	for position := int64(0); position < size; {
		header, err := mp4BoxHeaderAt(ctx, fetcher, window, position)
		if err != nil {
			return 0, 0, fmt.Errorf("read MP4 box header at %d: %w", position, err)
		}
		boxType, boxSize, err := parseMP4BoxHeader(header)
		if err != nil {
			return 0, 0, fmt.Errorf("parse MP4 box header at %d: %w", position, err)
		}
		if boxType == "moov" {
			if boxSize > size-position {
				boxSize = size - position
			}
			return position, boxSize, nil
		}
		if boxSize < 0 {
			// A box that runs to the end of the file cannot be followed by moov.
			break
		}
		if boxSize == 0 {
			return 0, 0, fmt.Errorf("MP4 box %s at %d has a zero size", boxType, position)
		}
		position += boxSize
	}
	return 0, 0, fmt.Errorf("MP4 moov box is missing")
}

// mp4BoxHeaderAt returns the header bytes of the box at position, reusing the
// probe window whenever it already covers them.
func mp4BoxHeaderAt(ctx context.Context, fetcher rangeFetcher, window []byte, position int64) ([]byte, error) {
	if position >= 0 && position+mp4BoxHeaderBytes <= int64(len(window)) {
		return window[position : position+mp4BoxHeaderBytes], nil
	}
	length := int64(mp4BoxHeaderBytes)
	if remaining := fetcher.Size() - position; remaining < length {
		length = remaining
	}
	if length <= 0 {
		return nil, io.EOF
	}
	return fetcher.FetchRange(ctx, position, length)
}

// parseMP4BoxHeader decodes one ISO base media file format box header. A
// returned size of -1 means "up to the end of the file", and 0 is reported
// separately because it is only legal for the last box.
func parseMP4BoxHeader(data []byte) (string, int64, error) {
	if len(data) < 8 {
		return "", 0, io.ErrUnexpectedEOF
	}
	declared := int64(binary.BigEndian.Uint32(data[0:4]))
	boxType := string(data[4:8])
	headerSize := int64(8)
	boxSize := declared
	switch declared {
	case 1:
		if len(data) < mp4BoxHeaderBytes {
			return "", 0, io.ErrUnexpectedEOF
		}
		boxSize = int64(binary.BigEndian.Uint64(data[8:16]))
		headerSize = mp4BoxHeaderBytes
	case 0:
		boxSize = -1
	}
	if boxSize >= 0 && boxSize < headerSize {
		return "", 0, fmt.Errorf("MP4 box %s declares the invalid size %d", boxType, boxSize)
	}
	return boxType, boxSize, nil
}

func decodeMP4Moov(data []byte) (*mp4.MoovBox, error) {
	box, err := mp4.DecodeBox(0, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse MP4 moov box: %w", err)
	}
	moov, ok := box.(*mp4.MoovBox)
	if !ok {
		return nil, fmt.Errorf("MP4 moov box has the unexpected type %T", box)
	}
	return moov, nil
}

// mp4SyncSamples lists the video samples that can be decoded on their own. A
// track without stss marks every sample as a sync sample, which is what the
// specification requires for intra-only encodes.
func mp4SyncSamples(stbl *mp4.StblBox) ([]uint32, error) {
	total := stbl.Stsz.GetNrSamples()
	if total == 0 {
		return nil, fmt.Errorf("video track has no samples")
	}
	samples := make([]uint32, 0, 64)
	if stbl.Stss != nil {
		for _, sampleNumber := range stbl.Stss.SampleNumber {
			if sampleNumber >= 1 && sampleNumber <= total {
				samples = append(samples, sampleNumber)
			}
		}
	} else {
		samples = make([]uint32, 0, total)
		for sampleNumber := uint32(1); sampleNumber <= total; sampleNumber++ {
			samples = append(samples, sampleNumber)
		}
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("video track has no sync samples")
	}
	sort.Slice(samples, func(left, right int) bool { return samples[left] < samples[right] })
	return samples, nil
}

// mp4DecodeTimes converts one-based sample numbers into seconds with a single
// walk over the stts entries. Asking SttsBox.GetDecodeTime for each keyframe
// rescans the entry list every time, which is quadratic for variable-frame-rate
// encodes, and it panics when a damaged stss points past the last entry.
func mp4DecodeTimes(stts *mp4.SttsBox, samples []uint32, timescale uint32) []float64 {
	result := make([]float64, len(samples))
	if timescale == 0 {
		return result
	}
	scale := float64(timescale)
	cursor := 0
	next := uint64(1)
	var base uint64
	for entry := 0; entry < len(stts.SampleCount) && cursor < len(samples); entry++ {
		count := uint64(stts.SampleCount[entry])
		delta := uint64(stts.SampleTimeDelta[entry])
		if count == 0 {
			continue
		}
		entryEnd := next + count - 1
		for cursor < len(samples) && uint64(samples[cursor]) < next {
			result[cursor] = float64(base) / scale
			cursor++
		}
		for cursor < len(samples) && uint64(samples[cursor]) <= entryEnd {
			result[cursor] = float64(base+(uint64(samples[cursor])-next)*delta) / scale
			cursor++
		}
		base += count * delta
		next = entryEnd + 1
	}
	for cursor < len(samples) {
		result[cursor] = float64(base) / scale
		cursor++
	}
	return result
}

func (i *mp4Index) info() videoInfo {
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
	if i == nil {
		return 0
	}
	return anchorFor(i.Keyframes, timestamp)
}

// demuxer returns the ffmpeg input format for the elementary stream that
// readKeyframe produces. MP4 samples are length-prefixed NAL units, so only
// codecs whose configuration record could be read are convertible; anything
// else reports an empty string and the caller falls back to the range proxy.
func (i *mp4Index) demuxer() string {
	if i == nil || len(i.Config) == 0 {
		return ""
	}
	switch i.Codec {
	case "h264", "hevc":
		return i.Codec
	default:
		return ""
	}
}

func (i *mp4Index) readKeyframe(ctx context.Context, timestamp float64) (keyframeSample, []byte, error) {
	if i == nil || i.Fetcher == nil {
		return keyframeSample{}, nil, fmt.Errorf("MP4 sample fetcher is unavailable")
	}
	if i.demuxer() == "" {
		return keyframeSample{}, nil, fmt.Errorf("MP4 codec configuration is unavailable for %s", i.Codec)
	}
	sample, ok := keyframeAtOrBefore(i.Keyframes, timestamp)
	if !ok || sample.Offset < 0 || sample.Size <= 0 {
		return keyframeSample{}, nil, fmt.Errorf("MP4 keyframe sample is unavailable")
	}
	if sample.Size > maxKeyframeBytes {
		return keyframeSample{}, nil, fmt.Errorf("MP4 keyframe sample is too large: %d bytes", sample.Size)
	}
	// Exactly the sample bytes are requested. Widening this read to an aligned
	// chunk grid is what made a single keyframe cost several megabytes.
	data, err := i.Fetcher.FetchRange(ctx, sample.Offset, sample.Size)
	if err != nil {
		return keyframeSample{}, nil, fmt.Errorf("read MP4 keyframe sample at %d: %w", sample.Offset, err)
	}
	converted, err := mp4SampleToAnnexB(data, i.Config, i.NALLength)
	if err != nil {
		return keyframeSample{}, nil, fmt.Errorf("convert MP4 %s sample: %w", i.Codec, err)
	}
	return sample, converted, nil
}

func (i *mp4Index) summary() map[string]any {
	if i == nil {
		return nil
	}
	summary := metadataSummary("mp4", i.info(), i.Keyframes)
	summary["moov_bytes"] = i.MoovBytes
	summary["nal_length_size"] = i.NALLength
	return summary
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

// mp4SampleToAnnexB turns one length-prefixed sample into a start-code stream
// and prepends the decoder configuration, so FFmpeg can decode the picture
// without any other sample of the same GOP.
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
