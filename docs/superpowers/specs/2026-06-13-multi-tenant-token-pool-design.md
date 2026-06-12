# Multi-Tenant Token Pool Design

**Date**: 2026-06-13
**Status**: proposed
**Branch**: `proposal/multi-tenant-pool`

## Goal

Support multiple independent token pools in the same project, where each pool
has its own Microsoft admin refresh token, tenant, SOCKS5 proxy, bot user prefix,
token producer, token queue, cleanup flow, and delete flow.

The first implementation should optimize for operational safety and low risk:
one `Manage_User` service can host many pools, while each `Get_Profile` process
is bound to exactly one pool and one email shard.

## Current Constraints

- `Manage_User/app.py` loads only the first entry from `admin_token.json`.
- `Manage_User` keeps global state: one `AdminTokenManager`, one `token_queue`,
  one `TokenProducer`, and one delete stats object.
- `StartupCleaner` and user creation use the global `bot_` prefix, so multiple
  instances can delete each other's users if they share a tenant.
- `/tokens/next`, `/users/delete`, `/proxy`, and `/status` are not pool-aware.
- `Get_Profile` assumes one token source, one proxy, one token manager, and one
  delete channel.
- The bitmap checkpoint is file-local and not safe for multiple processes
  writing the same checkpoint. Multi-instance runs need email sharding.

## Recommended Architecture

Introduce a `pool_id` as the primary routing key.

Each pool owns:

- Admin OAuth state via one `AdminTokenManager`
- One SOCKS5 proxy
- One bot user prefix
- One in-memory token queue
- One `TokenProducer`
- One cleanup/deletion context
- Per-pool stats

Recommended data flow:

```text
admin_token.json
  -> PoolRegistry
      -> PoolState(pool_01): AdminTokenManager + TokenProducer + queue + proxy
      -> PoolState(pool_02): AdminTokenManager + TokenProducer + queue + proxy

Get_Profile --pool pool_01
  -> GET  /pools/pool_01/tokens/next?count=300
  -> GET  /pools/pool_01/proxy
  -> POST /pools/pool_01/users/delete
```

## Configuration Shape

Replace "first admin wins" behavior with explicit pool records:

```json
[
  {
    "pool_id": "pool_01",
    "enabled": true,
    "domain": "example-a.com",
    "tenant_id": "tenant-a",
    "refresh_token": "...",
    "proxy": "socks5h://user:pass@host:port",
    "bot_prefix": "bot_p01_",
    "queue": {
      "low_watermark": 400,
      "target_size": 800,
      "max_size": 1000,
      "producer_batch_size": 100
    }
  },
  {
    "pool_id": "pool_02",
    "enabled": true,
    "domain": "example-b.com",
    "tenant_id": "tenant-b",
    "refresh_token": "...",
    "proxy": "socks5h://user:pass@host2:port",
    "bot_prefix": "bot_p02_"
  }
]
```

Validation rules:

- `pool_id` must be unique, stable, and match `^[a-zA-Z0-9_-]{1,64}$`.
- `bot_prefix` must be unique within the same tenant.
- `proxy` must normalize to `socks5h://...` or be empty.
- `tenant_id`, `refresh_token`, and `domain` are required for enabled pools.
- Queue defaults can be inherited when omitted.

## Manage_User Design

Add these core types:

```text
PoolConfig
PoolState
PoolRegistry
```

`PoolState` should contain:

```text
pool_id
config
admin_token_manager
token_queue
producer
delete_stats
startup_status
```

Startup sequence:

1. Load and validate all enabled pool configs.
2. Build one `PoolState` per config.
3. Run startup cleanup per pool, scoped by that pool's `bot_prefix`.
4. Start one `TokenProducer` thread per pool.
5. Mark each pool as `ready`, `warming`, or `degraded`.
6. Start Flask even if one pool is degraded, unless all pools fail config load.

