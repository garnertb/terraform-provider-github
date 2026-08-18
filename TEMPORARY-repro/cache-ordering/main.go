package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const requestsPerScenario = 4

func main() {
	live := flag.Bool("live", false, "also run the live mode against api.github.com (requires GITHUB_TOKEN)")
	flag.Parse()

	ctx := context.Background()

	if err := runHermetic(ctx, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "\nFAIL (hermetic): %v\n", err)
		os.Exit(1)
	}

	if !*live {
		fmt.Fprintln(os.Stdout, "\nLive mode not requested. Re-run with -live and GITHUB_TOKEN set to exercise api.github.com.")
		return
	}

	if err := runLive(ctx, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "\nFAIL (live): %v\n", err)
		os.Exit(1)
	}
}

// runHermetic is the primary deliverable: it needs no credentials and no network.
func runHermetic(ctx context.Context, w io.Writer) error {
	fmt.Fprintln(w, "== hermetic mode ==")

	const body = `{"login":"octocat","id":1}`

	const token = "hermetic-test-token-not-a-real-credential"

	origin := newOrigin(body)
	server := origin.start()
	defer server.Close()

	url := server.URL + "/user"

	// Guard 1: the origin emulator must actually honour a correct conditional request. Every number
	// below is meaningless if this fails, so it runs first and by hand -- no cache involved.
	if err := guardOriginHonoursConditionalRequests(ctx, origin, url, token, body); err != nil {
		return err
	}

	fmt.Fprintln(w, "guard 1 OK: origin returns 304 for a correctly computed If-None-Match, and 401 when unauthenticated")

	tmp, err := os.MkdirTemp("", "cache-ordering-repro")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	var results []runResult

	for _, order := range []ordering{orderingUpstream, orderingPatched} {
		origin.reset()

		result, err := scenario(ctx, order, token, url, filepath.Join(tmp, string(order)), requestsPerScenario)
		if err != nil {
			return fmt.Errorf("%s ordering: %w", order, err)
		}

		result.Origin = origin.snapshot()
		results = append(results, result)
	}

	// The provider's own client, built by internal/ghclient.newTransport. Uses the same emulated
	// origin so its numbers are directly comparable to the two library-only chains above.
	origin.reset()

	providerResult, err := providerScenario(ctx, token, server.URL+"/", filepath.Join(tmp, "provider"), requestsPerScenario)
	if err != nil {
		return fmt.Errorf("provider client: %w", err)
	}

	providerResult.Origin = origin.snapshot()
	results = append(results, providerResult)

	fmt.Fprintf(w, "\n%d identical GETs per chain against the emulated origin\n\n", requestsPerScenario)
	report(w, results)

	upstream, patched, provider := results[0], results[1], results[2]

	if err := errors.Join(
		guardUpstreamFailsForTheRightReason(upstream),
		guardPatchedRevalidates(patched),
		guardOrderingsDiffer(upstream, patched),
		guardProviderMatchesUpstream(provider, upstream),
	); err != nil {
		return err
	}

	fmt.Fprintln(w, "\nguard 2 OK: upstream ordering reached the origin every time, with a credential and a"+
		"\n            non-empty but incorrect If-None-Match -- the cache is wired up and is sending a"+
		"\n            validator that cannot match, rather than being bypassed")
	fmt.Fprintln(w, "guard 3 OK: patched ordering revalidated (origin 304s, client X-Cache: HIT)")
	fmt.Fprintln(w, "guard 4 OK: the two orderings produced materially different results")
	fmt.Fprintln(w, "guard 5 OK: the provider's own client reproduces the upstream ordering's behaviour")

	return nil
}

// guardOriginHonoursConditionalRequests proves the emulated origin is a working conditional-request
// server before any transport chain is measured against it.
func guardOriginHonoursConditionalRequests(ctx context.Context, o *origin, url, token, body string) error {
	authorization := "Bearer " + token

	// Compute the expected ETag independently of both the origin helper and the ghct library.
	digest := sha256.New()
	digest.Write([]byte(acceptHeader + ":"))
	digest.Write([]byte(authorization + ":"))
	digest.Write([]byte(body))
	expected := hex.EncodeToString(digest.Sum(nil))

	unconditional, err := rawGet(ctx, url, map[string]string{
		"Accept":        acceptHeader,
		"Authorization": authorization,
	})
	if err != nil {
		return err
	}

	if unconditional.StatusCode != http.StatusOK {
		return fmt.Errorf("origin self-check: unconditional request returned %d, want 200", unconditional.StatusCode)
	}

	if got := strings.Trim(unconditional.Header.Get("ETag"), `"`); got != expected {
		return fmt.Errorf("origin self-check: served ETag %q, but the independent computation says %q", got, expected)
	}

	if got := unconditional.Header.Get("Vary"); got != "Accept, Authorization" {
		return fmt.Errorf("origin self-check: Vary was %q, want %q", got, "Accept, Authorization")
	}

	// Both the strong and weak forms must produce a 304, as GitHub accepts both.
	for _, inm := range []string{`"` + expected + `"`, `W/"` + expected + `"`} {
		conditional, err := rawGet(ctx, url, map[string]string{
			"Accept":        acceptHeader,
			"Authorization": authorization,
			"If-None-Match": inm,
		})
		if err != nil {
			return err
		}

		if conditional.StatusCode != http.StatusNotModified {
			return fmt.Errorf("origin self-check: If-None-Match %s returned %d, want 304", inm, conditional.StatusCode)
		}
	}

	// A request without a credential must be unambiguously distinguishable from a wrong validator.
	anonymous, err := rawGet(ctx, url, map[string]string{"Accept": acceptHeader})
	if err != nil {
		return err
	}

	if anonymous.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("origin self-check: unauthenticated request returned %d, want 401", anonymous.StatusCode)
	}

	o.reset()

	return nil
}

