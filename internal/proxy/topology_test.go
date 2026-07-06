package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReload_SwapsTopology(t *testing.T) {
	// Write a topology file
	dir := t.TempDir()
	topoPath := filepath.Join(dir, "topology.json")
	topoJSON := `{
		"kernels": {
			"hp-laptop.primary": {
				"host": "hp-laptop",
				"http_local": "http://localhost:8000",
				"http_tailscale": "http://100.106.220.58:8000"
			},
			"hp-z440.primary": {
				"host": "hp-z440",
				"http_local": "http://localhost:8000",
				"http_tailscale": "http://100.82.243.13:8000"
			}
		},
		"routers": {
			"hp-laptop": {
				"host": "hp-laptop",
				"peers": ["http://100.82.243.13:9000"]
			}
		}
	}`
	if err := os.WriteFile(topoPath, []byte(topoJSON), 0644); err != nil {
		t.Fatal(err)
	}

	router := NewRouter([]ShardRule{
		{URNPrefix: "urn:moos:", TargetURL: "http://old-kernel"},
	})
	router.TopologyFile = topoPath
	router.LocalHost = "hp-laptop"

	// Before reload: old rule
	if got := router.Route("urn:moos:anything"); got != "http://old-kernel" {
		t.Fatalf("pre-reload Route() = %q, want http://old-kernel", got)
	}

	count, err := router.Reload()
	if err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if count == 0 {
		t.Fatal("Reload() returned 0 entries")
	}

	// After reload: kernel shards from file
	tbl := router.table()
	if len(tbl.Peers) != 1 || tbl.Peers[0] != "http://100.82.243.13:9000" {
		t.Fatalf("post-reload peers = %v, want [http://100.82.243.13:9000]", tbl.Peers)
	}

	// Local kernel should use http_local
	got := router.Route("urn:moos:kernel:hp-laptop.primary")
	if got != "http://localhost:8000" {
		t.Fatalf("Route(hp-laptop) = %q, want http://localhost:8000", got)
	}

	// Remote kernel should use http_tailscale
	got = router.Route("urn:moos:kernel:hp-z440.primary")
	if got != "http://100.82.243.13:8000" {
		t.Fatalf("Route(hp-z440) = %q, want http://100.82.243.13:8000", got)
	}
}

func TestAdminReload_LocalhostOnly(t *testing.T) {
	router := NewRouter([]ShardRule{})
	router.TopologyFile = "" // no file configured

	// Simulate a non-localhost request
	req := httptest.NewRequest(http.MethodPost, "/admin/topology/reload", nil)
	req.RemoteAddr = "192.168.1.50:12345"
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-localhost status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestAdminReload_LocalhostAllowed(t *testing.T) {
	dir := t.TempDir()
	topoPath := filepath.Join(dir, "topology.json")
	topoJSON := `{"kernels":{"test.primary":{"host":"test","http_local":"http://localhost:8000","http_tailscale":"http://10.0.0.1:8000"}},"routers":{"test":{"host":"test","peers":[]}}}`
	if err := os.WriteFile(topoPath, []byte(topoJSON), 0644); err != nil {
		t.Fatal(err)
	}

	router := NewRouter([]ShardRule{})
	router.TopologyFile = topoPath
	router.LocalHost = "test"

	req := httptest.NewRequest(http.MethodPost, "/admin/topology/reload", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("localhost status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	if !strings.Contains(rr.Body.String(), "reloaded") {
		t.Fatalf("body = %s, want 'reloaded'", rr.Body.String())
	}
}
