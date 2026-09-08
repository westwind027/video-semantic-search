package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// 1 MiB blocks: a throttled drive connection (~100 KB/s observed) fills
	// one in ~10s, so the file tail (Cues/moov) is ready quickly and a seek
	// never waits long for a single block. A 4 MiB block cost ~40s per block.
	streamCacheBlockSize = int64(1) << 20
	// Sequential playback is the common case: read ahead a few blocks so the
	// player buffer stays ahead of the render position.
	streamCachePrefetchBlocks = 8
	// Default cache budget; override with VIDEO_STREAM_CACHE_BYTES.
	streamCacheDefaultBytes = int64(2) << 30
	// The drive throttles a single connection hard (~100 KB/s observed) AND
	// rejects concurrent downloads with HTTP 403 once too many run in
	// parallel (observed live: 4+ concurrent fetches -> bursts of 403).
	// Three slots (browser + 2 prefetches) stay under that limit while still
	// doubling single-connection throughput. Raise via VIDEO_STREAM_CONCURRENCY
	// only if the account allows more.
	streamUpstreamDefault = 3
	// Prefetches may occupy at most this many upstream slots, leaving the
	// rest of the gate to the browser's own (higher priority) requests.
	streamPrefetchParallel = 2
	// The drive sometimes throttles by silently stalling a connection instead
	// of answering 403; a stalled fetch holds its gate slot forever and
	// deadlocks the whole pipeline. Abort reads that stay idle this long.
	streamUpstreamIdleTimeout = 45 * time.Second
)

// errGateBusy signals that the per-media upstream gate had no free slot, so an
// optional (prefetch) fetch was skipped instead of queueing.
var errGateBusy = errors.New("upstream gate busy")

// blockFetcher retrieves [offset, offset+length) from the upstream drive. A
// nonBlocking fetch gives up instead of waiting when the gate is full.
type blockFetcher func(ctx context.Context, offset, length int64, nonBlocking bool) (io.ReadCloser, error)

type cachedBlock struct {
	path     string
	size     int64
	lastUsed time.Time
}

type blockLoad struct {
	done chan struct{}
}

// streamCache keeps downloaded blocks of remote videos on disk so playback and
// re-seeks do not re-download the same ranges. Blocks live under
// <directory>/<mediaID>/block-XXXXXXXXXXXX and are evicted LRU-style once the
// total size exceeds the budget.
type streamCache struct {
	directory string // empty disables the cache
	blockSize int64
	maxBytes  int64

	mu         sync.Mutex
	blocks     map[string]*cachedBlock // "mediaID/blockIndex"
	loading    map[string]*blockLoad
	totalBytes int64

	// Prefetches must not starve browser requests: they may occupy at most
	// streamPrefetchParallel slots of the upstream gate.
	prefetchSlots chan struct{}

	// Prefetch chains must outlive the HTTP request that triggered them:
	// the request context is canceled as soon as the response is written,
	// which would kill the read-ahead while it is still filling blocks.
	prefetchCtx context.Context
}

// newStreamCache prepares the cache directory. A previous run's blocks are
// discarded: media may have been deleted and the size budget has to be
// rebuilt from live files anyway.
func newStreamCache(directory string, maxBytes int64) *streamCache {
	if directory == "" || maxBytes <= 0 {
		return &streamCache{}
	}
	_ = os.RemoveAll(directory)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return &streamCache{}
	}
	return &streamCache{
		directory:     directory,
		blockSize:     streamCacheBlockSize,
		maxBytes:      maxBytes,
		blocks:        make(map[string]*cachedBlock),
		loading:       make(map[string]*blockLoad),
		prefetchSlots: make(chan struct{}, streamPrefetchParallel),
		prefetchCtx:   context.Background(),
	}
}

// streamCacheConfig reads the cache configuration from the environment.
// VIDEO_STREAM_CACHE_DIR set to "off" (or an empty budget) disables caching.
func streamCacheConfig() (string, int64) {
	directory := strings.TrimSpace(os.Getenv("VIDEO_STREAM_CACHE_DIR"))
	if directory == "" {
		directory = "data/stream-cache"
	}
	if directory == "off" {
		return "", 0
	}
	maxBytes := streamCacheDefaultBytes
	if value := strings.TrimSpace(os.Getenv("VIDEO_STREAM_CACHE_BYTES")); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed > 0 {
			maxBytes = parsed
		}
	}
	return directory, maxBytes
}

