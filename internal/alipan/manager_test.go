package alipan

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartLoginCreatesURLAndQRCode(t *testing.T) {
	manager := NewManager(Config{
		ClientID:      "client-id",
		ClientSecret:  "client-secret",
		RedirectURI:   "oob",
		Scope:         DefaultScope,
		AuthorizeURL:  DefaultAuthorizeURL,
		TokenEndpoint: DefaultTokenEndpoint,
		APIBaseURL:    DefaultAPIBaseURL,
		ConfigPath:    filepath.Join(t.TempDir(), "profile.json"),
	})
	login, err := manager.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(login.AuthorizeURL, "client_id=client-id") || !strings.Contains(login.AuthorizeURL, "redirect_uri=oob") {
		t.Fatalf("unexpected authorize URL: %s", login.AuthorizeURL)
	}
	image, err := manager.QRCode(login.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(image) < 100 || string(image[:min(len(image), 8)]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("expected a PNG QR code, got %d bytes", len(image))
	}
}

func TestCompleteLoginExchangesTokenAndPersistsPublicProfile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			var payload map[string]string
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Errorf("decode token payload: %v", err)
			}
			if payload["code"] != "authorization-code" {
				t.Errorf("unexpected code: %q", payload["code"])
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"access_token": "access-secret", "refresh_token": "refresh-secret", "expires_in": 3600})
		case "/adrive/v1.0/user/getDriveInfo":
			if request.Header.Get("Authorization") != "Bearer access-secret" {
				t.Errorf("missing bearer token: %q", request.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"user_id": "user-1", "name": "测试用户", "default_drive_id": "drive-1", "resource_drive_id": "drive-2"})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "profile.json")
	manager := NewManager(Config{
		ClientID:      "client-id",
		ClientSecret:  "client-secret",
		RedirectURI:   "oob",
		AuthorizeURL:  server.URL + "/authorize",
		TokenEndpoint: server.URL + "/token",
		APIBaseURL:    server.URL,
		ConfigPath:    configPath,
	})
	login, err := manager.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := manager.CompleteLogin(context.Background(), login.SessionID, "authorization-code")
	if err != nil {
		t.Fatal(err)
	}
	if profile.UserID != "user-1" || profile.Nickname != "测试用户" || !profile.HasRefreshToken {
		t.Fatalf("unexpected public profile: %+v", profile)
	}
	if strings.Contains(string(mustRead(t, configPath)), "access-secret") == false {
		t.Fatal("expected access token to be persisted locally")
	}
	status := manager.Status()
	if !status.Connected || status.Profile == nil || status.Error != "" {
		t.Fatalf("unexpected status: %+v", status)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("profile was not saved: %v", err)
	}
}

