package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed config.example.yaml
var example []byte

type registry struct {
	Upstream  string   `yaml:"upstream"`
	AuthHosts []string `yaml:"auth_hosts"`
}
type config struct {
	Listen     string              `yaml:"listen"`
	PublicURL  string              `yaml:"public_url"`
	AllowCIDRs []string            `yaml:"allow_cidrs"`
	Timeout    string              `yaml:"timeout"`
	TLSCert    string              `yaml:"tls_cert"`
	TLSKey     string              `yaml:"tls_key"`
	Downloads  map[string]string   `yaml:"downloads"`
	Registries map[string]registry `yaml:"registries"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
var realmPattern = regexp.MustCompile(`realm="([^"]+)"`)

func parseURL(s string) (*url.URL, error) {
	u, e := url.Parse(s)
	if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid URL %q", s)
	}
	return u, nil
}
func loadConfig(path string) (config, error) {
	var c config
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	var extra any
	if e = d.Decode(&extra); e != io.EOF {
		return c, errors.New("exactly one YAML document required")
	}
	if c.Listen == "" {
		return c, errors.New("listen is required")
	}
	u, e := parseURL(c.PublicURL)
	if e != nil {
		return c, e
	}
	if u.Path != "" && u.Path != "/" {
		return c, errors.New("public_url must not include a path")
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if _, e = time.ParseDuration(c.Timeout); e != nil {
		return c, e
	}
	if duration, _ := time.ParseDuration(c.Timeout); duration <= 0 {
		return c, errors.New("timeout must be positive")
	}
	if len(c.AllowCIDRs) == 0 {
		return c, errors.New("allow_cidrs must not be empty")
	}
	for _, v := range c.AllowCIDRs {
		if _, _, e = net.ParseCIDR(v); e != nil {
			return c, e
		}
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return c, errors.New("tls_cert and tls_key must be configured together")
	}
	for name, target := range c.Downloads {
		if !namePattern.MatchString(name) {
			return c, errors.New("invalid download name")
		}
		if _, e = parseURL(target); e != nil {
			return c, e
		}
	}
	for name, r := range c.Registries {
		if !namePattern.MatchString(name) {
			return c, errors.New("invalid registry name")
		}
		u, e := parseURL(r.Upstream)
		if e != nil {
			return c, e
		}
		if u.Path != "" && u.Path != "/" {
			return c, errors.New("registry upstream must not include a path")
		}
	}
	return c, nil
}

type gateway struct {
	cfg    config
	client *http.Client
	nets   []*net.IPNet
	mu     sync.RWMutex
	realms map[string]*url.URL
}

func newGateway(c config) *gateway {
	duration, _ := time.ParseDuration(c.Timeout)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	g := &gateway{cfg: c, realms: map[string]*url.URL{}, client: &http.Client{
		Timeout: duration, Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many upstream redirects")
			}
			if req.URL.Scheme != "https" {
				return errors.New("upstream redirect must use HTTPS")
			}
			return nil
		}}}
	for _, v := range c.AllowCIDRs {
		_, n, _ := net.ParseCIDR(v)
		g.nets = append(g.nets, n)
	}
	return g
}

type followTransport struct{ client *http.Client }

