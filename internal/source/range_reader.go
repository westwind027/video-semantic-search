package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"video-semantic-search/internal/httpclient"
)

var ErrRangeUnsupported = errors.New("remote source does not support HTTP range requests")

const (
	// DefaultChunkSize is the aligned unit used by sequential readers such as
	// the FFmpeg range proxy, where one large request per hop beats many small
	// ones. Random access never uses this grid; see FetchRange.
	DefaultChunkSize = 4 << 20
	// DefaultMaxCacheBytes bounds the shared range cache. Acquisition only
	// needs container metadata plus the keyframe samples it decoded, so a long
	// movie must not pin hundreds of megabytes of media data in memory.
	DefaultMaxCacheBytes = 96 << 20
	// maxRangeLength rejects absurd lengths coming from a corrupt container
	// header before any allocation happens.
	maxRangeLength = 256 << 20
)

// RangeReader is a seekable view over a remote file. It downloads exactly the
// byte intervals its callers ask for and shares them between the container
// indexers and all FFmpeg processes used for one acquisition. The reader
// deliberately does not create a full local copy.
type RangeReader struct {
	URL        string
	ChunkSize  int64
	Client     *http.Client
	MaxRetries int
	// PrefetchChunks enables bounded look-ahead for sequential metadata reads.
	// It is intentionally opt-in: random frame reads must not download
	// neighbouring media chunks that may never be used.
	PrefetchChunks int
	// MaxCacheBytes bounds the shared range cache. Zero selects the default.
	MaxCacheBytes int64

	mu          sync.Mutex
	pos         int64
	lastReadEnd int64
	size        int64
	flights     map[rangeSpan]*rangeFlight
	requests    int64
	bytes       int64
	prefetchWG  sync.WaitGroup
	refreshURL  func(context.Context) (string, error)
	refreshMu   sync.Mutex

	cache rangeCache
}

// rangeSpan is an inclusive byte interval.
type rangeSpan struct {
	start int64
	end   int64
}

func (s rangeSpan) length() int64 { return s.end - s.start + 1 }

type rangeFlight struct {
	done chan struct{}
	data []byte
	err  error
}

type RangeStats struct {
	Requests        int64
	BytesDownloaded int64
	// CachedBlocks counts the disjoint byte intervals held in the cache. They
	// are no longer aligned chunks, so one block is one fetched interval.
	CachedBlocks int
	CachedBytes  int64
	Size         int64
}

func NewHTTPRangeReader(rawURL string, size int64, client *http.Client) *RangeReader {
	if client == nil {
		client = httpclient.NewDirectClient(45 * time.Second)
	}
	reader := &RangeReader{
		URL:           strings.TrimSpace(rawURL),
		ChunkSize:     DefaultChunkSize,
		Client:        client,
		MaxRetries:    3,
		MaxCacheBytes: DefaultMaxCacheBytes,
		lastReadEnd:   -1,
		size:          size,
		flights:       make(map[rangeSpan]*rangeFlight),
	}
	reader.cache.maxBytes = DefaultMaxCacheBytes
	return reader
}

// SetMaxCacheBytes bounds how much downloaded data stays resident. Eviction is
// least-recently-used, so the metadata and the samples still in use survive
// while speculative media bytes are dropped first.
func (r *RangeReader) SetMaxCacheBytes(bytes int64) {
	if bytes <= 0 {
		bytes = DefaultMaxCacheBytes
	}
	r.mu.Lock()
	r.MaxCacheBytes = bytes
	r.mu.Unlock()
	r.cache.setLimit(bytes)
}

// SetPrefetchChunks changes the bounded sequential look-ahead window. Calls
// should be made while no new sequential reader is using the RangeReader.
func (r *RangeReader) SetPrefetchChunks(count int) {
	if count < 0 {
		count = 0
	}
	if count > 8 {
		count = 8
	}
	r.mu.Lock()
	r.PrefetchChunks = count
	r.mu.Unlock()
}

