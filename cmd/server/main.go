package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
)

const (
	// dispatchDepth is how many accepted deliveries may wait for the
	// workflow engine. Beyond it GitHub is answered 503 and redelivers.
	dispatchDepth = 64
	// shutdownGrace bounds how long in-flight requests get to finish.
	shutdownGrace = 10 * time.Second
	// drainGrace bounds how long queued deliveries get to reach the engine
	// after the listener has closed; the rest are logged for redelivery.
	drainGrace = 20 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "conclave server:", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:8080", "address to listen on")
	githubPath := flag.String("github-config", "deploy/github.yaml", "GitHub App configuration file")
	reposPath := flag.String("repos", "deploy/repos.yaml", "repository access file; its repositories are the enabled ones")
	temporalAddr := flag.String("temporal-address", client.DefaultHostPort, "Temporal frontend address")
	namespace := flag.String("temporal-namespace", client.DefaultNamespace, "Temporal namespace")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: github.RedactAttr}))
	cfg, enabled, err := loadConfig(*githubPath, *reposPath)
	if err != nil {
		return err
	}
	// The secret is read once and removed from the environment, so nothing
	// this process starts later can inherit it.
	webhookSecret := []byte(os.Getenv(github.WebhookSecretEnv))
	if err := os.Unsetenv(github.WebhookSecretEnv); err != nil {
		return fmt.Errorf("unsetting %s: %w", github.WebhookSecretEnv, err)
	}

	tc, err := client.Dial(client.Options{HostPort: *temporalAddr, Namespace: *namespace, Logger: log})
	if err != nil {
		return fmt.Errorf("connecting to Temporal at %s: %w", *temporalAddr, err)
	}
	defer tc.Close()
	disp := github.NewDispatcher(temporalStarter{client: tc, ownerID: cfg.OwnerUserID, log: log}, log, dispatchDepth)
	recv, err := github.NewReceiver(cfg, enabled, webhookSecret, disp, log)
	if err != nil {
		return err
	}
	return serve(newServer(*addr, newMux(recv)), disp, log)
}

// loadConfig reads the GitHub App configuration and the enabled
// repositories. The App's private key is not loaded: only the worker holds
// it, so a compromised server can at most ask for a workflow to start.
func loadConfig(githubPath, reposPath string) (github.Config, map[string]bool, error) {
	var cfg github.Config
	err := withFile(githubPath, func(f *os.File) (err error) {
		cfg, err = github.LoadConfig(f)
		return err
	})
	if err != nil {
		return github.Config{}, nil, err
	}
	var access map[string]llm.RepoAccess
	err = withFile(reposPath, func(f *os.File) (err error) {
		access, err = llm.LoadRepoAccess(f)
		return err
	})
	if err != nil {
		return github.Config{}, nil, err
	}
	enabled := make(map[string]bool, len(access))
	for name := range access {
		enabled[name] = true
	}
	return cfg, enabled, nil
}

func withFile(path string, use func(*os.File) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := use(f); err != nil {
		return fmt.Errorf("loading %s: %w", path, err)
	}
	return nil
}

// newMux routes the webhook and a health check. Every other path and
// method is refused by the mux before reaching the receiver.
func newMux(webhook http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("POST /webhooks/github", webhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// newServer bounds every phase of a request, so a slow or idle client
// cannot hold a connection open: the server faces the internet, and its
// one handler answers in microseconds.
func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

// serve runs srv until SIGINT or SIGTERM, then shuts down in order: stop
// accepting deliveries, let in-flight ones finish, then drain the dispatch
// queue. The dispatcher runs on its own context, so the signal does not
// abandon what is queued.
func serve(srv *http.Server, disp *github.Dispatcher, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go disp.Run(context.Background())

	listenErr := make(chan error, 1)
	go func() { listenErr <- srv.ListenAndServe() }()
	log.Info("conclave server listening", "addr", srv.Addr)

	var err error
	select {
	case <-ctx.Done():
	case err = <-listenErr:
		err = fmt.Errorf("listening on %s: %w", srv.Addr, err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil && !errors.Is(shutdownErr, http.ErrServerClosed) {
		err = errors.Join(err, fmt.Errorf("shutting down: %w", shutdownErr))
	}
	disp.Close(drainGrace)
	log.Info("conclave server stopped")
	return err
}

