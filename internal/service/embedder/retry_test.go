package embedder

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rytsh/krabby/internal/config"
)

type embedTransport func(*http.Request) (*http.Response, error)

func (f embedTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func scriptedClient(t *testing.T, cfg config.Embedder, transport embedTransport) *Client {
	t.Helper()
	cfg.BaseURL = "http://embed.test/v1"
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport = transport
	return c
}

func embedReply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

const embeddingOK = `{"data":[{"embedding":[1,2]}]}`

func throttled(seconds string) *http.Response {
	r := embedReply(http.StatusTooManyRequests, `{"error":{"message":"quota exceeded"}}`)
	r.Header.Set("Retry-After", seconds)
	return r
}

func TestInteractiveEmbeddingHasOneRetryDeadline(t *testing.T) {
	for _, ping := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			calls := 0
			c := scriptedClient(t, config.Embedder{Timeout: 1500 * time.Millisecond}, func(*http.Request) (*http.Response, error) {
				calls++
				return throttled("1"), nil
			})
			start := time.Now()
			var err error
			if ping {
				err = c.Ping(context.Background())
			} else {
				_, err = c.EmbedQuery(context.Background(), "question")
			}
			if !errors.Is(err, context.DeadlineExceeded) || calls != 2 || time.Since(start) != 1500*time.Millisecond {
				t.Fatalf("ping=%t calls=%d elapsed=%v err=%v", ping, calls, time.Since(start), err)
			}
		})
	}
}

func TestQueryDeadlineIncludesWaitingForCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := scriptedClient(t, config.Embedder{Concurrency: 1, Timeout: time.Second}, func(*http.Request) (*http.Response, error) {
			t.Error("request sent while capacity was occupied")
			return embedReply(200, embeddingOK), nil
		})
		if err := c.requests.Acquire(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		defer c.requests.Release(1)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := c.EmbedQuery(ctx, "question")
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 100*time.Millisecond {
			t.Fatalf("elapsed=%v err=%v", time.Since(start), err)
		}
	})
}

func TestBackgroundEmbeddingKeepsRetryBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		c := scriptedClient(t, config.Embedder{Timeout: 100 * time.Millisecond}, func(*http.Request) (*http.Response, error) {
			calls++
			if calls < 3 {
				return throttled("1"), nil
			}
			return embedReply(200, embeddingOK), nil
		})
		start := time.Now()
		vecs, err := c.Embed(context.Background(), []string{"chunk"})
		if err != nil || calls != 3 || len(vecs) != 1 || time.Since(start) != 2*time.Second {
			t.Fatalf("calls=%d elapsed=%v vectors=%v err=%v", calls, time.Since(start), vecs, err)
		}
	})
}

func TestBackgroundEmbeddingExhaustsRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		c := scriptedClient(t, config.Embedder{}, func(*http.Request) (*http.Response, error) { calls++; return throttled("1"), nil })
		start := time.Now()
		_, err := c.Embed(context.Background(), []string{"chunk"})
		if err == nil || calls != maxEmbedRetries+1 || time.Since(start) != maxEmbedRetries*time.Second {
			t.Fatalf("calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
		}
	})
}

