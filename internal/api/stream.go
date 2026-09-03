package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"video-semantic-search/internal/alipan"
	"video-semantic-search/internal/httpclient"
	"video-semantic-search/internal/source"
)

// streamResolver abstracts the short-lived download-URL resolution for remote
// videos so tests can replace the alipan connector with a fake. *alipan.Manager
// satisfies it in production.
type streamResolver interface {
	ResolveVideo(ctx context.Context, driveID, fileID string) (source.Video, error)
}

const (
	// Signed drive URLs stay valid for hours; a short TTL keeps playback from
	// calling the drive API on every seek while still expiring before the URL.
	streamURLCacheTTL = 10 * time.Minute
)

// streamUpstreamConcurrency: drives throttle a single connection (~100 KB/s
// observed), so playback needs several parallel connections. Override with
// VIDEO_STREAM_CONCURRENCY.
func streamUpstreamConcurrency() int {
	if value := strings.TrimSpace(os.Getenv("VIDEO_STREAM_CONCURRENCY")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			return parsed
		}
	}
	return streamUpstreamDefault
}

type cachedStreamURL struct {
	url      string
	name     string
	size     int64
	expireAt time.Time
}

// streamProxy turns browser Range requests into authenticated upstream
// requests. Signed URLs are cached briefly and refreshed as soon as the drive
// reports 401/403, and each media has a small admission gate.
type streamProxy struct {
	resolver streamResolver
	client   *http.Client

	mu        sync.Mutex
	urls      map[string]cachedStreamURL
	admission map[string]chan struct{}
}

func newStreamProxy(resolver streamResolver) *streamProxy {
	return &streamProxy{
		resolver:  resolver,
		client:    httpclient.NewDirectClient(0),
		urls:      make(map[string]cachedStreamURL),
		admission: make(map[string]chan struct{}),
	}
}

func (p *streamProxy) resolve(ctx context.Context, mediaID, driveID, fileID string, force bool) (cachedStreamURL, error) {
	p.mu.Lock()
	cached, ok := p.urls[mediaID]
	p.mu.Unlock()
	if ok && !force && time.Now().Before(cached.expireAt) {
		return cached, nil
	}
	video, err := p.resolver.ResolveVideo(ctx, driveID, fileID)
	if err != nil {
		// A stale signed URL is usually still playable; prefer it over
		// failing the request when the drive API hiccups.
		if ok && !force {
			return cached, nil
		}
		return cachedStreamURL{}, err
	}
	entry := cachedStreamURL{url: video.URL, name: video.Name, size: video.Size, expireAt: time.Now().Add(streamURLCacheTTL)}
	p.mu.Lock()
	p.urls[mediaID] = entry
	p.mu.Unlock()
	return entry, nil
}

func (p *streamProxy) gate(mediaID string) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	gate, ok := p.admission[mediaID]
	if !ok {
		gate = make(chan struct{}, streamUpstreamConcurrency())
		p.admission[mediaID] = gate
	}
	return gate
}

// handleMediaStream serves the original video for in-browser playback.
// Local files are streamed straight from disk; aliyun drive videos are
// proxied so the browser never sees signed URLs or handles token refresh.
func (s *Server) handleMediaStream(response http.ResponseWriter, request *http.Request, rawMediaID string) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		methodNotAllowed(response)
		return
	}
	mediaID, err := url.PathUnescape(rawMediaID)
	if err != nil || mediaID == "" || strings.Contains(mediaID, "/") {
		http.NotFound(response, request)
		return
	}
	media, ok := s.store.GetMedia(mediaID)
	if !ok {
		writeError(response, http.StatusNotFound, fmt.Errorf("media not found"))
		return
	}
	metadata := media.Metadata
	driveID, _ := metadata["remote_drive_id"].(string)
	fileID, _ := metadata["remote_file_id"].(string)
	if strings.TrimSpace(driveID) != "" && strings.TrimSpace(fileID) != "" {
		s.serveRemoteStream(response, request, mediaID, driveID, fileID)
		return
	}
	localPath, _ := metadata["local_path"].(string)
	if strings.TrimSpace(localPath) == "" {
		writeError(response, http.StatusConflict, fmt.Errorf("该媒体没有可播放的本地文件或云盘来源"))
		return
	}
	s.serveLocalStream(response, request, localPath)
}

