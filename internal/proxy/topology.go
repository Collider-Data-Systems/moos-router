package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"
)

// TopologyTable holds the immutable routing state read by request-handling paths.
// Swap the pointer atomically; never mutate a live table.
type TopologyTable struct {
	Rules     []ShardRule
	TypeRules []TypeRule
	Peers     []string
	Kernels   []string // deduplicated target URLs for fan-out
}

// buildTable sorts and deduplicates a fresh TopologyTable (same logic as the
// old NewRouter constructor).
func buildTable(shardRules []ShardRule, typeRules []TypeRule, peers []string) *TopologyTable {
	// Copy + normalize shard rules
	rules := make([]ShardRule, len(shardRules))
	copy(rules, shardRules)
	for i := range rules {
		rules[i].URNPrefix = strings.TrimSpace(rules[i].URNPrefix)
		rules[i].TargetURL = strings.TrimRight(strings.TrimSpace(rules[i].TargetURL), "/")
	}
	sort.SliceStable(rules, func(i, j int) bool {
		li := len(rules[i].URNPrefix)
		lj := len(rules[j].URNPrefix)
		if li != lj {
			return li > lj
		}
		if rules[i].Priority != rules[j].Priority {
			return rules[i].Priority > rules[j].Priority
		}
		return rules[i].URNPrefix < rules[j].URNPrefix
	})

	// Copy + normalize type rules
	types := make([]TypeRule, len(typeRules))
	copy(types, typeRules)
	for i := range types {
		types[i].TypeID = strings.TrimSpace(types[i].TypeID)
		types[i].TargetURL = strings.TrimRight(strings.TrimSpace(types[i].TargetURL), "/")
	}
	sort.SliceStable(types, func(i, j int) bool {
		if types[i].TypeID != types[j].TypeID {
			return types[i].TypeID < types[j].TypeID
		}
		return types[i].Priority > types[j].Priority
	})

	// Normalize peers
	normPeers := make([]string, 0, len(peers))
	for _, p := range peers {
		trimmed := strings.TrimRight(strings.TrimSpace(p), "/")
		if trimmed != "" {
			normPeers = append(normPeers, trimmed)
		}
	}

	return &TopologyTable{
		Rules:     rules,
		TypeRules: types,
		Peers:     normPeers,
		Kernels:   uniqueKernelURLs(rules, types),
	}
}

// TopologyHolder wraps an atomic pointer to a TopologyTable for lock-free reads.
type TopologyHolder struct {
	ptr atomic.Pointer[TopologyTable]
}

// NewTopologyHolder creates a holder with an initial table.
func NewTopologyHolder(initial *TopologyTable) *TopologyHolder {
	h := &TopologyHolder{}
	h.ptr.Store(initial)
	return h
}

// Load returns the current topology table (lock-free).
func (h *TopologyHolder) Load() *TopologyTable {
	return h.ptr.Load()
}

// Store atomically swaps the topology table.
func (h *TopologyHolder) Store(t *TopologyTable) {
	h.ptr.Store(t)
}

// --- Topology file loading ---

// TopologyFile represents the relevant subset of moos-federation.topology.json
// that the router uses for peers and kernel shards.
type TopologyFile struct {
	Kernels map[string]TopologyKernel `json:"kernels"`
	Routers map[string]TopologyRouter `json:"routers"`
}

// TopologyKernel is a kernel entry in the topology file.
type TopologyKernel struct {
	Host          string `json:"host"`
	HTTPLocal     string `json:"http_local"`
	HTTPTailscale string `json:"http_tailscale"`
}

// TopologyRouter is a router entry in the topology file.
// DefaultKernel and ShardAliases reference kernel entries by map key, so URL
// selection (http_local vs http_tailscale) stays in one place.
type TopologyRouter struct {
	Host          string            `json:"host"`
	Peers         []string          `json:"peers"`
	DefaultKernel string            `json:"default_kernel"`
	ShardAliases  map[string]string `json:"shard_aliases"`
}

// LoadTopologyFile reads and parses a topology JSON file.
func LoadTopologyFile(path string) (*TopologyFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read topology file: %w", err)
	}
	var tf TopologyFile
	if err := json.Unmarshal(data, &tf); err != nil {
		return nil, fmt.Errorf("parse topology file: %w", err)
	}
	return &tf, nil
}

// kernelTargetURL picks the URL a router on localHost should use to reach a
// kernel: http_local for same-host kernels, http_tailscale for remote ones.
func kernelTargetURL(kernel TopologyKernel, localHost string) string {
	target := kernel.HTTPLocal
	if kernel.Host != localHost && kernel.HTTPTailscale != "" {
		target = kernel.HTTPTailscale
	}
	return target
}

// BuildTableFromFile builds a TopologyTable from a topology file for the given
// local hostname. It derives shard rules from kernel entries (non-local kernels
// get their Tailscale URL as the shard target); the router entry matching
// localHost contributes peers, the default rule (empty prefix, priority -1 —
// same shape as the --default flag), and extra shard-alias prefixes such as
// urn:moos:ws:<host>. A table with zero shard rules is refused so a bad file
// can never hot-swap a working router into one that 503s everything.
func BuildTableFromFile(tf *TopologyFile, localHost string, existingTypeRules []TypeRule) (*TopologyTable, error) {
	if len(tf.Kernels) == 0 {
		return nil, fmt.Errorf("topology file has no kernels — refusing empty routing table")
	}

	var shardRules []ShardRule
	var peers []string

	for name, kernel := range tf.Kernels {
		target := kernelTargetURL(kernel, localHost)
		if target == "" {
			continue
		}
		shardRules = append(shardRules, ShardRule{
			URNPrefix: "urn:moos:kernel:" + name,
			TargetURL: target,
			Priority:  0,
		})
	}

	if len(shardRules) == 0 {
		return nil, fmt.Errorf("topology file yields no reachable kernel for local-host %q — refusing empty routing table", localHost)
	}

	// Sort shards deterministically for reproducible /healthz output
	sort.Slice(shardRules, func(i, j int) bool {
		return shardRules[i].URNPrefix < shardRules[j].URNPrefix
	})

	// The router entry for this host contributes peers, the default rule and
	// alias shards.
	for _, router := range tf.Routers {
		if router.Host != localHost {
			continue
		}
		peers = router.Peers

		if router.DefaultKernel != "" {
			kernel, ok := tf.Kernels[router.DefaultKernel]
			if !ok {
				return nil, fmt.Errorf("default_kernel %q not found in kernels map", router.DefaultKernel)
			}
			target := kernelTargetURL(kernel, localHost)
			if target == "" {
				return nil, fmt.Errorf("default_kernel %q has no usable URL for local-host %q", router.DefaultKernel, localHost)
			}
			shardRules = append(shardRules, ShardRule{
				URNPrefix: "",
				TargetURL: target,
				Priority:  -1,
			})
		}

		for prefix, kernelName := range router.ShardAliases {
			kernel, ok := tf.Kernels[kernelName]
			if !ok {
				return nil, fmt.Errorf("shard_alias %q -> %q not found in kernels map", prefix, kernelName)
			}
			target := kernelTargetURL(kernel, localHost)
			if target == "" {
				return nil, fmt.Errorf("shard_alias %q -> %q has no usable URL for local-host %q", prefix, kernelName, localHost)
			}
			shardRules = append(shardRules, ShardRule{
				URNPrefix: prefix,
				TargetURL: target,
				Priority:  0,
			})
		}
		break
	}

	return buildTable(shardRules, existingTypeRules, peers), nil
}
