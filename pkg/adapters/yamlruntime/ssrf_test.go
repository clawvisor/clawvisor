package yamlruntime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func init() {
	allowPrivateNetworkTargetsForTests = true
}

func TestYAMLRuntimeSSRFBlocksPrivateTargetBeforeProxy(t *testing.T) {
	oldAllow := allowPrivateNetworkTargetsForTests
	allowPrivateNetworkTargetsForTests = false
	t.Cleanup(func() { allowPrivateNetworkTargetsForTests = oldAllow })

	var proxyHit bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit = true
		w.WriteHeader(http.StatusTeapot)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")

	client := yamlRuntimeHTTPClient(nil)
	_, err := client.Get("http://169.254.169.254/latest/meta-data")
	if err == nil {
		t.Fatal("expected private target to be blocked")
	}
	if !strings.Contains(err.Error(), "blocked IP") {
		t.Fatalf("expected blocked IP error, got %v", err)
	}
	if proxyHit {
		t.Fatal("request reached environment proxy before SSRF validation")
	}
}
