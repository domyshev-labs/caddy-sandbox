// container-runner supervises Caddy and its route controller without a shell.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

type child struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func start(path string, args ...string) (*child, error) {
	c := &child{cmd: exec.Command(path, args...), done: make(chan struct{})}
	c.cmd.Stdout, c.cmd.Stderr = os.Stdout, os.Stderr
	if err := c.cmd.Start(); err != nil {
		return nil, err
	}
	go func() { c.err = c.cmd.Wait(); close(c.done) }()
	return c, nil
}

func (c *child) stop(timeout time.Duration) {
	select {
	case <-c.done:
		return
	default:
	}
	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.done:
	case <-timer.C:
		log.Printf("shutdown timeout: killing %s", c.cmd.Path)
		_ = c.cmd.Process.Kill()
		<-c.done
	}
}

func ready(ctx context.Context, c *child, address string) error {
	// This example intentionally keeps the admin endpoint on container loopback.
	client := &http.Client{Timeout: time.Second}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return fmt.Errorf("Caddy exited during startup: %v", c.err)
		case <-ticker.C:
		}
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer cancel()
	caddy, err := start("/usr/bin/caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile")
	if err != nil {
		return err
	}
	defer caddy.stop(30 * time.Second)
	startup, finish := context.WithTimeout(ctx, 60*time.Second)
	err = ready(startup, caddy, "http://127.0.0.1:2019/config/")
	finish()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	sandbox, err := start("/usr/local/bin/caddy-sandbox")
	if err != nil {
		return err
	}
	// Stop the controller first so in-flight writes and rollback can finish
	// while Caddy is still available. Match the controller's four-minute drain.
	defer sandbox.stop(250 * time.Second)
	select {
	case <-ctx.Done():
		return nil
	case <-caddy.done:
		return fmt.Errorf("Caddy exited unexpectedly: %v", caddy.err)
	case <-sandbox.done:
		return fmt.Errorf("caddy-sandbox exited unexpectedly: %v", sandbox.err)
	}
}

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
