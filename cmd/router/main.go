package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"moos/router/internal/proxy"
)

type multiFlag []string

func (f *multiFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *multiFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("value cannot be empty")
	}
	*f = append(*f, value)
	return nil
}

func main() {
	listenAddr := flag.String("listen", ":9000", "router listen address")
	defaultKernel := flag.String("default", "", "fallback kernel URL when no prefix matches")
	healthTimeout := flag.Duration("health-timeout", 2*time.Second, "timeout for per-kernel health checks")
	topologyFile := flag.String("topology-file", "", "path to moos-federation.topology.json for hot-reload (enables POST /admin/topology/reload)")
	localHost := flag.String("local-host", "", "local hostname key in topology file (e.g. hp-laptop, hp-z440); auto-detected from MOOS_LOCAL_HOST env if unset")

	var shardValues multiFlag
	var typeMapValues multiFlag
	var peerValues multiFlag

	flag.Var(&shardValues, "shard", "shard rule: urn_prefix=http://host:port (repeatable)")
	flag.Var(&typeMapValues, "type-map", "type routing rule: type_id=http://host:port (repeatable, checked before shard rules)")
	flag.Var(&peerValues, "peer", "peer router URL for federation cascade (WF16, repeatable)")

	flag.Parse()

	// Parse --shard flags
	shardRules := make([]proxy.ShardRule, 0, len(shardValues)+1)
	for _, raw := range shardValues {
		parts := strings.SplitN(raw, "=", 2)
		if len(parts) != 2 {
			log.Fatalf("invalid --shard value %q (expected urn_prefix=http://host:port)", raw)
		}
		prefix := strings.TrimSpace(parts[0])
		target := strings.TrimSpace(parts[1])
		if target == "" {
			log.Fatalf("invalid --shard value %q: target URL cannot be empty", raw)
		}
		shardRules = append(shardRules, proxy.ShardRule{
			URNPrefix: prefix,
			TargetURL: target,
			Priority:  0,
		})
	}

	if fallback := strings.TrimSpace(*defaultKernel); fallback != "" {
		shardRules = append(shardRules, proxy.ShardRule{
			URNPrefix: "",
			TargetURL: fallback,
			Priority:  -1,
		})
	}

	// Parse --type-map flags
	typeRules := make([]proxy.TypeRule, 0, len(typeMapValues))
	for _, raw := range typeMapValues {
		parts := strings.SplitN(raw, "=", 2)
		if len(parts) != 2 {
			log.Fatalf("invalid --type-map value %q (expected type_id=http://host:port)", raw)
		}
		typeID := strings.TrimSpace(parts[0])
		target := strings.TrimSpace(parts[1])
		if typeID == "" || target == "" {
			log.Fatalf("invalid --type-map value %q: type_id and target URL cannot be empty", raw)
		}
		typeRules = append(typeRules, proxy.TypeRule{
			TypeID:    typeID,
			TargetURL: target,
			Priority:  0,
		})
	}

	router := proxy.NewRouter(shardRules, typeRules...)
	router.HealthTimeout = *healthTimeout

	for _, peerURL := range peerValues {
		trimmed := strings.TrimRight(strings.TrimSpace(peerURL), "/")
		if trimmed != "" {
			router.Peers = append(router.Peers, trimmed)
		}
	}
	if len(router.Peers) > 0 {
		router.SetPeers(router.Peers)
	}

	// Topology file configuration: the file is loaded at boot and becomes the
	// routing table; CLI --shard/--default flags act as a bootstrap fallback
	// when the file fails to load (e.g. mid-edit). POST /admin/topology/reload
	// re-reads the same file at runtime.
	resolvedHost := strings.TrimSpace(*localHost)
	if resolvedHost == "" {
		resolvedHost = strings.TrimSpace(os.Getenv("MOOS_LOCAL_HOST"))
	}
	if *topologyFile != "" {
		if resolvedHost == "" {
			// An empty local-host silently routes this machine's own kernels
			// via their Tailscale URLs (hairpin) — fail loud instead.
			log.Fatalf("router: --topology-file requires --local-host or MOOS_LOCAL_HOST")
		}
		router.TopologyFile = *topologyFile
		router.LocalHost = resolvedHost
		if count, err := router.Reload(); err != nil {
			if len(shardRules) == 0 {
				log.Fatalf("router: boot topology load failed with no --shard/--default fallback: %v", err)
			}
			log.Printf("router: WARNING boot topology load failed, serving CLI flag table (%d rules): %v", len(shardRules), err)
		} else {
			log.Printf("router: topology loaded from %s at boot (%d rules+peers, local-host=%s)", *topologyFile, count, resolvedHost)
		}
	}

	log.Printf("router: listening on %s, shards: %d, type-maps: %d, peers: %d",
		*listenAddr, len(shardRules), len(typeRules), len(router.Peers))

	if err := http.ListenAndServe(*listenAddr, router); err != nil {
		log.Fatalf("router: %v", err)
	}
}
