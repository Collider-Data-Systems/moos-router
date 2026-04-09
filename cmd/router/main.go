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

type shardFlags []string

func (s *shardFlags) String() string {
	return strings.Join(*s, ",")
}

func (s *shardFlags) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("shard cannot be empty")
	}
	*s = append(*s, value)
	return nil
}

func main() {
	listenAddr := flag.String("listen", ":9000", "router listen address")
	defaultKernel := flag.String("default", "", "fallback kernel URL when no prefix matches")
	healthTimeout := flag.Duration("health-timeout", 2*time.Second, "timeout for per-kernel health checks")
	var shardValues shardFlags
	flag.Var(&shardValues, "shard", "shard rule in format urn_prefix=http://host:port")
	flag.Parse()

	rules := make([]proxy.ShardRule, 0, len(shardValues)+1)
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

		rules = append(rules, proxy.ShardRule{
			URNPrefix: prefix,
			TargetURL: target,
			Priority:  0,
		})
	}

	if fallback := strings.TrimSpace(*defaultKernel); fallback != "" {
		rules = append(rules, proxy.ShardRule{
			URNPrefix: "",
			TargetURL: fallback,
			Priority:  -1,
		})
	}

	router := proxy.NewRouter(rules)
	router.HealthTimeout = *healthTimeout
	log.Printf("router: listening on %s, shards: %d", *listenAddr, len(rules))
	if err := http.ListenAndServe(*listenAddr, router); err != nil {
		log.Fatalf("router: %v", err)
	}
}
