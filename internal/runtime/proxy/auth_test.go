package proxy

import (
	"encoding/base64"
	"net/http"
	"testing"
)

func TestExtractBearerSecretAcceptsBearerAndBasic(t *testing.T) {
	t.Run("bearer", func(t *testing.T) {
		header := http.Header{}
		header.Set("Proxy-Authorization", "Bearer secret-token")
		secret, err := ExtractBearerSecret(header)
		if err != nil {
			t.Fatalf("ExtractBearerSecret: %v", err)
		}
		if secret != "secret-token" {
			t.Fatalf("unexpected secret %q", secret)
		}
	})

	t.Run("basic", func(t *testing.T) {
		header := http.Header{}
		creds := base64.StdEncoding.EncodeToString([]byte("clawvisor:secret-token"))
		header.Set("Proxy-Authorization", "Basic "+creds)
		secret, err := ExtractBearerSecret(header)
		if err != nil {
			t.Fatalf("ExtractBearerSecret: %v", err)
		}
		if secret != "secret-token" {
			t.Fatalf("unexpected secret %q", secret)
		}
	})
}

func TestProxyURLWithSecret(t *testing.T) {
	got, err := ProxyURLWithSecret("http://127.0.0.1:8080", "secret-token")
	if err != nil {
		t.Fatalf("ProxyURLWithSecret: %v", err)
	}
	if got != "http://clawvisor:secret-token@127.0.0.1:8080" {
		t.Fatalf("unexpected proxy URL %q", got)
	}
}
