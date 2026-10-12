# Changelog

Notable changes to Teranode, with an emphasis on anything an operator must act on.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

This file records **breaking changes**: anything that alters the behaviour of an
existing deployment without the operator changing their configuration, or that
requires a configuration or deployment change before upgrading. Changes that only
add capability, fix a bug without altering a documented contract, or remove
configuration that was already inert are not listed as breaking; a few are noted
under "Not breaking, but worth knowing" where they look alarming but are not.

## [Unreleased]

### Breaking — action required before upgrading

- **`grpc_admin_api_key` is now mandatory for the Blockchain service.** The service
  validates it at startup and refuses to start when it is empty, is a well-known
  placeholder (`testkey`, `changeme`, …), or is shorter than 16 characters. A value
  with surrounding whitespace is trimmed as it loads rather than rejected. Every Blockchain RPC except `HealthGRPC` requires
  the key, and the HTTP `invalidate`/`revalidate` routes require it too.

  Every service that talks to Blockchain must be given the same value. Upgrade
  credential-capable clients first and Blockchain last, or perform a coordinated
  restart — a Blockchain pod that starts before its clients have the key will reject
  them. The key travels in plaintext at `security_level_grpc=0`; use verified TLS
  across untrusted networks. (#1755)

- **Kubernetes: Kafka now sits behind a NetworkPolicy.** `deploy/kubernetes/kafka/kafka-shared-networkpolicy.yaml`
  restricts which pods may reach the shared Kafka broker. Pods that are not matched
  by the policy's selectors lose Kafka connectivity on upgrade. Check your pod labels
  against the policy before rolling it out. (#1617)

- **The shipped `bitcoin`/`bitcoin` RPC credentials are removed.** `settings.conf` no longer sets
  `rpc_user`/`rpc_pass` (only the `test` context keeps them). With both empty, the JSON-RPC server
  rejects every request, so miners, scripts and monitoring that relied on the default pair stop
  working. Set `rpc_user` and `rpc_pass` in `settings_local.conf` before upgrading. (#1790)

- **Asset admin routes fail closed without credentials.** `asset_requireAuthCredentials` defaults to
  `true`: while `rpc_user` or `rpc_pass` is empty, `POST fsm/state`, `POST block/invalidate`,
  `POST block/revalidate`, `GET settings`, and the dashboard's `/api/p2p/*` POSTs answer 401, and
  dashboard login fails. Set the credentials above, or set `asset_requireAuthCredentials = false`
  to restore anonymous admin access (not recommended). (#1790)

### Breaking — behaviour changes on upgrade

- **P2P HTTP endpoints are now rate limited.** `/health` and the websocket upgrade
  path are limited per source address, defaulting to 100 requests per second
  (`p2p_httpRateLimit`); requests over the limit receive HTTP 429. These endpoints
  were previously unlimited. Health checkers or probes polling faster than this will
  start being rejected. Set `p2p_httpRateLimit = 0` to disable. (#1566)

- **Settings that previously did nothing now take effect.** Each of the following
  carried a `key:` struct tag and appeared in the reference documentation, but
  `NewSettings()` never populated it, so the field stayed at its Go zero value no
  matter what was in `settings.conf`. They are now wired.

  **This affects every deployment, not only those that set a value.** The loader
  supplies non-zero code defaults, and the three peer-registry keys appear nowhere
  in `settings.conf`, so a node that has never mentioned them now runs the cleanup
  loop hourly, evicting peers on a 24-hour TTL under a 10,000-entry LRU bound.
  Review these before rolling out.

  | Setting | Previously | Now |
  |---|---|---|
  | `p2p_peer_registry_max_size` | `0` — LRU bound disabled | configured value enforced (default 10,000) |
  | `p2p_peer_registry_ttl` | `0` | configured value applies (default 24h) |
  | `p2p_peer_registry_cleanup_interval` | `0` — the cleanup loop never ran | cleanup runs at the configured interval (default 1h); peers are evicted |
  | `pruner_skipDuringCatchup` | `false` — flag unreachable | configurable |
  | `pruner_skipProcessExpiredPreservations` | `false` — flag unreachable | configurable |
  | `aerospike_enable_preserve_filter_expressions` | permanently `false` | configurable |
  | `subtreevalidation_txMetaCacheTrimRatio` | read straight from gocore, bypassing the typed settings | read from typed settings; `settings.conf` ships `5`, the code default is `2` |

  The peer-registry entries are the ones most likely to be noticed: with the cleanup
  interval at zero the eviction loop never started, so registries grew without bound.
  After upgrading, peers are evicted on the configured TTL and LRU bound.
  (#1645, #1641, #1644, #1746)

- **`aerospike_enable_preserve_filter_expressions` is mutually exclusive with
  `aerospike_use_native_teranode_ops` for preserve operations.** `PreserveTransactions`
  is native-op-covered (`subOpPreserveUntil`). With native ops active and this flag
  false, the preserve write and its prune-eligibility filter go through the native
  operate path. Setting this flag true short-circuits that dispatch and uses a
  client-side BatchWrite instead, so preserve silently stops using the native path
  while spend and setMined continue to use it. Do not enable both unless that trade
  is intended. (#1644)

- **Allowlisted signed peers get 413 for oversized bodies.** With a non-empty
  `asset_peerAuthAllowlist`, a signed request body larger than
  `max(1 MiB, 32 x subtreevalidation_missingTransactionsBatchSize + 4096)` (this node's value) is
  rejected with 413. Keep allowlisted peers' `subtreevalidation_missingTransactionsBatchSize` at or
  below this node's, or their catchup fails against it. Nodes with an empty allowlist (the default)
  never read the body and are unaffected. (#1788)

- **`teranode_asset_http_peer_auth_result_total` labels change meaning.** The allowlist check now
  runs first, so on a node with an empty allowlist every well-formed signed request counts as
  `not_allowlisted`; `replay`, `bad_digest`, `unknown_key` and `ok` are no longer emitted there, and
  `bad_sig` means only malformed headers. A new value, `body_too_large`, is added. Alerts on
  `result="replay"`, `"bad_digest"` or `"bad_sig"` go quiet on default nodes. (#1788)

- **Six more Asset routes are rate limited as heavy endpoints.** `GET /subtree/:hash/txs/json`,
  `/blockgraphdata/:period`, `/utxos/:hash/json` and the three `/merkle_proof/:hash` variants now
  share the per-IP heavy bucket (`asset_httpHeavyRateLimit`, default 10 req/s) instead of only the
  global limit (1024 req/s). SPV and merkle-proof clients, explorers, or anything behind one IP
  making more than 10 heavy requests a second will get 429. With `asset_peerAuthAllowlist` empty
  (the default) miners and peers are in the same per-IP bucket. Raise `asset_httpHeavyRateLimit`,
  or allowlist trusted peers. (#1789)

- **Cross-origin requests no longer get credentials by default.** With `asset_corsAllowOrigins`
  empty (the default) Asset still reflects any origin, but without
  `Access-Control-Allow-Credentials`; only exact-match listed origins get credentials. Credentialed
  cross-origin browser callers break, including the Vite dev dashboard on `:5173`. List them, e.g.
  `asset_corsAllowOrigins = http://localhost:5173`. The production dashboard is same-origin and
  unaffected. CORS is not CSRF protection. (#1793)

- **Setting `asset_trustedProxyCIDRs` stops trusting private and link-local proxies implicitly.**
  Only loopback and the listed CIDRs are trusted. If your ingress or load balancer sits on an
  unlisted RFC1918 address, every client resolves to the proxy's IP and shares one rate-limit and
  ban-list key. Add internal proxy ranges to the list explicitly. Nothing changes while the setting
  is empty. (#1793)

- **Global rate-limit 429s leave the access log and request metrics.** The access log now runs
  after the global limiter, so those 429s appear only in
  `teranode_asset_http_rate_limited_total{scope="global"}`, not in
  `teranode_asset_http_request_duration_seconds`/`..._response_size` or the WARN access log. The
  `method` label collapses non-standard methods to `other`. (#1793)

- **Catchup-route 429s move to `scope="heavy_catchup"`.** `GET /subtree/:hash`,
  `/subtree_data/:hash`, `/blocks/:hash`, `/block/:hash` and `POST /subtree/:hash/txs` now have their
  own heavy limiter and metric label; queries on `teranode_asset_http_rate_limited_total{scope="heavy"}`
  stop counting them. Use `scope=~"heavy|heavy_catchup"`. (#1801)

- **HTTP 429 has its own error code, `ERR_SERVICE_RATE_LIMITED` (52).** It was the generic
  `ERR_SERVICE_ERROR` (59). The new code is retryable and is not a transient local error. Code
  matching `ErrServiceError` on a 429, or log alerts on `SERVICE_ERROR (59)`, will miss it; an older
  service receiving it over gRPC sees an unknown enum value. Upgrade services together. (#1802)

- **Catchup fetches retry a peer's 429.** `POST /subtree/:hash/txs` and `GET /subtree_data` retry 429
  and 503 up to 6 times with backoff (honouring `Retry-After` up to 5s) instead of reporting the
  peer at once, so a serving peer can see up to 6 requests per batch. These requests are now signed
  when a peer key is configured, and the catchup POST is sent as `application/octet-stream`. (#1802)

- **One 10-minute deadline covers each missing-transactions batch.**
  `subtreevalidation_missingTransactionsFetchTimeout` (default 10m) bounds the POST, its retries and
  the body read, replacing the per-request `http_streaming_timeout` on this path. If you raised
  `http_streaming_timeout` above 10m, set this too. (#1802)

### Breaking — pending, not yet merged

- **Unauthenticated Blockchain read RPCs will be bounded.** Count and range
  parameters on the public read RPCs will be rejected with `InvalidArgument` above
  configurable caps: `blockchain_maxBlockHeadersPerRequest`,
  `blockchain_maxBlocksByHeightRange`, `blockchain_maxInvalidBlocksPerRequest` and
  `blockchain_maxMedianTimePastHeights`. Callers that currently request very large
  counts will start receiving errors.

  **Upgrade-order hazard:** the median-time-past cap rejects the whole-chain request
  that every pre-change client sends on first load, and those clients do not retry
  `InvalidArgument`. During a rolling upgrade, an old-image validator,
  subtreevalidation or blockvalidation pod talking to an upgraded Blockchain pod will
  fail every `EnsureMTPLoaded` and validate nothing until it is reimaged. No chain
  state is corrupted. Upgrade Blockchain last, or raise
  `blockchain_maxMedianTimePastHeights` for the duration of the rollout. (#1776)

- **`/lastblocks` returns at most 1000 blocks, and a negative `n` returns 400.** `n` was unbounded,
  and `n=-1` returned every block on SQLite (and a 500 on Postgres). It is now clamped to 1000, or
  lower via `asset_maxLastNBlocks`. (#1792)

- **Common-ancestor header routes reject more than 256 locator hashes with 400.** Applies to
  `/headers_to_common_ancestor/:hash` and `/headers_from_common_ancestor/:hash` (all formats). The
  catchup client sends about 32, so only non-standard clients are affected. A negative `n` on these
  routes also returns 400 now. (#1792)

- **`asset_concurrency_get_legacy_block_reader` now bounds whole streams, and defaults to 0
  (unlimited).** The permit used to be released after setup; it is now held for the whole,
  client-paced block stream. The default moved from -1 (NumCPU) to 0, so default nodes are less
  restricted than before. If you set an explicit value, it now caps concurrent `GET /block_legacy`,
  `/rest/block/:hash.bin` and SV-peer block streams, and extra requests wait 30s and then fail.
  Re-size it for whole-stream semantics, or remove it. (#1794)

- **Batch POSTs are capped at 16384 records by default.** `asset_maxBatchRecords` defaults to
  16384. `POST /utxos` (and `/hex`, `/json`) above it returns 413. On `POST /subtree/:hash/txs` the
  cap is never below this node's `subtreevalidation_missingTransactionsBatchSize`, but a peer
  configured with a larger batch size gets 413, which is not retried and is reported against the
  serving peer. Raise `asset_maxBatchRecords` (or set 0 to disable it on a node with no untrusted
  callers), and keep peer batch sizes at or below this node's. (#1797)

### Not breaking, but worth knowing

- **Dead configuration keys were removed.** These had no readers anywhere in the
  codebase, so removing them changes no behaviour. If you have them in a config
  file they were already doing nothing and will continue to be ignored — the
  config loader has no schema and does not reject or report unknown keys, which
  is precisely why these survived as long as they did. Removed:
  `securityLevelGRPC` — a casing variant that never matched the real
  `security_level_grpc` (#1674) — and `blockchain_subscription_timeout` (#1643).

  **`blockmaxsize` was not removed and is still live.** #1640 deleted a duplicate
  `BlockSettings.MaxSize` field that carried the same tag; the key itself is read
  at `settings/settings.go` into `Policy.BlockMaxSize`, enforced in Block Assembly
  and reported over RPC as `maxminedblocksize`. Leave it set.

- **Two Blockchain RPCs now work that previously did not.**
  `GetLatestBlockHeaderFromBlockLocator` and `GetBlockHeadersFromOldest` were
  implemented under names that did not satisfy the generated server interface, so
  both returned `Unimplemented` to every gRPC caller. They now dispatch correctly.
  Callers that treated `Unimplemented` as "unsupported" will start receiving real
  responses. (#1848, pending)

- **The Postgres circuit breaker is now reachable.** The breaker could not be turned
  on because its settings were never populated. Two forms exist and both are wired
  now: a global `postgres_circuitBreakerEnabled` and a per-service
  `<prefix>_postgres_circuitBreakerEnabled` that overrides it. The default remains
  off, so no deployment changes behaviour unless you opt in. (#1642)

- **Development nodes get their own RPC password.** `teranode-dev` now writes
  `rpc_user.dev.<name>` and a random `rpc_pass.dev.<name>`. A dev block written before this change
  has neither, so `teranode-dev generate`/`rpc` get 401 until you re-run `teranode-dev init`.
  New startup warnings flag plaintext admin access, cookies without `Secure`, and empty credentials. (#1790)

- **Websocket caps are available but off by default** (`asset_maxWebsocketConnections`,
  `asset_maxWebsocketConnectionsPerIP`, `asset_websocketReadLimit`). The per-IP cap separates only
  clients whose own or proxy-appended address is public. The p2p relay now forwards only its five
  relay channels (others had no subscribers), and startup warns about a plaintext p2p websocket and
  an unset read limit. (#1791)

- **Invalid `asset_corsAllowOrigins` entries stop Asset at startup** (a glob such as `*`, `null`, a
  path or userinfo). Responses gain an `X-Signature-Scope` header. (#1793)

- **The heavy-route limit is looser.** The two heavy buckets are independent per IP, so one IP can
  sustain up to 2x `asset_httpHeavyRateLimit` across all heavy routes, and the catchup-route burst
  rises from 10 to 40 at defaults (the catchup fan-out, capped at 4x the rate). If you relied on the
  old ceiling, lower `asset_httpHeavyRateLimit` or set `asset_httpHeavyRateBurst`. (#1801)

- **`asset_maxNBlocks` never goes below 100**, the catchup client's batch size; a lower value is
  raised with a startup warning. `asset_maxBlockHeaders` applies only to
  `/headers_to_common_ancestor`, never to the catchup route. (#1792, pending)

- **Legacy block and on-demand `subtree_data` streams fail rather than serving a reconstructed
  transaction that is incomplete.** The local-service client used for SV-peer block serving no
  longer follows redirects, so point `asset_httpAddress` at its final address. (#1794, pending)

- **A `POST /subtree/:hash/txs` body that isn't a whole number of 32-byte hashes returns 400
  instead of 500.** (#1797, pending)

- **New switches can withhold backend detail from anonymous callers** (`asset_publicErrorDetail`,
  `asset_publicHealthDetail`, `asset_healthStrictStatus`, `asset_publicPeersDetail`,
  `asset_txMetaRawEnabled`); all default to today's output. `asset_healthStrictStatus = true` makes a
  Kubernetes readiness probe on `:8090/health` fail whenever a dependency is unhealthy (for example
  Aerospike `stop_writes`), removing Asset pods from their Service. The Asset error log line gains a
  `correlation_id=` field. (#1796, pending)

- **The block graph is bucketed in the store**, and `GetBlockGraphData` responses (gRPC and JSON)
  gain a `bucket_seconds` field (omitted, i.e. 0, for ranges of 24h or less). The returned data
  points are unchanged. (#1798, pending)
