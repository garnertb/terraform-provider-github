package ghclient

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	ghct "github.com/bored-engineer/github-conditional-http-transport"
	"golang.org/x/oauth2"
	"golang.org/x/sync/semaphore"
)

func Test_cloneTransport(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name          string
		source        http.RoundTripper
		httpTransport bool
	}{
		{
			name:          "http_transport",
			source:        &http.Transport{},
			httpTransport: true,
		},
		{
			name:          "non_http_transport",
			source:        &testRoundTripper{err: errors.New("not used")},
			httpTransport: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := ClientOptions{MaxIdleConns: 10, IdleConnTimeout: 30 * time.Second}
			cloned := cloneTransport(tt.source, opts)

			if !tt.httpTransport && cloned != tt.source {
				t.Fatal("expected cloned transport to match original pointer")
			}

			if tt.httpTransport && cloned == tt.source {
				t.Fatal("expected cloned transport to have a different pointer")
			}

			htr, ok := cloned.(*http.Transport)

			if !tt.httpTransport && ok {
				t.Fatalf("expected cloned transport to not be an *http.Transport")
			}

			if tt.httpTransport && !ok {
				t.Fatalf("expected cloned transport to be an *http.Transport, got %T", cloned)
			}

			if !tt.httpTransport {
				return
			}

			if htr.ForceAttemptHTTP2 != true {
				t.Fatal("expected ForceAttemptHTTP2 to be true")
			}

			if htr.MaxIdleConns != opts.MaxIdleConns {
				t.Fatalf("expected MaxIdleConns to be %d, got %d", opts.MaxIdleConns, htr.MaxIdleConns)
			}

			if htr.MaxIdleConnsPerHost != opts.MaxIdleConns {
				t.Fatalf("expected MaxIdleConnsPerHost to be %d, got %d", opts.MaxIdleConns, htr.MaxIdleConnsPerHost)
			}

			if htr.IdleConnTimeout != opts.IdleConnTimeout {
				t.Fatalf("expected IdleConnTimeout to be %v, got %v", opts.IdleConnTimeout, htr.IdleConnTimeout)
			}
		})
	}
}

