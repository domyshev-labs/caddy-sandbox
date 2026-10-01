package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func fixture(t *testing.T) *service {
	t.Helper()
	s := &service{c: config{Token: strings.Repeat("x", 32), UpstreamIP: "192.0.2.20", DomainSuffix: "sandbox.example.com", PublicURL: "https://control.example.com", MinPort: 10000, MaxPort: 10099,
		File: filepath.Join(t.TempDir(), "sandbox.caddy")}, apps: map[string]int{}}
	s.current = s.render(s.apps)
	if err := atomicWrite(s.c.File, s.current, 0644); err != nil {
		t.Fatal(err)
	}
	s.run = func(bool) error { return nil }
	return s
}

func TestCommandExecutionModes(t *testing.T) {
	dir := t.TempDir()
	executable, output := filepath.Join(dir, "command"), filepath.Join(dir, "arguments")
	// Capture each argument separately to catch accidental shell evaluation,
	// especially for paths with spaces, and return a real command failure.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CS_TEST_ARGUMENTS\"\nif [ \"$CS_TEST_FAIL\" = 1 ]; then echo rejected >&2; exit 7; fi\n"
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CS_TEST_ARGUMENTS", output)
	s := &service{c: config{Caddy: executable, Docker: executable, Container: "caddy", ContainerConfig: "/config with spaces/Caddyfile"}}
	for _, mode := range []string{"local", "docker"} {
		s.c.CommandMode = mode
		for _, reload := range []bool{false, true} {
			op := "validate"
			if reload {
				op = "reload"
			}
			if err := s.command(reload); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			want := op + "\n--config\n/config with spaces/Caddyfile\n--adapter\ncaddyfile\n"
			if mode == "docker" {
				want = "exec\ncaddy\ncaddy\n" + want
			}
			if string(got) != want {
				t.Fatalf("%s: got %q, want %q", mode, got, want)
			}
		}
		t.Setenv("CS_TEST_FAIL", "1")
		if err := s.command(true); err == nil || !strings.Contains(err.Error(), "rejected") {
			t.Fatalf("%s: lost command error: %v", mode, err)
		}
		t.Setenv("CS_TEST_FAIL", "0")
	}
}

