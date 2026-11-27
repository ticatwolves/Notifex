// Command healthcheck is a tiny HTTP probe for container health checks. The
// runtime image is distroless (no curl/wget), so ECS and docker-compose call
// this binary instead: `/app/healthcheck http://127.0.0.1:9100/healthz`.
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"
)

func main() {
	target := "http://127.0.0.1:9100/healthz"
	if len(os.Args) > 1 {
		target = os.Args[1]
	}
	if err := probe(target); err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		os.Exit(1)
	}
}

func probe(target string) error {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		return fmt.Errorf("target must be an http URL on localhost, got %q", target)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Target is restricted to http://localhost above, so this cannot reach other hosts.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil) //nolint:gosec // G704: localhost-only, validated above
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: localhost-only, validated above
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