// handleMediaTranscode returns the drive-side live-transcoded HLS play URL
// (m3u8) for a cloud-drive media. Transcoded renditions stream from the
// drive's CDN without the per-connection throttling that hits original-file
// downloads, decode natively in browsers (H.264/AAC), and seek freely, so
// the player prefers this path and falls back to /stream only on failure.
func (s *Server) handleMediaTranscode(response http.ResponseWriter, request *http.Request, rawMediaID string) {
	if s.alipan == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("未配置阿里云盘连接"))
		return
	}
	mediaID, err := url.PathUnescape(rawMediaID)
	if err != nil || mediaID == "" || strings.Contains(mediaID, "/") {
		http.NotFound(response, request)
		return
	}
	media, ok := s.store.GetMedia(mediaID)
	if !ok {
		writeError(response, http.StatusNotFound, fmt.Errorf("media not found"))
		return
	}
	metadata := media.Metadata
	driveID, _ := metadata["remote_drive_id"].(string)
	fileID, _ := metadata["remote_file_id"].(string)
	if strings.TrimSpace(driveID) == "" || strings.TrimSpace(fileID) == "" {
		writeError(response, http.StatusConflict, fmt.Errorf("仅支持阿里云盘媒体的转码播放"))
		return
	}
	info, err := s.alipan.GetVideoPreviewPlayInfo(request.Context(), driveID, fileID)
	if errors.Is(err, alipan.ErrTranscodingPending) {
		writeJSON(response, http.StatusAccepted, map[string]any{"status": "transcoding"})
		return
	}
	if err != nil {
		writeError(response, http.StatusBadGateway, err)
		return
	}
	// The browser cannot reach the referer-signed CDN directly (Referer is
	// a forbidden header for JS), so hand it our same-origin proxy playlist.
	writeJSON(response, http.StatusOK, map[string]any{
		"status":      "ready",
		"url":         "/v1/media/" + url.PathEscape(mediaID) + "/transcode/playlist",
		"template_id": info.TemplateID,
		"duration":    info.Duration,
	})
}

func (s *Server) serveLocalStream(response http.ResponseWriter, request *http.Request, localPath string) {
	file, err := os.Open(localPath)
	if err != nil {
		writeError(response, http.StatusNotFound, fmt.Errorf("打开本地视频失败: %w", err))
		return
	}
	defer file.Close()
	// ServeContent implements Range, If-Range and 206 responses so the
	// browser can seek natively without any extra client-side logic.
	http.ServeContent(response, request, filepath.Base(localPath), time.Time{}, file)
}

func (s *Server) serveRemoteStream(response http.ResponseWriter, request *http.Request, mediaID, driveID, fileID string) {
	if s.streams == nil || s.streams.resolver == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("阿里云盘 connector 未配置"))
		return
	}
	// Known-size GETs go through the disk block cache so playback and re-seeks
	// do not re-download the same ranges from the drive.
	if request.Method == http.MethodGet {
		if entry, err := s.streams.resolve(request.Context(), mediaID, driveID, fileID, false); err == nil {
			size := entry.size
			if size <= 0 {
				if media, ok := s.store.GetMedia(mediaID); ok {
					if remote, isNumber := media.Metadata["remote_size"].(float64); isNumber {
						size = int64(remote)
					}
				}
			}
			if size > 0 && s.cache.enabled() {
				s.serveCachedRemoteStream(response, request, mediaID, driveID, fileID, size, entry.name)
				return
			}
		}
	}
	proxy := s.streams
	gate := proxy.gate(mediaID)
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-request.Context().Done():
		return
	}
	entry, err := proxy.resolve(request.Context(), mediaID, driveID, fileID, false)
	if err != nil {
		writeError(response, http.StatusBadGateway, err)
		return
	}
	var upstream *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		upstreamRequest, err := http.NewRequestWithContext(request.Context(), request.Method, entry.url, nil)
		if err != nil {
			writeError(response, http.StatusBadGateway, fmt.Errorf("构造云盘请求失败: %w", err))
			return
		}
		if rangeHeader := request.Header.Get("Range"); rangeHeader != "" {
			upstreamRequest.Header.Set("Range", rangeHeader)
		}
		resp, err := proxy.client.Do(upstreamRequest)
		if err != nil {
			// The signed URL can expire between seeks; refresh once before
			// reporting the failure.
			if attempt == 0 {
				if refreshed, refreshErr := proxy.resolve(request.Context(), mediaID, driveID, fileID, true); refreshErr == nil {
					entry = refreshed
					continue
				}
			}
			writeError(response, http.StatusBadGateway, fmt.Errorf("请求云盘视频流失败: %w", err))
			return
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if attempt == 0 {
				if refreshed, refreshErr := proxy.resolve(request.Context(), mediaID, driveID, fileID, true); refreshErr == nil {
					entry = refreshed
					continue
				}
			}
			writeError(response, http.StatusBadGateway, fmt.Errorf("云盘拒绝了播放请求（HTTP %d），请重新登录阿里云盘", resp.StatusCode))
			return
		}
		upstream = resp
		break
	}
	if upstream == nil {
		return
	}
	defer upstream.Body.Close()
	switch upstream.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable:
		// 200 (whole file), 206 (seek range) and 416 (out-of-range seek)
		// are all meaningful answers for a media element.
	default:
		writeError(response, http.StatusBadGateway, fmt.Errorf("云盘返回了 HTTP %d", upstream.StatusCode))
		return
	}
	response.Header().Set("Content-Type", streamContentType(upstream.Header.Get("Content-Type"), entry.name))
	response.Header().Set("Accept-Ranges", "bytes")
	response.Header().Set("Cache-Control", "no-store")
	for _, header := range []string{"Content-Length", "Content-Range"} {
		if value := upstream.Header.Get(header); value != "" {
			response.Header().Set(header, value)
		}
	}
	response.WriteHeader(upstream.StatusCode)
	if request.Method == http.MethodHead {
		return
	}
	// Stream progressively and flush so the player can start before the
	// whole chunk is buffered server-side.
	flusher := http.NewResponseController(response)
	buffer := make([]byte, 64*1024)
	for {
		read, readErr := upstream.Body.Read(buffer)
		if read > 0 {
			if _, writeErr := response.Write(buffer[:read]); writeErr != nil {
				return
			}
			_ = flusher.Flush()
		}
		if readErr != nil {
			return
		}
	}
}

