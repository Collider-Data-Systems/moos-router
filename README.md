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
| `POST /admin/topology/reload` | `handleAdminReload` | **Localhost-only.** Re-reads `--topology-file` and atomically swaps the routing table. A file that fails to parse or would yield an empty table is rejected (`500`) and the old table stays live. |
| `GET /admin/topology` | `handleAdminTopology` | **Localhost-only.** Dumps the live routing table (shard rules with priorities, type rules, peers, kernels, file path, local-host) so operators can diff running state against the topology file. |
| `POST /rewrites`, `POST /programs` | `handleRoutedPost` | **Single-kernel URN-routed write-proxy.** Reads the body, extracts a URN (`findURN`: first non-empty of `node_urn`, `target_urn`, `src_urn`, `relation_urn`, recursing into arrays — so a program batch routes by its first envelope), `Route()`s it to one kernel, forwards the original body verbatim. No URN → `422`; URN with no matching rule → `422`. Writes are **never** fanned out (atomic batch semantics rely on a single log's serializability). |
| `GET /state/nodes/{urn}` | `handleRoutedNodeRequest` | **Local-then-peer cascade.** URL-unescapes the path URN, tries the local shard via `Route()`; if that misses or returns `404`, cascades the same request to each `--peer` router in order, returning the first non-`404`. Exhausted → `404` `node ... not found in local shards or peers`. |
| everything else (e.g. `GET /state/nodes`, `GET /state/relations`) | `handleFanout` | **Fan-out-and-merge.** Broadcasts the request concurrently to every unique kernel; JSON-decodes each `2xx` body (arrays are flattened, objects appended) into one merged JSON array. **Partial success → `200`** with the merged survivors (failed/`non-2xx`/invalid-JSON kernels are logged and skipped). **All fail → `502`** `fan-out failed for all kernels: ...`. No kernels configured → `503`. |

### Timeouts (commit `1577e59`)

- **Fan-out** (`handleFanout`) and **cascade / single-node forward** (`tryForward`): **5s** per upstream request (`context.WithTimeout`).
- **Health** (`handleHealthz`): **2s** default per kernel, overridable via `--health-timeout`; `Router.HealthTimeout <= 0` falls back to the 2s default.

The single write-proxy path (`forwardSingle`) inherits the inbound request context (no extra deadline imposed by the router).

## type-map routing (on master)

`TypeRule` (`TypeID -> TargetURL`) rides the `--type-map type_id=http://host:port` (repeatable) flag. In `handleRoutedPost`, **type routing takes precedence over URN-prefix routing**: the body's `type_id` (via `findTypeID`) is matched against the type-map first, and only if it yields no target does the router fall back to URN-prefix `Route()`. Type rules are the only CLI-sourced routing state that survives a topology-file reload (the file has no type-map expression yet).

## Topology configuration (file-as-SOT)

The routing table comes from `--topology-file` (the shared `ffs0/dev/config/moos-federation.topology.json`), loaded **at boot** and re-readable at runtime via `POST /admin/topology/reload`. CLI `--shard`/`--default` flags act as a bootstrap fallback: if the file fails to load at boot, the router serves the flag table and logs loudly instead of dying (or exits if no flags were given either).

| Flag | Purpose |
|---|---|
| `--listen` | HTTP listen address (default `:9000`) |
| `--topology-file` | Path to `moos-federation.topology.json`. Loaded at boot; enables `POST /admin/topology/reload` + `GET /admin/topology`. |
| `--local-host` | This machine's hostname key in the topology file (e.g. `hp-z440`). **Required with `--topology-file`** (falls back to `MOOS_LOCAL_HOST` env); prevents silent Tailscale-hairpin misrouting. |
| `--shard` | Repeatable: `<urn-prefix>=<kernel-url>`. Longest-prefix match wins; ties broken by priority (desc). Bootstrap fallback when a topology file is configured. |
| `--default` | Fallback kernel URL for URNs matching no shard (added as an empty-prefix rule at priority `-1`). Bootstrap fallback when a topology file is configured. |
| `--peer` | Repeatable: peer router URL for the WF16 cross-workstation cascade. Replaced by the file's `routers[local-host].peers` when the file loads. |
| `--health-timeout` | Per-kernel `/healthz` timeout (default `2s`). |
| `--type-map` | Repeatable: `<type_id>=<kernel-url>`, checked before shard rules for writes. Survives reloads. |

The router consumes these topology-file keys (all others are ignored):

- `kernels.<name>.{host, http_local, http_tailscale}` → one shard rule `urn:moos:kernel:<name>` per kernel; same-host kernels use `http_local`, remote ones `http_tailscale`.
- `routers.<key>.{host, peers}` → peer list for the entry whose `host` matches `--local-host`.
- `routers.<key>.default_kernel` → the fallback rule (kernel-name reference; same shape as `--default`).
- `routers.<key>.shard_aliases` → extra URN-prefix rules, e.g. `"urn:moos:ws:hp-z440": "hp-z440.primary"`.

A reload that would produce an **empty routing table** (keyless JSON, no reachable kernels, dangling `default_kernel`/`shard_aliases` reference) is rejected and the previous table stays live. If the launcher flags and the topology JSON disagree, the live `GET /admin/topology` readback wins.

- **Launchers:** `ffs0/dev/scripts/ops/start_federation_{z440,laptop,hpprodesk}.ps1` start each box's kernels + router with the uniform shape `--listen :9000 --default <local-kernel> --topology-file <ffs0>/dev/config/moos-federation.topology.json --local-host <host>`.

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
- `ffs0` — private control/research workspace + KB; holds the topology file (SOT) and the launchers.
- `moos-config` — **LEGACY, do not use.**

All at `github.com/Collider-Data-Systems/*`.

## Status / License

Active, stateless, multi-kernel federation read path. Copyright (c) Collider-Data-Systems — redistribution rights not granted by default until a formal license is applied; contact the organization for current terms.

---
authored-by: agent:claude-cowork.hp-z440 / session:sam.z440-cowork-workspace / config-overhaul
