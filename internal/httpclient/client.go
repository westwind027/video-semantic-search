package httpclient

import (
	"net/http"
	"os"
	"strings"
	"time"
)

// NewDirectClient returns an HTTP client that never consults HTTP(S)_PROXY or
// ALL_PROXY from the process environment. Go's DefaultTransport does consult
// those variables, which is undesirable for this service because remote media
// requests must not silently inherit the IDE or shell proxy configuration.
func NewDirectClient(timeout time.Duration) *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{}
	} else {
		transport = transport.Clone()
	}
	transport.Proxy = nil
	transport.MaxIdleConns = 64
	transport.MaxIdleConnsPerHost = 32
	return &http.Client{Transport: transport, Timeout: timeout}
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
