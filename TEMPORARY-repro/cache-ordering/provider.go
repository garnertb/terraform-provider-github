package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	ghctbbolt "github.com/bored-engineer/github-conditional-http-transport/bbolt"
	"github.com/integrations/terraform-provider-github/v6/internal/ghclient"
)

// providerScenario drives the provider's own REST client -- the exact transport chain built by
// internal/ghclient.newTransport -- against the emulated origin.
//
// The library-only scenarios in chains.go prove that transport ordering is what determines whether
// the cache works. This scenario proves that the provider, as shipped, is on the losing side of that
// comparison. It is deliberately kept separate so the ordering proof does not depend on anything in
// this repository.
func providerScenario(ctx context.Context, token, baseURL, cacheDir string, n int) (runResult, error) {
	opts := ghclient.ClientOptions{
		BaseURL:         baseURL,
		UserAgent:       "cache-ordering-repro",
		Cache:           true,
		CachePath:       cacheDir,
		RetryMax:        3,
		RetryWaitMin:    time.Millisecond,
		RetryWaitMax:    10 * time.Millisecond,
		MaxIdleConns:    100,
		IdleConnTimeout: 90 * time.Second,
	}

	client, err := ghclient.NewTokenRESTClient(token, opts)
	if err != nil {
		return runResult{}, fmt.Errorf("failed to create provider REST client: %w", err)
	}

	observations := make([]clientObservation, 0, n)

	for i := range n {
		_, resp, err := client.Users.Get(ctx, "")
		if err != nil {
			return runResult{}, fmt.Errorf("request %d: %w", i, err)
		}

		observations = append(observations, clientObservation{
			Status:      resp.StatusCode,
			XCache:      resp.Header.Get("X-Cache"),
			CacheStatus: resp.Header.Get("Cache-Status"),
		})
	}

	found, varied, err := inspectProviderStore(ctx, cacheDir, baseURL+"user")
	if err != nil {
		return runResult{}, err
	}

	return runResult{
		Ordering:                  "provider",
		Client:                    observations,
		StoredEntryFound:          found,
		StoredVariedAuthorization: varied,
	}, nil
}

// inspectProviderStore reads back the provider's cache entry for url.
//
// The provider client owns its bbolt handle for the lifetime of the process and bbolt takes an
// exclusive file lock, so the database cannot simply be re-opened in place. Copying the file after
// the run has quiesced and opening the copy sidesteps the lock without disturbing the original.
func inspectProviderStore(ctx context.Context, cacheDir, url string) (bool, string, error) {
	original, err := os.ReadFile(filepath.Join(cacheDir, "cache.db"))
	if err != nil {
		return false, "", fmt.Errorf("failed to read provider cache store: %w", err)
	}

	copyDir, err := os.MkdirTemp("", "provider-cache-copy")
	if err != nil {
		return false, "", err
	}
	defer os.RemoveAll(copyDir)

	copyPath := filepath.Join(copyDir, "cache.db")
	if err := os.WriteFile(copyPath, original, 0o600); err != nil {
		return false, "", err
	}

	store, err := ghctbbolt.Open(copyPath, 0o600, nil, nil)
	if err != nil {
		return false, "", fmt.Errorf("failed to open copied cache store: %w", err)
	}
	defer store.DB.Close()

	return inspectStore(ctx, store, url)
}
