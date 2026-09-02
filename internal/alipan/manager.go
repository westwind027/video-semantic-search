package alipan

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"video-semantic-search/internal/httpclient"
	"video-semantic-search/internal/source"
)

const (
	DefaultAuthorizeURL      = "https://openapi.alipan.com/oauth/authorize"
	DefaultTokenEndpoint     = "https://openapi.alipan.com/oauth/access_token"
	DefaultAPIBaseURL        = "https://openapi.alipan.com"
	DefaultScope             = "user:base,file:all:read,file:all:write"
	DefaultTickstepBrokerURL = "https://api.tickstep.com"
	DefaultTickstepClientID  = "cf9f70e8fc61430f8ec5ab5cadf31375"
	DefaultTickstepVersion   = "v0.4.0"
	defaultPublicIPEndpoint  = "https://httpbin.org/ip"
	defaultProfileName       = "default"
	loginLifetime            = 10 * time.Minute
)

// Config contains only non-secret connector settings. Access and refresh
// tokens are loaded from ConfigPath and never exposed through the HTTP API.
type Config struct {
	ClientID          string
	ClientSecret      string
	LoginMode         string
	RedirectURI       string
	Scope             string
	AuthorizeURL      string
	TokenEndpoint     string
	APIBaseURL        string
	TickstepBrokerURL string
	TickstepIP        string
	ConfigPath        string
	HTTPClient        *http.Client
}

func ConfigFromEnv() Config {
	clientID := strings.TrimSpace(os.Getenv("ALIYUNPAN_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("ALIYUNPAN_CLIENT_SECRET"))
	loginMode := strings.ToLower(strings.TrimSpace(os.Getenv("ALIYUNPAN_LOGIN_MODE")))
	if loginMode == "" {
		if clientID != "" && clientSecret != "" {
			loginMode = "official"
		} else {
			loginMode = "tickstep"
		}
	}
	tickstepBrokerURL := strings.TrimRight(strings.TrimSpace(os.Getenv("ALIYUNPAN_TICKSTEP_BROKER_URL")), "/")
	if tickstepBrokerURL == "" {
		tickstepBrokerURL = DefaultTickstepBrokerURL
	}
	tickstepIP := strings.TrimSpace(os.Getenv("ALIYUNPAN_TICKSTEP_IP"))
	configPath := strings.TrimSpace(os.Getenv("VIDEO_SEARCH_ALIPAN_CONFIG"))
	if configPath == "" {
		if configDir := strings.TrimSpace(os.Getenv("ALIYUNPAN_CONFIG_DIR")); configDir != "" {
			configPath = filepath.Join(configDir, "video_search_alipan.json")
		} else {
			configPath = "data/alipan_profiles.json"
		}
	}
	redirectURI := strings.TrimSpace(os.Getenv("ALIYUNPAN_REDIRECT_URI"))
	if redirectURI == "" {
		redirectURI = "oob"
	}
	scope := strings.TrimSpace(os.Getenv("ALIYUNPAN_SCOPE"))
	if scope == "" {
		scope = DefaultScope
	}
	authorizeURL := strings.TrimSpace(os.Getenv("ALIYUNPAN_AUTHORIZE_URL"))
	if authorizeURL == "" {
		authorizeURL = DefaultAuthorizeURL
	}
	tokenEndpoint := strings.TrimSpace(os.Getenv("ALIYUNPAN_TOKEN_ENDPOINT"))
	if tokenEndpoint == "" {
		tokenEndpoint = DefaultTokenEndpoint
	}
	apiBaseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("ALIYUNPAN_API_BASE_URL")), "/")
	if apiBaseURL == "" {
		apiBaseURL = DefaultAPIBaseURL
	}
	return Config{
		ClientID:          clientID,
		ClientSecret:      clientSecret,
		LoginMode:         loginMode,
		RedirectURI:       redirectURI,
		Scope:             scope,
		AuthorizeURL:      authorizeURL,
		TokenEndpoint:     tokenEndpoint,
		APIBaseURL:        apiBaseURL,
		TickstepBrokerURL: tickstepBrokerURL,
		TickstepIP:        tickstepIP,
		ConfigPath:        configPath,
		HTTPClient:        httpclient.NewDirectClient(30 * time.Second),
	}
}

type Manager struct {
	config     Config
	httpClient *http.Client

	mu      sync.Mutex
	profile *profile
	loadErr error
	pending map[string]pendingLogin
}

type pendingLogin struct {
	AuthorizeURL string
	ExpiresAt    time.Time
	Provider     string
	TicketID     string
}

type profile struct {
	Name          string      `json:"name"`
	Source        string      `json:"source"`
	UserID        string      `json:"user_id"`
	Nickname      string      `json:"nickname"`
	AccountName   string      `json:"account_name,omitempty"`
	AccessToken   string      `json:"access_token"`
	RefreshToken  string      `json:"refresh_token,omitempty"`
	ExpiresAt     time.Time   `json:"expires_at"`
	TicketID      string      `json:"ticket_id,omitempty"`
	ActiveDriveID string      `json:"active_drive_id,omitempty"`
	Drives        []DriveInfo `json:"drives,omitempty"`
	ImportedAt    time.Time   `json:"imported_at"`
}

type DriveInfo struct {
	DriveID   string `json:"drive_id"`
	DriveName string `json:"drive_name"`
	DriveTag  string `json:"drive_tag,omitempty"`
}

