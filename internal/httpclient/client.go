package httpclient

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// NewDirectClient returns an HTTP client that never consults HTTP(S)_PROXY or
// ALL_PROXY from the process environment. Go's DefaultTransport does consult
// those variables, which is undesirable for this service because remote media
// requests must not silently inherit the IDE or shell proxy configuration.
func NewDirectClient(timeout time.Duration) *http.Client {
	client, _ := newClient(timeout, "")
	return client
}

// NewProxyClient creates an HTTP client whose proxy is explicitly selected by
// the caller. It is intentionally separate from NewDirectClient so a proxy
// cannot silently leak into the Go media/search services.
func NewProxyClient(timeout time.Duration, proxyURL string) (*http.Client, error) {
	return newClient(timeout, proxyURL)
}

func newClient(timeout time.Duration, proxyURL string) (*http.Client, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{}
	} else {
		transport = transport.Clone()
	}
	transport.Proxy = nil
	if strings.TrimSpace(proxyURL) != "" {
		parsed, err := url.Parse(strings.TrimSpace(proxyURL))
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("invalid HTTP proxy URL %q", proxyURL)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return nil, fmt.Errorf("unsupported HTTP proxy scheme %q", parsed.Scheme)
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	transport.MaxIdleConns = 64
	transport.MaxIdleConnsPerHost = 32
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

// WithoutProxyEnvironment removes proxy variables before starting external
// tools such as FFmpeg and FFprobe. They otherwise inherit the parent process
// environment independently of Go's HTTP transport.
func WithoutProxyEnvironment() []string {
	result := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY":
			continue
		default:
			result = append(result, entry)
		}
	}
	return result
}
