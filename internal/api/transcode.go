package api

// Proxying for the drive's live-transcoded HLS renditions. The CDN signs the
// m3u8/segment URLs against a fixed Referer
// (https://www.aliyundrive.com/, verified live) and rejects requests without
// it. Browsers cannot set Referer on XHR, so hls.js must load everything
// through this same-origin proxy, which adds the required headers.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"video-semantic-search/internal/alipan"
)

const (
	transcodeReferer   = "https://www.aliyundrive.com/"
	transcodeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"
	// Signed play-info URLs live for ~1h; refresh well before that.
	transcodeURLTTL = 10 * time.Minute
)

var transcodeClient = &http.Client{Timeout: 2 * time.Minute}

// transcodeEntry caches the signed CDN playlist URL per media.
type transcodeEntry struct {
	url     string
	expires time.Time
}

// transcodeSourceURL resolves (and caches) the signed CDN playlist URL for a
// cloud media, retriggering the live-transcoding API when expired.
func (s *Server) transcodeSourceURL(ctx context.Context, mediaID string) (string, error) {
	s.transcodeMu.Lock()
	if entry, ok := s.transcodeURLs[mediaID]; ok && time.Now().Before(entry.expires) {
		url := entry.url
		s.transcodeMu.Unlock()
		return url, nil
	}
	s.transcodeMu.Unlock()

	media, ok := s.store.GetMedia(mediaID)
	if !ok {
		return "", fmt.Errorf("media not found")
	}
	metadata := media.Metadata
	driveID, _ := metadata["remote_drive_id"].(string)
	fileID, _ := metadata["remote_file_id"].(string)
	if strings.TrimSpace(driveID) == "" || strings.TrimSpace(fileID) == "" {
		return "", fmt.Errorf("仅支持阿里云盘媒体的转码播放")
	}
	info, err := s.alipan.GetVideoPreviewPlayInfo(ctx, driveID, fileID)
	if err != nil {
		if err == alipan.ErrTranscodingPending {
			return "", fmt.Errorf("云端转码尚未完成")
		}
		return "", err
	}
	if strings.TrimSpace(info.URL) == "" {
		return "", fmt.Errorf("转码接口未返回播放地址")
	}
	s.transcodeMu.Lock()
	s.transcodeURLs[mediaID] = transcodeEntry{url: info.URL, expires: time.Now().Add(transcodeURLTTL)}
	s.transcodeMu.Unlock()
	return info.URL, nil
}

// fetchCDNWithHeaders GETs a CDN resource with the headers its signature
// expects (Referer + a browser-like User-Agent).
func fetchCDNWithHeaders(ctx context.Context, target string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Referer", transcodeReferer)
	request.Header.Set("User-Agent", transcodeUserAgent)
	return transcodeClient.Do(request)
}

// handleTranscodePlaylist serves the transcoding m3u8 with every referenced
// URI rewritten to the /transcode/proxy passthrough so the browser never hits
// the referer-guarded CDN directly.
func (s *Server) handleTranscodePlaylist(response http.ResponseWriter, request *http.Request, rawMediaID string) {
	mediaID, err := url.PathUnescape(rawMediaID)
	if err != nil || mediaID == "" || strings.Contains(mediaID, "/") {
		http.NotFound(response, request)
		return
	}
	if s.alipan == nil {
		writeError(response, http.StatusServiceUnavailable, fmt.Errorf("未配置阿里云盘连接"))
		return
	}
	cdnURL, err := s.transcodeSourceURL(request.Context(), mediaID)
	if err != nil {
		writeError(response, http.StatusBadGateway, err)
		return
	}
	rewritten, isPlaylist, err := s.proxyHLSResource(request.Context(), mediaID, cdnURL)
	if err != nil {
		writeError(response, http.StatusBadGateway, err)
		return
	}
	_ = isPlaylist // the source URL is always the playlist
	response.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(rewritten)
}

// handleTranscodeProxy streams one CDN resource (segment or sub-playlist).
// A response that turns out to be a playlist is rewritten recursively; media
// segments pass through untouched.
func (s *Server) handleTranscodeProxy(response http.ResponseWriter, request *http.Request, rawMediaID string) {
	mediaID, err := url.PathUnescape(rawMediaID)
	if err != nil || mediaID == "" || strings.Contains(mediaID, "/") {
		http.NotFound(response, request)
		return
	}
	target := request.URL.Query().Get("u")
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "https" || !isTranscodeCDNHost(parsed.Hostname()) {
		writeError(response, http.StatusBadRequest, fmt.Errorf("非法的分片地址"))
		return
	}
	body, isPlaylist, err := s.proxyHLSResource(request.Context(), mediaID, target)
	if err != nil {
		writeError(response, http.StatusBadGateway, err)
		return
	}
	contentType := "video/mp2t"
	if isPlaylist {
		contentType = "application/vnd.apple.mpegurl"
	}
	response.Header().Set("Content-Type", contentType)
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(body)
}

// proxyHLSResource fetches one CDN resource; if the payload is itself an m3u8
// (master playlists nest variant playlists) its URIs are rewritten to the
// proxy as well. The second result reports whether the payload is a playlist.
func (s *Server) proxyHLSResource(ctx context.Context, mediaID, target string) ([]byte, bool, error) {
	resp, err := fetchCDNWithHeaders(ctx, target)
	if err != nil {
		return nil, false, fmt.Errorf("获取转码资源失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("转码 CDN 返回 HTTP %d", resp.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, false, fmt.Errorf("读取转码资源失败: %w", err)
	}
	isPlaylist := strings.HasPrefix(strings.TrimSpace(string(payload)), "#EXTM3U") || strings.HasSuffix(target, ".m3u8")
	if !isPlaylist {
		return payload, false, nil
	}
	base := target[:strings.LastIndex(target, "/")+1]
	proxyPrefix := "/v1/media/" + url.PathEscape(mediaID) + "/transcode/proxy?u="
	return rewriteHLSPlaylist(payload, base, proxyPrefix), true, nil
}

// rewriteHLSPlaylist replaces every non-comment URI line with a proxy URL.
// Comment tags are passed through; the playlists served by live_transcoding
// carry no EXT-X-KEY/EXT-X-MAP URIs.
func rewriteHLSPlaylist(payload []byte, baseURL, proxyPrefix string) []byte {
	lines := strings.Split(string(payload), "\n")
	for index, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "http://") && !strings.HasPrefix(line, "https://") {
			line = baseURL + line
		}
		lines[index] = proxyPrefix + url.QueryEscape(line)
	}
	return []byte(strings.Join(lines, "\n"))
}

// isTranscodeCDNHost restricts the proxy to the drive's CDN hosts so it
// cannot be abused as a general SSRF relay.
func isTranscodeCDNHost(host string) bool {
	return strings.HasSuffix(host, "aliyundrive.net") ||
		strings.HasSuffix(host, "alipan.com") ||
		strings.HasSuffix(host, "aliyundrive.com") ||
		// Some files are transcoded onto the .cloud CDN instead (e.g.
		// video-preview-v6.aliyundrive.cloud); without this every segment of
		// such a file is rejected here and playback falls back to the
		// throttled original stream.
		strings.HasSuffix(host, "aliyundrive.cloud")
}