func Test_newTransport(t *testing.T) {
	t.Parallel()

	cacheBasePath := mustMkdirTemp(t, "", "*")
	t.Cleanup(func() {
		_ = os.RemoveAll(cacheBasePath)
	})

	for _, tt := range []struct {
		name        string
		tokenSource oauth2.TokenSource
		opts        ClientOptions
		wantErr     string
	}{
		{
			name:        "succeeds_with_empty_options",
			tokenSource: nil,
			opts:        ClientOptions{},
		},
		{
			name:        "succeeds_with_token",
			tokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}),
			opts:        ClientOptions{},
		},
		{
			name:        "succeeds_with_retry",
			tokenSource: nil,
			opts:        ClientOptions{RetryMax: 1, RetryWaitMin: time.Millisecond, RetryWaitMax: time.Millisecond},
		},
		{
			name:        "succeeds_with_throttler",
			tokenSource: nil,
			opts:        ClientOptions{Sema: semaphore.NewWeighted(1)},
		},
		{
			name:        "succeeds_with_cache",
			tokenSource: nil,
			opts:        ClientOptions{Cache: true, CachePath: mustMkdirTemp(t, cacheBasePath, "*")},
		},
		{
			name:        "succeeds_with_all_options",
			tokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}),
			opts:        ClientOptions{RetryMax: 1, RetryWaitMin: time.Millisecond, RetryWaitMax: time.Millisecond, Sema: semaphore.NewWeighted(1), Cache: true, CachePath: mustMkdirTemp(t, cacheBasePath, "*")},
		},
		{
			name:        "errors_with_invalid_cache_path",
			tokenSource: nil,
			opts:        ClientOptions{Cache: true, CachePath: "\x00c"},
			wantErr:     "failed to create cache store",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tr, err := newTransport(tt.tokenSource, tt.opts)
			if err != nil {
				if tt.wantErr == "" {
					t.Fatalf("failed to create transport: %v", err)
				}

				if !regexp.MustCompile(regexp.QuoteMeta(tt.wantErr)).MatchString(err.Error()) {
					t.Fatalf("expected error to match %q, got %v", tt.wantErr, err)
				}

				return
			}

			if tt.wantErr != "" {
				t.Fatalf("expected error %q, got nil", tt.wantErr)
			}

			if tr == nil {
				t.Fatal("expected transport to be non-nil")
			}
		})
	}

	t.Run("transport_retries_requests", func(t *testing.T) {
		t.Parallel()

		for _, tt := range []struct {
			name               string
			retryMax           int
			failures           int
			failStatusCode     int
			wantFailStatusCode bool
			wantError          string
		}{
			{
				name:               "no_retries",
				retryMax:           0,
				failures:           1,
				failStatusCode:     http.StatusInternalServerError,
				wantFailStatusCode: true,
			},
			{
				name:           "retries_until_success",
				retryMax:       3,
				failures:       2,
				failStatusCode: http.StatusInternalServerError,
			},
			{
				name:           "retries_until_failure",
				retryMax:       2,
				failures:       3,
				failStatusCode: http.StatusInternalServerError,
				wantError:      "giving up after",
			},
			{
				name:               "does_not_retry_on_4xx",
				retryMax:           3,
				failures:           1,
				failStatusCode:     http.StatusBadRequest,
				wantFailStatusCode: true,
			},
		} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				called := atomic.Int32{}
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					call := int(called.Add(1))

					if call <= tt.failures {
						w.WriteHeader(tt.failStatusCode)
						return
					}

					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("PASS"))
				}))
				defer ts.Close()

				opts := ClientOptions{RetryMax: tt.retryMax, RetryWaitMin: time.Millisecond, RetryWaitMax: time.Millisecond}
				tr, err := newTransport(nil, opts)
				if err != nil {
					t.Fatalf("failed to create transport: %v", err)
				}

				if tr == nil {
					t.Fatal("expected transport to be non-nil")
				}

				client := &http.Client{Transport: tr}

				res, err := client.Get(ts.URL)
				if err != nil {
					if tt.wantError != "" {
						if !regexp.MustCompile(regexp.QuoteMeta(tt.wantError)).MatchString(err.Error()) {
							t.Fatalf("expected error to match %q, got %v", tt.wantError, err)
						}

						return
					}

					t.Fatalf("failed to make request: %v", err)
				}
				defer res.Body.Close()

				if tt.wantError != "" {
					t.Fatalf("expected error %q, got nil", tt.wantError)
				}

				if tt.wantFailStatusCode {
					if res.StatusCode != tt.failStatusCode {
						t.Fatalf("expected status code %d, got %d", tt.failStatusCode, res.StatusCode)
					}

					return
				}

				if res.StatusCode != http.StatusOK {
					t.Fatalf("expected status code %d, got %d", http.StatusOK, res.StatusCode)
				}

				body, err := io.ReadAll(res.Body)
				if err != nil {
					t.Fatalf("failed to read response body: %v", err)
				}

				if string(body) != "PASS" {
					t.Fatalf("expected response body to be %q, got %q", "PASS", string(body))
				}
			})
		}
	})

	t.Run("transport_throttles_requests", func(t *testing.T) {
		t.Parallel()

		result := "FAIL"

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(result))
		}))
		defer ts.Close()

		opts := ClientOptions{Sema: semaphore.NewWeighted(1)}
		tr, err := newTransport(nil, opts)
		if err != nil {
			t.Fatalf("failed to create transport: %v", err)
		}

		if tr == nil {
			t.Fatal("expected transport to be non-nil")
		}

		client := &http.Client{Transport: tr}

		if err := opts.Sema.Acquire(t.Context(), 1); err != nil {
			t.Fatalf("failed to acquire semaphore: %v", err)
		}

		go func() {
			time.Sleep(1 * time.Second)
			result = "PASS"
			opts.Sema.Release(1)
		}()

		res, err := client.Get(ts.URL)
		if err != nil {
			t.Fatalf("failed to make request: %v", err)
		}
		defer res.Body.Close()

		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("failed to read response body: %v", err)
		}

		if string(body) != "PASS" {
			t.Fatalf("expected response body to be %q, got %q", "PASS", string(body))
		}
	})

	t.Run("transport_caches_requests", func(t *testing.T) {
		t.Parallel()

		etag := "test-etag"

		called := atomic.Int32{}
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called.Add(1)
			call := int(called.Load())

			if call <= 2 && r.Header.Get("If-None-Match") == etag {
				w.Header().Set("Etag", etag)
				w.WriteHeader(http.StatusNotModified)
				return
			}

			w.Header().Set("Etag", etag)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(strconv.Itoa(call)))
		}))
		defer ts.Close()

		opts := ClientOptions{Cache: true, CachePath: mustMkdirTemp(t, cacheBasePath, "*")}
		tr, err := newTransport(nil, opts)
		if err != nil {
			t.Fatalf("failed to create transport: %v", err)
		}

		if tr == nil {
			t.Fatal("expected transport to be non-nil")
		}

		client := &http.Client{Transport: tr}

		res1, err := client.Get(ts.URL)
		if err != nil {
			t.Fatalf("failed to make first request: %v", err)
		}

		b1, err := io.ReadAll(res1.Body)
		if err != nil {
			t.Fatalf("failed to read first response body: %v", err)
		}
		res1.Body.Close()

		res2, err := client.Get(ts.URL)
		if err != nil {
			t.Fatalf("failed to make second request: %v", err)
		}

		b2, err := io.ReadAll(res2.Body)
		if err != nil {
			t.Fatalf("failed to read second response body: %v", err)
		}
		res2.Body.Close()

		if string(b2) != string(b1) {
			t.Fatalf("expected cached response to match first response, got %q and %q", string(b2), string(b1))
		}

		res3, err := client.Get(ts.URL)
		if err != nil {
			t.Fatalf("failed to make third request: %v", err)
		}

		b3, err := io.ReadAll(res3.Body)
		if err != nil {
			t.Fatalf("failed to read third response body: %v", err)
		}
		res3.Body.Close()

		if string(b3) == string(b2) {
			t.Fatalf("expected cached response to not match last response, got %q and %q", string(b3), string(b2))
		}
	})

	t.Run("transport_caches_authenticated_requests", func(t *testing.T) {
		t.Parallel()

		srv := newVaryingETagServer(t)

		opts := ClientOptions{Cache: true, CachePath: mustMkdirTemp(t, cacheBasePath, "*")}
		tr, err := newTransport(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: testETagServerToken}), opts)
		if err != nil {
			t.Fatalf("failed to create transport: %v", err)
		}

		client := &http.Client{Transport: tr}

		b1, res1 := mustGet(t, client, srv.URL)
		if res1.Header.Get("X-Cache") != "MISS" {
			t.Fatalf("expected first response to be a cache miss, got %q", res1.Header.Get("X-Cache"))
		}

		b2, res2 := mustGet(t, client, srv.URL)

		if srv.unauthorized.Load() != 0 {
			t.Fatalf("expected no unauthorized requests, got %d", srv.unauthorized.Load())
		}

		if srv.notModified.Load() != 1 {
			t.Fatalf("expected 1 revalidated (304) request, got %d", srv.notModified.Load())
		}

		if res2.Header.Get("X-Cache") != "HIT" {
			t.Fatalf("expected second response to be a cache hit, got %q", res2.Header.Get("X-Cache"))
		}

		if b2 != b1 {
			t.Fatalf("expected cached response to match first response, got %q and %q", b2, b1)
		}
	})

	// Pins the regression fixed by moving the oauth2 transport outside the cache transport: when the
	// cache transport runs first it never sees the Authorization header GitHub varies its ETags on, so
	// it stores no X-Varied-Authorization marker and sends a recomputed, mismatching If-None-Match.
	t.Run("cache_transport_inside_oauth2_never_revalidates", func(t *testing.T) {
		t.Parallel()

		srv := newVaryingETagServer(t)

		store, err := createCacheStore(mustMkdirTemp(t, cacheBasePath, "*"))
		if err != nil {
			t.Fatalf("failed to create cache store: %v", err)
		}
		t.Cleanup(func() {
			_ = store.DB.Close()
		})

		tokenSource := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: testETagServerToken})
		client := &http.Client{Transport: ghct.NewTransport(store, &oauth2.Transport{
			Base:   cloneTransport(http.DefaultTransport, ClientOptions{}),
			Source: tokenSource,
		})}

		b1, _ := mustGet(t, client, srv.URL)
		b2, res2 := mustGet(t, client, srv.URL)

		if srv.unauthorized.Load() != 0 {
			t.Fatalf("expected no unauthorized requests, got %d", srv.unauthorized.Load())
		}

		if srv.notModified.Load() != 0 {
			t.Fatalf("expected the legacy ordering to never revalidate, got %d 304 responses", srv.notModified.Load())
		}

		if res2.Header.Get("X-Cache") != "MISS" {
			t.Fatalf("expected the legacy ordering to miss, got %q", res2.Header.Get("X-Cache"))
		}

		if b2 == b1 {
			t.Fatalf("expected the legacy ordering to refetch the body, got %q twice", b2)
		}
	})
}