// WaitPrefetch waits for already scheduled look-ahead requests. It is used
// before switching from metadata parsing to random frame reads.
func (r *RangeReader) WaitPrefetch() {
	r.prefetchWG.Wait()
}

// SetURLRefresher installs a callback for expiring signed URLs. Cloud drive
// download URLs can return 403 while the file itself is still valid. The
// callback is invoked at most once per failed range request generation and is
// serialized so concurrent frame workers share one fresh URL.
func (r *RangeReader) SetURLRefresher(refresh func(context.Context) (string, error)) {
	r.mu.Lock()
	r.refreshURL = refresh
	r.mu.Unlock()
}

func (r *RangeReader) Size() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.size
}

func (r *RangeReader) Stats() RangeStats {
	blocks, cachedBytes := r.cache.stats()
	r.mu.Lock()
	defer r.mu.Unlock()
	return RangeStats{Requests: r.requests, BytesDownloaded: r.bytes, CachedBlocks: blocks, CachedBytes: cachedBytes, Size: r.size}
}

// FetchRange returns exactly the bytes in [offset, offset+length). Cached
// intervals are reused, only the missing intervals are requested, and
// identical concurrent requests are merged into a single one. This is the
// entry point for random access such as container metadata and individual
// keyframe samples: unlike a sequential read it never widens the request to
// the aligned chunk grid, so a few hundred bytes of metadata do not cost a
// whole 4 MiB chunk.
func (r *RangeReader) FetchRange(ctx context.Context, offset, length int64) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if offset < 0 {
		return nil, fmt.Errorf("negative read offset %d", offset)
	}
	if length <= 0 {
		return nil, fmt.Errorf("non-positive range length %d", length)
	}
	if length > maxRangeLength {
		return nil, fmt.Errorf("range length %d exceeds the %d byte limit", length, maxRangeLength)
	}
	if err := r.ensureSize(ctx); err != nil {
		return nil, err
	}
	if size := r.Size(); size > 0 {
		if offset >= size {
			return nil, io.EOF
		}
		if length > size-offset {
			length = size - offset
		}
	}
	data := make([]byte, length)
	span := rangeSpan{start: offset, end: offset + length - 1}
	copied, err := r.fill(ctx, span, span, data)
	if err != nil {
		return nil, err
	}
	if int64(copied) < length {
		return nil, io.ErrUnexpectedEOF
	}
	return data, nil
}

// ensureSize learns the remote length with a one-byte request when the source
// did not report it, so tail-relative probes and clamping stay possible.
func (r *RangeReader) ensureSize(ctx context.Context) error {
	if r.Size() > 0 {
		return nil
	}
	if _, err := r.loadRange(ctx, 0, 0); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if r.Size() <= 0 {
		return errors.New("remote size is unavailable")
	}
	return nil
}

func (r *RangeReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	pos := r.pos
	r.mu.Unlock()
	n, err := r.ReadAtContext(context.Background(), p, pos)
	if n > 0 {
		r.mu.Lock()
		r.pos += int64(n)
		r.mu.Unlock()
	}
	return n, err
}

func (r *RangeReader) ReadAt(p []byte, offset int64) (int, error) {
	return r.ReadAtContext(context.Background(), p, offset)
}

func (r *RangeReader) ReadAtContext(ctx context.Context, p []byte, offset int64) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if offset < 0 {
		return 0, fmt.Errorf("negative read offset %d", offset)
	}
	if len(p) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	sequential := offset == r.lastReadEnd
	r.mu.Unlock()

	size := r.Size()
	if size > 0 && offset >= size {
		return 0, io.EOF
	}
	total := 0
	for total < len(p) {
		current := offset + int64(total)
		wantEnd := offset + int64(len(p)) - 1
		if size > 0 && wantEnd >= size {
			wantEnd = size - 1
		}
		if wantEnd < current {
			break
		}
		wanted := rangeSpan{start: current, end: wantEnd}
		fetch := r.alignSpan(wanted, sequential)
		copied, err := r.fill(ctx, fetch, wanted, p[total:total+int(wanted.length())])
		total += copied
		if err != nil {
			r.mu.Lock()
			r.lastReadEnd = -1
			r.mu.Unlock()
			return total, err
		}
		if copied == 0 {
			break
		}
		if sequential && fetch.length() >= r.effectiveChunkSize() && wantEnd == fetch.end {
			r.prefetchFrom(fetch.end + 1)
		}
		if wantEnd < fetch.end {
			break
		}
	}
	r.mu.Lock()
	r.lastReadEnd = offset + int64(total)
	r.mu.Unlock()
	if total < len(p) {
		return total, io.EOF
	}
	return total, nil
}

