package main

import (
	"broto-api/internal/api"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		client := http.Client{Timeout: 2 * time.Second}
		r, e := client.Get("http://127.0.0.1:8080/livez")
		if e != nil {
			os.Exit(1)
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		migration, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		if e := api.MigrateDatabase(migration); e != nil {
			slog.Error("database migration failed; inspect configuration without logging credentials")
			os.Exit(1)
		}
		return
	}
	if e := run(ctx); e != nil {
		slog.Error("server stopped", "error", e)
		os.Exit(1)
	}
}
func run(ctx context.Context) error {
	c, e := api.LoadConfig()
	if e != nil {
		return e
	}
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	a, e := api.New(startup, c)
	cancel()
	if e != nil {
		return errors.New("startup failed; verify configuration and database migrations")
	}
	defer a.DB.Close()
	l, e := net.Listen("tcp", c.Address)
	if e != nil {
		return e
	}
	srv := &http.Server{Handler: a.Routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 180 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	slog.Info("broto-api ready", "address", c.Address)
	return serve(ctx, srv, l, a.RunJobs, 90*time.Second)
}

// Wait for handlers and workers before closing the pool. ECS stopTimeout is 120s.
func serve(ctx context.Context, srv *http.Server, l net.Listener, jobs func(context.Context), grace time.Duration) error {
	workers, cancel := context.WithCancel(ctx)
	defer cancel()
	jobsDone := make(chan struct{})
	go func() { defer close(jobsDone); jobs(workers) }()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	var result error
	select {
	case <-ctx.Done():
	case result = <-served:
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), grace)
	defer stop()
	if e := srv.Shutdown(shutdown); e != nil {
		_ = srv.Close()
		result = e
	}
	select {
	case <-jobsDone:
	case <-shutdown.Done():
		result = shutdown.Err()
	}
	if errors.Is(result, http.ErrServerClosed) {
		return nil
	}
	return result
}