Do not block the whole service forever waiting for every pool to fill. Per-pool
readiness should be visible in `/status`.

## API Contract

Preferred new endpoints:

| Method | Path | Behavior |
|---|---|---|
| `GET` | `/pools` | List pool IDs and readiness, no secrets |
| `GET` | `/pools/<pool_id>/status` | Per-pool queue, producer, delete stats |
| `GET` | `/pools/<pool_id>/proxy` | Return that pool's proxy URL |
| `GET` | `/pools/<pool_id>/tokens/next?count=N` | Pop up to N tokens from that pool |
| `POST` | `/pools/<pool_id>/users/delete` | Delete users for that pool only |

Compatibility endpoints can remain:

- `/tokens/next?pool_id=pool_01`
- `/proxy?pool_id=pool_01`
- `/users/delete` with `{"pool_id": "pool_01", "emails": [...]}`

But the path-scoped `/pools/<pool_id>/...` endpoints should be the canonical
contract because they make ownership explicit.

Status codes:

- `200`: successful token/proxy/status/delete response
- `202`: token queue empty or warming
- `400`: invalid count, invalid email, malformed JSON
- `404`: unknown or disabled pool
- `422`: pool config is known but invalid/degraded
- `503`: pool exists but is not ready

Response shape for token fetch:

```json
{
  "pool_id": "pool_01",
  "tokens": [
    {
      "pool_id": "pool_01",
      "email": "bot_p01_abcd@example.com",
      "refresh_token": "...",
      "tenant_id": "tenant-a"
    }
  ],
  "count": 1
}
```

Response shape for status:

```json
{
  "pool_id": "pool_01",
  "ready": true,
  "queue_size": 612,
  "producer": {
    "running": true,
    "total_created": 1200,
    "total_tokens": 1180,
    "total_failed_create": 3,
    "total_failed_token": 17
  },
  "delete": {
    "total_deleted": 500,
    "total_failed_delete": 2
  }
}
```

## Security Rules

- Treat the API as internal-only by default. If binding beyond localhost, require
  an `X-Internal-Token` or equivalent shared secret.
- Never return refresh tokens in `/status`, `/pools`, or logs.
- Only `/tokens/next` returns refresh tokens.
- Validate `pool_id` against the loaded registry, not against arbitrary request
  body values.
- Validate delete emails before Graph calls.
- For runtime deletes, reject emails that do not match the pool's `bot_prefix`
  and expected domain unless an explicit admin override is added.
- Do not allow one pool to delete or purge users created by another pool.
- Redact proxy credentials in logs and status.

## Get_Profile Design

Phase 1 should keep `Get_Profile` single-pool:

```bash
get_profile.exe \
  --api http://localhost:5000 \
  --pool pool_01 \
  --emails emails_pool_01.txt \
  --result result_pool_01.txt \
  --checkpoint emails_pool_01.ckpt
```

Changes:

- Add `--pool` flag.
- API client calls `/pools/<pool_id>/tokens/next`.
- Proxy resolution calls `/pools/<pool_id>/proxy`.
- `TokenInfo` carries `PoolID`.
- Dead token cleanup calls `/pools/<pool_id>/users/delete`.
- Logs include pool ID.

This keeps each Go process simple and avoids cross-pool proxy routing inside
one worker pool.

## Email Sharding

Do not run multiple `Get_Profile` processes against the same email file and
checkpoint.

Recommended phase 1 operation:

```text
emails_pool_01.txt + emails_pool_01.ckpt -> Get_Profile --pool pool_01
emails_pool_02.txt + emails_pool_02.ckpt -> Get_Profile --pool pool_02
emails_pool_03.txt + emails_pool_03.ckpt -> Get_Profile --pool pool_03
```

Use a deterministic split command or script before launch. A future coordinator
can own central job assignment, but that is a separate feature.

## Implementation Plan

### Phase 0: Baseline Reliability