func (c *streamCache) enabled() bool {
	return c != nil && c.directory != ""
}

func (c *streamCache) blockKey(mediaID string, index int64) string {
	return mediaID + "/" + strconv.FormatInt(index, 10)
}

// blockLength returns the real length of a block, which is short for the last
// block of the file.
func (c *streamCache) blockLength(index, size int64) int64 {
	length := c.blockSize
	if remaining := size - index*c.blockSize; remaining < length {
		length = remaining
	}
	return length
}

// reader returns a ReadCloser over [start, end] backed by the block cache.
func (c *streamCache) reader(ctx context.Context, mediaID string, size, start, end int64, fetch blockFetcher) io.ReadCloser {
	return &blockReader{cache: c, ctx: ctx, mediaID: mediaID, size: size, fetch: fetch, pos: start, end: end}
}

type blockReader struct {
	cache   *streamCache
	ctx     context.Context
	mediaID string
	size    int64
	fetch   blockFetcher
	pos     int64
	end     int64
	current io.ReadCloser
}

func (r *blockReader) Read(p []byte) (int, error) {
	for {
		if r.current != nil {
			read, err := r.current.Read(p)
			if read > 0 {
				r.pos += int64(read)
				return read, nil
			}
			_ = r.current.Close()
			r.current = nil
			if errors.Is(err, io.EOF) {
				continue
			}
			if err != nil {
				return read, err
			}
			continue
		}
		if r.pos > r.end {
			return 0, io.EOF
		}
		index := r.pos / r.cache.blockSize
		block, err := r.cache.openStreamingBlock(r.ctx, r.mediaID, index, r.size, r.fetch)
		if err != nil {
			return 0, err
		}
		if skip := r.pos - index*r.cache.blockSize; skip > 0 {
			if _, err := io.CopyN(io.Discard, block, skip); err != nil {
				_ = block.Close()
				return 0, fmt.Errorf("seek into cached block: %w", err)
			}
		}
		limit := r.end - r.pos + 1
		if blockRemaining := (index+1)*r.cache.blockSize - r.pos; blockRemaining < limit {
			limit = blockRemaining
		}
		r.current = newLimitedCloser(block, limit)
		go r.cache.startPrefetch(r.cache.prefetchCtx, r.mediaID, index+1, r.size, r.fetch)
	}
}

func (r *blockReader) Close() error {
	if r.current != nil {
		err := r.current.Close()
		r.current = nil
		return err
	}
	return nil
}

// limitedCloser caps reads at limit bytes and closes the underlying block file.
type limitedCloser struct {
	reader io.Reader
	closer io.Closer
}

func newLimitedCloser(file io.ReadCloser, limit int64) *limitedCloser {
	return &limitedCloser{reader: io.LimitReader(file, limit), closer: file}
}

func (l *limitedCloser) Read(p []byte) (int, error) { return l.reader.Read(p) }
func (l *limitedCloser) Close() error               { return l.closer.Close() }