// alignSpan decides whether a read is widened to the aligned chunk grid.
// Sequential and large reads profit from one big request per hop and from
// sharing the cached block with the other FFmpeg processes of the same
// acquisition. Small random reads must stay exact: widening them is what made
// metadata probes and single keyframe samples download megabytes each.
func (r *RangeReader) alignSpan(span rangeSpan, sequential bool) rangeSpan {
	chunkSize := r.effectiveChunkSize()
	if chunkSize <= 0 {
		return span
	}
	if !sequential && span.length()*2 < chunkSize {
		return span
	}
	start := (span.start / chunkSize) * chunkSize
	end := ((span.end / chunkSize) * chunkSize) + chunkSize - 1
	if size := r.Size(); size > 0 && end >= size {
		end = size - 1
	}
	if end < span.end {
		return span
	}
	return rangeSpan{start: start, end: end}
}

func (r *RangeReader) effectiveChunkSize() int64 {
	r.mu.Lock()
	chunkSize := r.ChunkSize
	r.mu.Unlock()
	if chunkSize <= 0 {
		return DefaultChunkSize
	}
	return chunkSize
}

// fill caches the fetch span and copies the wanted span out of the cache. The
// wanted span must be contained in the fetch span. Fetching more than is
// copied is how sequential reads keep the aligned chunk grid, while missing
// intervals are computed against the cache so overlapping readers never
// download the same bytes twice.
func (r *RangeReader) fill(ctx context.Context, fetch, wanted rangeSpan, dst []byte) (int, error) {
	if int64(len(dst)) < wanted.length() {
		wanted.end = wanted.start + int64(len(dst)) - 1
	}
	if size := r.Size(); size > 0 {
		if fetch.start >= size || wanted.start >= size {
			return 0, io.EOF
		}
		if fetch.end >= size {
			fetch.end = size - 1
		}
		if wanted.end >= size {
			wanted.end = size - 1
		}
	}
	if wanted.end < wanted.start {
		return 0, io.EOF
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		lastErr = nil
		for _, span := range r.cache.missing(fetch.start, fetch.length()) {
			if _, err := r.loadRange(ctx, span.start, span.end); err != nil {
				lastErr = err
				break
			}
		}
		if lastErr != nil {
			if errors.Is(lastErr, ctx.Err()) {
				break
			}
			continue
		}
		copied, complete := r.cache.copyTo(wanted.start, wanted.length(), dst)
		if complete {
			return copied, nil
		}
		// A concurrent eviction dropped bytes that were just fetched.
	}
	copied, complete := r.cache.copyTo(wanted.start, wanted.length(), dst)
	if lastErr != nil {
		return copied, lastErr
	}
	if !complete {
		return copied, io.ErrUnexpectedEOF
	}
	return copied, nil
}

func (r *RangeReader) prefetchFrom(start int64) {
	r.mu.Lock()
	count := r.PrefetchChunks
	chunkSize := r.ChunkSize
	size := r.size
	r.mu.Unlock()
	if count <= 0 || chunkSize <= 0 {
		return
	}
	for index := 0; index < count; index++ {
		chunkStart := start + int64(index)*chunkSize
		if size > 0 && chunkStart >= size {
			break
		}
		chunkEnd := chunkStart + chunkSize - 1
		if size > 0 && chunkEnd >= size {
			chunkEnd = size - 1
		}
		r.prefetchWG.Add(1)
		go func(span rangeSpan) {
			defer r.prefetchWG.Done()
			_, _ = r.loadRange(context.Background(), span.start, span.end)
		}(rangeSpan{start: chunkStart, end: chunkEnd})
	}
}

