# mo:os router

WF16 federation gateway for [mo:os](https://github.com/Collider-Data-Systems/moos-kernel) kernels. Routes envelopes by URN prefix to the kernel that owns the targeted shard. Go, stdlib-first.

## What it is

The router sits between MCP / HTTP clients and a federation of mo:os kernels, each with its own sovereign log. It inspects an envelope's URN-shaped fields, matches the prefix against a shard map, and forwards the envelope (or a query) to the right kernel without merging logs.

```
MCP / HTTP clients
        |
        v
   moos-router       (WF16 — URN-prefix shard routing)
        |
   +----+----+----+----+
   |    |    |    |    |
  k0   k1   k2   k3   ...    (per-kernel sovereign logs)
```

Routing is **read-mostly transparent** for state queries (`GET /state/...`) — the router proxies to whichever kernel owns the URN's prefix. Writes (`POST /programs`, `POST /rewrites`) require the client to address the correct kernel directly; the router does not re-route writes (atomic batch semantics rely on a single log's serializability).

WF16 is one of the 21 rewrite categories in the ontology (`ffs0/kb/superset/ontology.json` — private workspace). The router materializes WF16 routing rules from `shard_rule` nodes resolved at startup against a connected kernel's state.

## Running

```bash
go run ./cmd/moos-router \
  --shard-config shards.json \
  --listen :9000
```

Shard config example:

```json
{
  "shards": [
    {"prefix": "urn:moos:kernel:hp-z440.primary", "kernel_endpoint": "http://localhost:8000"},
    {"prefix": "urn:moos:kernel:hp-z440.lola",    "kernel_endpoint": "http://localhost:8001"},
    {"prefix": "urn:moos:kernel:hp-z440.menno",   "kernel_endpoint": "http://localhost:8002"},
    {"prefix": "urn:moos:kernel:hp-z440.moos",    "kernel_endpoint": "http://localhost:8003"},
    {"prefix": "urn:moos:kernel:hp-laptop.primary", "kernel_endpoint": "https://api.my-tiny-data-collider.nl"}
  ]
}
```

## Testing

```bash
go test ./...
```

## Companion repositories

- **moos-kernel** — the rewriting kernel itself
- **ffs0** — private workspace + doctrine notes + ontology

## Status

Active. Used in the Z440 5-kernel + hp-laptop 1-kernel federation. Cloudflare tunnel between machines pending; intra-host routing on `:8000–:8003` plus router on `:9000` is daily-driven.

## License

(TBD — assignment in progress)

## Contact

Maintained by [@Collider-Data-Systems](https://github.com/Collider-Data-Systems).