// serveCachedRemoteStream answers a browser Range request from the disk block
// cache, downloading missing blocks from the drive and reading ahead so
// sequential playback stays ahead of the render position.
func (s *Server) serveCachedRemoteStream(response http.ResponseWriter, request *http.Request, mediaID, driveID, fileID string, size int64, name string) {
	fetch := s.streams.blockFetcher(mediaID, driveID, fileID)
	// MKV/WebM keep their seek index (Cues) at the file tail: the browser
	// reads the head, then jumps to the tail before playing. Warm the tail
	// right away so that jump is answered from cache instead of waiting for
	// a throttled single-connection download.
	s.cache.prefetchTail(s.cache.prefetchCtx, mediaID, size, fetch)
	start, end, status, satisfiable := parseStreamRange(request.Header.Get("Range"), size)
	if !satisfiable {
		response.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		writeError(response, http.StatusRequestedRangeNotSatisfiable, fmt.Errorf("请求的范围超出视频大小"))
		return
	}
	response.Header().Set("Content-Type", streamContentType("", name))
	response.Header().Set("Accept-Ranges", "bytes")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if status == http.StatusPartialContent {
		response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}
	if request.Method == http.MethodHead {
		response.WriteHeader(status)
		return
	}
	reader := s.cache.reader(request.Context(), mediaID, size, start, end, fetch)
	defer reader.Close()
	response.WriteHeader(status)
	flusher := http.NewResponseController(response)
	buffer := make([]byte, 64*1024)
	position := start
	for retries := 0; ; {
		read, readErr := reader.Read(buffer)
		if read > 0 {
			if _, writeErr := response.Write(buffer[:read]); writeErr != nil {
				return
			}
			position += int64(read)
			_ = flusher.Flush()
		}
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) || position > end {
			return
		}
		if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
			// The browser disconnected; rebuilding a reader on a dead
			// request context can only fail again.
			return
		}
		// Transient upstream failures (throttling, dropped connections) must
		// not truncate a response the browser already accepted; rebuild the
		// reader at the current position instead.
		retries++
		if retries > 3 {
			return
		}
		log.Printf("stream %s: 上游读取中断于字节 %d（%v），重建 reader 续传", mediaID, position, readErr)
		reader.Close()
		reader = s.cache.reader(request.Context(), mediaID, size, position, end, fetch)
	}
}

func streamContentType(upstream, name string) string {
	if value := strings.TrimSpace(upstream); strings.HasPrefix(strings.ToLower(value), "video/") {
		return value
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".m4v", ".mov":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".ts":
		return "video/mp2t"
	}
	if value := strings.TrimSpace(upstream); value != "" {
		return value
	}
	if byExtension := mime.TypeByExtension(filepath.Ext(name)); byExtension != "" {
		return byExtension
	}
	return "application/octet-stream"
}
