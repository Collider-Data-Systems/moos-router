package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRoute_LongestPrefixMatch(t *testing.T) {
	router := NewRouter([]ShardRule{
		{URNPrefix: "urn:moos:", TargetURL: "http://kernel-a", Priority: 0},
		{URNPrefix: "urn:moos:ws:hp-laptop", TargetURL: "http://kernel-b", Priority: 0},
	})

	got := router.Route("urn:moos:ws:hp-laptop:something")
	if got != "http://kernel-b" {
		t.Fatalf("Route() = %q, want %q", got, "http://kernel-b")
	}
}

func TestRoute_PriorityTiebreak(t *testing.T) {
	router := NewRouter([]ShardRule{
		{URNPrefix: "urn:moos:ws:", TargetURL: "http://kernel-low", Priority: 1},
		{URNPrefix: "urn:moos:ws:", TargetURL: "http://kernel-high", Priority: 10},
	})

	got := router.Route("urn:moos:ws:hp-laptop:thing")
	if got != "http://kernel-high" {
		t.Fatalf("Route() = %q, want %q", got, "http://kernel-high")
	}
}

func TestRoute_NoMatch(t *testing.T) {
	router := NewRouter([]ShardRule{{URNPrefix: "urn:moos:ws:", TargetURL: "http://kernel-a", Priority: 0}})

	got := router.Route("urn:other:thing")
	if got != "" {
		t.Fatalf("Route() = %q, want empty string", got)
	}
}

func TestServeHTTP_PostRewrites_Routed(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{"hp": 0, "z440": 0}
	var hpMethod, hpPath, hpBody string

	hpKernel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		mu.Lock()
		hits["hp"]++
		hpMethod = r.Method
		hpPath = r.URL.Path
		hpBody = string(body)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kernel":"hp"}`))
	}))
	defer hpKernel.Close()

	z440Kernel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits["z440"]++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kernel":"z440"}`))
	}))
	defer z440Kernel.Close()

	router := NewRouter([]ShardRule{
		{URNPrefix: "urn:moos:ws:hp-laptop", TargetURL: hpKernel.URL, Priority: 0},
		{URNPrefix: "urn:moos:ws:hp-z440", TargetURL: z440Kernel.URL, Priority: 0},
	})

	req := httptest.NewRequest(http.MethodPost, "/rewrites", strings.NewReader(`{"node_urn":"urn:moos:ws:hp-laptop:foo"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	mu.Lock()
	defer mu.Unlock()

	if hits["hp"] != 1 {
		t.Fatalf("hp kernel hits = %d, want 1", hits["hp"])
	}
	if hits["z440"] != 0 {
		t.Fatalf("z440 kernel hits = %d, want 0", hits["z440"])
	}
	if hpMethod != http.MethodPost {
		t.Fatalf("method = %q, want %q", hpMethod, http.MethodPost)
	}
	if hpPath != "/rewrites" {
		t.Fatalf("path = %q, want %q", hpPath, "/rewrites")
	}
	if hpBody != `{"node_urn":"urn:moos:ws:hp-laptop:foo"}` {
		t.Fatalf("body = %q, want original rewrite body", hpBody)
	}
}

func TestServeHTTP_FanOut_StateNodes(t *testing.T) {
	var mu sync.Mutex
	paths := map[string]string{}

	kernelA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths["A"] = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"urn":"A"}]`))
	}))
	defer kernelA.Close()

	kernelB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths["B"] = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"urn":"B"}]`))
	}))
	defer kernelB.Close()

	router := NewRouter([]ShardRule{
		{URNPrefix: "urn:moos:ws:hp-laptop", TargetURL: kernelA.URL, Priority: 0},
		{URNPrefix: "urn:moos:ws:hp-z440", TargetURL: kernelB.URL, Priority: 0},
	})

	req := httptest.NewRequest(http.MethodGet, "/state/nodes", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var nodes []struct {
		URN string `json:"urn"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&nodes); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	seen := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		seen[node.URN] = true
	}

	if !seen["A"] || !seen["B"] {
		t.Fatalf("merged URNs = %+v, want both A and B", seen)
	}

	mu.Lock()
	defer mu.Unlock()
	if paths["A"] != "/state/nodes" || paths["B"] != "/state/nodes" {
		t.Fatalf("fan-out paths = %+v, want both requests on /state/nodes", paths)
	}
}

func TestServeHTTP_FanOut_StateNodesPartialSuccess(t *testing.T) {
	kernelA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"urn":"A"}]`))
	}))
	defer kernelA.Close()

	kernelB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "downstream unavailable"})
	}))
	defer kernelB.Close()

	router := NewRouter([]ShardRule{
		{URNPrefix: "urn:moos:ws:hp-laptop", TargetURL: kernelA.URL, Priority: 0},
		{URNPrefix: "urn:moos:ws:hp-z440", TargetURL: kernelB.URL, Priority: 0},
	})

	req := httptest.NewRequest(http.MethodGet, "/state/nodes", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var nodes []struct {
		URN string `json:"urn"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&nodes); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(nodes) != 1 || nodes[0].URN != "A" {
		t.Fatalf("nodes = %+v, want only surviving kernel payload", nodes)
	}
}

