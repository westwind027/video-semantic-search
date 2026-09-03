package alipan

// Web-session QR login against the aliyunpan passport endpoints. This is the
// browser-login token system (separate from the OpenAPI tokens): the passport
// issues a rotating refresh token, which we exchange via
// aliyunpan_web.GetAccessTokenFromRefreshToken to drive transcoded playback.
// Flow verified live: generate.do returns {ck, t, codeContent}; query.do polls
// qrCodeStatus NEW/SCANED/EXPIRED/CANCELED/CONFIRMED; on CONFIRMED the base64
// bizExt payload carries the refresh token.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tickstep/aliyunpan-api/aliyunpan_web"
)

const (
	webPassportGenerateURL = "https://passport.aliyundrive.com/newlogin/qrcode/generate.do?appName=aliyun_drive&fromSite=52&appEntrance=web&isMobile=false&lang=zh_CN&returnUrl=&bizParams=&_bx-v=2.0.31"
	webPassportQueryURL    = "https://passport.aliyundrive.com/newlogin/qrcode/query.do?appName=aliyun_drive&fromSite=52&_bx-v=2.0.31"
	// The passport QR session lives ~5 minutes per scan cycle.
	webLoginLifetime = 5 * time.Minute

	// Proactive web token renewal: the access token lasts ~2h; refresh well
	// ahead of expiry so playback never waits on a token round trip.
	webRefreshInterval = 10 * time.Minute
	webRefreshAhead    = 30 * time.Minute

	browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"
)

type webPassportGenerateResponse struct {
	Content struct {
		Data struct {
			Ck          string `json:"ck"`
			T           int64  `json:"t"`
			CodeContent string `json:"codeContent"`
		} `json:"data"`
	} `json:"content"`
}

type webPassportQueryResponse struct {
	Content struct {
		Data struct {
			QRCodeStatus string `json:"qrCodeStatus"`
			BizExt       string `json:"bizExt"`
		} `json:"data"`
	} `json:"content"`
}

// StartWebLogin creates a passport QR login session for the web token system.
func (m *Manager) StartWebLogin() (LoginStart, error) {
	if m == nil {
		return LoginStart{}, errors.New("阿里云盘 connector 未配置")
	}
	request, err := http.NewRequest(http.MethodGet, webPassportGenerateURL, nil)
	if err != nil {
		return LoginStart{}, fmt.Errorf("构造扫码请求: %w", err)
	}
	request.Header.Set("User-Agent", browserUserAgent)
	response, err := m.httpClient.Do(request)
	if err != nil {
		return LoginStart{}, fmt.Errorf("请求阿里云盘扫码接口: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return LoginStart{}, fmt.Errorf("读取扫码接口响应: %w", err)
	}
	var parsed webPassportGenerateResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return LoginStart{}, fmt.Errorf("解析扫码接口响应: %w", err)
	}
	data := parsed.Content.Data
	if strings.TrimSpace(data.CodeContent) == "" || strings.TrimSpace(data.Ck) == "" || data.T == 0 {
		return LoginStart{}, errors.New("扫码接口未返回二维码数据")
	}
	sessionID, err := randomID()
	if err != nil {
		return LoginStart{}, fmt.Errorf("创建登录会话: %w", err)
	}
	expiresAt := time.Now().Add(webLoginLifetime)
	m.mu.Lock()
	m.cleanupPendingLocked(time.Now())
	m.pending[sessionID] = pendingLogin{AuthorizeURL: data.CodeContent, ExpiresAt: expiresAt, Provider: "web", WebCk: data.Ck, WebT: data.T}
	m.mu.Unlock()
	return LoginStart{
		SessionID:    sessionID,
		AuthorizeURL: data.CodeContent,
		QRCodeURL:    "/v1/connectors/alipan/login/" + sessionID + "/qr",
		ExpiresAt:    expiresAt,
		Mode:         "web",
	}, nil
}

