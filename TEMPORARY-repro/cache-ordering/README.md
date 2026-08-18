# Repro: transport ordering defeats the HTTP response cache

**Temporary.** This directory exists to demonstrate a bug. Delete it once the bug is resolved.

Every factual claim below links to the source it rests on. Links are commit-pinned:
`github-conditional-http-transport` at [`94778cb`][ghct-tree] (tags `v0.0.7` and `bbolt/v0.0.2`, both
the same commit) and this provider at [`67f3fd1`][prov-tree].

## Claim

In [`internal/ghclient/transport.go`][newTransport], `newTransport` builds the RoundTripper chain so
that the conditional-request cache ("ghct") is the **outermost** layer and `oauth2.Transport` is the
**innermost**:

```
cloneTransport -> oauth2 -> logging -> retryablehttp -> throttler -> ratelimit -> ghct
                  ^ innermost                                                     ^ outermost
```

[`oauth2` is applied first][oauth2-innermost], so it ends up deepest;
[ghct is applied last][ghct-outermost], so it ends up on top. RoundTripper chains execute
outermost-first, so ghct processes every request *before* the `Authorization` header exists.

GitHub derives its ETags from the request's `Authorization` header and advertises
`Vary: Accept, Authorization`. ghct exists specifically to handle that — its
[`VaryHeaders`][vary-headers] are exactly `Accept`, `Authorization`, `Cookie`. With the credential
invisible to it, three paths break:

| # | Path | Source | What goes wrong |
|---|---|---|---|
| 1 | store | [`transport.go:165-174`][store-path] | `req.Header.Values("Authorization")` is empty, so the [`X-Varied-Authorization`][vary-prefix] marker is never written into the entry |
| 2 | vary / reuse | [`vary.go:21-25`][vary-check] | compares [`HashToken("")`][hash-token] — a non-empty digest of the empty string — against a missing stored value, so it is always false |
| 3 | recompute | [`conditional.go:45-50`][branch-3] via [`hash.go:28-34`][hash-loop] | recomputes the expected ETag *without* `Authorization`, while GitHub hashed *with* it |

Result: a cache hit rate of ~0% for authenticated users.

The credential is **not** stripped from the wire. `oauth2` still authenticates; it just does so below
the cache. The harness asserts this explicitly, since "the cache broke auth" is a plausible-sounding
but incorrect reading of the symptom and leads to the wrong file.

## Running it

```sh
export GOTOOLCHAIN=auto                 # go.mod requires a recent toolchain
export PATH="/opt/homebrew/bin:$PATH"   # only if go is not already on PATH

# Hermetic: no credentials, no network.
go run ./TEMPORARY-repro/cache-ordering

# Optional: the same comparison against the real api.github.com.
GITHUB_TOKEN=$(gh auth token) go run ./TEMPORARY-repro/cache-ordering -live
```

The program exits non-zero if any assertion fails. Live mode skips cleanly when `GITHUB_TOKEN` is
unset, and issues eight requests total against a read-only endpoint.

## What it builds

Three chains, four identical `GET`s through each:

| Chain | Built from | Ordering |
|---|---|---|
| `upstream` | the libraries directly, no provider code | ghct outermost, oauth2 innermost (as on `main`) |
| `patched` | the libraries directly, no provider code | oauth2 directly outside ghct |
| `provider` | [`ghclient.NewTokenRESTClient`][rest-client] | whatever the provider actually ships |

The `upstream` and `patched` chains deliberately **do not import `internal/ghclient`**. The bug is a
property of these libraries composed in this order and nothing else, so the core comparison can be
audited without trusting anything in this repository. The `provider` chain is a separate, additional
scenario that ties the result back to the shipped code.

Every chain uses a real `oauth2.StaticTokenSource` and a real bbolt-backed ghct store in a temp dir.
That store is [keyed on the request URL alone][bbolt-key], which is what lets the harness read an
entry back and inspect its headers directly.

## The emulated origin (hermetic mode)

`origin.go` stands up an `httptest.Server` that emulates GitHub's documented behaviour:

- ETag is `sha256` over the request's `Accept`, `Authorization` and `Cookie` values (in that fixed
  order, each followed by `:`) then the response body, hex-encoded.
- Sends `Vary: Accept, Authorization` and `Cache-Control: private, max-age=60`.
- Returns `304` when `If-None-Match` matches, accepting both `"hex"` and `W/"hex"`.
- Returns `401` when there is no `Authorization` header, so "no credential" and "wrong validator" are
  never confusable.
