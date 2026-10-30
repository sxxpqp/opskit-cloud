package main

import (
	"bytes"
	"errors"
	"flag"
	"gopkg.in/yaml.v3"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func writeExclusive(path string, data []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(data)
	closeErr := f.Close()
	if e != nil {
		return e
	}
	return closeErr
}
func renderValue(v any, c config) any {
	switch x := v.(type) {
	case map[string]any:
		for k, value := range x {
			x[k] = renderValue(value, c)
		}
		return x
	case []any:
		for i, value := range x {
			x[i] = renderValue(value, c)
		}
		return x
	case string:
		u, e := url.Parse(x)
		if e != nil {
			return x
		}
		legacy := map[string]string{"raw-github": "github", "raw-githubusercontent": "raw", "kubernetes-binaries": "kubernetes", "golang": "golang", "docker-ce": "docker-ce"}
		if strings.HasPrefix(u.Path, "/repository/") {
			parts := strings.SplitN(strings.TrimPrefix(u.Path, "/repository/"), "/", 2)
			if name, ok := legacy[parts[0]]; ok {
				suffix := ""
				if len(parts) == 2 {
					suffix = "/" + parts[1]
				}
				if u.RawQuery != "" {
					suffix += "?" + u.RawQuery
				}
				return c.PublicURL + "/" + name + suffix
			}
		}
		if u.Hostname() == "mirrors.aliyun.com" || u.Hostname() == "mirrors.tuna.tsinghua.edu.cn" {
			for _, prefix := range []string{"/golang", "/docker-ce"} {
				if strings.HasPrefix(u.Path, prefix) {
					return c.PublicURL + u.RequestURI()
				}
			}
		}
		for name, upstream := range c.Downloads {
			base, _ := url.Parse(upstream)
			if u.Host != base.Host {
				continue
			}
			prefix := strings.TrimRight(base.Path, "/")
			if u.Path != prefix && !strings.HasPrefix(u.Path, prefix+"/") {
				continue
			}
			suffix := strings.TrimPrefix(u.Path, prefix)
			if u.RawQuery != "" {
				suffix += "?" + u.RawQuery
			}
			return c.PublicURL + "/" + name + suffix
		}
	}
	return v
}
func render(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	input := fs.String("input", "", "existing full opskit YAML")
	file := fs.String("config", "config.yaml", "proxy configuration")
	output := fs.String("output", "config.generated.yaml", "new opskit YAML")
	if e := fs.Parse(args); e != nil {
		return e
	}
	c, e := loadConfig(*file)
	if e != nil {
		return e
	}
	b, e := os.ReadFile(*input)
	if e != nil {
		return e
	}
	var data map[string]any
	d := yaml.NewDecoder(bytes.NewReader(b))
	if e = d.Decode(&data); e != nil {
		return e
	}
	if data == nil {
		return errors.New("input must be a mapping")
	}
	var extra any
	if e = d.Decode(&extra); e != io.EOF {
		return errors.New("exactly one YAML document required")
	}
	for _, key := range []string{"install", "kubeblocks"} {
		if v, ok := data[key]; ok {
			data[key] = renderValue(v, c)
		}
	}
	mirrors := map[string]any{"docker": []string{c.PublicURL}, "docker_insecure_registries": []string{},
		"go": c.PublicURL + "/go", "npm": c.PublicURL + "/npm", "pip": c.PublicURL + "/pip/simple/", "maven": c.PublicURL + "/maven"}
	registries := map[string]string{"docker.io": "dockerhub", "registry.k8s.io": "k8s", "k8s.gcr.io": "k8s", "quay.io": "quay", "ghcr.io": "ghcr", "registry.cn-hangzhou.aliyuncs.com": "aliyun", "container-registry.oracle.com": "oracle"}
	containerd := map[string]any{}
	for host, name := range registries {
		if _, ok := c.Registries[name]; !ok {
			continue
		}
		server := "https://" + host
		if host == "docker.io" {
			server = "https://registry-1.docker.io"
		}
		containerd[host] = map[string]any{"server": server, "endpoint": c.PublicURL + "/v2/" + name, "override_path": true, "skip_verify": false}
	}
	mirrors["containerd"] = containerd
	data["mirrors"] = mirrors
	b, e = yaml.Marshal(data)
	if e != nil {
		return e
	}
	return writeExclusive(*output, b)
}
