# mo:os router

WF16 federation gateway for [mo:os](https://github.com/Collider-Data-Systems/moos-kernel) kernels. Routes envelopes by URN prefix to the kernel that owns the targeted shard. Go, stdlib-first.

## What it is

The router sits between clients and a federation of mo:os kernels, each with its own sovereign log. It inspects an envelope's URN-shaped fields, matches the prefix against a shard map, and forwards the envelope (or a query) to the right kernel without merging logs.

Clients can reach the router over plain HTTP or via the [Model Context Protocol](https://modelcontextprotocol.io/) (MCP, the JSON-RPC 2.0 protocol Anthropic Claude Desktop and similar tools speak).

```
HTTP / MCP clients
        |
        v
   moos-router       (WF16 — URN-prefix shard routing)
        |
   +----+----+----+----+
   |    |    |    |    |
  k0   k1   k2   k3   ...    (per-kernel sovereign logs)
```

Routing is **read-mostly transparent** for state queries (`GET /state/...`) — the router proxies to whichever kernel owns the URN's prefix. Writes (`POST /programs`, `POST /rewrites`) require the client to address the correct kernel directly; the router does not re-route writes (atomic batch semantics rely on a single log's serializability).

WF16 is one of the rewrite categories in the mo:os ontology — a strata-aware type system loaded by each kernel at boot. The router materializes routing rules from `shard_rule` nodes resolved at startup against a connected kernel's state. The ontology is published with the kernel; consult [moos-kernel](https://github.com/Collider-Data-Systems/moos-kernel) for the canonical type list.

## Running

```bash
go run ./cmd/router \
  --listen :9000 \
  --shard urn:moos:kernel:host-a.primary=http://kernel-a.example.com:8000 \
  --shard urn:moos:kernel:host-a.shard1=http://kernel-a.example.com:8001 \
  --shard urn:moos:kernel:host-b.primary=https://kernel-b.example.com \
  --default http://kernel-a.example.com:8000 \
  --peer https://router.partner-org.example.com
```

### CLI Flags

| Flag | Purpose |
|---|---|
| `--listen` | HTTP listen address (e.g. `:9000`) |
| `--shard` | Repeatable: `<urn-prefix>=<kernel-url>`. Routes by URN prefix match. |
| `--default` | Fallback kernel URL for URNs matching no shard |
| `--peer` | Repeatable: peer router URL for WF16 cross-workstation cascade |

The endpoints above are placeholders — substitute your own kernel hostnames and ports.

## Testing

```bash
go test ./...
```

Tests focus on proxy routing logic. Kernel-side gates (§M11 session-liveness, §M12 admin-capability, operad validation, fold) are each kernel's responsibility — the router passes them through.

## Stateless contract

The router is **stateless**. It does NOT log envelope bodies, validate operads, or enforce gates. Log-is-truth stays at each `moos-kernel` instance. This binary is a thin read-fanout + write-proxy.

## Companion repositories

- [moos-kernel](https://github.com/Collider-Data-Systems/moos-kernel) — the rewriting kernel itself

## Status

Active. Used in multi-kernel federations where each kernel keeps its own sovereign log and the router cascades cross-shard reads.

## License

Copyright © Collider-Data-Systems. All rights reserved. A formal open-source license will be applied in a future release; until then, redistribution rights are not granted by default. Contact the organization for current terms.

## Contact

Maintained by [@Collider-Data-Systems](https://github.com/Collider-Data-Systems).