// pollWebLogin queries the passport QR status; on CONFIRMED it completes the
// web login (exchange + persist + activate the web client).
func (m *Manager) pollWebLogin(ctx context.Context, sessionID string, pending pendingLogin) (LoginStatus, error) {
	form := url.Values{}
	form.Set("t", strconv.FormatInt(pending.WebT, 10))
	form.Set("ck", pending.WebCk)
	form.Set("appName", "aliyun_drive")
	form.Set("appEntrance", "web")
	form.Set("isMobile", "false")
	form.Set("lang", "zh_CN")
	form.Set("returnUrl", "")
	form.Set("fromSite", "52")
	form.Set("bizParams", "")
	form.Set("navlanguage", "zh-CN")
	form.Set("navPlatform", "MacIntel")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, webPassportQueryURL, strings.NewReader(form.Encode()))
	if err != nil {
		return LoginStatus{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	request.Header.Set("User-Agent", browserUserAgent)
	response, err := m.httpClient.Do(request)
	if err != nil {
		return LoginStatus{}, fmt.Errorf("查询扫码状态: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 256<<10))
	if err != nil {
		return LoginStatus{}, fmt.Errorf("读取扫码状态响应: %w", err)
	}
	var parsed webPassportQueryResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return LoginStatus{}, fmt.Errorf("解析扫码状态响应: %w", err)
	}
	data := parsed.Content.Data
	switch data.QRCodeStatus {
	case "", "NEW":
		return LoginStatus{SessionID: sessionID, State: "waiting", Message: "等待扫码，请用阿里云盘 App 扫一扫"}, nil
	case "SCANED", "SCAN_SUCCESS":
		return LoginStatus{SessionID: sessionID, State: "scanned", Message: "已扫码，请在手机上确认登录（如提示安全验证请按手机提示操作）"}, nil
	case "EXPIRED":
		m.mu.Lock()
		delete(m.pending, sessionID)
		m.mu.Unlock()
		return LoginStatus{SessionID: sessionID, State: "expired", Message: "二维码已过期，请重新生成"}, nil
	case "CANCELED":
		m.mu.Lock()
		delete(m.pending, sessionID)
		m.mu.Unlock()
		return LoginStatus{SessionID: sessionID, State: "failed", Message: "已取消登录，请重新扫码"}, nil
	case "CONFIRMED":
		log.Printf("alipan web login: 二维码已确认，正在解析令牌 (session=%s)", sessionID)
		return m.completeWebLogin(sessionID, data.BizExt)
	default:
		return LoginStatus{SessionID: sessionID, State: "waiting", Message: "扫码状态: " + data.QRCodeStatus}, nil
	}
}

// completeWebLogin extracts the refresh token from the confirmed QR payload,
// builds the web client and persists everything for restarts.
func (m *Manager) completeWebLogin(sessionID, bizExt string) (LoginStatus, error) {
	refreshToken := extractWebRefreshToken(bizExt)
	if refreshToken == "" {
		log.Printf("alipan web login: bizExt 中未解析到 refreshToken (session=%s)", sessionID)
		m.mu.Lock()
		delete(m.pending, sessionID)
		m.mu.Unlock()
		return LoginStatus{SessionID: sessionID, State: "failed", Message: "登录已确认但未解析到令牌，请重新扫码"}, nil
	}
	token, apiErr := aliyunpan_web.GetAccessTokenFromRefreshToken(refreshToken)
	if apiErr != nil || token == nil || strings.TrimSpace(token.AccessToken) == "" {
		m.mu.Lock()
		delete(m.pending, sessionID)
		m.mu.Unlock()
		message := "换取 Web 访问令牌失败，请重新扫码"
		if apiErr != nil {
			message = fmt.Sprintf("换取 Web 访问令牌失败: %s", apiErr.Err)
		}
		log.Printf("alipan web login: %s (session=%s)", message, sessionID)
		return LoginStatus{SessionID: sessionID, State: "failed", Message: message}, nil
	}
	client := m.buildWebClient(token)
	info := driveInfoResponse{}
	if ui, uiErr := client.GetUserInfo(); uiErr == nil && ui != nil {
		info = driveInfoResponse{UserID: ui.UserId, Name: ui.Nickname, DefaultDriveID: ui.FileDriveId, ResourceDriveID: ui.ResourceDriveId}
	}
	// Merge into the existing (API) profile when present: overwriting it
	// would wipe the OpenAPI credentials the user just authorized with the
	// first QR code in the two-step flow.
	public, err := m.mergeWebProfile(token, info)
	if err != nil {
		m.mu.Lock()
		delete(m.pending, sessionID)
		m.mu.Unlock()
		return LoginStatus{}, err
	}
	m.webMu.Lock()
	m.webToken = token
	m.webClient = client
	m.webMu.Unlock()
	m.startWebTokenRefresher()
	log.Printf("alipan web login: Web token 授权完成 (user=%s)", info.Name)
	m.mu.Lock()
	delete(m.pending, sessionID)
	m.mu.Unlock()
	return LoginStatus{SessionID: sessionID, State: "completed", Message: "Web 登录成功，云端转码播放可用", Profile: &public}, nil
}

// mergeWebProfile stores the web refresh token without clobbering the API
// credentials; with no existing profile at all it creates a web-only one.
func (m *Manager) mergeWebProfile(token *aliyunpan_web.WebLoginToken, info driveInfoResponse) (PublicProfile, error) {
	m.mu.Lock()
	hasProfile := m.profile != nil
	m.mu.Unlock()
	if !hasProfile {
		expiresAt := parseWebExpireTime(token.ExpireTime, 2*time.Hour)
		return m.persistProfile("web", token.AccessToken, "", expiresAt, "", info)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.profile.WebRefreshToken = token.RefreshToken
	if err := saveProfile(m.config.ConfigPath, *m.profile); err != nil {
		return PublicProfile{}, fmt.Errorf("保存阿里云盘凭据: %w", err)
	}
	return m.profile.public(), nil
}

// buildWebClient assembles a WebPanClient for the given token and registers
// the browser-like device session it needs for signed requests.
func (m *Manager) buildWebClient(token *aliyunpan_web.WebLoginToken) *aliyunpan_web.WebPanClient {
	appConfig := aliyunpan_web.AppConfig{
		AppId:    "25dzX3vbYqktVxyX",
		DeviceId: "T6ZJyY7JqX6EN2cDzLCxMVYZ",
	}
	client := aliyunpan_web.NewWebPanClient(*token, aliyunpan_web.AppLoginToken{}, appConfig, aliyunpan_web.SessionConfig{
		DeviceName: "Chrome浏览器",
		ModelName:  "Windows网页版",
	})
	_, _ = client.CreateSession(&aliyunpan_web.CreateSessionParam{
		DeviceName: "Chrome浏览器",
		ModelName:  "Windows网页版",
	})
	return client
}

// startWebTokenRefresher launches the once-per-process background loop that
// renews the web access token before it expires.
func (m *Manager) startWebTokenRefresher() {
	if m == nil {
		return
	}
	m.webRefreshOnce.Do(func() {
		go func() {
			m.refreshWebToken() // bootstrap/renew the web client proactively
			ticker := time.NewTicker(webRefreshInterval)
			defer ticker.Stop()
			for range ticker.C {
				m.refreshWebToken()
			}
		}()
	})
}

// refreshWebToken exchanges the rotating refresh token when the access token
// is about to expire (or when no client exists yet), keeping transcoded
// playback always ready across restarts.
func (m *Manager) refreshWebToken() {
	m.webMu.Lock()
	token := m.webToken
	m.webMu.Unlock()
	latest, hasLatest := m.latestWebRefreshToken()
	if token == nil {
		if !hasLatest {
			return
		}
		// Bootstrap: rebuild the client from the persisted refresh token.
		newToken, apiErr := aliyunpan_web.GetAccessTokenFromRefreshToken(latest)
		if apiErr != nil || newToken == nil || strings.TrimSpace(newToken.AccessToken) == "" {
			return
		}
		client := m.buildWebClient(newToken)
		m.webMu.Lock()
		m.webToken = newToken
		m.webClient = client
		m.webMu.Unlock()
		m.mu.Lock()
		if m.profile != nil {
			m.profile.WebRefreshToken = newToken.RefreshToken
			_ = saveProfile(m.config.ConfigPath, *m.profile)
		}
		m.mu.Unlock()
		return
	}
	if hasLatest {
		token.RefreshToken = latest
	}
	expireTime := parseWebExpireTime(token.ExpireTime, webRefreshAhead)
	if time.Until(expireTime) > webRefreshAhead {
		return
	}
	newToken, apiErr := aliyunpan_web.GetAccessTokenFromRefreshToken(token.RefreshToken)
	if apiErr != nil || newToken == nil || strings.TrimSpace(newToken.AccessToken) == "" {
		return
	}
	client := m.buildWebClient(newToken)
	m.webMu.Lock()
	m.webToken = newToken
	m.webClient = client
	m.webMu.Unlock()
	m.mu.Lock()
	if m.profile != nil {
		m.profile.WebRefreshToken = newToken.RefreshToken
		_ = saveProfile(m.config.ConfigPath, *m.profile)
	}
	m.mu.Unlock()
}

func (m *Manager) latestWebRefreshToken() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.profile != nil {
		if value := strings.TrimSpace(m.profile.WebRefreshToken); value != "" {
			return value, true
		}
	}
	if value := strings.TrimSpace(m.config.WebRefreshToken); value != "" {
		return value, true
	}
	return "", false
}

// extractWebRefreshToken decodes the base64 bizExt payload and pulls the web
// refresh token (pds_login_result.refreshToken); it falls back to a generic
// recursive search in case the upstream layout shifts.
func extractWebRefreshToken(bizExt string) string {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(bizExt))
	if err != nil {
		return ""
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ""
	}
	if result, ok := parsed["pds_login_result"].(map[string]any); ok {
		if value, ok := result["refreshToken"].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return findJSONString(parsed, "refreshToken")
}

func findJSONString(node any, key string) string {
	switch value := node.(type) {
	case map[string]any:
		if candidate, ok := value[key].(string); ok && strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
		for _, child := range value {
			if found := findJSONString(child, key); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range value {
			if found := findJSONString(child, key); found != "" {
				return found
			}
		}
	}
	return ""
}

func parseWebExpireTime(value string, fallbackIn time.Duration) time.Time {
	if parsed, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(value), time.Local); err == nil {
		return parsed
	}
	return time.Now().Add(fallbackIn)
}
