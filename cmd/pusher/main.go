// Binary pusher serves the Sierra→Follow Up Boss tag-sync webhook.
//
// Configuration is taken from the environment:
//
//	SIERRA_API_KEY  (required) Sierra API key
//	FUB_API_KEY     (required) Follow Up Boss API key
//	PORT            HTTP listen port (default 8080, Cloud Run injects this)
//	LOG_LEVEL       slog level: debug|info|warn|error (default info)
//
// API base URLs for both Sierra and FUB are compiled-in constants — they
// have never changed and live in the respective client packages.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/fub"
	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/handler"
	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/logging"
	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/retry"
	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/sierra"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	logger := logging.New(parseLogLevel(os.Getenv("LOG_LEVEL")))

	sierraKey, err := requiredEnv("SIERRA_API_KEY")
	if err != nil {
		return err
	}
	fubKey, err := requiredEnv("FUB_API_KEY")
	if err != nil {
		return err
	}
	port := envOr("PORT", "8080")

	httpClient := &http.Client{Timeout: 5 * time.Second}
	retrier := retry.New(httpClient)

	h := &handler.Handler{
		Sierra: sierra.New(sierraKey, retrier),
		FUB:    fub.New(fubKey, retrier),
		Logger: logger,
	}

	mux := http.NewServeMux()
	h.Routes(mux)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown_signal")
	case err := <-errCh:
		if err != nil {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func requiredEnv(k string) (string, error) {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return "", fmt.Errorf("missing required env %s", k)
	}
	return v, nil
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