// guardUpstreamFailsForTheRightReason is the load-bearing assertion. A ~0% hit rate is only evidence
// of this bug if the cache was present, active, and defeated by the missing Authorization header.
func guardUpstreamFailsForTheRightReason(r runResult) error {
	obs := r.Origin

	if len(obs) != requestsPerScenario {
		return fmt.Errorf("guard 2: origin saw %d requests, want %d -- requests are not reaching the origin",
			len(obs), requestsPerScenario)
	}

	for i, o := range obs {
		// Not stripped: oauth2 still authenticates, it just does so below the cache.
		if !o.HadAuth {
			return fmt.Errorf("guard 2: origin request %d had no Authorization header -- "+
				"the failure is a missing credential on the wire, not a cache-visibility problem", i)
		}

		if o.Status == http.StatusUnauthorized {
			return fmt.Errorf("guard 2: origin request %d returned 401 -- see above", i)
		}

		// Wired up: the cache is running and is contributing a validator on every request.
		if o.IfNoneMatch == "" {
			return fmt.Errorf("guard 2: origin request %d carried no If-None-Match -- "+
				"the cache transport is not in the chain at all", i)
		}

		// Defeated: that validator can never match, because it was computed without the credential.
		if etagMatches(o.IfNoneMatch, o.ComputedETag) {
			return fmt.Errorf("guard 2: origin request %d sent a matching validator -- "+
				"upstream ordering is not broken in the way described", i)
		}

		if o.Status != http.StatusOK {
			return fmt.Errorf("guard 2: origin request %d returned %d, want 200", i, o.Status)
		}
	}

	// The store was writable and was written to: this is not a silent persistence failure.
	if !r.StoredEntryFound {
		return errors.New("guard 2: no cache entry was persisted -- the store path failed, " +
			"so the 200s say nothing about header visibility")
	}

	// And the specific marker the library needs is the thing that is missing.
	if r.StoredVariedAuthorization != "" {
		return errors.New("guard 2: the cache entry does contain an X-Varied-Authorization marker, " +
			"which contradicts the claim that the cache cannot see the Authorization header")
	}

	return nil
}

// guardPatchedRevalidates proves the positive case is genuinely positive.
func guardPatchedRevalidates(r runResult) error {
	notModified := countStatuses(r.Origin)[http.StatusNotModified]
	if notModified == 0 {
		return errors.New("guard 3: patched ordering produced no 304s at the origin")
	}

	if want := requestsPerScenario - 1; notModified != want {
		return fmt.Errorf("guard 3: patched ordering produced %d origin 304s, want %d "+
			"(only the first request should miss)", notModified, want)
	}

	hits := countClientCacheHits(r.Client)
	if hits != notModified {
		return fmt.Errorf("guard 3: origin served %d 304s but the client only saw %d X-Cache: HIT responses",
			notModified, hits)
	}

	for i, o := range r.Client {
		if o.Status != http.StatusOK {
			return fmt.Errorf("guard 3: client response %d was %d, want 200 -- "+
				"the cache should rewrite a wire 304 into a 200 with the cached body", i, o.Status)
		}
	}

	if !r.StoredEntryFound || r.StoredVariedAuthorization == "" {
		return errors.New("guard 3: patched ordering did not persist an X-Varied-Authorization marker")
	}

	return nil
}

// guardOrderingsDiffer catches a broken harness that would report identical numbers for both chains.
func guardOrderingsDiffer(upstream, patched runResult) error {
	up := countStatuses(upstream.Origin)[http.StatusNotModified]
	pa := countStatuses(patched.Origin)[http.StatusNotModified]

	if up == pa {
		return fmt.Errorf("guard 4: both orderings produced %d origin 304s -- "+
			"this is a harness fault, not a finding", up)
	}

	return nil
}

// guardProviderMatchesUpstream ties the library-only proof back to the shipped provider.
func guardProviderMatchesUpstream(provider, upstream runResult) error {
	providerHits := countClientCacheHits(provider.Client)
	if providerHits != 0 {
		return fmt.Errorf("guard 5: the provider client reported %d cache hits, want 0 -- "+
			"the provider is not reproducing the upstream ordering", providerHits)
	}

	if notModified := countStatuses(provider.Origin)[http.StatusNotModified]; notModified != 0 {
		return fmt.Errorf("guard 5: the provider client produced %d origin 304s, want 0", notModified)
	}

	if !provider.StoredEntryFound {
		return errors.New("guard 5: the provider persisted no cache entry")
	}

	if provider.StoredVariedAuthorization != "" {
		return errors.New("guard 5: the provider's cache entry contains an X-Varied-Authorization marker")
	}

	if countStatuses(upstream.Origin)[http.StatusNotModified] != 0 {
		return errors.New("guard 5: the upstream library chain produced 304s, so the comparison is void")
	}

	return nil
}

// rawGet issues a single request with no transport chain at all.
func rawGet(ctx context.Context, url string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	return resp, nil
}
