# TEMPORARY: `cache_path` never produced cache hits for authenticated users

> **This document is temporary.** It exists to explain a one-line transport ordering fix and the
> evidence behind it. Delete it, or fold the relevant parts into `ARCHITECTURE.md` / `DECISIONS.md`,
> once the change has been reviewed.

## Symptom

The provider's `cache_path` option enabled the conditional-request cache transport
(`github.com/bored-engineer/github-conditional-http-transport`, imported as `ghct`) but produced a
~0% cache hit rate. Because every real user of the provider is authenticated, the feature was
effectively inert: no `304 Not Modified` responses were ever served from the cache.

## Mechanism

`newTransport` in `internal/ghclient/transport.go` builds the RoundTripper chain inside-out. Before
this change the construction order was:

```go
tr := cloneTransport(http.DefaultTransport, opts)
if tokenSource != nil { tr = &oauth2.Transport{Base: tr, Source: tokenSource} } // innermost
tr = logging.NewLoggingHTTPTransport(tr)
tr = &retryablehttp.RoundTripper{...}
tr = &throttler{...}
tr = ratelimit.New(tr, ...)
if opts.Cache { tr = ghct.NewTransport(store, tr) }                             // outermost
```

A RoundTripper chain executes **outermost-first**: the outermost transport sees the request before
any inner transport has touched it. `oauth2.Transport` attaches the `Authorization` header on the way
down, so every transport above it — including `ghct` — observed a request with no credential.

That matters because GitHub varies its ETags on the credential. GitHub computes an ETag as
`sha256(Accept + ":" + Authorization + ":" + Cookie + ":" + body)` and advertises
`Vary: Accept, Authorization, ...` on the response. The `ghct` library is built specifically around
that behaviour, and all three of its `Authorization`-dependent paths broke:

| path | file (v0.0.7) | behaviour without `Authorization` |
| --- | --- | --- |
| store | `transport.go` (~line 169) | Iterates the response's `Vary` headers and copies matching **request** headers into `X-Varied-*` markers. `req.Header.Values("Authorization")` was empty, so `X-Varied-Authorization` was never written. |
| reuse | `vary.go` (~line 23) | Compares `HashToken(req.Header.Get("Authorization"))` against the stored `X-Varied-Authorization`. `HashToken("")` is a non-empty digest of the empty string, so it never matched the missing stored value — always false. |
| recompute | `hash.go` (~line 29) / `conditional.go` | With the vary check false, the library recomputes the expected ETag over the cached body. It hashed **without** `Authorization` while GitHub hashed **with** it, so the resulting `If-None-Match` was guaranteed to mismatch and GitHub always answered `200`. |

The net effect on every cached request was: store an entry that can never be matched, then send a
wrong `If-None-Match` forever.

> Note: the original bug report referenced the store path as `cht.go`. In v0.0.7 that code lives in
> `transport.go`; the line number and logic are otherwise as described.