func (t followTransport) RoundTrip(r *http.Request) (*http.Response, error) { return t.client.Do(r) }
func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "read-only proxy", 405)
		return
	}
	if r.URL.Path == "/healthz" {
		w.WriteHeader(200)
		w.Write([]byte("ok\n"))
		return
	}
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		http.Error(w, "invalid client", 403)
		return
	}
	allowed := false
	for _, n := range g.nets {
		if n.Contains(net.ParseIP(host)) {
			allowed = true
		}
	}
	if !allowed {
		http.Error(w, "client IP not allowed", 403)
		return
	}
	path := r.URL.EscapedPath()
	if strings.HasPrefix(path, "/auth/") {
		name := strings.TrimPrefix(path, "/auth/")
		g.mu.RLock()
		target := g.realms[name]
		g.mu.RUnlock()
		if target == nil {
			http.Error(w, "unknown authentication endpoint; request registry first", 404)
			return
		}
		// The upstream realm is learned only from an allowlisted registry challenge.
		g.proxy(w, r, target, target.Path, "", false)
		return
	}
	if strings.HasPrefix(path, "/v2") {
		name := "dockerhub"
		suffix := strings.TrimPrefix(path, "/v2")
		if suffix != "" && !strings.HasPrefix(suffix, "/") {
			http.NotFound(w, r)
			return
		}
		first, rest, _ := strings.Cut(strings.TrimPrefix(suffix, "/"), "/")
		if _, ok := g.cfg.Registries[first]; ok {
			name = first
			suffix = "/" + rest
		}
		reg, ok := g.cfg.Registries[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		target, _ := url.Parse(reg.Upstream)
		g.proxy(w, r, target, "/v2"+suffix, name, false)
		return
	}
	name, rest, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	targetString, ok := g.cfg.Downloads[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	target, _ := url.Parse(targetString)
	g.proxy(w, r, target, strings.TrimRight(target.Path, "/")+"/"+rest, "", name == "npm" || name == "pip")
}
func (g *gateway) proxy(w http.ResponseWriter, r *http.Request, target *url.URL, path, registryName string, rewriteBody bool) {
	p := &httputil.ReverseProxy{
		Transport: followTransport{g.client},
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path, _ = url.PathUnescape(path)
			pr.Out.URL.RawPath = path
			pr.Out.Host = target.Host
			pr.Out.RequestURI = ""
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Forwarded")
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Set("Accept-Encoding", "identity")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			log.Printf("upstream request failed: %s", r.URL.Path)
			http.Error(w, "upstream request failed", 502)
		},
		ModifyResponse: func(res *http.Response) error {
			res.Header.Del("Set-Cookie")
			if registryName != "" {
				challenge := res.Header.Get("WWW-Authenticate")
				match := realmPattern.FindStringSubmatch(challenge)
				if len(match) == 2 {
					realm, e := url.Parse(match[1])
					if e != nil || realm.Scheme != "https" || realm.User != nil || realm.Host == "" {
						return errors.New("invalid registry authentication URL")
					}
					reg := g.cfg.Registries[registryName]
					allowed := strings.EqualFold(realm.Host, target.Host)
					for _, host := range reg.AuthHosts {
						if strings.EqualFold(realm.Host, host) {
							allowed = true
						}
					}
					if !allowed {
						return errors.New("registry authentication host is not allowlisted")
					}
					g.mu.Lock()
					g.realms[registryName] = realm
					g.mu.Unlock()
					res.Header.Set("WWW-Authenticate", strings.Replace(challenge, match[1], g.cfg.PublicURL+"/auth/"+registryName, 1))
				}
			}
			contentType := res.Header.Get("Content-Type")
			if rewriteBody && r.Method != "HEAD" && (strings.Contains(contentType, "json") || strings.Contains(contentType, "html")) {
				body, e := io.ReadAll(io.LimitReader(res.Body, 16*1024*1024+1))
				res.Body.Close()
				if e != nil {
					return e
				}
				if len(body) > 16*1024*1024 {
					return errors.New("metadata exceeds 16 MiB")
				}
				body = g.rewriteLinks(body)
				res.Body = io.NopCloser(bytes.NewReader(body))
				res.ContentLength = int64(len(body))
				res.Header.Set("Content-Length", fmt.Sprint(len(body)))
				res.Header.Del("ETag")
				res.Header.Del("Content-MD5")
				res.Header.Del("Digest")
			}
			return nil
		}}
	p.ServeHTTP(w, r)
}
func (g *gateway) rewriteLinks(b []byte) []byte {
	names := make([]string, 0, len(g.cfg.Downloads))
	for n := range g.cfg.Downloads {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return len(g.cfg.Downloads[names[i]]) > len(g.cfg.Downloads[names[j]]) })
	for _, name := range names {
		upstream := strings.TrimRight(g.cfg.Downloads[name], "/")
		b = bytes.ReplaceAll(b, []byte(upstream+"/"), []byte(g.cfg.PublicURL+"/"+name+"/"))
	}
	return b
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	file := fs.String("config", "config.yaml", "configuration file")
	listen := fs.String("listen", "", "override listening address")
	public := fs.String("public-url", "", "override client-facing URL")
	check := fs.Bool("check", false, "validate config without listening")
	if e := fs.Parse(args); e != nil {
		return e
	}
	c, e := loadConfig(*file)
	if e != nil {
		return e
	}
	if *listen != "" {
		c.Listen = *listen
	}
	if *public != "" {
		u, e := parseURL(*public)
		if e != nil || (u.Path != "" && u.Path != "/") {
			return errors.New("invalid --public-url")
		}
		c.PublicURL = strings.TrimRight(*public, "/")
	}
	if *check {
		return nil
	}
	server := &http.Server{Addr: c.Listen, Handler: newGateway(c), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	log.Printf("listening on %s", c.Listen)
	if c.TLSCert != "" {
		e = server.ListenAndServeTLS(c.TLSCert, c.TLSKey)
	} else {
		e = server.ListenAndServe()
	}
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}

