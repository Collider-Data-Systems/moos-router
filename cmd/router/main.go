package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
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

	log.Printf("router: listening on %s, shards: %d, type-maps: %d, peers: %d",
		*listenAddr, len(shardRules), len(typeRules), len(router.Peers))

	if err := http.ListenAndServe(*listenAddr, router); err != nil {
		log.Fatalf("router: %v", err)
	}
}