func (r *RangeReader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.pos + offset
	case io.SeekEnd:
		if r.size <= 0 {
			return 0, errors.New("cannot seek from end before remote size is known")
		}
		next = r.size + offset
	default:
		return 0, fmt.Errorf("unsupported seek mode %d", whence)
	}
	if next < 0 {
		return 0, fmt.Errorf("negative seek position %d", next)
	}
	r.pos = next
	return next, nil
}

// loadRange returns the bytes of one inclusive interval, merging identical
// concurrent requests and reusing the shared cache. The returned slice aliases
// cached data and must be treated as read-only.
func (r *RangeReader) loadRange(ctx context.Context, start, end int64) ([]byte, error) {
	if end < start {
		return nil, io.EOF
	}
	if data, ok := r.cache.get(start, end-start+1); ok {
		return data, nil
	}
	span := rangeSpan{start: start, end: end}
	r.mu.Lock()
	if flight, ok := r.flights[span]; ok {
		r.mu.Unlock()
		select {
		case <-flight.done:
			return flight.data, flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &rangeFlight{done: make(chan struct{})}
	r.flights[span] = flight
	r.mu.Unlock()

	data, size, err := r.fetchBytes(ctx, start, end)
	r.mu.Lock()
	if err == nil && size > 0 {
		r.size = size
	}
	flight.data = data
	flight.err = err
	delete(r.flights, span)
	close(flight.done)
	r.mu.Unlock()
	if err == nil {
		r.cache.insert(start, data)
	}
	return data, err
}

// fetchBytes issues one HTTP range request for an exact interval. Retries stay
// limited to throttling, server errors, and expired signed URLs.
func (r *RangeReader) fetchBytes(ctx context.Context, start, end int64) ([]byte, int64, error) {
	if strings.TrimSpace(r.currentURL()) == "" {
		return nil, 0, errors.New("remote source URL is empty")
	}
	if size := r.Size(); size > 0 {
		if start >= size {
			return nil, size, io.EOF
		}
		if end >= size {
			end = size - 1
		}
	}
	if end < start {
		return nil, r.Size(), io.EOF
	}

	maxRetries := r.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	var lastErr error
	refreshed := false
	for attempt := 0; attempt <= maxRetries; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.currentURL(), nil)
		if err != nil {
			return nil, 0, err
		}
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		request.Header.Set("Accept-Encoding", "identity")
		response, err := r.Client.Do(request)
		if err != nil {
			lastErr = err
		} else {
			data, size, readErr := r.readRangeResponse(response, start, end)
			if readErr == nil {
				r.mu.Lock()
				r.requests++
				r.bytes += int64(len(data))
				r.mu.Unlock()
				return data, size, nil
			}
			lastErr = readErr
			if (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) && !refreshed {
				refreshed = true
				if refreshErr := r.refreshSignedURL(ctx, request.URL.String()); refreshErr == nil {
					continue
				} else {
					lastErr = fmt.Errorf("refresh signed URL: %w", refreshErr)
				}
			}
			if response.StatusCode != http.StatusTooManyRequests && response.StatusCode < 500 {
				break
			}
		}
		if attempt < maxRetries {
			select {
			case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			}
		}
	}
	return nil, 0, fmt.Errorf("read remote range %d-%d: %w", start, end, lastErr)
}

func (r *RangeReader) currentURL() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.URL
}