func TestTickstepLoginPollsQRCodeAndRefreshesToken(t *testing.T) {
	var createIP string
	var createUserAgent string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/qrcode/create"):
			createIP = request.URL.Query().Get("ip")
			createUserAgent = request.Header.Get("User-Agent")
			_ = json.NewEncoder(response).Encode(map[string]any{"code": 0, "data": map[string]any{"tokenId": "stale-ticket", "tokenUrl": "https://example.test/?tokenId=ticket-1"}})
		case strings.HasSuffix(request.URL.Path, "/qrcode/result"):
			_ = json.NewEncoder(response).Encode(map[string]any{"code": 0, "data": map[string]any{"qrCodeStatus": "NEW"}})
		case strings.HasSuffix(request.URL.Path, "/common/ticket-1/login"):
			_ = json.NewEncoder(response).Encode(map[string]any{"code": 0, "data": map[string]any{"openapi": map[string]any{"accessToken": "tickstep-access", "expired": time.Now().Add(-time.Minute).Unix()}}})
		case strings.HasSuffix(request.URL.Path, "/openapi/ticket-1/refresh"):
			if request.Header.Get("old-token") != "tickstep-access" {
				t.Errorf("unexpected old token: %q", request.Header.Get("old-token"))
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"code": 0, "data": map[string]any{"accessToken": "tickstep-refreshed", "expired": time.Now().Add(time.Hour).Unix()}})
		case request.URL.Path == "/adrive/v1.0/user/getDriveInfo":
			if request.Header.Get("Authorization") != "Bearer tickstep-access" {
				t.Errorf("unexpected drive info token: %q", request.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(response).Encode(map[string]any{"user_id": "tickstep-user", "name": "扫码用户", "default_drive_id": "drive-1"})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	manager := NewManager(Config{LoginMode: "tickstep", TickstepBrokerURL: server.URL, APIBaseURL: server.URL, TickstepIP: "198.51.100.10", ConfigPath: filepath.Join(t.TempDir(), "profile.json")})
	login, err := manager.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	if login.Mode != "tickstep" || !strings.Contains(login.AuthorizeURL, "ticket-1") {
		t.Fatalf("unexpected tickstep login: %+v", login)
	}
	if createIP != "198.51.100.10" {
		t.Fatalf("tickstep create ip = %q, want configured public ip", createIP)
	}
	if createUserAgent != "aliyunpan/v0.4.0" {
		t.Fatalf("tickstep user-agent = %q, want aliyunpan/v0.4.0", createUserAgent)
	}
	status, err := manager.PollLogin(context.Background(), login.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "completed" || status.Profile == nil || status.Profile.Source != "tickstep" {
		t.Fatalf("unexpected poll status: %+v", status)
	}
	accessToken, err := manager.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if accessToken != "tickstep-refreshed" {
		t.Fatalf("unexpected refreshed token: %q", accessToken)
	}
}

func TestAliyunFilesListAndResolveVideo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.URL.Path {
		case "/adrive/v1.0/openFile/list":
			_ = json.NewEncoder(response).Encode(map[string]any{
				"items": []map[string]any{
					{"drive_id": "drive-1", "file_id": "folder-1", "parent_file_id": "root", "name": "电影", "type": "folder"},
					{"drive_id": "drive-1", "file_id": "video-1", "parent_file_id": "root", "name": "片段.mp4", "type": "file", "category": "video", "size": 1234, "content_hash": "hash-1"},
				},
				"next_marker": "next-1",
			})
		case "/adrive/v1.0/openFile/get":
			_ = json.NewEncoder(response).Encode(map[string]any{"drive_id": "drive-1", "file_id": "video-1", "name": "片段.mp4", "type": "file", "category": "video", "file_extension": ".mp4", "size": 1234, "content_hash": "hash-1", "video_media_metadata": map[string]any{"duration": 12.5}})
		case "/adrive/v1.0/openFile/getDownloadUrl":
			_ = json.NewEncoder(response).Encode(map[string]any{"url": "https://media.example/video.mp4?signature=redacted"})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	manager := NewManager(Config{LoginMode: "official", APIBaseURL: server.URL, ConfigPath: filepath.Join(t.TempDir(), "profile.json")})
	if _, err := manager.persistProfile("test", "access-token", "", time.Now().Add(time.Hour), "", driveInfoResponse{UserID: "user-1", Name: "用户", DefaultDriveID: "drive-1"}); err != nil {
		t.Fatal(err)
	}
	files, err := manager.ListFiles(context.Background(), "", "root", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if files.DriveID != "drive-1" || len(files.Items) != 2 || files.NextMarker != "next-1" {
		t.Fatalf("unexpected file list: %+v", files)
	}
	video, err := manager.ResolveVideo(context.Background(), "drive-1", "video-1")
	if err != nil {
		t.Fatal(err)
	}
	if video.URL == "" || video.Name != "片段.mp4" || video.Fingerprint != "hash-1" {
		t.Fatalf("unexpected resolved video: %+v", video)
	}
}

func TestListVideoFilesWalksFoldersWithoutCycles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var payload struct {
			ParentFileID string `json:"parent_file_id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode list payload: %v", err)
		}
		var items []map[string]any
		switch payload.ParentFileID {
		case "root":
			items = []map[string]any{{"file_id": "folder-1", "name": "folder-1", "type": "folder"}}
		case "folder-1":
			items = []map[string]any{{"file_id": "video-1", "name": "one.mp4", "type": "file", "category": "video"}, {"file_id": "folder-2", "name": "folder-2", "type": "folder"}}
		case "folder-2":
			items = []map[string]any{{"file_id": "video-2", "name": "two.mkv", "type": "file"}, {"file_id": "folder-1", "name": "cycle", "type": "folder"}}
		}
		_ = json.NewEncoder(response).Encode(map[string]any{"items": items})
	}))
	defer server.Close()

	manager := NewManager(Config{LoginMode: "official", APIBaseURL: server.URL, ConfigPath: filepath.Join(t.TempDir(), "profile.json")})
	if _, err := manager.persistProfile("test", "access-token", "", time.Now().Add(time.Hour), "", driveInfoResponse{UserID: "user-1", DefaultDriveID: "drive-1"}); err != nil {
		t.Fatal(err)
	}
	files, err := manager.ListVideoFiles(context.Background(), "drive-1", "root", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].FileID != "video-1" || files[1].FileID != "video-2" {
		t.Fatalf("recursive files = %+v", files)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
