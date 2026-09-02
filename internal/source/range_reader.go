package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"video-semantic-search/internal/httpclient"
)

var ErrRangeUnsupported = errors.New("remote source does not support HTTP range requests")

// RangeReader is a seekable view over a remote file. It fetches aligned byte
// chunks on demand and shares them between all FFmpeg processes used for one
// acquisition. The reader deliberately does not create a full local copy.
type RangeReader struct {
	URL        string
	ChunkSize  int64
	Client     *http.Client
	MaxRetries int
	// PrefetchChunks enables bounded look-ahead for sequential metadata reads.
	// It is intentionally opt-in: random frame reads must not download
	// neighbouring media chunks that may never be used.
	PrefetchChunks int

	mu          sync.Mutex
	pos         int64
	lastReadEnd int64
	size        int64
	chunks      map[int64][]byte
	flights     map[int64]*rangeChunkFlight
	requests    int64
	bytes       int64
	prefetchWG  sync.WaitGroup
	refreshURL  func(context.Context) (string, error)
	refreshMu   sync.Mutex
}

type rangeChunkFlight struct {
	done chan struct{}
	data []byte
	err  error
}

type RangeStats struct {
	Requests        int64
	BytesDownloaded int64
	CachedChunks    int
	Size            int64
}

func NewHTTPRangeReader(rawURL string, size int64, client *http.Client) *RangeReader {
	if client == nil {
		client = httpclient.NewDirectClient(45 * time.Second)
	}
	return &RangeReader{
		URL:         strings.TrimSpace(rawURL),
		ChunkSize:   4 << 20,
		Client:      client,
		MaxRetries:  3,
		lastReadEnd: -1,
		size:        size,
		chunks:      make(map[int64][]byte),
		flights:     make(map[int64]*rangeChunkFlight),
	}
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
// callback is invoked at most once per failed chunk request generation and is
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
	r.mu.Lock()
	defer r.mu.Unlock()
	return RangeStats{Requests: r.requests, BytesDownloaded: r.bytes, CachedChunks: len(r.chunks), Size: r.size}
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
		chunkStart := r.chunkStart(current)
		chunk, err := r.loadChunk(ctx, chunkStart)
		if err != nil {
			r.mu.Lock()
			r.lastReadEnd = -1
			r.mu.Unlock()
			return total, err
		}
		within := current - chunkStart
		if within < 0 || within >= int64(len(chunk)) {
			return total, fmt.Errorf("range reader returned invalid chunk at %d", current)
		}
		copied := copy(p[total:], chunk[within:])
		total += copied
		if sequential && copied > 0 && within+int64(copied) >= int64(len(chunk)) && int64(len(chunk)) >= r.ChunkSize {
			r.prefetchFrom(chunkStart + r.ChunkSize)
		}
		if copied == 0 || int64(len(chunk)) < r.ChunkSize {
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
		r.prefetchWG.Add(1)
		go func(start int64) {
			defer r.prefetchWG.Done()
			_, _ = r.loadChunk(context.Background(), start)
		}(chunkStart)
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

func (r *RangeReader) chunkStart(offset int64) int64 {
	chunkSize := r.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 4 << 20
	}
	return (offset / chunkSize) * chunkSize
}

func (r *RangeReader) loadChunk(ctx context.Context, start int64) ([]byte, error) {
	r.mu.Lock()
	if cached, ok := r.chunks[start]; ok {
		r.mu.Unlock()
		return cached, nil
	}
	if flight, ok := r.flights[start]; ok {
		r.mu.Unlock()
		select {
		case <-flight.done:
			return flight.data, flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &rangeChunkFlight{done: make(chan struct{})}
	r.flights[start] = flight
	r.mu.Unlock()

	data, size, err := r.fetchChunk(ctx, start)
	r.mu.Lock()
	if err == nil {
		r.chunks[start] = data
		if size > 0 {
			r.size = size
		}
	}
	flight.data = data
	flight.err = err
	delete(r.flights, start)
	close(flight.done)
	r.mu.Unlock()
	return data, err
}

func (r *RangeReader) fetchChunk(ctx context.Context, start int64) ([]byte, int64, error) {
	if strings.TrimSpace(r.currentURL()) == "" {
		return nil, 0, errors.New("remote source URL is empty")
	}
	chunkSize := r.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 4 << 20
	}
	end := start + chunkSize - 1
	if size := r.Size(); size > 0 && end >= size {
		end = size - 1
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