const (
	testETagServerToken = "test-token"
	testETagServerETag  = `"varying-etag"`
)

// varyingETagServer is an httptest.Server that mimics GitHub's conditional request behaviour: it
// requires an Authorization header, advertises that its ETags vary on Accept and Authorization, and
// only answers 304 when the exact ETag it issued is echoed back in If-None-Match.
type varyingETagServer struct {
	*httptest.Server

	unauthorized atomic.Int32
	notModified  atomic.Int32
	served       atomic.Int32
}

func newVaryingETagServer(t *testing.T) *varyingETagServer {
	t.Helper()

	srv := &varyingETagServer{}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testETagServerToken {
			srv.unauthorized.Add(1)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		w.Header().Set("Vary", "Accept, Authorization")
		w.Header().Set("Etag", testETagServerETag)

		if r.Header.Get("If-None-Match") == testETagServerETag {
			srv.notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strconv.Itoa(int(srv.served.Add(1)))))
	}))
	t.Cleanup(srv.Close)

	return srv
}

func mustGet(t *testing.T, client *http.Client, url string) (string, *http.Response) {
	t.Helper()

	res, err := client.Get(url)
	if err != nil {
		t.Fatalf("failed to make request: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected status code %d, got %d", http.StatusOK, res.StatusCode)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	return string(body), res
}
