# moos-router

WF16 federation **read router** for [mo:os](https://github.com/Collider-Data-Systems/moos-kernel) kernels. A thin, **stateless** HTTP proxy that fans reads across a set of sovereign kernel logs and routes writes to the single kernel-of-record. Go, stdlib-only (`go.mod`: `module moos/router`, `go 1.23`, zero dependencies).

## What it is — and what it is NOT

The router is **NOT a kernel**. It holds **no rewrite log**, **no operad / type system**, and enforces **none of the kernel gates** (no §M11 session-liveness, no §M12 admin-capability, no fold, no operad validation). **Log-is-truth stays inside each `moos-kernel` instance.** This binary only inspects request shape, picks a target kernel (or fans out), and proxies — it never persists or mutates state.

```
HTTP / MCP clients
        |
        v
   moos-router        (WF16 — stateless read fan-out + write-proxy, :9000)
        |
   +----+----+----+----+ - - peer routers (WF16 cascade)
   |    |    |    |    |
  k0   k1   k2   k3  ...      (per-kernel sovereign logs — truth lives here)
```

## Dispatch (master)

`ServeHTTP` (`internal/proxy/proxy.go`) routes by method + path, in this order:

| Match | Handler | Behavior |
|---|---|---|
| `GET /healthz` | `handleHealthz` | **Health fan-in.** Concurrently `GET /healthz` on every unique kernel URL; returns `{"status":"ok","kernels":[{url,status,log_len,error?}]}` sorted by URL. Router itself is always `ok`; per-kernel `status:"down"` + `error` on failure/timeout. |
| `POST /rewrites`, `POST /programs` | `handleRoutedPost` | **Single-kernel URN-routed write-proxy.** Reads the body, extracts a URN (`findURN`: first non-empty of `node_urn`, `target_urn`, `src_urn`, `relation_urn`, recursing into arrays — so a program batch routes by its first envelope), `Route()`s it to one kernel, forwards the original body verbatim. No URN → `422`; URN with no matching rule → `422`. Writes are **never** fanned out (atomic batch semantics rely on a single log's serializability). |
| `GET /state/nodes/{urn}` | `handleRoutedNodeRequest` | **Local-then-peer cascade.** URL-unescapes the path URN, tries the local shard via `Route()`; if that misses or returns `404`, cascades the same request to each `--peer` router in order, returning the first non-`404`. Exhausted → `404` `node ... not found in local shards or peers`. |
| everything else (e.g. `GET /state/nodes`, `GET /state/relations`) | `handleFanout` | **Fan-out-and-merge.** Broadcasts the request concurrently to every unique kernel; JSON-decodes each `2xx` body (arrays are flattened, objects appended) into one merged JSON array. **Partial success → `200`** with the merged survivors (failed/`non-2xx`/invalid-JSON kernels are logged and skipped). **All fail → `502`** `fan-out failed for all kernels: ...`. No kernels configured → `503`. |

### Timeouts (commit `1577e59`)

- **Fan-out** (`handleFanout`) and **cascade / single-node forward** (`tryForward`): **5s** per upstream request (`context.WithTimeout`).
- **Health** (`handleHealthz`): **2s** default per kernel, overridable via `--health-timeout`; `Router.HealthTimeout <= 0` falls back to the 2s default.

The single write-proxy path (`forwardSingle`) inherits the inbound request context (no extra deadline imposed by the router).

## type-map-routing (feature branch only)

> **Present only on branch `feat/type-map-routing`** (worktree `D:\HPZ440\moos-router-feat-type-map-routing`, currently at `c038c87`). **NOT on `master`** (`1577e59`).

The feature adds a `TypeRule` (`TypeID -> TargetURL`) table and a `--type-map type_id=http://host:port` (repeatable) flag. In `handleRoutedPost`, **type routing takes precedence over URN-prefix routing**: the body's `type_id` (via `findTypeID`) is matched against the type-map first, and only if it yields no target does the router fall back to URN-prefix `Route()`. `NewRouter` becomes variadic (`NewRouter(rules, typeRules...)`) so existing shard-only callers compile unchanged; the unique-kernel set is computed over both tables.

**master vs. the feat worktree differ** — `master` has shard + peer routing only (`NewRouter([]ShardRule)`, no `--type-map`, no `TypeRule`/`RouteByType`/`findTypeID`). Build and run from the worktree if you need type routing.

## Topology configuration (CLI flags — not read from JSON)

The shard table, peer list, and (on the feat branch) type-map are supplied **entirely via CLI flags**. The router does **not** read any JSON config file at runtime.

| Flag | Purpose |
|---|---|
| `--listen` | HTTP listen address (default `:9000`) |
| `--shard` | Repeatable: `<urn-prefix>=<kernel-url>`. Longest-prefix match wins; ties broken by priority (desc). |
| `--default` | Fallback kernel URL for URNs matching no shard (added as an empty-prefix rule at priority `-1`). |
| `--peer` | Repeatable: peer router URL for the WF16 cross-workstation cascade. |
| `--health-timeout` | Per-kernel `/healthz` timeout (default `2s`). |
| `--type-map` | **feat/type-map-routing only.** Repeatable: `<type_id>=<kernel-url>`, checked before shard rules for writes. |

- **Human SOT (mirror):** `ffs0/dev/config/moos-federation.topology.json` is the authored, human-readable federation port map. It is **a mirror, not consumed by the binary** — the launcher translates it into flags. If the launcher and the topology JSON disagree, the live `/healthz` readback wins (re-read, don't trust the doc).
- **Launcher:** `D:\HPZ440\start_federation.ps1` starts the Z440 kernels + the router on `:9000`, passing the `--shard`/`--default`/`--peer` flags. (It prefers the `moos-router-feat-type-map-routing` worktree's `moos-router.exe` and falls back to `moos-router/moos-router.exe`.)

## Build / Run

```bash
go build ./...     # compile (artifact moos-router.exe is gitignored)
go test ./...      # proxy routing tests (internal/proxy/proxy_test.go)
```

Run (master shape):

```bash
go run ./cmd/router \
  --listen :9000 \
  --shard urn:moos:ws:hp-z440=http://localhost:8000 \
  --shard urn:moos:kernel:hp-z440.menno=http://localhost:8001 \
  --shard urn:moos:kernel:hp-z440.lola=http://localhost:8002 \
  --shard urn:moos:kernel:hp-z440.moos=http://localhost:8003 \
  --default http://localhost:8000 \
  --peer http://100.106.220.58:9000
```

Tests cover proxy routing only — kernel-side concerns (operad validation, §M11/§M12 gates, fold) are each kernel's responsibility and pass through untouched.

## Companion repositories

- [moos-kernel](https://github.com/Collider-Data-Systems/moos-kernel) — the rewriting kernel (WF01–WF21 validator, ADD/LINK/MUTATE/UNLINK evaluator, §M11/§M12 gates, fold). The router federates instances of it.
- `ffs0` — private control/research workspace + KB; holds the topology mirror and the launcher.
- `moos-config` — **LEGACY, do not use.**

All at `github.com/Collider-Data-Systems/*`.

## Status / License

Active, stateless, multi-kernel federation read path. Copyright (c) Collider-Data-Systems — redistribution rights not granted by default until a formal license is applied; contact the organization for current terms.

---
authored-by: agent:claude-cowork.hp-z440 / session:sam.z440-cowork-workspace / config-overhaul