// File is the safe, non-secret representation used by the browser file
// picker. Signed download URLs are deliberately kept out of this type.
type File struct {
	DriveID       string `json:"drive_id"`
	FileID        string `json:"file_id"`
	ParentFileID  string `json:"parent_file_id,omitempty"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	Category      string `json:"category,omitempty"`
	Size          int64  `json:"size,omitempty"`
	FileExtension string `json:"file_extension,omitempty"`
	MimeType      string `json:"mime_type,omitempty"`
	ContentHash   string `json:"content_hash,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

type FileList struct {
	DriveID      string `json:"drive_id"`
	ParentFileID string `json:"parent_file_id"`
	Items        []File `json:"items"`
	NextMarker   string `json:"next_marker,omitempty"`
}

type remoteFile struct {
	File
	URL                  string         `json:"url,omitempty"`
	Thumbnail            string         `json:"thumbnail,omitempty"`
	VideoMediaMetadata   map[string]any `json:"video_media_metadata,omitempty"`
	VideoPreviewMetadata map[string]any `json:"video_preview_metadata,omitempty"`
}

type fileListResponse struct {
	Items      []remoteFile `json:"items"`
	NextMarker string       `json:"next_marker"`
}

type fileDownloadURLResponse struct {
	URL         string `json:"url"`
	DownloadURL string `json:"download_url"`
	Expiration  string `json:"expiration"`
}

// PublicProfile intentionally omits all credentials.
type PublicProfile struct {
	Name            string      `json:"name"`
	Source          string      `json:"source"`
	UserID          string      `json:"user_id"`
	Nickname        string      `json:"nickname"`
	AccountName     string      `json:"account_name,omitempty"`
	ExpiresAt       time.Time   `json:"expires_at"`
	HasRefreshToken bool        `json:"has_refresh_token"`
	ActiveDriveID   string      `json:"active_drive_id,omitempty"`
	Drives          []DriveInfo `json:"drives,omitempty"`
}

type Status struct {
	Provider         string         `json:"provider"`
	LoginMode        string         `json:"login_mode"`
	Configured       bool           `json:"configured"`
	Connected        bool           `json:"connected"`
	Pending          bool           `json:"pending"`
	PendingSessionID string         `json:"pending_session_id,omitempty"`
	RedirectURI      string         `json:"redirect_uri,omitempty"`
	Profile          *PublicProfile `json:"profile,omitempty"`
	Error            string         `json:"error,omitempty"`
}

type LoginStart struct {
	SessionID    string    `json:"session_id"`
	AuthorizeURL string    `json:"authorize_url"`
	QRCodeURL    string    `json:"qr_code_url"`
	ExpiresAt    time.Time `json:"expires_at"`
	Mode         string    `json:"mode"`
}

type LoginStatus struct {
	SessionID string         `json:"session_id"`
	State     string         `json:"state"`
	Message   string         `json:"message,omitempty"`
	Profile   *PublicProfile `json:"profile,omitempty"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpireIn     int64  `json:"expire_in"`
	ExpiresTime  string `json:"expires_time"`
	ExpiredAt    int64  `json:"expired_at"`
	Message      string `json:"message"`
	Code         string `json:"code"`
}

type driveInfoResponse struct {
	UserID          string `json:"user_id"`
	Name            string `json:"name"`
	Avatar          string `json:"avatar"`
	DefaultDriveID  string `json:"default_drive_id"`
	ResourceDriveID string `json:"resource_drive_id"`
	BackupDriveID   string `json:"backup_drive_id"`
}

func NewManager(config Config) *Manager {
	if config.RedirectURI == "" {
		config.RedirectURI = "oob"
	}
	if config.Scope == "" {
		config.Scope = DefaultScope
	}
	if config.AuthorizeURL == "" {
		config.AuthorizeURL = DefaultAuthorizeURL
	}
	if config.TokenEndpoint == "" {
		config.TokenEndpoint = DefaultTokenEndpoint
	}
	if config.APIBaseURL == "" {
		config.APIBaseURL = DefaultAPIBaseURL
	}
	if config.TickstepBrokerURL == "" {
		config.TickstepBrokerURL = DefaultTickstepBrokerURL
	}
	if config.LoginMode == "" {
		if config.ClientID != "" && config.ClientSecret != "" {
			config.LoginMode = "official"
		} else {
			config.LoginMode = "tickstep"
		}
	}
	config.LoginMode = strings.ToLower(strings.TrimSpace(config.LoginMode))
	if config.HTTPClient == nil {
		config.HTTPClient = httpclient.NewDirectClient(30 * time.Second)
	}
	m := &Manager{config: config, httpClient: config.HTTPClient, pending: make(map[string]pendingLogin)}
	m.profile, m.loadErr = loadProfile(config.ConfigPath)
	return m
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupPendingLocked(time.Now())
	status := Status{
		Provider:    "aliyun-drive",
		LoginMode:   m.loginMode(),
		Configured:  m.configured(),
		Pending:     len(m.pending) > 0,
		RedirectURI: m.config.RedirectURI,
	}
	for sessionID := range m.pending {
		status.PendingSessionID = sessionID
		break
	}
	if m.loadErr != nil {
		status.Error = fmt.Sprintf("读取阿里云盘凭据失败: %v", m.loadErr)
	}
	if m.profile != nil && m.profile.AccessToken != "" {
		status.Connected = !m.profile.ExpiresAt.IsZero() && m.profile.ExpiresAt.After(time.Now())
		public := m.profile.public()
		status.Profile = &public
	}
	if !status.Configured && status.Error == "" {
		status.Error = "未配置阿里云盘登录参数"
	}
	return status
}

func (m *Manager) StartLogin() (LoginStart, error) {
	if m == nil {
		return LoginStart{}, errors.New("阿里云盘 connector 未配置")
	}
	if m.loginMode() == "official" && m.config.ClientID == "" {
		return LoginStart{}, errors.New("请先配置 ALIYUNPAN_CLIENT_ID")
	}
	if m.loginMode() != "official" && m.loginMode() != "tickstep" {
		return LoginStart{}, fmt.Errorf("不支持的阿里云盘登录模式: %s", m.loginMode())
	}
	if m.loginMode() == "tickstep" {
		return m.startTickstepLogin()
	}
	sessionID, err := randomID()
	if err != nil {
		return LoginStart{}, fmt.Errorf("创建登录会话: %w", err)
	}
	loginURL, err := m.buildAuthorizeURL(sessionID)
	if err != nil {
		return LoginStart{}, err
	}
	expiresAt := time.Now().Add(loginLifetime)
	m.mu.Lock()
	m.cleanupPendingLocked(time.Now())
	m.pending[sessionID] = pendingLogin{AuthorizeURL: loginURL, ExpiresAt: expiresAt, Provider: "official"}
	m.mu.Unlock()
	mode := "code"
	if !strings.EqualFold(m.config.RedirectURI, "oob") {
		mode = "callback"
	}
	return LoginStart{SessionID: sessionID, AuthorizeURL: loginURL, QRCodeURL: "/v1/connectors/alipan/login/" + sessionID + "/qr", ExpiresAt: expiresAt, Mode: mode}, nil
}

func (m *Manager) loginMode() string {
	mode := strings.ToLower(strings.TrimSpace(m.config.LoginMode))
	if mode == "" {
		if m.config.ClientID != "" && m.config.ClientSecret != "" {
			return "official"
		}
		return "tickstep"
	}
	return mode
}

func (m *Manager) configured() bool {
	if m.loginMode() == "tickstep" {
		return m.config.TickstepBrokerURL != ""
	}
	return m.loginMode() == "official" && m.config.ClientID != "" && m.config.ClientSecret != ""
}

func (m *Manager) detectPublicIP(ctx context.Context) string {
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, defaultPublicIPEndpoint, nil)
	if err != nil {
		return ""
	}
	request.Header.Set("Accept", "application/json")
	response, err := m.httpClient.Do(request)
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return ""
	}
	var body struct {
		Origin string `json:"origin"`
	}
	if json.Unmarshal(data, &body) != nil {
		return ""
	}
	for _, candidate := range strings.Split(body.Origin, ",") {
		candidate = strings.TrimSpace(candidate)
		if net.ParseIP(candidate) != nil {
			return candidate
		}
	}
	return ""
}

func tickstepHeaders() map[string]string {
	return map[string]string{
		"Accept":       "application/json, text/plain, */*",
		"Content-Type": "application/json;charset=UTF-8",
		"User-Agent":   "aliyunpan/" + DefaultTickstepVersion,
	}
}

func (m *Manager) startTickstepLogin() (LoginStart, error) {
	key, err := randomID()
	if err != nil {
		return LoginStart{}, fmt.Errorf("创建登录密钥: %w", err)
	}
	endpoint, err := url.Parse(m.config.TickstepBrokerURL + "/auth/tickstep/aliyunpan/token/qrcode/create")
	if err != nil {
		return LoginStart{}, fmt.Errorf("解析 tickstep 登录地址: %w", err)
	}
	ip := strings.TrimSpace(m.config.TickstepIP)
	if ip == "" {
		ipContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ip = m.detectPublicIP(ipContext)
		cancel()
	}
	if ip == "" {
		ip = "127.0.0.1"
	}
	query := endpoint.Query()
	query.Set("ip", ip)
	query.Set("os", runtime.GOOS)
	query.Set("arch", runtime.GOARCH)
	query.Set("version", DefaultTickstepVersion)
	query.Set("key", key)
	endpoint.RawQuery = query.Encode()
	var created tickstepEnvelope[tickstepQRCodeData]
	if err := m.getJSONWithHeaders(context.Background(), endpoint.String(), tickstepHeaders(), &created); err != nil {
		return LoginStart{}, fmt.Errorf("创建 tickstep 扫码会话: %w", err)
	}
	ticketID := strings.TrimSpace(created.Data.TokenID)
	if created.Data.TokenURL != "" {
		if tokenURL, parseErr := url.Parse(created.Data.TokenURL); parseErr == nil {
			if parsedTicketID := strings.TrimSpace(tokenURL.Query().Get("tokenId")); parsedTicketID != "" {
				ticketID = parsedTicketID
			}
		}
	}
	if ticketID == "" {
		return LoginStart{}, errors.New("tickstep 登录响应缺少 tokenId")
	}
	authorize, err := url.Parse(DefaultAuthorizeURL)
	if err != nil {
		return LoginStart{}, err
	}
	authorizeQuery := authorize.Query()
	authorizeQuery.Set("client_id", DefaultTickstepClientID)
	authorizeQuery.Set("redirect_uri", m.config.TickstepBrokerURL+"/auth/tickstep/aliyunpan/token/openapi/"+ticketID+"/auth2")
	authorizeQuery.Set("scope", "user:base,file:all:read,file:all:write,file:share:write,album:shared:read")
	authorizeQuery.Set("response_type", "code")
	authorize.RawQuery = authorizeQuery.Encode()
	sessionID, err := randomID()
	if err != nil {
		return LoginStart{}, fmt.Errorf("创建登录会话: %w", err)
	}
	expiresAt := time.Now().Add(loginLifetime)
	m.mu.Lock()
	m.cleanupPendingLocked(time.Now())
	// The browser UI exposes one active QR code. Invalidate older sessions so
	// polling cannot finish a stale QR session after a new one is generated.
	m.pending = make(map[string]pendingLogin)
	m.pending[sessionID] = pendingLogin{AuthorizeURL: authorize.String(), ExpiresAt: expiresAt, Provider: "tickstep", TicketID: ticketID}
	m.mu.Unlock()
	return LoginStart{SessionID: sessionID, AuthorizeURL: authorize.String(), QRCodeURL: "/v1/connectors/alipan/login/" + sessionID + "/qr", ExpiresAt: expiresAt, Mode: "tickstep"}, nil
}

type tickstepEnvelope[T any] struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data T      `json:"data"`
}

type tickstepQRCodeData struct {
	TokenID     string `json:"tokenId"`
	TokenURL    string `json:"tokenUrl"`
	ExpiredTime int64  `json:"expiredTime"`
}

type tickstepQRCodeDataResult struct {
	QRCodeStatus string `json:"qrCodeStatus"`
}

type tickstepToken struct {
	AccessToken string `json:"accessToken"`
	Expired     int64  `json:"expired"`
}

type tickstepCommonTokenData struct {
	OpenAPI *tickstepToken `json:"openapi"`
}

func (m *Manager) PollLogin(ctx context.Context, sessionID string) (LoginStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	pending, ok := m.pending[sessionID]
	if ok && time.Now().After(pending.ExpiresAt) {
		delete(m.pending, sessionID)
		ok = false
	}
	m.mu.Unlock()
	if !ok {
		return LoginStatus{SessionID: sessionID, State: "expired", Message: "登录会话不存在或已失效"}, nil
	}
	if pending.Provider != "tickstep" {
		return LoginStatus{SessionID: sessionID, State: "waiting", Message: "等待浏览器 OAuth 回调或授权码"}, nil
	}
	var result tickstepEnvelope[tickstepQRCodeDataResult]
	endpoint := m.config.TickstepBrokerURL + "/auth/tickstep/aliyunpan/token/qrcode/result?tokenId=" + url.QueryEscape(pending.TicketID)
	if err := m.getJSONWithHeaders(ctx, endpoint, tickstepHeaders(), &result); err != nil {
		return LoginStatus{}, fmt.Errorf("查询 tickstep 扫码状态: %w", err)
	}
	state := normalizeTickstepStatus(result.Data.QRCodeStatus)
	if state == "expired" || state == "failed" {
		return LoginStatus{SessionID: sessionID, State: state, Message: tickstepStatusMessage(state)}, nil
	}
	var tokenResult tickstepEnvelope[tickstepCommonTokenData]
	tokenEndpoint := m.config.TickstepBrokerURL + "/auth/tickstep/aliyunpan/token/common/" + url.PathEscape(pending.TicketID) + "/login"
	tokenCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	tokenErr := m.getJSONWithHeaders(tokenCtx, tokenEndpoint, tickstepHeaders(), &tokenResult)
	cancel()
	if tokenErr != nil {
		return LoginStatus{SessionID: sessionID, State: state, Message: tickstepStatusMessage(state)}, nil
	}
	if tokenResult.Data.OpenAPI == nil || tokenResult.Data.OpenAPI.AccessToken == "" {
		return LoginStatus{SessionID: sessionID, State: "confirmed", Message: "扫码已确认，但登录令牌尚未就绪"}, nil
	}
	info, err := m.getDriveInfo(ctx, tokenResult.Data.OpenAPI.AccessToken)
	if err != nil {
		// tickstep can expose the token before the OpenAPI account endpoint is
		// ready. Keep the session alive so the browser can retry instead of
		// turning a transient post-scan delay into a failed login request.
		return LoginStatus{SessionID: sessionID, State: "confirmed", Message: "扫码已确认，正在校验阿里云盘账户（会自动重试）"}, nil
	}
	expiresAt := time.Unix(tokenResult.Data.OpenAPI.Expired, 0).UTC()
	if tokenResult.Data.OpenAPI.Expired <= 0 {
		expiresAt = time.Now().Add(2 * time.Hour).UTC()
	}
	public, err := m.persistProfile("tickstep", tokenResult.Data.OpenAPI.AccessToken, "", expiresAt, pending.TicketID, info)
	if err != nil {
		return LoginStatus{}, err
	}
	m.mu.Lock()
	delete(m.pending, sessionID)
	m.mu.Unlock()
	return LoginStatus{SessionID: sessionID, State: "completed", Message: "阿里云盘登录成功", Profile: &public}, nil
}

func normalizeTickstepStatus(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")
	value = strings.ReplaceAll(value, " ", "_")
	switch value {
	case "", "new", "waiting", "pending", "created":
		return "waiting"
	case "scanned", "scaned", "scan_success", "authorized":
		return "scanned"
	case "confirmed", "confirm", "success", "successful", "done", "completed", "login_success", "logged_in":
		return "confirmed"
	case "expired", "timeout", "timed_out":
		return "expired"
	case "failed", "error", "cancelled", "canceled":
		return "failed"
	default:
		return value
	}
}

func tickstepStatusMessage(state string) string {
	switch state {
	case "waiting":
		return "等待扫码"
	case "scanned", "scaned":
		return "二维码已扫描，请在手机上确认登录"
	case "expired":
		return "二维码已过期，请重新生成"
	case "failed":
		return "扫码登录失败，请重新生成二维码"
	case "confirmed":
		return "扫码已确认，正在获取登录令牌"
	default:
		return "等待扫码"
	}
}

func (m *Manager) QRCode(sessionID string) ([]byte, error) {
	m.mu.Lock()
	pending, ok := m.pending[sessionID]
	if ok && time.Now().After(pending.ExpiresAt) {
		delete(m.pending, sessionID)
		ok = false
	}
	m.mu.Unlock()
	if !ok {
		return nil, errors.New("登录二维码已失效，请重新开始登录")
	}
	return qrcode.Encode(pending.AuthorizeURL, qrcode.Medium, 320)
}

func (m *Manager) CompleteLogin(ctx context.Context, sessionID, code string) (PublicProfile, error) {
	if m == nil {
		return PublicProfile{}, errors.New("阿里云盘 connector 未配置")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(code) == "" {
		return PublicProfile{}, errors.New("授权码不能为空")
	}
	m.mu.Lock()
	pending, ok := m.pending[sessionID]
	if ok && time.Now().After(pending.ExpiresAt) {
		delete(m.pending, sessionID)
		ok = false
	}
	m.mu.Unlock()
	if !ok {
		return PublicProfile{}, errors.New("登录会话不存在或已失效，请重新扫码")
	}
	if m.config.ClientSecret == "" {
		return PublicProfile{}, errors.New("请先配置 ALIYUNPAN_CLIENT_SECRET")
	}
	token, err := m.exchangeToken(ctx, code)
	if err != nil {
		return PublicProfile{}, err
	}
	info, err := m.getDriveInfo(ctx, token.AccessToken)
	if err != nil {
		return PublicProfile{}, err
	}
	public, err := m.persistProfile("official-oauth", token.AccessToken, token.RefreshToken, token.expiry(), "", info)
	if err != nil {
		return PublicProfile{}, err
	}
	m.mu.Lock()
	m.loadErr = nil
	delete(m.pending, sessionID)
	m.mu.Unlock()
	return public, nil
}

func (m *Manager) HandleCallback(ctx context.Context, state, code string) (PublicProfile, error) {
	return m.CompleteLogin(ctx, state, code)
}

func (m *Manager) Logout() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := removeProfile(m.config.ConfigPath); err != nil {
		return err
	}
	m.profile = nil
	m.pending = make(map[string]pendingLogin)
	return nil
}

func (m *Manager) buildAuthorizeURL(state string) (string, error) {
	u, err := url.Parse(m.config.AuthorizeURL)
	if err != nil {
		return "", fmt.Errorf("解析阿里云盘授权地址: %w", err)
	}
	query := u.Query()
	query.Set("client_id", m.config.ClientID)
	query.Set("redirect_uri", m.config.RedirectURI)
	query.Set("scope", m.config.Scope)
	query.Set("response_type", "code")
	query.Set("state", state)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func (m *Manager) exchangeToken(ctx context.Context, code string) (tokenResponse, error) {
	payload := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     m.config.ClientID,
		"client_secret": m.config.ClientSecret,
		"code":          strings.TrimSpace(code),
		"redirect_uri":  m.config.RedirectURI,
	}
	var token tokenResponse
	if err := m.postJSON(ctx, m.config.TokenEndpoint, payload, &token); err != nil {
		return token, fmt.Errorf("阿里云盘授权码换 token: %w", err)
	}
	if token.AccessToken == "" {
		return token, errors.New("阿里云盘 token 响应没有 access_token")
	}
	return token, nil
}

func (m *Manager) getDriveInfo(ctx context.Context, accessToken string) (driveInfoResponse, error) {
	var info driveInfoResponse
	if err := m.postJSONWithBearer(ctx, m.config.APIBaseURL+"/adrive/v1.0/user/getDriveInfo", accessToken, map[string]any{}, &info); err != nil {
		return info, fmt.Errorf("读取阿里云盘账户信息: %w", err)
	}
	if info.UserID == "" {
		return info, errors.New("阿里云盘账户信息缺少 user_id")
	}
	return info, nil
}

// ListFiles returns one directory level from the active OpenAPI drive. The
// marker is passed through so the UI can page without loading an entire drive.
func (m *Manager) ListFiles(ctx context.Context, driveID, parentFileID, marker string, limit int) (FileList, error) {
	accessToken, resolvedDriveID, err := m.accessTokenAndDrive(ctx, driveID)
	if err != nil {
		return FileList{}, err
	}
	if strings.TrimSpace(parentFileID) == "" {
		parentFileID = "root"
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	payload := map[string]any{
		"drive_id":        resolvedDriveID,
		"parent_file_id":  parentFileID,
		"limit":           limit,
		"order_by":        "name",
		"order_direction": "ASC",
		"fields":          "*",
	}
	if strings.TrimSpace(marker) != "" {
		payload["marker"] = marker
	}
	var result fileListResponse
	if err := m.postJSONWithBearer(ctx, m.config.APIBaseURL+"/adrive/v1.0/openFile/list", accessToken, payload, &result); err != nil {
		return FileList{}, fmt.Errorf("列出阿里云盘文件: %w", err)
	}
	items := make([]File, 0, len(result.Items))
	for _, item := range result.Items {
		item.DriveID = firstNonEmpty(item.DriveID, resolvedDriveID)
		items = append(items, item.File)
	}
	return FileList{DriveID: resolvedDriveID, ParentFileID: parentFileID, Items: items, NextMarker: result.NextMarker}, nil
}

// ListVideoFiles walks a directory tree for the directory-processing action.
// It returns metadata only; no video bytes are downloaded during browsing.
func (m *Manager) ListVideoFiles(ctx context.Context, driveID, parentFileID string, maxFiles int) ([]File, error) {
	if maxFiles <= 0 || maxFiles > 1000 {
		maxFiles = 500
	}
	if strings.TrimSpace(parentFileID) == "" {
		parentFileID = "root"
	}
	type folder struct{ id string }
	queue := []folder{{id: parentFileID}}
	visited := make(map[string]struct{})
	files := make([]File, 0)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if _, ok := visited[current.id]; ok {
			continue
		}
		visited[current.id] = struct{}{}
		marker := ""
		for {
			listing, err := m.ListFiles(ctx, driveID, current.id, marker, 200)
			if err != nil {
				return nil, err
			}
			for _, item := range listing.Items {
				if strings.EqualFold(strings.TrimSpace(item.Type), "folder") {
					queue = append(queue, folder{id: item.FileID})
					continue
				}
				if isVideoFile(item.Name, item.FileExtension, item.Category, item.MimeType) {
					files = append(files, item)
					if len(files) >= maxFiles {
						return files, nil
					}
				}
			}
			if listing.NextMarker == "" {
				break
			}
			marker = listing.NextMarker
		}
	}
	return files, nil
}

// ResolveVideo obtains a fresh signed URL only for the lifetime of one
// acquisition. The URL is never persisted in the index or returned to the
// browser.
func (m *Manager) ResolveVideo(ctx context.Context, driveID, fileID string) (source.Video, error) {
	accessToken, resolvedDriveID, err := m.accessTokenAndDrive(ctx, driveID)
	if err != nil {
		return source.Video{}, err
	}
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return source.Video{}, errors.New("阿里云盘 file_id 不能为空")
	}
	file, err := m.getRemoteFile(ctx, accessToken, resolvedDriveID, fileID)
	if err != nil {
		return source.Video{}, err
	}
	if !isVideoFile(file.Name, file.FileExtension, file.Category, file.MimeType) {
		return source.Video{}, fmt.Errorf("云盘文件不是可处理的视频: %s", file.Name)
	}
	streamURL, err := m.getDownloadURL(ctx, accessToken, resolvedDriveID, fileID)
	if err != nil {
		return source.Video{}, err
	}
	metadata := map[string]any{
		"drive_id":             resolvedDriveID,
		"file_id":              file.FileID,
		"parent_file_id":       file.ParentFileID,
		"name":                 file.Name,
		"category":             file.Category,
		"mime_type":            file.MimeType,
		"file_extension":       file.FileExtension,
		"content_hash":         file.ContentHash,
		"created_at":           file.CreatedAt,
		"updated_at":           file.UpdatedAt,
		"video_media_metadata": file.VideoMediaMetadata,
	}
	return source.Video{URL: streamURL, Name: file.Name, Size: file.Size, Fingerprint: firstNonEmpty(file.ContentHash, fmt.Sprintf("alipan:%s:%s:%d:%s", resolvedDriveID, file.FileID, file.Size, file.UpdatedAt)), Metadata: metadata}, nil
}

func (m *Manager) accessTokenAndDrive(ctx context.Context, driveID string) (string, string, error) {
	accessToken, err := m.AccessToken(ctx)
	if err != nil {
		return "", "", err
	}
	driveID = strings.TrimSpace(driveID)
	if driveID == "" {
		m.mu.Lock()
		if m.profile != nil {
			driveID = strings.TrimSpace(m.profile.ActiveDriveID)
		}
		m.mu.Unlock()
	}
	if driveID == "" {
		return "", "", errors.New("阿里云盘未找到可用 drive_id")
	}
	return accessToken, driveID, nil
}

func (m *Manager) getRemoteFile(ctx context.Context, accessToken, driveID, fileID string) (remoteFile, error) {
	payload := map[string]any{"drive_id": driveID, "file_id": fileID, "url_expire_sec": 14400, "fields": "*"}
	var file remoteFile
	if err := m.postJSONWithBearer(ctx, m.config.APIBaseURL+"/adrive/v1.0/openFile/get", accessToken, payload, &file); err != nil {
		return remoteFile{}, fmt.Errorf("读取阿里云盘文件详情: %w", err)
	}
	file.DriveID = firstNonEmpty(file.DriveID, driveID)
	file.FileID = firstNonEmpty(file.FileID, fileID)
	return file, nil
}

func (m *Manager) getDownloadURL(ctx context.Context, accessToken, driveID, fileID string) (string, error) {
	payload := map[string]any{"drive_id": driveID, "file_id": fileID, "expire_sec": 14400}
	var result fileDownloadURLResponse
	if err := m.postJSONWithBearer(ctx, m.config.APIBaseURL+"/adrive/v1.0/openFile/getDownloadUrl", accessToken, payload, &result); err != nil {
		return "", fmt.Errorf("获取阿里云盘视频下载地址: %w", err)
	}
	streamURL := firstNonEmpty(result.URL, result.DownloadURL)
	if streamURL == "" {
		return "", errors.New("阿里云盘下载接口未返回 url")
	}
	return streamURL, nil
}

func isVideoFile(name, extension, category, mimeType string) bool {
	if strings.EqualFold(strings.TrimSpace(category), "video") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "video/") {
		return true
	}
	ext := strings.ToLower(strings.TrimSpace(extension))
	if ext == "" {
		ext = strings.ToLower(filepath.Ext(name))
	}
	switch ext {
	case ".mp4", ".mkv", ".mov", ".avi", ".webm", ".m4v", ".ts", ".flv", ".wmv", ".mpeg", ".mpg", ".3gp", ".rmvb":
		return true
	default:
		return false
	}
}

// AccessToken returns a usable OpenAPI access token and refreshes it shortly
// before expiry. This keeps the credential refresh inside the connector so
// future file adapters do not need to know which login mode was used.
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	if m == nil {
		return "", errors.New("阿里云盘 connector 未配置")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	if m.profile == nil || m.profile.AccessToken == "" {
		m.mu.Unlock()
		return "", errors.New("阿里云盘尚未登录")
	}
	current := *m.profile
	m.mu.Unlock()
	if current.ExpiresAt.After(time.Now().Add(5 * time.Minute)) {
		return current.AccessToken, nil
	}

	var (
		accessToken string
		expiresAt   time.Time
		refresh     = current.RefreshToken
		err         error
	)
	if current.Source == "tickstep" {
		accessToken, expiresAt, err = m.refreshTickstepToken(ctx, current)
	} else {
		if refresh == "" {
			return "", errors.New("阿里云盘 refresh token 不存在，请重新登录")
		}
		var token tokenResponse
		token, err = m.exchangeRefreshToken(ctx, refresh)
		if err == nil {
			accessToken = token.AccessToken
			expiresAt = token.expiry()
			if token.RefreshToken != "" {
				refresh = token.RefreshToken
			}
		}
	}
	if err != nil {
		return "", err
	}
	if accessToken == "" {
		return "", errors.New("阿里云盘刷新响应没有 access_token")
	}
	updated := current
	updated.AccessToken = accessToken
	updated.RefreshToken = refresh
	updated.ExpiresAt = expiresAt
	if err := saveProfile(m.config.ConfigPath, updated); err != nil {
		return "", fmt.Errorf("保存阿里云盘刷新 token: %w", err)
	}
	m.mu.Lock()
	m.profile = &updated
	m.mu.Unlock()
	return accessToken, nil
}

func (m *Manager) persistProfile(source, accessToken, refreshToken string, expiresAt time.Time, ticketID string, info driveInfoResponse) (PublicProfile, error) {
	drives := make([]DriveInfo, 0, 3)
	for _, drive := range []struct {
		id   string
		name string
		tag  string
	}{{info.DefaultDriveID, "文件", "file"}, {info.ResourceDriveID, "资源库", "resource"}, {info.BackupDriveID, "备份盘", "backup"}} {
		if drive.id != "" {
			drives = append(drives, DriveInfo{DriveID: drive.id, DriveName: drive.name, DriveTag: drive.tag})
		}
	}
	stored := profile{
		Name:          defaultProfileName,
		Source:        source,
		UserID:        info.UserID,
		Nickname:      info.Name,
		AccessToken:   accessToken,
		RefreshToken:  refreshToken,
		ExpiresAt:     expiresAt,
		TicketID:      ticketID,
		ActiveDriveID: info.DefaultDriveID,
		Drives:        drives,
		ImportedAt:    time.Now().UTC(),
	}
	if err := saveProfile(m.config.ConfigPath, stored); err != nil {
		return PublicProfile{}, fmt.Errorf("保存阿里云盘凭据: %w", err)
	}
	m.mu.Lock()
	m.profile = &stored
	m.loadErr = nil
	m.mu.Unlock()
	return stored.public(), nil
}

func (m *Manager) postJSON(ctx context.Context, endpoint string, payload any, target any) error {
	return m.postJSONRequest(ctx, endpoint, "", payload, target)
}

func (m *Manager) postJSONWithBearer(ctx context.Context, endpoint, token string, payload any, target any) error {
	return m.postJSONRequest(ctx, endpoint, token, payload, target)
}

func (m *Manager) postJSONRequest(ctx context.Context, endpoint, bearer string, payload any, target any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	for attempt := 0; attempt < 5; attempt++ {
		response, err := m.httpClient.Do(request)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		response.Body.Close()
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}
		if response.StatusCode == http.StatusTooManyRequests && attempt < 4 {
			if err := waitForRetry(ctx, response.Header.Get("Retry-After"), data); err != nil {
				return err
			}
			request, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("build retry request: %w", err)
			}
			request.Header.Set("Accept", "application/json")
			request.Header.Set("Content-Type", "application/json")
			if bearer != "" {
				request.Header.Set("Authorization", "Bearer "+bearer)
			}
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return fmt.Errorf("HTTP %d: %s", response.StatusCode, responseMessage(data))
		}
		if err := json.Unmarshal(data, target); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		return nil
	}
	return errors.New("阿里云盘请求重试次数耗尽")
}

func waitForRetry(ctx context.Context, retryAfter string, data []byte) error {
	delay := time.Second
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds > 0 {
		delay = time.Duration(seconds) * time.Second
	} else {
		fields := strings.Fields(string(data))
		for index, field := range fields {
			if field != "毫秒" || index == 0 {
				continue
			}
			if milliseconds, parseErr := strconv.Atoi(fields[index-1]); parseErr == nil && milliseconds > 0 {
				delay = time.Duration(milliseconds) * time.Millisecond
				break
			}
		}
	}
	// Leave a small margin because the provider's remaining-window value is
	// rounded and the request may spend time in transit.
	delay += 500 * time.Millisecond
	if delay < 500*time.Millisecond {
		delay = 500 * time.Millisecond
	}
	if delay > 15*time.Second {
		delay = 15 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (m *Manager) getJSON(ctx context.Context, endpoint string, target any) error {
	return m.getJSONWithHeaders(ctx, endpoint, nil, target)
}

func (m *Manager) getJSONWithHeaders(ctx context.Context, endpoint string, headers map[string]string, target any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := m.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, responseMessage(data))
	}
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if envelope.Code != 0 {
		return fmt.Errorf("%s", firstNonEmpty(envelope.Msg, "remote code "+fmt.Sprint(envelope.Code)))
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (m *Manager) exchangeRefreshToken(ctx context.Context, refreshToken string) (tokenResponse, error) {
	payload := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     m.config.ClientID,
		"client_secret": m.config.ClientSecret,
		"refresh_token": refreshToken,
	}
	var token tokenResponse
	if err := m.postJSON(ctx, m.config.TokenEndpoint, payload, &token); err != nil {
		return token, fmt.Errorf("阿里云盘 refresh token 换 token: %w", err)
	}
	return token, nil
}

func (m *Manager) refreshTickstepToken(ctx context.Context, current profile) (string, time.Time, error) {
	if current.TicketID == "" {
		return "", time.Time{}, errors.New("tickstep 登录会话缺少 ticket_id，请重新扫码登录")
	}
	endpoint := m.config.TickstepBrokerURL + "/auth/tickstep/aliyunpan/token/openapi/" + url.PathEscape(current.TicketID) + "/refresh?userId=" + url.QueryEscape(current.UserID)
	var result tickstepEnvelope[tickstepToken]
	if err := m.getJSONWithHeaders(ctx, endpoint, map[string]string{"old-token": current.AccessToken}, &result); err != nil {
		return "", time.Time{}, fmt.Errorf("刷新 tickstep OpenAPI token: %w", err)
	}
	expiresAt := time.Unix(result.Data.Expired, 0).UTC()
	if result.Data.Expired <= 0 {
		expiresAt = time.Now().Add(2 * time.Hour).UTC()
	}
	return result.Data.AccessToken, expiresAt, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func responseMessage(data []byte) string {
	var body struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Msg     string `json:"msg"`
	}
	if json.Unmarshal(data, &body) == nil {
		for _, value := range []string{body.Message, body.Msg, body.Code} {
			if strings.TrimSpace(value) != "" {
				return value
			}
		}
	}
	return strings.TrimSpace(string(data))
}

func (t tokenResponse) expiry() time.Time {
	if t.ExpiredAt > 0 {
		return time.Unix(t.ExpiredAt, 0).UTC()
	}
	if t.ExpiresIn > 0 {
		return time.Now().Add(time.Duration(t.ExpiresIn) * time.Second).UTC()
	}
	if t.ExpireIn > 0 {
		return time.Now().Add(time.Duration(t.ExpireIn) * time.Second).UTC()
	}
	if t.ExpiresTime != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, t.ExpiresTime); err == nil {
			return parsed.UTC()
		}
	}
	return time.Now().Add(2 * time.Hour).UTC()
}

func (p profile) public() PublicProfile {
	drives := append([]DriveInfo(nil), p.Drives...)
	return PublicProfile{Name: p.Name, Source: p.Source, UserID: p.UserID, Nickname: p.Nickname, AccountName: p.AccountName, ExpiresAt: p.ExpiresAt, HasRefreshToken: p.RefreshToken != "", ActiveDriveID: p.ActiveDriveID, Drives: drives}
}

func randomID() (string, error) {
	data := make([]byte, 24)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func (m *Manager) cleanupPendingLocked(now time.Time) {
	for id, pending := range m.pending {
		if !now.Before(pending.ExpiresAt) {
			delete(m.pending, id)
		}
	}
}

func loadProfile(path string) (*profile, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("阿里云盘凭据路径为空")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var stored profile
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, err
	}
	return &stored, nil
}

func saveProfile(path string, stored profile) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("阿里云盘凭据路径为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".alipan-credentials-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func removeProfile(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