- Records every request it serves.

**Counting happens at the origin, not the client.** On a wire `304` ghct
[copies the cached status and body over the response][rewrite-304] — or, when the
[speculative `[]` guess][branch-1] matched and nothing was stored,
[synthesises a `200`][rewrite-speculative] — before returning it. Client-side status codes are
therefore `200` in every column and prove nothing on their own. The client-side signal is the
[`X-Cache` / `Cache-Status` pair][set-cache-status], set to [`HIT`][hit] on a revalidation and
[`MISS`][miss] otherwise.

## Guarding against a vacuous proof

The failure mode to worry about is producing the expected numbers for the wrong reason. Five
assertions guard that, and all of them are hard failures:

1. **The origin actually works.** Before any chain is measured, a hand-rolled request with an
   independently computed `If-None-Match` must return `304`, in both strong and weak form; the
   served ETag must equal the independent computation; and an unauthenticated request must `401`.
2. **The negative is negative for the right reason.** Under `upstream` ordering the origin must have
   received all four requests, *each carrying a credential* (so the failure is not a stripped
   header), *each carrying a non-empty `If-None-Match`* (so the cache is genuinely in the chain and
   not bypassed), and *none of them matching* (so the validator is wrong, not absent). The cache
   entry must exist — proving the store path is writable and was written — while lacking the
   `X-Varied-Authorization` marker.
3. **The positive is positive.** `patched` must produce exactly three origin `304`s, the client must
   report the same number of `X-Cache: HIT` responses, every client status must be `200`, and the
   stored entry must carry an `X-Varied-Authorization` marker.
4. **The orderings differ.** Identical results across both chains is reported as a harness fault,
   not as a finding.
5. **The provider matches `upstream`.** Zero hits, zero origin `304`s, entry stored, marker absent.

Guard 2 is the load-bearing one: it is what separates "the cache is sending a wrong validator" from
the several other reasons a hit rate could be zero.

Guard 2 checks the *stored marker*, not the size of the database file. A bbolt file is non-empty the
moment it is [created and its bucket initialised][bbolt-open], so file size would prove nothing.

**Once the ordering is corrected, guard 5 fails.** That is intended, and it is the quickest way to
confirm a candidate fix: guard 5 asserts that the shipped client still behaves like `upstream`, so a
fix makes it behave like `patched` instead. Guards 1–4 are independent of the provider and continue
to pass either way.

## Observed output

Hermetic, no credentials:

```
== hermetic mode ==
guard 1 OK: origin returns 304 for a correctly computed If-None-Match, and 401 when unauthenticated

4 identical GETs per chain against the emulated origin

ORDERING    ORIGIN STATUSES         CLIENT STATUSES     X-Cache:HIT  STORED X-Varied-Authorization
--------------------------------------------------------------------------------------------------------------
upstream    200 x4                  200 x4              0            (marker absent)
patched     200 x1, 304 x3          200 x4              3            present, len=44
provider    200 x4                  200 x4              0            (marker absent)

guard 2 OK: upstream ordering reached the origin every time, with a credential and a
            non-empty but incorrect If-None-Match -- the cache is wired up and is sending a
            validator that cannot match, rather than being bypassed
guard 3 OK: patched ordering revalidated (origin 304s, client X-Cache: HIT)
guard 4 OK: the two orderings produced materially different results
guard 5 OK: the provider's own client reproduces the upstream ordering's behaviour
```

Live, against `https://api.github.com/user`:

```
== live mode ==

4 identical GETs of https://api.github.com/user per chain
(origin statuses are unobservable against the real API; read the X-Cache column)

ORDERING    ORIGIN STATUSES         CLIENT STATUSES     X-Cache:HIT  STORED X-Varied-Authorization
--------------------------------------------------------------------------------------------------------------
upstream    none                    200 x4              0            (marker absent)
patched     none                    200 x4              3            present, len=44

live cache hits: upstream=0 patched=3 (of 4 requests)
```

### Why `len=44` is a consistency check, not a coincidence

The stored marker is not the token. [`transport.go:169-171`][store-hash] replaces the header value
with [`HashToken(vals[0])`][hash-token] before writing it. `HashToken` strips the `Bearer ` /
`token ` / `Basic ` prefix and returns `base64(sha256(bare token))`. A SHA-256 digest is 32 bytes,
and `base64.StdEncoding` of 32 bytes is always 44 characters — regardless of how long the token is.