// openBlock returns a file reader positioned at the start of the block,
// downloading it first when needed. Concurrent requests for the same block
// share one download. A nonBlocking caller gives up instead of queueing when
// the upstream gate is full.
func (c *streamCache) openBlock(ctx context.Context, mediaID string, index, size int64, fetch blockFetcher, nonBlocking bool) (io.ReadCloser, error) {
	if !c.enabled() {
		return nil, errors.New("stream cache is disabled")
	}
	key := c.blockKey(mediaID, index)
	length := c.blockLength(index, size)
	if length <= 0 {
		return nil, io.EOF
	}
	for attempt := 0; attempt < 2; attempt++ {
		c.mu.Lock()
		if block, ok := c.blocks[key]; ok {
			block.lastUsed = time.Now()
			c.mu.Unlock()
			file, err := os.Open(block.path)
			if err == nil {
				return file, nil
			}
			// The file was evicted between lookup and open; reload it.
			c.mu.Lock()
			delete(c.blocks, key)
			c.mu.Unlock()
			continue
		}
		if load, busy := c.loading[key]; busy {
			c.mu.Unlock()
			select {
			case <-load.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		load := &blockLoad{done: make(chan struct{})}
		c.loading[key] = load
		c.mu.Unlock()
		c.downloadBlock(ctx, mediaID, key, index, length, fetch, load, nonBlocking)
	}
	return nil, errors.New("stream cache: unable to load block")
}

// openStreamingBlock returns a reader that serves an uncached block directly
// from the upstream while writing it into the cache, so playback starts after
// one round trip instead of waiting for the whole block. The block becomes a
// regular cache entry once fully consumed.
func (c *streamCache) openStreamingBlock(ctx context.Context, mediaID string, index, size int64, fetch blockFetcher) (io.ReadCloser, error) {
	if !c.enabled() {
		return nil, errors.New("stream cache is disabled")
	}
	key := c.blockKey(mediaID, index)
	length := c.blockLength(index, size)
	if length <= 0 {
		return nil, io.EOF
	}
	c.mu.Lock()
	if block, ok := c.blocks[key]; ok {
		block.lastUsed = time.Now()
		c.mu.Unlock()
		return os.Open(block.path)
	}
	if _, busy := c.loading[key]; busy {
		c.mu.Unlock()
		// Another request is filling this block; fall back to the
		// download-then-read path so both share the same download.
		return c.openBlock(ctx, mediaID, index, size, fetch, false)
	}
	load := &blockLoad{done: make(chan struct{})}
	c.loading[key] = load
	c.mu.Unlock()

	// Fetch on the cache's own context: when the browser disconnects the
	// request context dies, but the detached background reader should still
	// finish this block into the cache. The idle timeout bounds its life.
	body, err := fetch(c.prefetchCtx, index*c.blockSize, length, false)
	if err != nil {
		c.mu.Lock()
		delete(c.loading, key)
		c.mu.Unlock()
		close(load.done)
		return nil, err
	}
	mediaDir := filepath.Join(c.directory, mediaID)
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		c.mu.Lock()
		delete(c.loading, key)
		c.mu.Unlock()
		close(load.done)
		return nil, err
	}
	temp, err := os.CreateTemp(mediaDir, "incoming-*")
	if err != nil {
		c.mu.Lock()
		delete(c.loading, key)
		c.mu.Unlock()
		close(load.done)
		_ = body.Close()
		return nil, err
	}
	return &streamTeeReader{
		cache: c, body: body, temp: temp,
		tempPath: temp.Name(), finalPath: filepath.Join(mediaDir, fmt.Sprintf("block-%012d", index)),
		key: key, load: load, length: length,
	}, nil
}

// downloadBlock fetches one block from upstream and moves it into the cache.
func (c *streamCache) downloadBlock(ctx context.Context, mediaID, key string, index, length int64, fetch blockFetcher, load *blockLoad, nonBlocking bool) {
	var err error
	defer func() {
		c.mu.Lock()
		delete(c.loading, key)
		c.mu.Unlock()
		close(load.done)
	}()
	body, err := fetch(ctx, index*c.blockSize, length, nonBlocking)
	if err != nil {
		return
	}
	defer body.Close()
	mediaDir := filepath.Join(c.directory, mediaID)
	if err = os.MkdirAll(mediaDir, 0o755); err != nil {
		return
	}
	temp, err := os.CreateTemp(mediaDir, "incoming-*")
	if err != nil {
		return
	}
	tempPath := temp.Name()
	written, copyErr := io.Copy(temp, body)
	closeErr := temp.Close()
	switch {
	case copyErr != nil:
		err = copyErr
	case closeErr != nil:
		err = closeErr
	case written != length:
		err = fmt.Errorf("上游只返回了 %d/%d 字节", written, length)
	}
	if err != nil {
		_ = os.Remove(tempPath)
		return
	}
	finalPath := filepath.Join(mediaDir, fmt.Sprintf("block-%012d", index))
	if err = os.Rename(tempPath, finalPath); err != nil {
		_ = os.Remove(tempPath)
		return
	}
	c.register(key, finalPath, written)
}

func (c *streamCache) register(key, path string, size int64) {
	c.mu.Lock()
	c.blocks[key] = &cachedBlock{path: path, size: size, lastUsed: time.Now()}
	c.totalBytes += size
	c.evictLocked()
	c.mu.Unlock()
}

// evictLocked drops least-recently-used blocks until the cache fits the budget.
func (c *streamCache) evictLocked() {
	if c.totalBytes <= c.maxBytes {
		return
	}
	keys := make([]string, 0, len(c.blocks))
	for key := range c.blocks {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		return c.blocks[keys[left]].lastUsed.Before(c.blocks[keys[right]].lastUsed)
	})
	for _, key := range keys {
		if c.totalBytes <= c.maxBytes {
			break
		}
		block := c.blocks[key]
		if err := os.Remove(block.path); err == nil {
			c.totalBytes -= block.size
		}
		delete(c.blocks, key)
	}
}

