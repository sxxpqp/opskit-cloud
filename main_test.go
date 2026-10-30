package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayForwardsAndRewrites(t *testing.T) {
	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{\"tarball\":\"https://registry.npmjs.org/pkg/-/pkg.tgz\"}"))
	}))
	defer upstream.Close()
	c := config{PublicURL: "https://cache.example.com", Timeout: "1m", AllowCIDRs: []string{"127.0.0.1/32"},
		Downloads: map[string]string{"npm": upstream.URL, "npm-files": "https://registry.npmjs.org"}}
	g := newGateway(c)
	req := httptest.NewRequest("GET", "http://proxy/npm/@scope%2fpkg?version=1", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != 200 || got != "/@scope%2fpkg?version=1" {
		t.Fatalf("%d %s", w.Code, got)
	}
	if !strings.Contains(w.Body.String(), "https://cache.example.com/npm-files/pkg/-/pkg.tgz") {
		t.Fatal(w.Body.String())
	}
	req.RemoteAddr = "192.0.2.1:123"
	w = httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("allowed unknown client")
	}
}
func TestRegistryChallengeAndToken(t *testing.T) {
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{\"token\":\"ok\"}")) }))
	defer tokenServer.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/org/image/manifests/latest" {
			t.Error(r.URL.Path)
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+tokenServer.URL+`/token",service="registry"`)
		w.WriteHeader(401)
	}))
	defer upstream.Close()
	tokenHost := strings.TrimPrefix(tokenServer.URL, "https://")
	c := config{PublicURL: "https://cache.example.com", Timeout: "1m", AllowCIDRs: []string{"127.0.0.1/32"},
		Registries: map[string]registry{"ghcr": {Upstream: upstream.URL, AuthHosts: []string{tokenHost}}}}
	g := newGateway(c)
	g.client.Transport = tokenServer.Client().Transport
	req := httptest.NewRequest("GET", "http://proxy/v2/ghcr/org/image/manifests/latest", nil)
	req.RemoteAddr = "127.0.0.1:123"
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "https://cache.example.com/auth/ghcr") {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	req = httptest.NewRequest("GET", "http://proxy/auth/ghcr?service=registry", nil)
	req.RemoteAddr = "127.0.0.1:123"
	w = httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ok") {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestConfigAndRendering(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	if e := initConfig([]string{"--output", file, "--public-url", "https://cache.example.com", "--allow-cidrs", "192.0.2.1/32"}); e != nil {
		t.Fatal(e)
	}
	c, e := loadConfig(file)
	if e != nil {
		t.Fatal(e)
	}
	if c.PublicURL != "https://cache.example.com" || c.AllowCIDRs[0] != "192.0.2.1/32" {
		t.Fatal(c)
	}
	if e := initConfig([]string{"--output", file}); e == nil {
		t.Fatal("overwrote config")
	}
	if got := renderValue("https://old.example.com/repository/raw-github/org/repo/file", c); got != "https://cache.example.com/github/org/repo/file" {
		t.Fatal(got)
	}
	source := filepath.Join(t.TempDir(), "source.yaml")
	os.WriteFile(source, []byte("schema_version: 1\ns3:\n  endpoint: https://storage.example.com\ninstall:\n  kind:\n    download_base: https://github.com\n"), 0600)
	out := filepath.Join(t.TempDir(), "out.yaml")
	if e := render([]string{"--config", file, "--input", source, "--output", out}); e != nil {
		t.Fatal(e)
	}
	b, _ := os.Open(out)
	defer b.Close()
	data, _ := io.ReadAll(b)
	if !strings.Contains(string(data), "https://storage.example.com") || !strings.Contains(string(data), "https://cache.example.com/github") {
		t.Fatal(string(data))
	}
}