Read the two columns separately, because they prove different things:

- **Presence** of the marker is the discriminator. It is only written when
  [`len(vals) > 0`][store-path], so its absence in the upstream row means `ghct` saw no
  `Authorization` header at all. That is the bug.
- **`len=44`** only confirms the stored value is a digest rather than a raw credential. It is *not*
  additional evidence that a token was present — `HashToken("")` is also 44 characters. It is a
  sanity check that the harness is reading the field it thinks it is, and it holds identically in
  hermetic mode (a fake token) and live mode (a real `gho_` token) despite those being different
  lengths.

Both are asserted in-process rather than printed. The cache stores only the digest, never the raw
credential, and the harness does not reintroduce one into its output — which is also worth knowing
before inspecting an on-disk `cache.db`.

## Scope: this is a demonstration, not a regression test

There is deliberately no `_test.go` here. This is a `main` you run and read.

The reasoning: a `go test` that only passes inside this repository asks a reader to trust this
repository. A program that builds the two chains from the pinned libraries and runs them does not,
and it can be lifted out and run against `github-conditional-http-transport` on its own.

The trade-off is that this directory is not wired into CI and can rot against a future `ghct`
release. A failure here means "re-read the library", not necessarily "the bug is back". Pinning the
behaviour in CI is a separate job, and belongs next to the fix in `internal/ghclient` rather than
here.

## The fix

Move `oauth2.Transport` so that it wraps ghct rather than sitting beneath it — that is, apply it
after the cache instead of before. The `patched` chain in [`chains.go`](./chains.go) is exactly that
ordering, and is the only difference between the two library-only chains.

## Who is affected

- [`Cache: true` is set unconditionally][cache-true] for the non-legacy client.
- [`cache_path` defaults to `""`][cache-path-schema], but that does **not** disable the cache. Every
  path into the non-legacy client goes through one of three source constructors —
  [`NewTokenSource`][src-token], [`NewAppSource`][src-app], [`NewAnonymousSource`][src-anon] — and
  each begins with the same guard: if `opts.Cache && opts.CacheBasePath == ""`, it allocates a fresh
  `os.MkdirTemp` directory and uses that. So an unset `cache_path` gives you a cache in a new
  temporary directory per process, discarded when the run ends. It chooses where the cache lives and
  how long it survives, not whether it exists.
- The attribute's description — ["if not set there will be no caching **between runs**"][cache-path-schema]
  — is accurate. Caching still happens *within* a run; only persistence across runs is lost.
- **However**, [`legacy_client` defaults to `true`][legacy-default], so the affected population is
  users who explicitly set `legacy_client = false`. That is the likely reason a ~0% hit rate went
  unnoticed.
- The cache is REST-only: [`getGraphQLClientOptions` never sets `Cache`][graphql-opts], unlike
  [`getRESTClientOptions`][rest-opts].

> **Note on the `provider` row in the table.** That scenario calls
> [`NewTokenRESTClient`][rest-client] directly and supplies its own `CachePath`, bypassing the
> `MkdirTemp` guard above so the store stays in a temp dir the harness can reopen and inspect. This
> does not affect what the row demonstrates — the ordering lives in [`newTransport`][newTransport],
> which is reached identically either way, and `CachePath` only decides where the store file sits.
> It does mean cache *location* behaviour should not be read off this harness.

## What this does not prove

- **It does not show that credential rotation breaks caching.** Rotation is handled.
  [`addConditionalHeaders`][conditional] has three branches, not two: [no entry][branch-1]
  (speculative `[]` guess), [entry with a matching vary marker][branch-2] (reuse the stored ETag),
  and [entry with a *non-matching* marker][branch-3] (**recompute** the expected ETag from current
  headers plus the cached body). A failed vary check selects the slower path; it is not a miss, and
  it behaves correctly once `Authorization` is visible. Worth stating because a zero hit rate
  invites rotation as an explanation, and that would be the wrong lead.
- **It does not prove the hermetic origin's ETag rule is byte-for-byte GitHub's.** See below. The
  live mode is what removes that dependency.
- **It says nothing about non-cacheable requests.** ghct
  [bypasses non-GET/HEAD, ranged, and `/rate_limit` requests][cacheable] before any of this applies.

## Caveat on the ETag formula

