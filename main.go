package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const header = "# Managed by caddy-sandbox. Do not edit manually.\n"

var namePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

//go:embed docs.html
var docsHTML string

var docsTemplate = template.Must(template.New("docs").Parse(docsHTML))

type config struct {
	Listen, Token, UpstreamIP, File, Container, ContainerConfig, Docker string
	DomainSuffix, PublicURL                                             string
	MinPort, MaxPort                                                    int
}

func env(key, fallback string) string {
	if v := os.Getenv("CS_" + key); v != "" {
		return v
	}
	return fallback
}

func readConfig() (config, error) {
	c := config{Listen: env("LISTEN", "127.0.0.1:9000"), Token: env("TOKEN", ""),
		UpstreamIP: env("UPSTREAM_IP", "192.0.2.20"), File: env("FILE", "/opt/caddy/config/sandbox.caddy"),
		Container: env("CONTAINER", "caddy"), ContainerConfig: env("CONTAINER_CONFIG", "/etc/caddy/Caddyfile"),
		Docker: env("DOCKER", "/usr/bin/docker")}
	c.DomainSuffix = env("DOMAIN_SUFFIX", "sandbox.example.com")
	c.PublicURL = env("PUBLIC_URL", "https://control.example.com")
	if !validDomain(c.DomainSuffix) {
		return c, errors.New("CS_DOMAIN_SUFFIX must be a valid DNS domain")
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return c, errors.New("CS_PUBLIC_URL must be an absolute HTTP(S) URL without credentials, a path, a query, or a fragment")
	}
	c.PublicURL = strings.TrimSuffix(c.PublicURL, "/")
	if c.MinPort, err = strconv.Atoi(env("MIN_PORT", "10000")); err != nil {
		return c, err
	}
	if c.MaxPort, err = strconv.Atoi(env("MAX_PORT", "10099")); err != nil {
		return c, err
	}
	if len(c.Token) < 32 {
		return c, errors.New("CS_TOKEN must contain at least 32 characters")
	}
	ip := net.ParseIP(c.UpstreamIP)
	if ip == nil || ip.To4() == nil {
		return c, errors.New("CS_UPSTREAM_IP must be a literal IPv4 address")
	}
	c.UpstreamIP = ip.String()
	if c.MinPort < 1 || c.MaxPort > 65535 || c.MinPort > c.MaxPort {
		return c, errors.New("invalid port range")
	}
	if !filepath.IsAbs(c.File) || !filepath.IsAbs(c.Docker) || !filepath.IsAbs(c.ContainerConfig) {
		return c, errors.New("file, Docker executable and container config paths must be absolute")
	}
	if strings.HasPrefix(c.Container, "-") || c.Container == "" {
		return c, errors.New("invalid container name")
	}
	return c, nil
}

type app struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	URL  string `json:"url"`
}

func validDomain(domain string) bool {
	if len(domain) > 253 || !strings.Contains(domain, ".") || net.ParseIP(domain) != nil {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if !namePattern.MatchString(label) {
			return false
		}
	}
	// Leave room for the application label and its separating dot.
	return len(domain) <= 189
}

func (s *service) describe(name string, port int) app {
	return app{name, port, "https://" + name + "." + s.c.DomainSuffix}
}

type service struct {
	c         config
	mu        sync.Mutex
	apps      map[string]int
	current   []byte
	unhealthy bool
	run       func(bool) error
}

func (s *service) render(apps map[string]int) []byte {
	names := make([]string, 0, len(apps))
	for n := range apps {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(header)
	for _, n := range names {
		fmt.Fprintf(&b, "\n%s.%s {\n    reverse_proxy %s:%d\n}\n", n, s.c.DomainSuffix, s.c.UpstreamIP, apps[n])
	}
	return []byte(b.String())
}

func (s *service) parse(data []byte) (map[string]int, error) {
	text := strings.TrimSpace(string(data))
	if strings.HasPrefix(text, strings.TrimSpace(header)) {
		text = strings.TrimSpace(strings.TrimPrefix(text, strings.TrimSpace(header)))
	}
	p := regexp.MustCompile(`^([a-z0-9-]+)\.` + regexp.QuoteMeta(s.c.DomainSuffix) + `[ \t]*\{\s*reverse_proxy[ \t]+` + regexp.QuoteMeta(s.c.UpstreamIP) + `:([0-9]{1,5})\s*\}`)
	apps, ports := map[string]int{}, map[int]bool{}
	for text != "" {
		m := p.FindStringSubmatch(text)
		if m == nil || !namePattern.MatchString(m[1]) {
			return nil, errors.New("import file contains unsupported content")
		}
		port, _ := strconv.Atoi(m[2])
		if port < s.c.MinPort || port > s.c.MaxPort || ports[port] {
			return nil, errors.New("invalid or duplicate port in import file")
		}
		if _, found := apps[m[1]]; found {
			return nil, errors.New("duplicate app name in import file")
		}
		apps[m[1]], ports[port] = port, true
		text = strings.TrimSpace(text[len(m[0]):])
	}
	return apps, nil
}

// Temporary files are siblings of the destination, so rename is atomic.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".caddy-sandbox-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *service) command(reload bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	subcommand := "validate"
	if reload {
		subcommand = "reload"
	}
	cmd := exec.CommandContext(ctx, s.c.Docker, "exec", s.c.Container, "caddy", subcommand,
		"--config", s.c.ContainerConfig, "--adapter", "caddyfile")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("caddy %s: %w: %s", subcommand, err, out)
	}
	return nil
}

func (s *service) clearJournal() error {
	if err := os.Remove(s.c.File + ".pending"); err != nil {
		return err
	}
	return syncDir(filepath.Dir(s.c.File))
}