func (r *RangeReader) refreshSignedURL(ctx context.Context, expiredURL string) error {
	r.mu.Lock()
	refresh := r.refreshURL
	r.mu.Unlock()
	if refresh == nil {
		return errors.New("signed URL refresher is not configured")
	}
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	// Another worker may have refreshed the URL while this request was in
	// flight. Reuse that URL instead of issuing another provider API call.
	if current := r.currentURL(); current != expiredURL && strings.TrimSpace(current) != "" {
		return nil
	}
	freshURL, err := refresh(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(freshURL) == "" {
		return errors.New("refresher returned an empty signed URL")
	}
	r.mu.Lock()
	r.URL = strings.TrimSpace(freshURL)
	r.mu.Unlock()
	return nil
}

func (r *RangeReader) readRangeResponse(response *http.Response, start, end int64) ([]byte, int64, error) {
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		if response.StatusCode == http.StatusOK {
			return nil, 0, ErrRangeUnsupported
		}
		return nil, 0, fmt.Errorf("remote range request returned HTTP %d", response.StatusCode)
	}
	contentRange := strings.TrimSpace(response.Header.Get("Content-Range"))
	rangeStart, rangeEnd, size, err := parseContentRange(contentRange)
	if err != nil {
		return nil, 0, err
	}
	if rangeStart != start || rangeEnd < rangeStart {
		return nil, 0, fmt.Errorf("remote range response %q does not match %d-%d", contentRange, start, end)
	}
	wanted := end - start + 1
	if rangeEnd-rangeStart+1 < wanted {
		wanted = rangeEnd - rangeStart + 1
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, wanted+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) != wanted {
		return nil, 0, fmt.Errorf("remote range returned %d bytes, expected %d", len(data), wanted)
	}
	return data, size, nil
}

func parseContentRange(value string) (int64, int64, int64, error) {
	parts := strings.Fields(value)
	if len(parts) != 2 || parts[0] != "bytes" {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range %q", value)
	}
	ranges := strings.Split(parts[1], "/")
	if len(ranges) != 2 {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range %q", value)
	}
	bounds := strings.Split(ranges[0], "-")
	if len(bounds) != 2 {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range %q", value)
	}
	start, err := strconv.ParseInt(bounds[0], 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range %q", value)
	}
	end, err := strconv.ParseInt(bounds[1], 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range %q", value)
	}
	size, err := strconv.ParseInt(ranges[1], 10, 64)
	if err != nil || size <= 0 {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range size %q", value)
	}
	return start, end, size, nil
}

// rangeCache stores the byte intervals that were already downloaded. Blocks
// stay sorted and non-overlapping, so one read can be assembled from several
// earlier requests and only the missing intervals are fetched again. The cache
// is bounded: least-recently-used blocks are dropped once the limit is
// exceeded, which keeps a long acquisition from pinning the whole movie.
type rangeCache struct {
	mu       sync.Mutex
	blocks   []*cacheBlock
	total    int64
	maxBytes int64
	clock    uint64
}

type cacheBlock struct {
	start int64
	data  []byte
	used  uint64
}

// end returns the exclusive end offset of the block.
func (b *cacheBlock) end() int64 { return b.start + int64(len(b.data)) }

func (c *rangeCache) setLimit(maxBytes int64) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxCacheBytes
	}
	c.mu.Lock()
	c.maxBytes = maxBytes
	c.evictLocked()
	c.mu.Unlock()
}

func (c *rangeCache) stats() (int, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.blocks), c.total
}

// searchLocked returns the index of the first block that could contain
// offset, i.e. the first block whose exclusive end is greater than offset.
func (c *rangeCache) searchLocked(offset int64) int {
	return sort.Search(len(c.blocks), func(index int) bool {
		return c.blocks[index].end() > offset
	})
}

func (c *rangeCache) touchLocked(block *cacheBlock) {
	c.clock++
	block.used = c.clock
}

// get returns the cached bytes of one interval without copying. Callers must
// treat the result as read-only because it aliases the shared cache.
func (c *rangeCache) get(offset, length int64) ([]byte, bool) {
	if length <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	index := c.searchLocked(offset)
	if index >= len(c.blocks) {
		return nil, false
	}
	block := c.blocks[index]
	if block.start > offset || block.end() < offset+length {
		return nil, false
	}
	c.touchLocked(block)
	from := offset - block.start
	return block.data[from : from+length], true
}

