package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultHealthTimeout = 2 * time.Second

// ShardRule maps a URN prefix to a target kernel base URL.
type ShardRule struct {
	URNPrefix string
	TargetURL string // e.g. "http://localhost:8000"
	Priority  int    // higher wins on prefix overlap
}

// TypeRule maps a node type_id to a target kernel base URL.
// Type routing is checked before URN-prefix routing.
// Useful for routing by ontology stratum or functional specialisation
// (e.g. all prg_task nodes → Z440, all session nodes → hp-laptop).
type TypeRule struct {
	TypeID    string
	TargetURL string
	Priority  int // higher wins when multiple rules match the same type_id
}

// Router holds the shard table and routes requests.
// The topology (Rules, TypeRules, Peers, kernels) is accessed via an atomic
// pointer so it can be swapped at runtime without restarting the process.
type Router struct {
	topology      *TopologyHolder
	client        *http.Client
	HealthTimeout time.Duration

	// TopologyFile is the path to the topology JSON file used by Reload().
	// Empty means reload is disabled.
	TopologyFile string
	// LocalHost is the hostname key for this workstation in the topology file.
	LocalHost string

	// Deprecated direct fields kept only for NewRouter backward compat.
	Rules     []ShardRule
	TypeRules []TypeRule
	Peers     []string
	kernels   []string
}

// table returns the current topology snapshot (lock-free read).
func (r *Router) table() *TopologyTable {
	return r.topology.Load()
}

// NewRouter creates a Router from shard rules and optional type rules.
// typeRules is variadic so existing callers with only shard rules compile unchanged.
func NewRouter(rules []ShardRule, typeRules ...TypeRule) *Router {
	tbl := buildTable(rules, typeRules, nil)
	return &Router{
		topology:      NewTopologyHolder(tbl),
		Rules:         tbl.Rules,
		TypeRules:     tbl.TypeRules,
		Peers:         tbl.Peers,
		kernels:       tbl.Kernels,
		client:        &http.Client{},
		HealthTimeout: defaultHealthTimeout,
	}
}

// SetPeers updates the peer list on the current topology table.
// Called after NewRouter when peers are parsed separately from shard rules.
func (r *Router) SetPeers(peers []string) {
	old := r.table()
	newTbl := buildTable(old.Rules, old.TypeRules, peers)
	r.topology.Store(newTbl)
	// Keep deprecated fields in sync for any code that reads them directly.
	r.Peers = newTbl.Peers
	r.kernels = newTbl.Kernels
}

// Reload reads the topology file and atomically swaps the routing table.
// Returns the number of shard rules + peers in the new table, or an error.
func (r *Router) Reload() (int, error) {
	if r.TopologyFile == "" {
		return 0, fmt.Errorf("no topology file configured")
	}
	tf, err := LoadTopologyFile(r.TopologyFile)
	if err != nil {
		return 0, err
	}
	old := r.table()
	newTbl := BuildTableFromFile(tf, r.LocalHost, old.TypeRules)
	r.topology.Store(newTbl)
	// Keep deprecated fields in sync.
	r.Rules = newTbl.Rules
	r.TypeRules = newTbl.TypeRules
	r.Peers = newTbl.Peers
	r.kernels = newTbl.Kernels
	return len(newTbl.Rules) + len(newTbl.Peers), nil
}

// Route returns the target base URL for the given URN, or "" if no rule matches.
// Longest-prefix match wins; ties broken by Priority (desc).
func (r *Router) Route(urn string) string {
	tbl := r.table()
	for _, rule := range tbl.Rules {
		if strings.HasPrefix(urn, rule.URNPrefix) {
			return rule.TargetURL
		}
	}
	return ""
}

// RouteByType returns the target base URL for the given type_id, or "" if no rule matches.
// Type routing is checked before URN-prefix routing in handleRoutedPost.
func (r *Router) RouteByType(typeID string) string {
	tbl := r.table()
	for _, rule := range tbl.TypeRules {
		if rule.TypeID == typeID {
			return rule.TargetURL
		}
	}
	return ""
}

// ServeHTTP implements http.Handler.
// - POST /admin/topology/reload: localhost-guarded hot-reload of topology file
// - POST /rewrites and POST /programs: type routing first, then URN-prefix routing
// - GET /state/nodes/{urn}: extract URN from path, route, proxy
// - All other paths: broadcast to all kernels (fan-out), merge responses
// - GET /healthz: return router health + list of kernel health statuses
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodPost && req.URL.Path == "/admin/topology/reload" {
		r.handleAdminReload(w, req)
		return
	}

	if req.Method == http.MethodGet && req.URL.Path == "/healthz" {
		r.handleHealthz(w, req)
		return
	}

	if req.Method == http.MethodPost && (req.URL.Path == "/rewrites" || req.URL.Path == "/programs") {
		r.handleRoutedPost(w, req)
		return
	}

	if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/state/nodes/") {
		r.handleRoutedNodeRequest(w, req)
		return
	}

	r.handleFanout(w, req)
}

