# moos-router

Federation router for mo:os. Implements WF16 (cross-kernel cascade) as a thin HTTP proxy.
Consumed by `moos-kernel` instances sharding cross-kernel queries and by the Z440 federation topology (kernel 0 :8000 + federation :8001/:8002/:8003 + optional peer across the LAN).

Repo at `github.com/Collider-Data-Systems/moos-router` since T=172.

---

## The rule

`moos-router` is NOT a kernel. It does NOT log rewrites, does NOT validate operads, does NOT enforce §M11/§M12 gates. It is a stateless read-path fanout + write-proxy.

Log-is-truth stays at each `moos-kernel` instance. This binary merely:

1. Fans out read queries across known kernels (per `--shard` URN mapping).
2. Proxies write envelopes to the appropriate kernel-of-record.
3. Cascades reads to peer routers via `--peer` (WF16 federation read).

---

## Package Structure

```
cmd/router       — entry point; flag parsing; HTTP serve loop
internal/proxy   — shard map, peer cascade, request routing
```

---

## Running

```bash
go run ./cmd/router \
  --listen :9000 \
  --default http://localhost:8000 \
  --topology-file ../ffs0/dev/config/moos-federation.topology.json \
  --local-host hp-z440
```

### CLI Flags

| Flag | Purpose |
|------|---------|
| `--listen` | HTTP listen address (typically `:9000`) |
| `--topology-file` | Topology JSON (SOT): loaded at boot, hot-reloadable via `POST /admin/topology/reload`; introspect via `GET /admin/topology` (both localhost-only) |
| `--local-host` | Hostname key in the topology file; required with `--topology-file` (env fallback `MOOS_LOCAL_HOST`) |
| `--shard` | Repeatable: `<urn-prefix>=<kernel-url>`. Bootstrap fallback if the file fails to load. |
| `--default` | Fallback kernel URL for URNs matching no shard. Bootstrap fallback if the file fails to load. |
| `--peer` | Repeatable: peer router URL for WF16 cross-workstation cascade. Replaced by the file's peers on load. |
| `--type-map` | Repeatable: `<type_id>=<kernel-url>`, checked before shard rules; survives reloads. |

Full topology-file key reference: `README.md` §Topology configuration.

---

## Testing

```bash
go test ./...
```

Tests are focused on the proxy routing logic. Kernel-side gates (§M11/§M12, operad validation, fold) are the kernel's responsibility — the router passes them through.

---

## Sibling repos

- `ffs0/` — personal portable workspace; reads `kb/superset/ontology.json` for kernel operand grammar (not consumed here).
- `moos-kernel/` — the actual kernel. WF01–WF20 validator, ADD/LINK/MUTATE/UNLINK evaluator, §M11 liveness + §M12 admin gates (T=171 round-11).
- `moos-config/` — **LEGACY**, do not use.

---

## Safety

- Router is stateless. Do not add logging of envelope bodies — log-is-truth stays at each kernel.
- Never commit secrets or credentials.
- Build artifact `moos-router.exe` is gitignored; do not commit.
