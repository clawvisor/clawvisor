package llmproxy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMemoryCallerNonceCache_MintConsumeRoundTrip(t *testing.T) {
	c := NewMemoryCallerNonceCache(time.Minute)
	target := NonceTarget{Host: "api.github.com", Method: "POST", Path: "/repos/x/y/issues"}
	nonce, err := c.Mint(context.Background(), "agent-1", target)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(nonce, NoncePrefix) {
		t.Errorf("nonce missing prefix: %q", nonce)
	}
	agentID, err := c.Consume(context.Background(), nonce, target)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if agentID != "agent-1" {
		t.Errorf("agentID = %q, want agent-1", agentID)
	}
	// One-shot: second consume returns NotFound.
	if _, err := c.Consume(context.Background(), nonce, target); !errors.Is(err, ErrNonceNotFound) {
		t.Errorf("second consume = %v, want ErrNonceNotFound", err)
	}
}

func TestMemoryCallerNonceCache_TargetMismatch(t *testing.T) {
	c := NewMemoryCallerNonceCache(time.Minute)
	minted := NonceTarget{Host: "api.github.com", Method: "POST", Path: "/repos/x/y/issues"}
	nonce, err := c.Mint(context.Background(), "agent-1", minted)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Different host.
	_, err = c.Consume(context.Background(), nonce, NonceTarget{Host: "api.openai.com", Method: "POST", Path: "/repos/x/y/issues"})
	if !errors.Is(err, ErrNonceTargetMismatch) {
		t.Errorf("host mismatch: err=%v, want ErrNonceTargetMismatch", err)
	}
	// Nonce is consumed on mismatch — second attempt with the original
	// (matching) target returns NotFound. This is the one-shot defense.
	_, err = c.Consume(context.Background(), nonce, minted)
	if !errors.Is(err, ErrNonceNotFound) {
		t.Errorf("after-mismatch consume should be NotFound, got %v", err)
	}
}

func TestMemoryCallerNonceCache_MethodAndPathBinding(t *testing.T) {
	cases := []struct {
		name     string
		consumed NonceTarget
		wantErr  error
	}{
		{
			name:     "method_mismatch",
			consumed: NonceTarget{Host: "api.github.com", Method: "DELETE", Path: "/repos/x/y/issues"},
			wantErr:  ErrNonceTargetMismatch,
		},
		{
			name:     "path_mismatch",
			consumed: NonceTarget{Host: "api.github.com", Method: "POST", Path: "/repos/x/y"},
			wantErr:  ErrNonceTargetMismatch,
		},
		{
			name:     "exact_match",
			consumed: NonceTarget{Host: "api.github.com", Method: "POST", Path: "/repos/x/y/issues"},
			wantErr:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewMemoryCallerNonceCache(time.Minute)
			nonce, _ := c.Mint(context.Background(), "agent-1", NonceTarget{
				Host: "api.github.com", Method: "POST", Path: "/repos/x/y/issues",
			})
			_, err := c.Consume(context.Background(), nonce, tc.consumed)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// Normalization must produce identical targets across Mint and Consume
// for the same call regardless of case or trailing slashes.
func TestMemoryCallerNonceCache_NormalizationStable(t *testing.T) {
	c := NewMemoryCallerNonceCache(time.Minute)
	nonce, err := c.Mint(context.Background(), "agent-1", NonceTarget{
		Host: "API.Github.com",
		Method: "post",
		Path: "/repos/x/y/issues/",
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Different casing / trailing slash on consume — must match.
	agentID, err := c.Consume(context.Background(), nonce, NonceTarget{
		Host: "api.github.com",
		Method: "POST",
		Path: "/repos/x/y/issues",
	})
	if err != nil {
		t.Fatalf("Consume normalized: %v", err)
	}
	if agentID != "agent-1" {
		t.Errorf("agentID = %q, want agent-1", agentID)
	}
}

func TestMemoryCallerNonceCache_Expired(t *testing.T) {
	c := NewMemoryCallerNonceCache(time.Minute)
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }
	target := NonceTarget{Host: "api.github.com", Method: "POST", Path: "/x"}
	nonce, _ := c.Mint(context.Background(), "agent-1", target)

	// Advance past TTL.
	now = now.Add(2 * time.Minute)

	_, err := c.Consume(context.Background(), nonce, target)
	if !errors.Is(err, ErrNonceNotFound) {
		t.Fatalf("expired consume = %v, want ErrNonceNotFound", err)
	}
}

// One-shot semantics under concurrency: two goroutines racing to consume
// the same nonce — exactly one succeeds, the other gets NotFound.
func TestMemoryCallerNonceCache_ConcurrentConsumeIsOneShot(t *testing.T) {
	c := NewMemoryCallerNonceCache(time.Minute)
	target := NonceTarget{Host: "api.github.com", Method: "POST", Path: "/x"}
	nonce, _ := c.Mint(context.Background(), "agent-1", target)

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, results[idx] = c.Consume(context.Background(), nonce, target)
		}(i)
	}
	wg.Wait()

	nilCount := 0
	notFoundCount := 0
	for _, err := range results {
		switch {
		case err == nil:
			nilCount++
		case errors.Is(err, ErrNonceNotFound):
			notFoundCount++
		default:
			t.Errorf("unexpected err: %v", err)
		}
	}
	if nilCount != 1 || notFoundCount != 1 {
		t.Errorf("expected one success + one NotFound; got nil=%d notFound=%d", nilCount, notFoundCount)
	}
}
