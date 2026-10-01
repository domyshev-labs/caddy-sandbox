package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestWaitForAdminAPI(t *testing.T) {
	c := &child{done: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config/" {
			t.Error("unexpected readiness path", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := ready(ctx, c, server.URL+"/config/"); err != nil {
		t.Fatal(err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := ready(ctx, c, bad.URL); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unready Caddy did not time out: %v", err)
	}
	c.err = errors.New("startup failed")
	close(c.done)
	if err := ready(context.Background(), c, bad.URL); err == nil {
		t.Fatal("ignored Caddy startup exit")
	}
}

func TestChildHelper(t *testing.T) {
	if os.Getenv("CS_RUNNER_TEST_CHILD") != "1" {
		return
	}
	if os.Getenv("CS_RUNNER_TEST_EXIT") == "1" {
		os.Exit(7)
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestChildExitAndShutdown(t *testing.T) {
	t.Setenv("CS_RUNNER_TEST_CHILD", "1")
	t.Setenv("CS_RUNNER_TEST_EXIT", "1")
	c, err := start(os.Args[0], "-test.run=^TestChildHelper$")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.done:
		if c.err == nil {
			t.Fatal("lost nonzero exit status")
		}
	case <-time.After(5 * time.Second):
		c.stop(time.Second)
		t.Fatal("child was not reaped")
	}
	// Stopping an already exited process must not hang.
	c.stop(time.Second)
	t.Setenv("CS_RUNNER_TEST_EXIT", "0")
	c, err = start(os.Args[0], "-test.run=^TestChildHelper$")
	if err != nil {
		t.Fatal(err)
	}
	c.stop(time.Second)
	select {
	case <-c.done:
	default:
		t.Fatal("stop returned without reaping child")
	}
}