func TestCommandModeConfiguration(t *testing.T) {
	t.Setenv("CS_TOKEN", strings.Repeat("x", 32))
	t.Setenv("CS_COMMAND_MODE", "")
	c, err := readConfig()
	if err != nil || c.CommandMode != "docker" {
		t.Fatalf("default mode: %v, %v", c, err)
	}
	t.Setenv("CS_COMMAND_MODE", "local")
	if _, err := readConfig(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CS_CADDY", "relative/caddy")
	if _, err := readConfig(); err == nil {
		t.Fatal("accepted relative executable")
	}
	t.Setenv("CS_CADDY", "/usr/bin/caddy")
	t.Setenv("CS_COMMAND_MODE", "shell")
	if _, err := readConfig(); err == nil {
		t.Fatal("accepted unknown mode")
	}
}

func request(s *service, method, path, body string, auth bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth {
		r.Header.Set("Authorization", "Bearer "+s.c.Token)
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}

func TestCustomDomainConfiguration(t *testing.T) {
	s := fixture(t)
	s.c.DomainSuffix = "apps.example.org"
	s.c.PublicURL = "https://control.example.org"
	s.current = s.render(s.apps)
	if err := atomicWrite(s.c.File, s.current, 0644); err != nil {
		t.Fatal(err)
	}
	w := request(s, "PUT", "/apps/demo", `{"port":10003}`, true)
	if w.Code != 201 || !strings.Contains(w.Body.String(), "https://demo.apps.example.org") {
		t.Fatal(w.Code, w.Body.String())
	}
	data, err := os.ReadFile(s.c.File)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.parse(data); err != nil {
		t.Fatal(err)
	}
	if _, err := s.parse(bytes.ReplaceAll(data, []byte("apps.example.org"), []byte("sandbox.example.com"))); err == nil {
		t.Fatal("accepted a different domain suffix")
	}
	w = request(s, "GET", "/", "", false)
	if !strings.Contains(w.Body.String(), s.c.PublicURL) || !strings.Contains(w.Body.String(), s.c.DomainSuffix) {
		t.Fatal("documentation ignores configured domain")
	}
	for _, domain := range []string{"*.example.org", "example.org {", "a..org", "-example.org", "example.org/path", "EXAMPLE.org", "127.0.0.1"} {
		if validDomain(domain) {
			t.Fatalf("accepted invalid suffix %q", domain)
		}
	}
}

func TestAPIAndPersistence(t *testing.T) {
	s := fixture(t)
	calls := 0
	s.run = func(bool) error { calls++; return nil }
	if w := request(s, "GET", "/healthz", "", false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(s, "PUT", "/apps/demo", `{"port":10003}`, true); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if calls != 2 {
		t.Fatal("expected validate and reload", calls)
	}
	if w := request(s, "PUT", "/apps/demo", `{"port":10003}`, true); w.Code != 200 || calls != 2 {
		t.Fatal("PUT not idempotent")
	}
	if w := request(s, "PUT", "/apps/other", `{"port":10003}`, true); w.Code != 409 {
		t.Fatal(w.Code)
	}
	if w := request(s, "GET", "/apps/demo", "", true); w.Code != 200 || !strings.Contains(w.Body.String(), "demo.sandbox.example.com") {
		t.Fatal(w.Code)
	}
	data, _ := os.ReadFile(s.c.File)
	loaded, err := s.parse(data)
	if err != nil || loaded["demo"] != 10003 {
		t.Fatal(loaded, err)
	}
	if _, err := os.Stat(s.c.File + ".pending"); !os.IsNotExist(err) {
		t.Fatal("journal should be cleared")
	}
	if w := request(s, "PUT", "/apps/demo", `{"port":10004}`, true); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := request(s, "DELETE", "/apps/demo", "", true); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := request(s, "DELETE", "/apps/demo", "", true); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := request(s, "GET", "/apps", "", true); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatal(w.Body.String())
	}
}

func TestRejectInvalidRequests(t *testing.T) {
	s := fixture(t)
	for _, tc := range []struct{ name, body string }{
		{"Demo", `{"port":10001}`}, {"-demo", `{"port":10001}`}, {"demo-", `{"port":10001}`},
		{"demo.test", `{"port":10001}`}, {strings.Repeat("a", 64), `{"port":10001}`},
		{"demo", `{"port":9999}`}, {"demo", `{"port":10100}`}, {"demo", `{"port":"10001"}`},
		{"demo", `{"port":10001,"ip":"127.0.0.1"}`}, {"demo", `{"port":10001} {}`},
		{"demo", `null`}, {"demo", strings.Repeat(" ", 1025) + `{"port":10001}`},
	} {
		if w := request(s, "PUT", "/apps/"+tc.name, tc.body, true); w.Code != 400 {
			t.Fatalf("%q %q: %d", tc.name, tc.body, w.Code)
		}
	}
	if len(s.apps) != 0 {
		t.Fatal("invalid request changed registry")
	}
}

func TestStrictImportPolicy(t *testing.T) {
	s := fixture(t)
	good := s.render(map[string]int{"demo": 10003})
	for _, bad := range []string{
		strings.ReplaceAll(string(good), "192.0.2.20", "192.0.2.21"),
		strings.ReplaceAll(string(good), "sandbox.example.com", "example.net"),
		strings.ReplaceAll(string(good), "demo.sandbox", "*.sandbox"),
		strings.ReplaceAll(string(good), "10003", "8080"),
		strings.ReplaceAll(string(good), "\n}", "\n    file_server\n}"),
		string(good) + "import other.caddy\n", string(good) + string(good),
		string(good) + strings.ReplaceAll(strings.TrimPrefix(string(good), header), "demo.", "other."),
	} {
		if _, err := s.parse([]byte(bad)); err == nil {
			t.Fatalf("accepted forbidden content: %s", bad)
		}
	}
}

func TestRollbackOnValidateAndReloadFailure(t *testing.T) {
	for _, failedCall := range []int{1, 2} {
		t.Run(fmt.Sprint(failedCall), func(t *testing.T) {
			s := fixture(t)
			old := bytes.Clone(s.current)
			calls := 0
			s.run = func(bool) error {
				calls++
				if calls == failedCall {
					return errors.New("simulated failure")
				}
				return nil
			}
			w := request(s, "PUT", "/apps/demo", `{"port":10003}`, true)
			if w.Code != 503 || len(s.apps) != 0 || s.unhealthy {
				t.Fatal(w.Code, s.apps, s.unhealthy)
			}
			data, _ := os.ReadFile(s.c.File)
			if !bytes.Equal(data, old) {
				t.Fatal("old file not restored")
			}
			if calls != failedCall+2 {
				t.Fatal("rollback must validate and reload previous file", calls)
			}
		})
	}
}

func TestFailedRollbackAndRestartRecovery(t *testing.T) {
	s := fixture(t)
	old := bytes.Clone(s.current)
	s.run = func(bool) error { return errors.New("Docker unavailable") }
	if w := request(s, "PUT", "/apps/demo", `{"port":10003}`, true); w.Code != 503 || !s.unhealthy {
		t.Fatal(w.Code)
	}
	if w := request(s, "GET", "/healthz", "", true); w.Code != 503 {
		t.Fatal(w.Code)
	}
	if w := request(s, "PUT", "/apps/other", `{"port":10004}`, true); w.Code != 503 {
		t.Fatal(w.Code)
	}
	if _, err := os.Stat(s.c.File + ".pending"); err != nil {
		t.Fatal(err)
	}
	// Simulate a process dying with a candidate file installed and journal intact.
	if err := atomicWrite(s.c.File, s.render(map[string]int{"demo": 10003}), 0644); err != nil {
		t.Fatal(err)
	}
	restarted := &service{c: s.c, run: func(bool) error { return nil }}
	if err := restarted.initialize(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(s.c.File)
	if !bytes.Equal(data, old) || len(restarted.apps) != 0 {
		t.Fatal("recovery did not restore old registry")
	}
}

func TestExternalEditsNotOverwritten(t *testing.T) {
	s := fixture(t)
	if err := os.WriteFile(s.c.File, []byte("external edit"), 0644); err != nil {
		t.Fatal(err)
	}
	if w := request(s, "PUT", "/apps/demo", `{"port":10003}`, true); w.Code != 503 {
		t.Fatal(w.Code)
	}
	data, _ := os.ReadFile(s.c.File)
	if string(data) != "external edit" {
		t.Fatal("external changes overwritten")
	}
}

func TestConcurrentPortAllocation(t *testing.T) {
	s := fixture(t)
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, name := range []string{"one", "two"} {
		wg.Add(1)
		go func(n string) { defer wg.Done(); results <- request(s, "PUT", "/apps/"+n, `{"port":10003}`, true).Code }(name)
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for code := range results {
		counts[code]++
	}
	if counts[201] != 1 || counts[409] != 1 {
		t.Fatal(counts)
	}
}

func TestDocumentationAndAuthenticationBoundary(t *testing.T) {
	s := fixture(t)
	s.c.MinPort, s.c.MaxPort = 11000, 11099
	w := request(s, "GET", "/", "", false)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatal(w.Code, w.Header())
	}
	for _, text := range []string{"Caddy API", "/apps/{name}", "/healthz", "11000", "11099", s.c.UpstreamIP} {
		if !strings.Contains(w.Body.String(), text) {
			t.Fatalf("documentation missing %q", text)
		}
	}
	if strings.Contains(w.Body.String(), s.c.Token) {
		t.Fatal("documentation leaked token")
	}
	if w := request(s, "HEAD", "/", "", false); w.Code != 200 {
		t.Fatal(w.Code)
	}
	for _, path := range []string{"/apps", "/apps/demo", "/healthz", "/unknown"} {
		if w := request(s, "GET", path, "", false); w.Code != 401 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	if w := request(s, "GET", "/unknown", "", true); w.Code != 404 {
		t.Fatal("root route matched unknown path", w.Code)
	}
	if w := request(s, "POST", "/", "", false); w.Code != 401 {
		t.Fatal("root exemption permits write methods", w.Code)
	}
}
