// Copyright (c) Ippon
// SPDX-License-Identifier: MPL-2.0

package retry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func TestSkillsReadAndDeleteRetry429(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			var calls atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != method {
					t.Errorf("method = %s, want %s", r.Method, method)
				}
				if got, want := r.URL.Path, "/v1/skills/skill_123/versions/1"; got != want {
					t.Errorf("path = %s, want %s", got, want)
				}
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After-Ms", "1")
					http.Error(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limited"}}`, http.StatusTooManyRequests)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{}`)
			})

			client := anthropic.NewClient(
				option.WithAPIKey("test"),
				option.WithBaseURL("https://anthropic.test"),
				option.WithHTTPClient(newHTTPClient(handlerTransport(handler), 0)),
				option.WithMaxRetries(0),
			)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if method == http.MethodGet {
				_, err := client.Beta.Skills.Versions.Get(ctx, "1", anthropic.BetaSkillVersionGetParams{SkillID: "skill_123"})
				if err != nil {
					t.Fatalf("Read after 429: %v", err)
				}
			} else {
				_, err := client.Beta.Skills.Versions.Delete(ctx, "1", anthropic.BetaSkillVersionDeleteParams{SkillID: "skill_123"})
				if err != nil {
					t.Fatalf("Delete after 429: %v", err)
				}
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("calls = %d, want 2", got)
			}
		})
	}
}

func TestSkillsConcurrentCallsHonorSharedCooldown(t *testing.T) {
	var calls atomic.Int32
	client := newHTTPClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return handlerTransport(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})).RoundTrip(req)
	}), 15*time.Millisecond)
	if err := client.Transport.(*rateLimitTransport).skillsLimiter.deferFor(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://anthropic.test/v1/skills/skill_123", nil)
			_, err := client.Do(req)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("error = %v, want deadline", err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 0 {
		t.Fatalf("%d requests bypassed shared cooldown", calls.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func handlerTransport(handler http.Handler) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder.Result(), nil
	})
}

func TestRetryDelayFromExhaustedQuotaReset(t *testing.T) {
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	headers := http.Header{
		"Anthropic-Ratelimit-Skills-Remaining": []string{"0"},
		"Anthropic-Ratelimit-Skills-Reset":     []string{now.Add(3 * time.Second).Format(time.RFC3339Nano)},
	}
	if got := retryDelayFromHeaders(headers, now); got != 3*time.Second {
		t.Fatalf("retry delay = %s, want 3s", got)
	}
}
