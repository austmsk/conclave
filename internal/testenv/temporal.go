package testenv

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
)

// TemporalImage is the dev server image, the same as
// deploy/docker-compose.yml.
const TemporalImage = "temporalio/temporal:1.9.1"

// temporalReady bounds how long StartTemporal waits for the server to
// serve the default namespace.
const temporalReady = 2 * time.Minute

// StartTemporal starts a Temporal dev server in a container for t and
// returns its frontend address, host:port. The server keeps its state in
// memory and serves the default namespace; the container is terminated
// when t ends. Under -short the test is skipped (or failed, when
// RequireEnv is set); without Docker it fails, so an integration test can
// never pass by not running.
func StartTemporal(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		Skip(t, "integration test: run without -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), temporalReady)
	defer cancel()
	ctr, err := testcontainers.Run(ctx, TemporalImage,
		testcontainers.WithCmd("server", "start-dev", "--ip", "0.0.0.0", "--headless"),
		testcontainers.WithExposedPorts("7233/tcp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("7233/tcp").WithStartupTimeout(temporalReady)),
	)
	t.Cleanup(func() {
		if ctr == nil {
			return
		}
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Errorf("terminating Temporal: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("starting Temporal: %v", err)
	}
	addr, err := ctr.PortEndpoint(ctx, "7233/tcp", "")
	if err != nil {
		t.Fatalf("reading Temporal's address: %v", err)
	}
	if err := awaitNamespace(ctx, addr); err != nil {
		t.Fatalf("Temporal at %s never served the default namespace: %v", addr, err)
	}
	return addr
}

// awaitNamespace waits until the server at addr describes the default
// namespace: the port listens before the frontend is ready to serve it.
func awaitNamespace(ctx context.Context, addr string) error {
	var last error
	for ctx.Err() == nil {
		last = describeDefault(ctx, addr)
		if last == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%w (last: %v)", ctx.Err(), last)
}

func describeDefault(ctx context.Context, addr string) error {
	nc, err := client.NewNamespaceClient(client.Options{HostPort: addr, Logger: quietLogger()})
	if err != nil {
		return err
	}
	defer nc.Close()
	_, err = nc.Describe(ctx, client.DefaultNamespace)
	return err
}

// quietLogger drops the SDK's own logging while the server comes up.
func quietLogger() tlog.Logger {
	return tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
}