The formula is often written as `sha256(Accept + ":" + Authorization + ":" + Cookie + ":" + body)`.
Read literally, that emits a separator for a header that is absent. ghct does not do that:
[`hash.go`][hash-loop] iterates `requestHeaders.Values(name)` and contributes **nothing at all** — no
value, no colon — when a header is missing. This origin follows ghct's semantics.

For these requests the two readings coincide (`Accept` and `Authorization` are always present,
`Cookie` never is), so the result is unaffected. It does mean hermetic mode shares one assumption
with the library under test. Live mode does not: it runs against real GitHub, where the ETag rule is
GitHub's own, and reproduces the same 0-vs-3 split.

Anyone building their own origin from the shorthand formula should note this. Emitting
`Accept:Auth::body` where ghct computes `Accept:Auth:body` produces a mismatch on every request, and
makes a correctly ordered chain look broken.

[ghct-tree]: https://github.com/bored-engineer/github-conditional-http-transport/tree/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc
[prov-tree]: https://github.com/integrations/terraform-provider-github/tree/67f3fd10bda01461da6241543c1cd07e8e553f86

[store-path]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/transport.go#L165-L174
[store-hash]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/transport.go#L169-L171
[hash-token]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/hash.go#L40-L61
[vary-prefix]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/vary.go#L10-L11
[vary-check]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/vary.go#L21-L25
[vary-headers]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/hash.go#L13-L18
[hash-loop]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/hash.go#L28-L34
[conditional]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/conditional.go#L13-L53
[branch-1]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/conditional.go#L16-L23
[branch-2]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/conditional.go#L26-L29
[branch-3]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/conditional.go#L45-L50
[set-cache-status]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/transport.go#L35-L42
[hit]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/transport.go#L112-L118
[miss]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/transport.go#L188-L194
[rewrite-304]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/transport.go#L135-L137
[rewrite-speculative]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/transport.go#L138-L142
[cacheable]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/cacheable.go#L7-L18
[bbolt-key]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/bbolt/bbolt.go#L29
[bbolt-open]: https://github.com/bored-engineer/github-conditional-http-transport/blob/94778cbb26ea34bb63c577dfffaa99ebfb59e1cc/bbolt/bbolt.go#L70-L87

[newTransport]: https://github.com/integrations/terraform-provider-github/blob/67f3fd10bda01461da6241543c1cd07e8e553f86/internal/ghclient/transport.go#L32-L71
[oauth2-innermost]: https://github.com/integrations/terraform-provider-github/blob/67f3fd10bda01461da6241543c1cd07e8e553f86/internal/ghclient/transport.go#L35-L40
[ghct-outermost]: https://github.com/integrations/terraform-provider-github/blob/67f3fd10bda01461da6241543c1cd07e8e553f86/internal/ghclient/transport.go#L61-L68
[rest-client]: https://github.com/integrations/terraform-provider-github/blob/67f3fd10bda01461da6241543c1cd07e8e553f86/internal/ghclient/rest.go#L40-L44
[rest-opts]: https://github.com/integrations/terraform-provider-github/blob/67f3fd10bda01461da6241543c1cd07e8e553f86/internal/ghclient/options.go#L25-L39
[graphql-opts]: https://github.com/integrations/terraform-provider-github/blob/67f3fd10bda01461da6241543c1cd07e8e553f86/internal/ghclient/options.go#L42-L54
[cache-true]: https://github.com/integrations/terraform-provider-github/blob/67f3fd10bda01461da6241543c1cd07e8e553f86/github/provider.go#L609-L618
[cache-path-schema]: https://github.com/integrations/terraform-provider-github/blob/67f3fd10bda01461da6241543c1cd07e8e553f86/github/provider.go#L166-L171
[legacy-default]: https://github.com/integrations/terraform-provider-github/blob/67f3fd10bda01461da6241543c1cd07e8e553f86/github/provider.go#L160-L165
[src-token]: https://github.com/integrations/terraform-provider-github/blob/67f3fd10bda01461da6241543c1cd07e8e553f86/internal/ghclient/token.go#L20-L27
[src-app]: https://github.com/integrations/terraform-provider-github/blob/67f3fd10bda01461da6241543c1cd07e8e553f86/internal/ghclient/app.go#L45-L51
[src-anon]: https://github.com/integrations/terraform-provider-github/blob/67f3fd10bda01461da6241543c1cd07e8e553f86/internal/ghclient/anonymous.go#L20-L27