func TestServeHTTP_FanOut_AllFailures(t *testing.T) {
	kernelA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "downstream unavailable"})
	}))
	defer kernelA.Close()

	kernelB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "offline"})
	}))
	defer kernelB.Close()

	router := NewRouter([]ShardRule{
		{URNPrefix: "urn:moos:ws:hp-laptop", TargetURL: kernelA.URL, Priority: 0},
		{URNPrefix: "urn:moos:ws:hp-z440", TargetURL: kernelB.URL, Priority: 0},
	})

	req := httptest.NewRequest(http.MethodGet, "/state/nodes", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadGateway)
	}

	var payload map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&payload); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if !strings.Contains(payload["error"], "fan-out failed for all kernels") {
		t.Fatalf("error = %q, want all-kernels failure message", payload["error"])
	}
}

func TestServeHTTP_Healthz(t *testing.T) {
	var mu sync.Mutex
	paths := map[string]string{}

	kernelA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths["A"] = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","log_len":5}`))
	}))
	defer kernelA.Close()

	kernelB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths["B"] = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","log_len":5}`))
	}))
	defer kernelB.Close()

	router := NewRouter([]ShardRule{
		{URNPrefix: "urn:moos:ws:hp-laptop", TargetURL: kernelA.URL, Priority: 0},
		{URNPrefix: "urn:moos:ws:hp-z440", TargetURL: kernelB.URL, Priority: 0},
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var response struct {
		Status  string `json:"status"`
		Kernels []struct {
			URL    string `json:"url"`
			Status string `json:"status"`
			LogLen int    `json:"log_len"`
		} `json:"kernels"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if response.Status != "ok" {
		t.Fatalf("status = %q, want %q", response.Status, "ok")
	}
	if len(response.Kernels) != 2 {
		t.Fatalf("kernels length = %d, want 2", len(response.Kernels))
	}

	seen := map[string]bool{}
	for _, kernel := range response.Kernels {
		seen[kernel.URL] = kernel.Status == "ok" && kernel.LogLen == 5
	}

	if !seen[kernelA.URL] || !seen[kernelB.URL] {
		t.Fatalf("kernel status map = %+v, want both kernels healthy", seen)
	}

	mu.Lock()
	defer mu.Unlock()
	if paths["A"] != "/healthz" || paths["B"] != "/healthz" {
		t.Fatalf("health paths = %+v, want both requests on /healthz", paths)
	}
}

func TestServeHTTP_Healthz_UsesConfiguredTimeout(t *testing.T) {
	kernel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","log_len":5}`))
	}))
	defer kernel.Close()

	router := NewRouter([]ShardRule{{URNPrefix: "urn:moos:ws:hp-laptop", TargetURL: kernel.URL, Priority: 0}})
	router.HealthTimeout = 5 * time.Millisecond

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var response struct {
		Kernels []struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"kernels"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if len(response.Kernels) != 1 {
		t.Fatalf("kernels length = %d, want 1", len(response.Kernels))
	}
	if response.Kernels[0].Status != "down" {
		t.Fatalf("kernel status = %q, want down", response.Kernels[0].Status)
	}
	if response.Kernels[0].Error == "" {
		t.Fatalf("kernel error = empty, want timeout error")
	}
}

func TestRouteByType_Match(t *testing.T) {
	router := NewRouter(nil, TypeRule{TypeID: "prg_task", TargetURL: "http://z440"})

	got := router.RouteByType("prg_task")
	if got != "http://z440" {
		t.Fatalf("RouteByType() = %q, want %q", got, "http://z440")
	}
}

func TestRouteByType_NoMatch(t *testing.T) {
	router := NewRouter(nil, TypeRule{TypeID: "prg_task", TargetURL: "http://z440"})

	got := router.RouteByType("session")
	if got != "" {
		t.Fatalf("RouteByType() = %q, want empty", got)
	}
}

func TestServeHTTP_TypeRouting_TakesPrecedence(t *testing.T) {
	// type-map kernel (receives the request)
	var typeHits int
	var mu sync.Mutex
	typeKernel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		typeHits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"routed":"by-type"}`))
	}))
	defer typeKernel.Close()

	// shard kernel (should NOT receive the request)
	var shardHits int
	shardKernel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		shardHits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"routed":"by-shard"}`))
	}))
	defer shardKernel.Close()

	router := NewRouter(
		[]ShardRule{{URNPrefix: "urn:moos:", TargetURL: shardKernel.URL}},
		TypeRule{TypeID: "channel", TargetURL: typeKernel.URL},
	)

	body := `{"rewrite_type":"ADD","type_id":"channel","node_urn":"urn:moos:channel:messaging.wa-sam"}`
	req := httptest.NewRequest(http.MethodPost, "/rewrites", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if typeHits != 1 {
		t.Fatalf("type kernel hits = %d, want 1", typeHits)
	}
	if shardHits != 0 {
		t.Fatalf("shard kernel hits = %d, want 0 (type routing should take precedence)", shardHits)
	}
}

func TestServeHTTP_TypeRouting_FallbackToShard(t *testing.T) {
	var shardHits int
	var mu sync.Mutex
	shardKernel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		shardHits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"routed":"by-shard"}`))
	}))
	defer shardKernel.Close()

	router := NewRouter(
		[]ShardRule{{URNPrefix: "urn:moos:", TargetURL: shardKernel.URL}},
		TypeRule{TypeID: "channel", TargetURL: "http://127.0.0.1:19999"}, // wrong type, won't match
	)

	// type_id is "session" — no type rule matches, falls back to shard
	body := `{"rewrite_type":"ADD","type_id":"session","node_urn":"urn:moos:session:sam.t166"}`
	req := httptest.NewRequest(http.MethodPost, "/rewrites", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if shardHits != 1 {
		t.Fatalf("shard kernel hits = %d, want 1", shardHits)
	}
}