Merge or reapply the reliability branch first:

- Retry paths should not drop jobs or mark checkpoint bits.
- Delete cleanup should retry transient failures.
- Queue shutdown should be panic-safe.
- `/users/delete` should validate request payloads.

### Phase 1: Pool Registry in Manage_User

- Add `PoolConfig` validation.
- Add `PoolState` and `PoolRegistry`.
- Replace global `_token_mgr`, `_producer`, `token_queue`, and delete stats with
  per-pool state.
- Keep a default pool fallback when exactly one pool is configured.

### Phase 2: Pool-Scoped User Lifecycle

- Make `BulkUserCreator` accept `bot_prefix`.
- Make `StartupCleaner` accept `bot_prefix`.
- Make purge/delete scoped to the same prefix/domain.
- Make `TokenProducer` accept per-pool queue settings and proxy.

### Phase 3: Pool-Aware API

- Add `/pools/<pool_id>/...` endpoints.
- Keep compatibility endpoints temporarily.
- Add API tests for unknown pool, disabled pool, empty queue, valid token fetch,
  invalid delete email, and cross-prefix delete rejection.

### Phase 4: Get_Profile Pool Flag

- Add `--pool`.
- Update token API client to path-scoped endpoints.
- Include pool ID in token/dead-delete paths.
- Add tests for API URL construction and delete payload routing.

### Phase 5: Multi-Instance Runner

Optional helper script:

```text
run_pool pool_01 emails_pool_01.txt 5000
run_pool pool_02 emails_pool_02.txt 5000
run_pool pool_03 emails_pool_03.txt 5000
```

This can be PowerShell for Windows and shell/systemd templates for Linux.

### Phase 6: Optional Single-Process Multi-Pool Go Scheduler

Only build this if operationally needed. It requires:

- Pool-aware token queues in Go.
- Per-pool SOCKS5 Loki clients.
- Token selection policy.
- Fairness and backpressure across pools.
- More complex retry semantics.

This is not recommended for the first implementation.

## Tests

Python:

- Config validation: duplicate `pool_id`, duplicate `bot_prefix` in same tenant,
  missing required fields, malformed proxy.
- Registry routing: `/pools`, `/pools/<pool_id>/status`,
  `/pools/<pool_id>/tokens/next`.
- Delete validation: unknown pool, invalid email, cross-prefix email rejection.
- Producer isolation: pool A producer does not push into pool B queue.
- Cleanup isolation: startup cleanup uses only the configured prefix.

Go:

- `--pool` flag required when multiple pools are configured.
- Token fetch URL uses `/pools/<pool_id>/tokens/next`.
- Proxy fetch URL uses `/pools/<pool_id>/proxy`.
- Delete worker sends to `/pools/<pool_id>/users/delete`.
- Logs and token structs preserve `PoolID`.

Integration:

- Two fake pools with separate queues and proxies.
- Token fetch from pool A never returns pool B token.
- Dead token from pool B deletes through pool B endpoint.
- Two `Get_Profile` processes with separate email/checkpoint files can run
  concurrently without checkpoint collision.

## Risks and Mitigations

- **Cross-tenant deletion**: Require `pool_id` path ownership and prefix/domain
  validation before delete.
- **Proxy mismatch**: Resolve proxy per pool and bind each `Get_Profile` process
  to one pool.
- **Startup delay**: Report per-pool readiness instead of blocking the whole
  service on every pool.
- **Config drift**: Validate pool config at startup and expose degraded pools in
  `/pools`.
- **Operational overload**: Start with one Go process per pool and explicit email
  shards; avoid single-process multi-pool scheduling until needed.

## Recommendation

Implement phases 0 through 4 first. That gives multi-admin, multi-proxy, and
multi-email-shard support with the least architectural risk. Defer the
single-process multi-pool Go scheduler until there is evidence that operating
one `Get_Profile` process per pool is not enough.