func (r *Router) handleRoutedPost(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	urn, typeID, err := extractBodyFields(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Type routing takes precedence over URN-prefix routing.
	target := ""
	if typeID != "" {
		target = r.RouteByType(typeID)
	}

	// Fall back to URN-prefix routing.
	if target == "" {
		if urn == "" {
			writeError(w, http.StatusUnprocessableEntity, "no URN found in request body")
			return
		}
		target = r.Route(urn)
	}

	if target == "" {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("no shard or type rule matches (urn=%s type_id=%s)", urn, typeID))
		return
	}

	r.forwardSingle(w, req, target, body)
}

func (r *Router) handleRoutedNodeRequest(w http.ResponseWriter, req *http.Request) {
	urn := strings.TrimPrefix(req.URL.Path, "/state/nodes/")
	if urn == "" {
		writeError(w, http.StatusBadRequest, "missing node URN in path")
		return
	}

	decodedURN, err := url.PathUnescape(urn)
	if err == nil {
		urn = decodedURN
	}

	// Try local shard first
	target := r.Route(urn)
	if target != "" {
		resp, body, err := r.tryForward(req, target)
		if err == nil && resp.StatusCode != http.StatusNotFound {
			copyHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			return
		}
	}

	// Cascade to peer routers (WF16 federation read path)
	for _, peerURL := range r.table().Peers {
		resp, body, err := r.tryForward(req, peerURL)
		if err != nil {
			log.Printf("proxy: peer cascade skip %s: %v", peerURL, err)
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			continue
		}
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return
	}

	writeError(w, http.StatusNotFound, fmt.Sprintf("node %s not found in local shards or peers", urn))
}

func (r *Router) tryForward(req *http.Request, targetBaseURL string) (*http.Response, []byte, error) {
	targetURL := joinURL(targetBaseURL, req.URL.Path, req.URL.RawQuery)
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()
	proxyReq, err := http.NewRequestWithContext(ctx, req.Method, targetURL, nil)
	if err != nil {
		return nil, nil, err
	}
	copyHeaders(proxyReq.Header, req.Header)

	resp, err := r.client.Do(proxyReq)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, err
	}
	return resp, body, nil
}

func (r *Router) forwardSingle(w http.ResponseWriter, req *http.Request, targetBaseURL string, body []byte) {
	targetURL := joinURL(targetBaseURL, req.URL.Path, req.URL.RawQuery)
	proxyReq, err := http.NewRequestWithContext(req.Context(), req.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to create upstream request")
		return
	}
	copyHeaders(proxyReq.Header, req.Header)

	resp, err := r.client.Do(proxyReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	written, err := io.Copy(w, resp.Body)
	if err != nil {
		log.Printf("proxy: copy response body from %s after %d bytes: %v", targetURL, written, err)
	}
}

func (r *Router) handleFanout(w http.ResponseWriter, req *http.Request) {
	tbl := r.table()
	if len(tbl.Kernels) == 0 {
		writeError(w, http.StatusServiceUnavailable, "no kernels configured")
		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	type fanoutResult struct {
		url  string
		body []byte
		err  error
	}

	results := make(chan fanoutResult, len(tbl.Kernels))
	var wg sync.WaitGroup

	for _, kernelURL := range tbl.Kernels {
		kernelURL := kernelURL
		wg.Add(1)
		go func() {
			defer wg.Done()

			targetURL := joinURL(kernelURL, req.URL.Path, req.URL.RawQuery)
			subCtx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
			defer cancel()
			subReq, err := http.NewRequestWithContext(subCtx, req.Method, targetURL, bytes.NewReader(body))
			if err != nil {
				results <- fanoutResult{url: kernelURL, err: fmt.Errorf("create request: %w", err)}
				return
			}
			copyHeaders(subReq.Header, req.Header)

			resp, err := r.client.Do(subReq)
			if err != nil {
				results <- fanoutResult{url: kernelURL, err: fmt.Errorf("request failed: %w", err)}
				return
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				results <- fanoutResult{url: kernelURL, err: fmt.Errorf("read response: %w", err)}
				return
			}

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				trimmed := strings.TrimSpace(string(respBody))
				results <- fanoutResult{url: kernelURL, err: fmt.Errorf("upstream status %d: %s", resp.StatusCode, trimmed)}
				return
			}

			results <- fanoutResult{url: kernelURL, body: respBody}
		}()
	}

	wg.Wait()
	close(results)

	merged := make([]any, 0)
	failures := make([]string, 0)
	successes := 0
	for result := range results {
		if result.err != nil {
			log.Printf("proxy: fan-out skipped %s: %v", result.url, result.err)
			failures = append(failures, fmt.Sprintf("%s: %v", result.url, result.err))
			continue
		}

		successes++
		if len(result.body) == 0 {
			continue
		}

		var decoded any
		if err := json.Unmarshal(result.body, &decoded); err != nil {
			successes--
			log.Printf("proxy: fan-out skipped %s: invalid JSON: %v", result.url, err)
			failures = append(failures, fmt.Sprintf("%s: invalid JSON: %v", result.url, err))
			continue
		}

		switch payload := decoded.(type) {
		case []any:
			merged = append(merged, payload...)
		default:
			merged = append(merged, payload)
		}
	}

	if successes == 0 {
		msg := "fan-out failed for all kernels"
		if len(failures) > 0 {
			msg += ": " + strings.Join(failures, "; ")
		}
		writeError(w, http.StatusBadGateway, msg)
		return
	}

	writeJSON(w, http.StatusOK, merged)
}

func (r *Router) handleAdminReload(w http.ResponseWriter, req *http.Request) {
	// Localhost guard: only allow reload from loopback.
	host := req.Host
	if h, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		host = h
	}
	if host != "127.0.0.1" && host != "::1" && host != "localhost" {
		writeError(w, http.StatusForbidden, "admin endpoints are localhost-only")
		return
	}

	count, err := r.Reload()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	log.Printf("admin: topology reloaded from %s (%d rules+peers)", r.TopologyFile, count)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "reloaded",
		"file":    r.TopologyFile,
		"entries": count,
	})
}

