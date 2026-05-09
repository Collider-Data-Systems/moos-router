# moos-router

WF16 federation router for mo:os sovereign kernels. It is a small Go HTTP gateway that routes writes to a kernel and fans out reads across kernels.

## Role

`moos-router` is federation substrate, not graph truth. Kernels keep sovereign logs; the router decides where an HTTP request should go.

Current capabilities:

- URN-prefix shard routing with longest-prefix match.
- Type-based routing with `--type-map`, checked before URN-prefix routing.
- Peer cascade for `GET /state/nodes/{urn}` across router peers.
- Fan-out/merge for broad read paths such as `GET /state/nodes`.
- Router health at `GET /healthz`, including per-kernel health checks.

## Running

```powershell
go run ./cmd/router --listen :9000 --default http://localhost:8000
```

Example with explicit shards and a type route:

```powershell
go run ./cmd/router `
  --listen :9000 `
  --shard urn:moos:kernel:hp-laptop=http://localhost:8000 `
  --shard urn:moos:kernel:hp-z440=http://192.168.1.13:8000 `
  --type-map session=http://localhost:8000 `
  --peer http://192.168.1.13:9000
```

Flags:

| Flag | Purpose |
| --- | --- |
| `--listen` | Router listen address, default `:9000`. |
| `--default` | Fallback kernel URL when no prefix matches. |
| `--shard` | Repeatable `urn_prefix=http://host:port` routing rule. |
| `--type-map` | Repeatable `type_id=http://host:port` routing rule; checked before shard rules for writes. |
| `--peer` | Repeatable peer router URL for WF16 federation read cascade. |
| `--health-timeout` | Per-kernel health-check timeout. |

## Routing Semantics

- `POST /rewrites` and `POST /programs`: inspect request body, route by `type_id` first, then by URN prefix.
- `GET /state/nodes/{urn}`: route to the local shard first, then try peer routers if the node is not found.
- Other paths: fan out to known kernels and merge compatible responses.
- `GET /healthz`: return router health and downstream kernel health.

The router does not validate envelopes. Validation belongs to `moos-kernel` and its loaded operad.

## Testing

```powershell
go test ./...
```

Tests cover longest-prefix routing, priority tie-breaks, routed writes, fan-out, partial success, type-map precedence, and peer cascade behavior.

## Relationship To The Project

`moos-router` sits between kernels and projection/application surfaces. It is part of the runtime substrate with `moos-kernel`, but it is not an application group.

Application domains such as `my-tiny-data-collider` should be modeled in HG as groups, purposes, programs, channels, and external surfaces. Those applications may use the router to reach kernels, but their website/DNS/server code should remain separate from this router.

As the T189/T200 projection lane matures, the router's job stays deliberately narrow: route graph traffic by URN/type/topology while Calendar, GitHub Projects, dashboards, websites, DNS, and Workspace remain application or projection surfaces above the kernel/router substrate.

## Companion Repositories

- `moos-kernel` — log/fold/operad/session runtime.
- `ffs0` — private ontology, projection planners, skills, reports, and operator dashboard.

All current repositories live under `github.com/Collider-Data-Systems/*`.