// missing lists the sub-intervals of [offset, offset+length) that are not
// cached yet, in ascending order.
func (c *rangeCache) missing(offset, length int64) []rangeSpan {
	if length <= 0 {
		return nil
	}
	end := offset + length
	c.mu.Lock()
	defer c.mu.Unlock()
	spans := make([]rangeSpan, 0, 2)
	cursor := offset
	for index := c.searchLocked(offset); index < len(c.blocks) && cursor < end; index++ {
		block := c.blocks[index]
		if block.start >= end {
			break
		}
		if block.start > cursor {
			spans = append(spans, rangeSpan{start: cursor, end: minInt64(block.start, end) - 1})
		}
		c.touchLocked(block)
		if block.end() > cursor {
			cursor = block.end()
		}
	}
	if cursor < end {
		spans = append(spans, rangeSpan{start: cursor, end: end - 1})
	}
	return spans
}

// copyTo copies one interval out of the cache and reports whether the interval
// was completely available.
func (c *rangeCache) copyTo(offset, length int64, dst []byte) (int, bool) {
	if length <= 0 {
		return 0, true
	}
	if int64(len(dst)) < length {
		length = int64(len(dst))
	}
	end := offset + length
	c.mu.Lock()
	defer c.mu.Unlock()
	written := 0
	cursor := offset
	for index := c.searchLocked(offset); index < len(c.blocks) && cursor < end; index++ {
		block := c.blocks[index]
		if block.start >= end {
			break
		}
		if block.start > cursor {
			return written, false
		}
		from := cursor - block.start
		to := minInt64(end, block.end()) - block.start
		copied := copy(dst[written:], block.data[from:to])
		written += copied
		cursor += int64(copied)
		c.touchLocked(block)
		if int64(copied) < to-from {
			return written, false
		}
	}
	return written, cursor >= end
}

// insert adds downloaded bytes. The new interval is clipped against blocks
// that appeared while it was in flight, which keeps the list sorted and
// non-overlapping without copying media data.
func (c *rangeCache) insert(offset int64, data []byte) {
	if len(data) == 0 {
		return
	}
	end := offset + int64(len(data))
	c.mu.Lock()
	pieces := make([]rangeSpan, 0, 2)
	cursor := offset
	for index := c.searchLocked(offset); index < len(c.blocks); index++ {
		block := c.blocks[index]
		if block.start >= end {
			break
		}
		if block.end() <= cursor {
			continue
		}
		if block.start > cursor {
			pieces = append(pieces, rangeSpan{start: cursor, end: block.start - 1})
		}
		if block.end() > cursor {
			cursor = block.end()
		}
		if cursor >= end {
			break
		}
	}
	if cursor < end {
		pieces = append(pieces, rangeSpan{start: cursor, end: end - 1})
	}
	for _, piece := range pieces {
		from := piece.start - offset
		c.insertLocked(piece.start, data[from:from+piece.length()])
	}
	c.evictLocked()
	c.mu.Unlock()
}

func (c *rangeCache) insertLocked(start int64, data []byte) {
	if len(data) == 0 {
		return
	}
	block := &cacheBlock{start: start, data: data}
	c.touchLocked(block)
	index := sort.Search(len(c.blocks), func(position int) bool {
		return c.blocks[position].start > start
	})
	c.blocks = append(c.blocks, nil)
	copy(c.blocks[index+1:], c.blocks[index:])
	c.blocks[index] = block
	c.total += int64(len(data))
}

