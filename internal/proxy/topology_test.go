package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTopology(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	topoPath := filepath.Join(dir, "topology.json")
	if err := os.WriteFile(topoPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return topoPath
}

func TestReload_SwapsTopology(t *testing.T) {
	topoPath := writeTopology(t, `{
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
	}`)

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

	// No default_kernel in the file: non-kernel URNs no longer match anything.
	// Routers that need a fallback must declare routers[host].default_kernel.
	if got := router.Route("urn:moos:session:sam.example"); got != "" {
		t.Fatalf("Route(non-kernel URN) = %q, want \"\" without default_kernel", got)
	}
}

func TestReload_PreservesDefaultAndAliases(t *testing.T) {
	topoPath := writeTopology(t, `{
		"kernels": {
			"hp-z440.primary": {
				"host": "hp-z440",
				"http_local": "http://localhost:8000",
				"http_tailscale": "http://100.82.243.13:8000"
			},
			"hp-laptop.primary": {
				"host": "hp-laptop",
				"http_local": "http://localhost:8000",
				"http_tailscale": "http://100.106.220.58:8000"
			}
		},
		"routers": {
			"hp-z440": {
				"host": "hp-z440",
				"peers": ["http://100.106.220.58:9000"],
				"default_kernel": "hp-z440.primary",
				"shard_aliases": {
					"urn:moos:ws:hp-z440": "hp-z440.primary"
				}
			}
		}
	}`)

	router := NewRouter([]ShardRule{
		{URNPrefix: "urn:moos:", TargetURL: "http://old-kernel"},
	})
	router.TopologyFile = topoPath
	router.LocalHost = "hp-z440"

	if _, err := router.Reload(); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}

	// The default rule (empty prefix, priority -1) catches non-kernel URNs.
	if got := router.Route("urn:moos:session:sam.example"); got != "http://localhost:8000" {
		t.Fatalf("Route(session URN) = %q, want default http://localhost:8000", got)
	}

	// The ws alias routes like the old --shard urn:moos:ws:hp-z440=... flag.
	if got := router.Route("urn:moos:ws:hp-z440"); got != "http://localhost:8000" {
		t.Fatalf("Route(ws alias) = %q, want http://localhost:8000", got)
	}

	// Kernel prefixes still win over the default for remote kernels.
	if got := router.Route("urn:moos:kernel:hp-laptop.primary"); got != "http://100.106.220.58:8000" {
		t.Fatalf("Route(remote kernel) = %q, want http://100.106.220.58:8000", got)
	}
}

func TestReload_RejectsEmptyFile(t *testing.T) {
	topoPath := writeTopology(t, `{}`)

	router := NewRouter([]ShardRule{
		{URNPrefix: "urn:moos:", TargetURL: "http://old-kernel"},
	})
	router.TopologyFile = topoPath
	router.LocalHost = "hp-z440"

	if _, err := router.Reload(); err == nil {
		t.Fatal("Reload() of keyless file succeeded, want error")
	}

	// The old table must survive a rejected reload.
	if got := router.Route("urn:moos:anything"); got != "http://old-kernel" {
		t.Fatalf("post-rejected-reload Route() = %q, want http://old-kernel", got)
	}
}

func TestReload_RejectsDanglingDefaultKernel(t *testing.T) {
	topoPath := writeTopology(t, `{
		"kernels": {
			"hp-z440.primary": {"host": "hp-z440", "http_local": "http://localhost:8000"}
		},
		"routers": {
			"hp-z440": {"host": "hp-z440", "peers": [], "default_kernel": "no-such-kernel"}
		}
	}`)

	router := NewRouter([]ShardRule{
		{URNPrefix: "urn:moos:", TargetURL: "http://old-kernel"},
	})
	router.TopologyFile = topoPath
	router.LocalHost = "hp-z440"

	_, err := router.Reload()
	if err == nil {
		t.Fatal("Reload() with dangling default_kernel succeeded, want error")
	}
	if !strings.Contains(err.Error(), "no-such-kernel") {
		t.Fatalf("error = %v, want mention of no-such-kernel", err)
	}

	if got := router.Route("urn:moos:anything"); got != "http://old-kernel" {
		t.Fatalf("post-rejected-reload Route() = %q, want http://old-kernel", got)
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
	topoPath := writeTopology(t, `{"kernels":{"test.primary":{"host":"test","http_local":"http://localhost:8000","http_tailscale":"http://10.0.0.1:8000"}},"routers":{"test":{"host":"test","peers":[]}}}`)

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

func TestAdminTopology_LocalhostOnly(t *testing.T) {
	router := NewRouter([]ShardRule{
		{URNPrefix: "urn:moos:kernel:test.primary", TargetURL: "http://localhost:8000"},
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/topology", nil)
	req.RemoteAddr = "192.168.1.50:12345"
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-localhost status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestAdminTopology_ReturnsLiveTable(t *testing.T) {
	router := NewRouter([]ShardRule{
		{URNPrefix: "urn:moos:kernel:test.primary", TargetURL: "http://localhost:8000"},
		{URNPrefix: "", TargetURL: "http://localhost:8000", Priority: -1},
	})
	router.SetPeers([]string{"http://peer:9000"})

	req := httptest.NewRequest(http.MethodGet, "/admin/topology", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("localhost status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	body := rr.Body.String()
	for _, want := range []string{"urn:moos:kernel:test.primary", "http://peer:9000", `"priority":-1`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body = %s, want it to contain %q", body, want)
		}
	}
}