func (s *service) rollback(old []byte) error {
	if err := atomicWrite(s.c.File, old, 0644); err != nil {
		return err
	}
	if err := s.run(false); err != nil {
		return err
	}
	if err := s.run(true); err != nil {
		return err
	}
	return s.clearJournal()
}

// A journal allows recovery after a crash between replacing the file and reload.
func (s *service) initialize() error {
	if info, err := os.Lstat(s.c.File); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("CS_FILE must already exist as a regular file (not a symlink): %s", s.c.File)
	}
	if old, err := os.ReadFile(s.c.File + ".pending"); err == nil {
		if _, err = s.parse(old); err != nil {
			return fmt.Errorf("invalid recovery journal: %w", err)
		}
		log.Print("recovering interrupted configuration update")
		if err = s.rollback(old); err != nil {
			return fmt.Errorf("recovery failed: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := os.ReadFile(s.c.File)
	if err != nil {
		return err
	}
	apps, err := s.parse(data)
	if err != nil {
		return err
	}
	// Reconcile the running configuration with the persisted file on every start.
	if err = s.run(false); err != nil {
		return err
	}
	if err = s.run(true); err != nil {
		return err
	}
	s.apps, s.current = apps, data
	return nil
}

func (s *service) apply(next map[string]int) error {
	onDisk, err := os.ReadFile(s.c.File)
	if err != nil {
		return err
	}
	if !bytes.Equal(onDisk, s.current) {
		return errors.New("import file changed outside this API; restart after reviewing it")
	}
	if err = atomicWrite(s.c.File+".pending", s.current, 0600); err != nil {
		s.unhealthy = true
		return err
	}
	candidate := s.render(next)
	err = atomicWrite(s.c.File, candidate, 0644)
	if err == nil {
		err = s.run(false)
	}
	if err == nil {
		err = s.run(true)
	}
	if err == nil {
		err = s.clearJournal()
	}
	if err != nil {
		// clearJournal may have removed the journal before a directory fsync failed.
		if journalErr := atomicWrite(s.c.File+".pending", s.current, 0600); journalErr != nil {
			s.unhealthy = true
			return fmt.Errorf("update failed: %v; journal failed: %w", err, journalErr)
		}
		if rollbackErr := s.rollback(s.current); rollbackErr != nil {
			s.unhealthy = true
			return fmt.Errorf("update failed: %v; rollback failed: %w", err, rollbackErr)
		}
		return err
	}
	s.apps, s.current = next, candidate
	return nil
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func problem(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}

func (s *service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if err := docsTemplate.Execute(w, s.c); err != nil {
			log.Printf("render API documentation: %v", err)
		}
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.unhealthy {
			problem(w, 503, "configuration recovery required")
			return
		}
		respond(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /apps", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		list := make([]app, 0, len(s.apps))
		for n, p := range s.apps {
			list = append(list, s.describe(n, p))
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		respond(w, 200, list)
	})
	mux.HandleFunc("GET /apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		name := r.PathValue("name")
		port, found := s.apps[name]
		if !found {
			problem(w, 404, "app not found")
			return
		}
		respond(w, 200, s.describe(name, port))
	})
	mutate := func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !namePattern.MatchString(name) {
			problem(w, 400, "invalid app name")
			return
		}
		var body struct {
			Port int `json:"port"`
		}
		if r.Method == "PUT" {
			r.Body = http.MaxBytesReader(w, r.Body, 1024)
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil {
				problem(w, 400, "expected JSON with only an integer port")
				return
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				problem(w, 400, "unexpected data after JSON")
				return
			}
			if body.Port < s.c.MinPort || body.Port > s.c.MaxPort {
				problem(w, 400, "port outside allowed range")
				return
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.unhealthy {
			problem(w, 503, "configuration recovery required")
			return
		}
		oldPort, exists := s.apps[name]
		if r.Method == "DELETE" && !exists {
			w.WriteHeader(204)
			return
		}
		if r.Method == "PUT" && exists && oldPort == body.Port {
			respond(w, 200, s.describe(name, oldPort))
			return
		}
		next := make(map[string]int, len(s.apps)+1)
		for n, p := range s.apps {
			if r.Method == "PUT" && n != name && p == body.Port {
				problem(w, 409, "port already assigned to another app")
				return
			}
			next[n] = p
		}
		if r.Method == "DELETE" {
			delete(next, name)
		} else {
			next[name] = body.Port
		}
		if err := s.apply(next); err != nil {
			log.Printf("update %s %s: %v", r.Method, name, err)
			problem(w, 503, "configuration update failed; check server logs")
			return
		}
		log.Printf("applied %s %s", r.Method, name)
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		status := 200
		if !exists {
			status = 201
		}
		respond(w, status, s.describe(name, body.Port))
	}
	mux.HandleFunc("PUT /apps/{name}", mutate)
	mux.HandleFunc("DELETE /apps/{name}", mutate)
	expected := sha256.Sum256([]byte("Bearer " + s.c.Token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// Caddy still restricts the documentation by source IP.
		if r.URL.Path == "/" && (r.Method == "GET" || r.Method == "HEAD") {
			mux.ServeHTTP(w, r)
			return
		}
		given := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(expected[:], given[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			problem(w, 401, "unauthorized")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func main() {
	c, err := readConfig()
	if err != nil {
		log.Fatal(err)
	}
	lock, err := os.OpenFile(c.File+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		log.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		log.Fatal("another instance holds the configuration lock")
	}
	s := &service{c: c}
	s.run = s.command
	if err = s.initialize(); err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: c.Listen, Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 5 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-stop.Done()
		ctx, done := context.WithTimeout(context.Background(), 4*time.Minute)
		defer done()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	log.Printf("listening on %s; upstream %s; ports %d-%d", c.Listen, c.UpstreamIP, c.MinPort, c.MaxPort)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
}