// evictLocked drops least-recently-used blocks until the cache fits its limit.
// The last remaining block always survives, so a caller can never loop on an
// interval that is larger than the whole cache.
func (c *rangeCache) evictLocked() {
	if c.maxBytes <= 0 {
		return
	}
	for c.total > c.maxBytes && len(c.blocks) > 1 {
		oldest := 0
		for index := 1; index < len(c.blocks); index++ {
			if c.blocks[index].used < c.blocks[oldest].used {
				oldest = index
			}
		}
		c.total -= int64(len(c.blocks[oldest].data))
		c.blocks = append(c.blocks[:oldest], c.blocks[oldest+1:]...)
	}
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

// RangeProxy gives command-line FFmpeg a normal seekable HTTP URL while all
// reads are fulfilled by RangeReader. The proxy is bound to loopback and is
// intended to live only for one acquisition.
type RangeProxy struct {
	reader   *RangeReader
	listener net.Listener
	server   *http.Server
	url      string
	once     sync.Once
	closeErr error
}

func NewRangeProxy(reader *RangeReader) (*RangeProxy, error) {
	if reader == nil {
		return nil, errors.New("range proxy reader is nil")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen range proxy: %w", err)
	}
	proxy := &RangeProxy{reader: reader, listener: listener, url: "http://" + listener.Addr().String() + "/video"}
	proxy.server = &http.Server{Handler: http.HandlerFunc(proxy.serveHTTP)}
	go func() {
		_ = proxy.server.Serve(listener)
	}()
	return proxy, nil
}

func (p *RangeProxy) URL() string { return p.url }

func (p *RangeProxy) Close() error {
	if p == nil || p.server == nil {
		return nil
	}
	p.once.Do(func() {
		p.closeErr = p.server.Close()
	})
	return p.closeErr
}

func (p *RangeProxy) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	size := p.reader.Size()
	if size <= 0 {
		if _, err := p.reader.ReadAtContext(request.Context(), make([]byte, 1), 0); err != nil && !errors.Is(err, io.EOF) {
			http.Error(response, err.Error(), http.StatusBadGateway)
			return
		}
		size = p.reader.Size()
	}
	if size <= 0 {
		http.Error(response, "remote size is unavailable", http.StatusBadGateway)
		return
	}
	response.Header().Set("Accept-Ranges", "bytes")
	response.Header().Set("Content-Type", "application/octet-stream")

	start, end, partial, err := parseHTTPRange(request.Header.Get("Range"), size)
	if err != nil {
		response.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		response.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if !partial {
		start, end = 0, size-1
	}
	response.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if partial {
		response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		response.WriteHeader(http.StatusPartialContent)
	}
	if request.Method == http.MethodHead {
		return
	}
	p.stream(response, request, start, end)
}

func (p *RangeProxy) stream(response http.ResponseWriter, request *http.Request, start, end int64) {
	buffer := make([]byte, 256*1024)
	for offset := start; offset <= end; {
		want := int64(len(buffer))
		if remaining := end - offset + 1; remaining < want {
			want = remaining
		}
		n, err := p.reader.ReadAtContext(request.Context(), buffer[:want], offset)
		if n > 0 {
			if _, writeErr := response.Write(buffer[:n]); writeErr != nil {
				return
			}
			offset += int64(n)
		}
		if err != nil {
			return
		}
		if n == 0 {
			return
		}
	}
}

func parseHTTPRange(value string, size int64) (int64, int64, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, 0, false, nil
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value[6:], ",") {
		return 0, 0, false, errors.New("only one bytes range is supported")
	}
	bounds := strings.Split(strings.TrimSpace(value[6:]), "-")
	if len(bounds) != 2 {
		return 0, 0, false, errors.New("invalid bytes range")
	}
	if bounds[0] == "" {
		length, err := strconv.ParseInt(bounds[1], 10, 64)
		if err != nil || length <= 0 {
			return 0, 0, false, errors.New("invalid suffix range")
		}
		if length > size {
			length = size
		}
		return size - length, size - 1, true, nil
	}
	start, err := strconv.ParseInt(bounds[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, errors.New("invalid range start")
	}
	end := size - 1
	if bounds[1] != "" {
		end, err = strconv.ParseInt(bounds[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, false, errors.New("invalid range end")
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true, nil
}