type values []string

func (v *values) String() string     { return strings.Join(*v, ",") }
func (v *values) Set(s string) error { *v = append(*v, s); return nil }
func initConfig(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	output := fs.String("output", "config.yaml", "new config file")
	listen := fs.String("listen", ":8080", "listen address")
	public := fs.String("public-url", "http://127.0.0.1:8080", "public URL")
	cidrs := fs.String("allow-cidrs", "127.0.0.1/32", "comma-separated client CIDRs")
	var upstreams values
	fs.Var(&upstreams, "upstream", "repeatable download-name=https://upstream/")
	if e := fs.Parse(args); e != nil {
		return e
	}
	u, e := parseURL(*public)
	if e != nil || (u.Path != "" && u.Path != "/") {
		return errors.New("invalid --public-url")
	}
	networks := strings.Split(*cidrs, ",")
	for i, v := range networks {
		networks[i] = strings.TrimSpace(v)
		if _, _, e = net.ParseCIDR(networks[i]); e != nil {
			return e
		}
	}
	var doc yaml.Node
	if e = yaml.Unmarshal(example, &doc); e != nil {
		return e
	}
	root := doc.Content[0]
	set := func(node *yaml.Node, key string, value any) error {
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				old := node.Content[i+1]
				next := &yaml.Node{}
				if e := next.Encode(value); e != nil {
					return e
				}
				next.HeadComment = old.HeadComment
				next.LineComment = old.LineComment
				node.Content[i+1] = next
				return nil
			}
		}
		return fmt.Errorf("unknown configuration key %q", key)
	}
	for key, value := range map[string]any{"listen": *listen, "public_url": strings.TrimRight(*public, "/"), "allow_cidrs": networks} {
		if e = set(root, key, value); e != nil {
			return e
		}
	}
	for _, entry := range upstreams {
		name, target, ok := strings.Cut(entry, "=")
		if !ok {
			return errors.New("upstream must be name=https://host/")
		}
		if _, e = parseURL(target); e != nil {
			return e
		}
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == "downloads" {
				if e = set(root.Content[i+1], name, target); e != nil {
					return e
				}
			}
		}
	}
	data, e := yaml.Marshal(&doc)
	if e != nil {
		return e
	}
	return writeExclusive(*output, data)
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: opskit-cloud init|serve|render [flags]")
	}
	switch args[0] {
	case "init":
		return initConfig(args[1:])
	case "serve":
		return serve(args[1:])
	case "render":
		return render(args[1:])
	}
	return errors.New("unknown command")
}
func main() {
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