// startPrefetch fills upcoming blocks in the background so sequential playback
// never waits on the drive. Blocks download in parallel: drives throttle a
// single connection, and only the aggregate of several connections exceeds a
// typical video bitrate. One chain runs per served block read.
func (c *streamCache) startPrefetch(ctx context.Context, mediaID string, fromIndex, size int64, fetch blockFetcher) {
	if !c.enabled() {
		return
	}
	lastIndex := (size - 1) / c.blockSize
	for index := fromIndex; index < fromIndex+streamCachePrefetchBlocks && index <= lastIndex; index++ {
		blockIndex := index
		go func() {
			for attempt := 0; attempt < 4; attempt++ {
				if ctx.Err() != nil {
					return
				}
				key := c.blockKey(mediaID, blockIndex)
				c.mu.Lock()
				_, cached := c.blocks[key]
				_, busy := c.loading[key]
				c.mu.Unlock()
				if cached || busy {
					return
				}
				// Bound how many prefetches run at once so the browser's own
				// requests always find free upstream slots.
				select {
				case c.prefetchSlots <- struct{}{}:
					file, err := c.openBlock(ctx, mediaID, blockIndex, size, fetch, true)
					<-c.prefetchSlots
					if err == nil {
						_ = file.Close()
						return
					}
				case <-ctx.Done():
					return
				}
				// A transient busy gate or remote read failure should not
				// permanently lose a read-ahead block. Retry with a small
				// bounded backoff; browser reads remain higher priority.
				select {
				case <-time.After(time.Duration(attempt+1) * 10 * time.Millisecond):
				case <-ctx.Done():
					return
				}
			}
		}()
	}
}

// prefetchTail warms the blocks at the end of the file with blocking fetches:
// MKV/WebM keep their seek index (Cues) and many MP4 files keep moov there,
// so browsers read the head first and then jump to the tail before playing.
// Without this that first tail read would queue behind a 1-connection
// throttled download and stall startup for tens of seconds.
func (c *streamCache) prefetchTail(ctx context.Context, mediaID string, size int64, fetch blockFetcher) {
	if !c.enabled() {
		return
	}
	lastIndex := (size - 1) / c.blockSize
	from := lastIndex - streamPrefetchParallel + 1
	if from < 0 {
		from = 0
	}
	for index := from; index <= lastIndex; index++ {
		blockIndex := index
		go func() {
			if ctx.Err() != nil {
				return
			}
			key := c.blockKey(mediaID, blockIndex)
			c.mu.Lock()
			_, cached := c.blocks[key]
			_, busy := c.loading[key]
			c.mu.Unlock()
			if cached || busy {
				return
			}
			file, err := c.openBlock(ctx, mediaID, blockIndex, size, fetch, false)
			if err != nil {
				return
			}
			_ = file.Close()
		}()
	}
}

