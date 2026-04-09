package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

// Router holds the shard table and routes requests.
type Router struct {
	Rules         []ShardRule
	kernels       []string
	client        *http.Client
	HealthTimeout time.Duration
}

// NewRouter creates a Router from a list of rules.
func NewRouter(rules []ShardRule) *Router {
	copied := make([]ShardRule, len(rules))
	copy(copied, rules)

	for i := range copied {
		copied[i].URNPrefix = strings.TrimSpace(copied[i].URNPrefix)
		copied[i].TargetURL = strings.TrimRight(strings.TrimSpace(copied[i].TargetURL), "/")
	}

	sort.SliceStable(copied, func(i, j int) bool {
		li := len(copied[i].URNPrefix)
		lj := len(copied[j].URNPrefix)
		if li != lj {
			return li > lj
		}
		if copied[i].Priority != copied[j].Priority {
			return copied[i].Priority > copied[j].Priority
		}
		return copied[i].URNPrefix < copied[j].URNPrefix
	})

	return &Router{
		Rules:         copied,
		kernels:       uniqueKernelURLs(copied),
		client:        &http.Client{},
		HealthTimeout: defaultHealthTimeout,
	}
}

// Route returns the target base URL for the given URN, or "" if no rule matches.
// Longest-prefix match wins; ties broken by Priority (desc).
func (r *Router) Route(urn string) string {
	for _, rule := range r.Rules {
		if strings.HasPrefix(urn, rule.URNPrefix) {
			return rule.TargetURL
		}
	}
	return ""
}

// ServeHTTP implements http.Handler.
// - POST /rewrites and POST /programs: extract URN from JSON body, route to correct kernel, proxy the request
// - GET /state/nodes/{urn}: extract URN from path, route, proxy
// - All other paths: broadcast to all kernels (fan-out), merge responses
// - GET /healthz: return router health + list of kernel health statuses
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
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

	urn, err := extractURNFromBody(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if urn == "" {
		writeError(w, http.StatusUnprocessableEntity, "no URN found in request body")
		return
	}

	target := r.Route(urn)
	if target == "" {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("no shard rule matches URN %s", urn))
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

	target := r.Route(urn)
	if target == "" {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("no shard rule matches URN %s", urn))
		return
	}

	r.forwardSingle(w, req, target, nil)
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
	if len(r.kernels) == 0 {
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

	results := make(chan fanoutResult, len(r.kernels))
	var wg sync.WaitGroup

	for _, kernelURL := range r.kernels {
		kernelURL := kernelURL
		wg.Add(1)
		go func() {
			defer wg.Done()

			targetURL := joinURL(kernelURL, req.URL.Path, req.URL.RawQuery)
			subReq, err := http.NewRequestWithContext(req.Context(), req.Method, targetURL, bytes.NewReader(body))
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

func (r *Router) handleHealthz(w http.ResponseWriter, req *http.Request) {
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

	kernels := make([]kernelStatus, len(r.kernels))
	var wg sync.WaitGroup

	for i, kernelURL := range r.kernels {
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

func extractURNFromBody(body []byte) (string, error) {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	return findURN(payload), nil
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

func uniqueKernelURLs(rules []ShardRule) []string {
	seen := make(map[string]struct{}, len(rules))
	urls := make([]string, 0, len(rules))

	for _, rule := range rules {
		u := strings.TrimRight(strings.TrimSpace(rule.TargetURL), "/")
		if u == "" {
			continue
		}
		if _, exists := seen[u]; exists {
			continue
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
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
