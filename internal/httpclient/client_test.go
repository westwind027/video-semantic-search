package httpclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewDirectClientIgnoresEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewDirectClient(0)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("direct client still has an HTTP proxy")
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("direct request failed: %v", err)
	}
	response.Body.Close()
}

func TestWithoutProxyEnvironmentRemovesAllProxyVariants(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.invalid")
	t.Setenv("https_proxy", "http://proxy.invalid")
	t.Setenv("ALL_PROXY", "socks5://proxy.invalid")
	for _, entry := range WithoutProxyEnvironment() {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY":
			t.Fatalf("proxy environment entry remained: %q", entry)
		}
	}
}