// streamTeeReader serves an uncached block straight from the upstream while
// writing the same bytes into a cache file. When the upstream ends normally
// the block is promoted into the cache; on early close or error it is
// discarded and the next reader starts over.
type streamTeeReader struct {
	cache     *streamCache
	body      io.ReadCloser
	temp      *os.File
	tempPath  string
	finalPath string
	key       string
	load      *blockLoad
	length    int64
	written   int64
	finished  bool
}

func (t *streamTeeReader) Read(p []byte) (int, error) {
	read, err := t.body.Read(p)
	if read > 0 {
		if _, writeErr := t.temp.Write(p[:read]); writeErr != nil {
			t.abort()
			return read, writeErr
		}
		t.written += int64(read)
	}
	if errors.Is(err, io.EOF) {
		t.finish()
		return read, io.EOF
	}
	if err != nil {
		t.abort()
		return read, err
	}
	return read, nil
}

// finish promotes the fully downloaded block into the cache.
func (t *streamTeeReader) finish() {
	if t.finished {
		return
	}
	t.finished = true
	_ = t.body.Close()
	_ = t.temp.Close()
	t.settle()
}

// abort discards a partially downloaded block.
func (t *streamTeeReader) abort() {
	if t.finished {
		return
	}
	t.finished = true
	_ = t.body.Close()
	_ = t.temp.Close()
	_ = os.Remove(t.tempPath)
	t.cache.mu.Lock()
	delete(t.cache.loading, t.key)
	t.cache.mu.Unlock()
	close(t.load.done)
}

// settle renames the temp file into place once the block is complete.
func (t *streamTeeReader) settle() {
	if t.written == t.length {
		if err := os.Rename(t.tempPath, t.finalPath); err == nil {
			t.cache.register(t.key, t.finalPath, t.written)
		} else {
			_ = os.Remove(t.tempPath)
		}
	} else {
		_ = os.Remove(t.tempPath)
	}
	t.cache.mu.Lock()
	delete(t.cache.loading, t.key)
	t.cache.mu.Unlock()
	close(t.load.done)
}

// detach hands the remaining upstream download to a background goroutine so
// the block still lands in the cache after the reader moves on. Readers stop
// at the byte they were asked for, which is usually the end of the block; the
// drive response keeps flowing into the cache without blocking playback.
func (t *streamTeeReader) detach() {
	if t.finished {
		return
	}
	t.finished = true
	go func() {
		copied, err := io.Copy(t.temp, t.body)
		t.written += copied
		_ = t.body.Close()
		_ = t.temp.Close()
		if err != nil {
			_ = os.Remove(t.tempPath)
			t.cache.mu.Lock()
			delete(t.cache.loading, t.key)
			t.cache.mu.Unlock()
			close(t.load.done)
			return
		}
		t.settle()
	}()
}

// Close is called when the reader moves past this block (or the request
// ends); the upstream download keeps filling the cache in the background.
func (t *streamTeeReader) Close() error {
	t.detach()
	return nil
}

// gatedBody keeps the per-media upstream gate held until the response body is
// fully consumed, so the gate still bounds concurrent drive connections.
type gatedBody struct {
	io.ReadCloser
	release func()
}

