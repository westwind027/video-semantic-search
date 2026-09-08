package api

import "testing"

// Regression: some drive files are transcoded onto the .cloud CDN (e.g.
// video-preview-v6.aliyundrive.cloud). When the whitelist missed that host,
// every segment of such a file was rejected with 400 and playback silently
// fell back to the throttled original-file stream.
func TestIsTranscodeCDNHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"cn-beijing-video-preview.aliyundrive.net", true},
		{"video-preview-v6.aliyundrive.cloud", true},
		{"www.alipan.com", true},
		{"www.aliyundrive.com", true},
		{"example.com", false},
		{"aliyundrive.cloud.example.com", false},
	}
	for _, testCase := range cases {
		if got := isTranscodeCDNHost(testCase.host); got != testCase.want {
			t.Fatalf("isTranscodeCDNHost(%q) = %v, want %v", testCase.host, got, testCase.want)
		}
	}
}