func (r *Router) handleHealthz(w http.ResponseWriter, req *http.Request) {
	tbl := r.table()
	type upstreamHealth struct {
		Status string `json:"status"`
		LogLen int    `json:"log_len"`
	}

	type kernelStatus struct {
		URL    string `json:"url"`
		Status string `json:"status"`
		LogLen int    `json:"log_len,omitempty"`
		Error  string `json:"error,omitempty"`
	}

	kernels := make([]kernelStatus, len(tbl.Kernels))
	var wg sync.WaitGroup

	for i, kernelURL := range tbl.Kernels {
		i, kernelURL := i, kernelURL
		wg.Add(1)
		go func() {
			defer wg.Done()

			entry := kernelStatus{URL: kernelURL, Status: "down"}
			ctx, cancel := context.WithTimeout(req.Context(), r.healthTimeout())
			defer cancel()

			hReq, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(kernelURL, "/healthz", ""), nil)
			if err != nil {
				entry.Error = err.Error()
				kernels[i] = entry
				return
			}

			resp, err := r.client.Do(hReq)
			if err != nil {
				entry.Error = err.Error()
				kernels[i] = entry
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				entry.Error = fmt.Sprintf("status %d", resp.StatusCode)
				kernels[i] = entry
				return
			}

			var health upstreamHealth
			if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
				entry.Error = err.Error()
				kernels[i] = entry
				return
			}

			entry.Status = health.Status
			entry.LogLen = health.LogLen
			kernels[i] = entry
		}()
	}

	wg.Wait()
	sort.Slice(kernels, func(i, j int) bool {
		return kernels[i].URL < kernels[j].URL
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"kernels": kernels,
	})
}

func (r *Router) healthTimeout() time.Duration {
	if r == nil || r.HealthTimeout <= 0 {
		return defaultHealthTimeout
	}
	return r.HealthTimeout
}

// extractBodyFields extracts the primary URN and type_id from a rewrite envelope
// or the first envelope in a program batch.
func extractBodyFields(body []byte) (urn, typeID string, err error) {
	var payload any
	if err = json.Unmarshal(body, &payload); err != nil {
		return
	}
	urn = findURN(payload)
	typeID = findTypeID(payload)
	return
}

func findURN(payload any) string {
	orderedKeys := []string{"node_urn", "target_urn", "src_urn", "relation_urn"}

	switch v := payload.(type) {
	case map[string]any:
		for _, key := range orderedKeys {
			value, ok := v[key]
			if !ok {
				continue
			}
			asString, ok := value.(string)
			if ok && strings.TrimSpace(asString) != "" {
				return strings.TrimSpace(asString)
			}
		}
	case []any:
		for _, item := range v {
			if urn := findURN(item); urn != "" {
				return urn
			}
		}
	}

	return ""
}

func findTypeID(payload any) string {
	switch v := payload.(type) {
	case map[string]any:
		if t, ok := v["type_id"]; ok {
			if s, ok := t.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	case []any:
		for _, item := range v {
			if t := findTypeID(item); t != "" {
				return t
			}
		}
	}
	return ""
}

func uniqueKernelURLs(shardRules []ShardRule, typeRules []TypeRule) []string {
	seen := make(map[string]struct{})
	urls := make([]string, 0)

	add := func(u string) {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" {
			return
		}
		if _, exists := seen[u]; exists {
			return
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}

	for _, rule := range shardRules {
		add(rule.TargetURL)
	}
	for _, rule := range typeRules {
		add(rule.TargetURL)
	}

	sort.Strings(urls)
	return urls
}

func joinURL(baseURL, path, rawQuery string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	fullURL := baseURL + path
	if rawQuery != "" {
		fullURL += "?" + rawQuery
	}
	return fullURL
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("proxy: encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": strings.TrimSpace(msg)})
}