The doc comment on `newTransport` already described the intended ordering ("wraps the provided token
source with OAuth2 authentication, adds conditional request caching"), which suggests the ordering
was an accident rather than a deliberate tradeoff.

## Fix

The `oauth2.Transport` construction moved from the top of `newTransport` to the bottom, immediately
before `return tr, nil`. The lines themselves are unchanged. Resulting execution order:

```
oauth2 -> ghct -> ratelimit -> throttler -> retry -> logging -> base
```

`ghct` now sees the `Authorization` header, writes the `X-Varied-Authorization` marker, matches it on
reuse, and sends the stored ETag verbatim as `If-None-Match`.

## Evidence

Measured before the change was written, using a standalone Go harness against the live GitHub API.
These numbers are reproduced from that run and were **not** re-measured as part of this commit.

Number of `304` responses observed, per ordering:

| ordering | cold | warm (same auth) | warm (changed `Accept`) |
| --- | --- | --- | --- |
| upstream | 0 | **0** | **0** |
| patched | 0 | **10** | **10** |

The changed-`Accept` column exercises the library's recompute path, which is the same path a rotated
token takes.

End-to-end against a real 8,760-instance Terraform state:

| build | billable API requests (cold) | billable API requests (warm) | wall clock |
| --- | --- | --- | --- |
| upstream | 7,830 | 7,830 | 398s |
| patched | 4,336 (-44.6%) | 1,779 (-77.3%) | 387s |

Separately confirmed against the live API: `304` responses consume zero rate-limit quota, and GitHub
accepts both the strong (`"hex"`) and weak (`W/"hex"`) `If-None-Match` forms.

## Test coverage

Two subtests were added to `Test_newTransport` in `internal/ghclient/transport_test.go`. Both run
against an `httptest.Server` that mimics GitHub: it requires `Authorization`, advertises
`Vary: Accept, Authorization`, and answers `304` only when the exact ETag it issued is echoed back.

- `transport_caches_authenticated_requests` builds a client through the real `newTransport` with a
  token source and `Cache: true`, issues the same request twice, and asserts the second is a cache
  hit backed by exactly one `304`. Against the previous ordering this fails with
  `expected 1 revalidated (304) request, got 0`.
- `cache_transport_inside_oauth2_never_revalidates` constructs the previous ordering explicitly and
  asserts it never revalidates, pinning the regression.

The pre-existing `transport_caches_requests` subtest passes under both orderings because it uses no
token source, which is why the bug went unnoticed.

## Follow-up (fixed): `Authorization` was visible to the logging transport

`oauth2.Transport` used to sit *inside* `logging.NewLoggingHTTPTransport`, so the logging transport
ran first and dumped the request before the credential was attached — redaction by accident. With
oauth2 now outermost, the logging transport sees the fully-authenticated request.

`helper/logging.NewLoggingHTTPTransport` (terraform-plugin-sdk v2.40.1) calls
`httputil.DumpRequestOut(req, true)` at `logging_http_transport.go:201` and turns every header into a
`tflog` field via `fieldHeadersFromRequestReader`. Neither it nor the standard library redacts
anything: on Go 1.26.0, `DumpRequestOut` emits `Authorization` verbatim (verified directly). So with
`TF_LOG_PROVIDER=DEBUG` the reorder would have written the raw token to the provider log.

### What was done

`newTransport` now wraps the logging transport in a `maskingTransport`, which applies
`tflog.MaskFieldValuesWithFieldKeys(req.Context(), "Authorization")` to the request context before
delegating. The wrap happens at the point the logging transport is constructed, so nothing can be
reordered between the two later.

### Why not mask at provider configuration time

`tflog.MaskFieldValuesWithFieldKeys` (terraform-plugin-log v0.11.0, `tflog/provider.go:225`) stores
the masking options in the context it returns; there is no global state. For that mask to apply, the
returned context has to be the one the transport logs against — `req.Context()`.

It cannot be. `helper/schema.ConfigureContextFunc` is
`func(context.Context, *ResourceData) (interface{}, diag.Diagnostics)` (`helper/schema/provider.go:178`)
— no context out-parameter, so a masked context built during `Configure` is unreturnable. And each
CRUD RPC derives its own context from its own incoming gRPC context via `logging.InitContext(ctx)`
(`helper/schema/grpc_provider.go:724`, `:798`, `:1359`), so it would not inherit one even if it could
be stored.

Masking at `Configure` would therefore compile, run, and do nothing — a fix that appears to work while
the credential still leaks. `Test_newTransport/logging_transport_leaks_authorization_without_in_chain_masking`
pins this: it masks one context, logs a control entry through it (asserting that entry *is* masked, so
the sink is provably working), then issues a request on an unmasked context through an unmasked chain
and asserts the token appears verbatim.

### Why not move logging above oauth2

That restores the old redaction-by-accident but loses the transport's stated purpose. Logging would
run before `ghct` adds `If-None-Match`, before retry, and before rate limiting — so it would show
neither conditional requests, nor 304 revalidations, nor retried attempts, and would log
cache-served responses instead of wire responses. Rejected.

### Scope and a note for reviewers

The mask covers the `Authorization` field key only. `DumpRequestOut` puts headers in the dump, but the
SDK consumes them into discrete fields with `ReadMIMEHeader` before the remainder becomes
`tf_http_req_body`, so the body field does not carry the credential. Response fields never do.

An alternative worth considering upstream: apply the mask once at the provider's `tflog` sink instead
of scoped to this chain. That is defence in depth — it would catch any future transport or code path
that logs headers — at the cost of being a provider-wide policy rather than a local guarantee. The
chain-scoped version is implemented here because it is self-contained and testable; the broader
version is a maintainer's call.

## Note on branch base

This branch is based on `integrations/terraform-provider-github` `main`
(`c55240a2fb1c0dc51090bdbebd4b8bb5aae1c9e0`, 30 commits past `v6.13.0`), not on this fork's `main`,
which is a v5-era commit from August 2023 and does not contain `internal/ghclient` at all.
`internal/ghclient/transport.go` is byte-identical between `v6.13.0` and upstream `main`.