func TestPermanentErrorsDoNotDisableDimensions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"credentials", 401, `{"error":{"message":"invalid API key"}}`},
		{"forbidden", 403, `{"error":{"message":"dimensions not allowed"}}`},
		{"model", 400, `{"error":{"message":"unknown model"}}`},
		{"invalid_dimension_value", 400, `{"error":{"message":"dimensions must be between 128 and 3072"}}`},
		{"malformed_json", 200, `{"data":`},
		{"missing_vector", 200, `{"data":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := scriptedClient(t, config.Embedder{Dim: 256}, func(*http.Request) (*http.Response, error) { calls++; return embedReply(tc.status, tc.body), nil })
			if _, err := c.Embed(context.Background(), []string{"input"}); err == nil {
				t.Fatal("expected failure")
			}
			if calls != 1 || c.requestDim() != 256 {
				t.Fatalf("calls=%d requested dim=%d", calls, c.requestDim())
			}
		})
	}
}

func TestDimensionFallbackOnLastRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		c := scriptedClient(t, config.Embedder{Dim: 256}, func(r *http.Request) (*http.Response, error) {
			calls++
			if calls <= maxEmbedRetries {
				return throttled("1"), nil
			}
			var req embedRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				return nil, err
			}
			if req.Dimensions > 0 {
				return embedReply(400, `{"error":{"message":"unknown field: dimensions"}}`), nil
			}
			return embedReply(200, embeddingOK), nil
		})
		vecs, err := c.Embed(context.Background(), []string{"chunk"})
		if err != nil || len(vecs) != 1 || calls != maxEmbedRetries+2 || !c.noDims.Load() {
			t.Fatalf("calls=%d vectors=%v err=%v", calls, vecs, err)
		}
	})
}

func TestRequestConcurrencyIsSharedAcrossCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var active, peak, calls atomic.Int64
		c := scriptedClient(t, config.Embedder{Concurrency: 2, Batch: 1}, func(*http.Request) (*http.Response, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for p := peak.Load(); n > p; p = peak.Load() {
				if peak.CompareAndSwap(p, n) {
					break
				}
			}
			calls.Add(1)
			time.Sleep(time.Second)
			return embedReply(200, embeddingOK), nil
		})
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				vecs, err := c.Embed(context.Background(), []string{"a", "b"})
				if err != nil || len(vecs) != 2 {
					t.Errorf("vectors=%v err=%v", vecs, err)
				}
			})
		}
		wg.Wait()
		if peak.Load() != 2 || calls.Load() != 16 || active.Load() != 0 {
			t.Fatalf("peak=%d calls=%d active=%d", peak.Load(), calls.Load(), active.Load())
		}
	})
}

func TestBackoffDoesNotOccupyRequestCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var slowCalls atomic.Int64
		c := scriptedClient(t, config.Embedder{Concurrency: 1, Timeout: time.Second}, func(r *http.Request) (*http.Response, error) {
			var req embedRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				return nil, err
			}
			if req.Input[0] == "slow" && slowCalls.Add(1) == 1 {
				return throttled("10"), nil
			}
			return embedReply(200, embeddingOK), nil
		})
		done := make(chan error, 1)
		go func() { _, err := c.Embed(context.Background(), []string{"slow"}); done <- err }()
		synctest.Wait()
		start := time.Now()
		if _, err := c.EmbedQuery(context.Background(), "fast"); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) != 0 {
			t.Fatal("query waited for another request's backoff")
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

type brokenEmbedBody struct{ err error }

func (b brokenEmbedBody) Read([]byte) (int, error) { return 0, b.err }
func (b brokenEmbedBody) Close() error             { return nil }

func TestResponseReadErrorIsPreserved(t *testing.T) {
	failure := errors.New("connection reset while reading")
	c := scriptedClient(t, config.Embedder{}, func(*http.Request) (*http.Response, error) {
		r := embedReply(200, "")
		r.Body = brokenEmbedBody{failure}
		return r, nil
	})
	_, _, _, err := c.embedBatchOnce(context.Background(), []string{"input"}, 0)
	var retry retryableErr
	if !errors.Is(err, failure) || !errors.As(err, &retry) {
		t.Fatalf("read error lost: %v", err)
	}
}

func TestCancelledRequestKeepsDimensionsAndReleasesCapacity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	c := scriptedClient(t, config.Embedder{Dim: 256, Concurrency: 1}, func(*http.Request) (*http.Response, error) {
		calls++
		cancel()
		return nil, ctx.Err()
	})
	_, err := c.Embed(ctx, []string{"input"})
	if !errors.Is(err, context.Canceled) || calls != 1 || c.requestDim() != 256 {
		t.Fatalf("calls=%d dimension=%d err=%v", calls, c.requestDim(), err)
	}
	if !c.requests.TryAcquire(1) {
		t.Fatal("cancelled request leaked its permit")
	}
	c.requests.Release(1)
}

func TestResponseReadFailureRetriesWithoutDroppingDimensions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		c := scriptedClient(t, config.Embedder{Dim: 256}, func(r *http.Request) (*http.Response, error) {
			calls++
			var req embedRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				return nil, err
			}
			if req.Dimensions != 256 {
				t.Errorf("request %d dropped dimensions", calls)
			}
			if calls == 1 {
				resp := embedReply(200, "")
				resp.Body = brokenEmbedBody{io.ErrUnexpectedEOF}
				return resp, nil
			}
			return embedReply(200, embeddingOK), nil
		})
		vecs, err := c.Embed(context.Background(), []string{"input"})
		if err != nil || len(vecs) != 1 || calls != 2 || c.noDims.Load() {
			t.Fatalf("calls=%d vecs=%v err=%v", calls, vecs, err)
		}
	})
}

func TestRetryDelaysStayBounded(t *testing.T) {
	for _, tc := range []struct {
		body string
		want time.Duration
	}{
		{"retry in 10.6s", 11 * time.Second},
		{"retry after 10s", 10 * time.Second},
		{`"retryDelay": "0.25s"`, time.Second},
		{"retry in 999999999999999999999999s", maxEmbedBackoff},
		{"no retry hint", 0},
	} {
		if got := parseRetryPhrase(tc.body); got != tc.want {
			t.Errorf("%q: wait=%v want=%v", tc.body, got, tc.want)
		}
	}
	for attempt := 0; attempt <= maxEmbedRetries; attempt++ {
		for range 100 {
			if wait := backoffDelay(attempt, 0); wait < 0 || wait > maxEmbedBackoff {
				t.Fatalf("attempt=%d wait=%v", attempt, wait)
			}
		}
	}
	synctest.Test(t, func(t *testing.T) {
		r := embedReply(429, "")
		r.Header.Set("Retry-After", time.Now().Add(10*time.Second).UTC().Format(http.TimeFormat))
		if wait := retryAfterHint(r, nil); wait != 10*time.Second {
			t.Fatalf("HTTP-date wait=%v", wait)
		}
		r.Header.Set("Retry-After", "9223372036854775807")
		if wait := retryAfterHint(r, nil); wait != maxEmbedBackoff {
			t.Fatalf("overflowing seconds wait=%v", wait)
		}
	})
}