func (b *gatedBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

// blockFetcher returns a closure that fetches ranges from the drive with the
// usual signed-URL refresh-on-401/403 handling.
func (p *streamProxy) blockFetcher(mediaID, driveID, fileID string) blockFetcher {
	return func(ctx context.Context, offset, length int64, nonBlocking bool) (io.ReadCloser, error) {
		gate := p.gate(mediaID)
		var release func()
		if nonBlocking {
			select {
			case gate <- struct{}{}:
				release = func() { <-gate }
			default:
				return nil, errGateBusy
			}
		} else {
			select {
			case gate <- struct{}{}:
				release = func() { <-gate }
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		requestCtx, cancel := context.WithCancel(ctx)
		entry, err := p.resolve(ctx, mediaID, driveID, fileID, false)
		if err != nil {
			cancel()
			release()
			return nil, err
		}
		for attempt := 0; attempt < 2; attempt++ {
			upstreamRequest, err := http.NewRequestWithContext(requestCtx, http.MethodGet, entry.url, nil)
			if err != nil {
				cancel()
				release()
				return nil, fmt.Errorf("构造云盘请求失败: %w", err)
			}
			upstreamRequest.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
			resp, err := p.client.Do(upstreamRequest)
			if err != nil {
				cancel()
				if attempt == 0 {
					log.Printf("stream %s: 上游请求失败（%v），刷新签名 URL 重试", mediaID, err)
					if refreshed, refreshErr := p.resolve(ctx, mediaID, driveID, fileID, true); refreshErr == nil {
						entry = refreshed
						requestCtx, cancel = context.WithCancel(ctx)
						continue
					}
				}
				release()
				return nil, err
			}
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				cancel()
				log.Printf("stream %s: 云盘返回 HTTP %d（可能并发限流），刷新签名 URL 重试", mediaID, resp.StatusCode)
				if attempt == 0 {
					if refreshed, refreshErr := p.resolve(ctx, mediaID, driveID, fileID, true); refreshErr == nil {
						entry = refreshed
						requestCtx, cancel = context.WithCancel(ctx)
						continue
					}
				}
				release()
				return nil, fmt.Errorf("云盘拒绝了播放请求（HTTP %d），请重新登录阿里云盘", resp.StatusCode)
			}
			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				cancel()
				release()
				return nil, fmt.Errorf("云盘返回了 HTTP %d", resp.StatusCode)
			}
			// On success the cancel func is handed to the idle-timeout body,
			// which owns the upstream request lifecycle from now on.
			return newIdleTimeoutBody(&gatedBody{ReadCloser: resp.Body, release: release}, cancel), nil
		}
		cancel()
		release()
		return nil, errors.New("云盘请求重试次数耗尽")
	}
}

// idleTimeoutBody aborts the upstream request when no bytes arrive for
// streamUpstreamIdleTimeout; drives throttle by stalling connections, and a
// stalled body would otherwise hold its gate slot forever.
type idleTimeoutBody struct {
	io.ReadCloser
	cancel   context.CancelFunc
	watchdog *time.Timer
	mu       sync.Mutex
}

func newIdleTimeoutBody(body io.ReadCloser, cancel context.CancelFunc) *idleTimeoutBody {
	reader := &idleTimeoutBody{ReadCloser: body, cancel: cancel}
	reader.watchdog = time.AfterFunc(streamUpstreamIdleTimeout, cancel)
	return reader
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	b.watchdog.Reset(streamUpstreamIdleTimeout)
	b.mu.Unlock()
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.mu.Lock()
		b.watchdog.Stop()
		b.mu.Unlock()
	}
	return n, err
}

func (b *idleTimeoutBody) Close() error {
	b.mu.Lock()
	b.watchdog.Stop()
	b.mu.Unlock()
	b.cancel()
	return b.ReadCloser.Close()
}

// parseStreamRange resolves a browser Range header into an inclusive byte
// interval against a known file size. ok=false means the range is not
// satisfiable (416); an absent or unsupported header serves the whole file.
func parseStreamRange(header string, size int64) (start, end int64, status int, ok bool) {
	if strings.TrimSpace(header) == "" || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(header)), "bytes=") {
		return 0, size - 1, http.StatusOK, true
	}
	spec := strings.TrimSpace(strings.TrimSpace(header)[len("bytes="):])
	if strings.Contains(spec, ",") {
		return 0, 0, 0, false
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, 0, false
	}
	if dash == 0 {
		suffix, err := strconv.ParseInt(spec[1:], 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, 0, false
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, size - 1, http.StatusPartialContent, true
	}
	start, err := strconv.ParseInt(spec[:dash], 10, 64)
	if err != nil || start < 0 {
		return 0, 0, 0, false
	}
	end = size - 1
	if tail := strings.TrimSpace(spec[dash+1:]); tail != "" {
		if end, err = strconv.ParseInt(tail, 10, 64); err != nil || end < start {
			return 0, 0, 0, false
		}
		if end > size-1 {
			end = size - 1
		}
	}
	if start >= size {
		return 0, 0, 0, false
	}
	return start, end, http.StatusPartialContent, true
}
