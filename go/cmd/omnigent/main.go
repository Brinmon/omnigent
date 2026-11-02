// Command omnigent is the unified entry point for the Go build.
//
// P1 provides `serve`: an HTTP server that answers the read surface the web
// client needs to boot. The runner and harness subcommands arrive with P3, and
// until they do `serve` is enough to compare the Go build against the Python
// one on the endpoints that already exist.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Brinmon/omnigent/go/internal/server"
)

// version is set at build time by goreleaser; the default matches the Python
// package version so /v1/info compares cleanly during the rewrite.
var version = "0.21.0.dev0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := runServe(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "omnigent serve: %v\n", err)
			os.Exit(1)
		}
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage: omnigent <command> [options]

Commands:
  serve     Start the HTTP server
  version   Print the version

Run "omnigent serve --help" for server options.
`)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	host := fs.String("host", "127.0.0.1", "address to bind")
	port := fs.Int("port", 8080, "port to bind")
	dbURI := fs.String("database-uri", "", "database URI (default: $OMNIGENT_DATABASE_URI or the local chat.db)")
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn, error")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := newLogger(*logLevel)
	slog.SetDefault(logger)

	uri := *dbURI
	if uri == "" {
		uri = os.Getenv("OMNIGENT_DATABASE_URI")
	}
	if uri == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory for the default database: %w", err)
		}
		uri = "sqlite:///" + filepath.Join(home, ".omnigent", "chat.db")
	}

	dbPath, err := sqlitePath(uri)
	if err != nil {
		return err
	}
	// Create the parent directory so a first run against the default location
	// succeeds rather than failing on a missing .omnigent/.
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create database directory %s: %w", dir, err)
		}
	}

	conn, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		return fmt.Errorf("open database %s: %w", dbPath, err)
	}
	defer func() { _ = conn.Close() }()

	// Serialise writers: SQLite allows one at a time, and the Python side sets
	// the same pragma. WAL keeps readers unblocked.
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := conn.Exec(pragma); err != nil {
			return fmt.Errorf("apply %s: %w", pragma, err)
		}
	}
	if err := conn.Ping(); err != nil {
		return fmt.Errorf("ping database %s: %w", dbPath, err)
	}

	addr := net.JoinHostPort(*host, fmt.Sprint(*port))
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           server.New(conn, version).Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Listen before announcing so a bind failure is a clean error rather than a
	// "started" line followed by a crash.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	scheme := "http"
	logger.Info("omnigent server listening",
		"url", scheme+"://"+addr,
		"database", dbPath,
		"version", version,
	)

	// Shut down on SIGINT/SIGTERM so a Ctrl-C leaves no orphaned socket.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if serveErr := httpServer.Serve(listener); serveErr != nil &&
			!errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// sqlitePath extracts a filesystem path from a sqlite URI.
//
// Only SQLite is supported in P1; the Python build accepts more backends, and
// saying so plainly here beats a confusing open error later.
func sqlitePath(uri string) (string, error) {
	const prefix = "sqlite:///"
	if !strings.HasPrefix(uri, prefix) {
		if strings.HasPrefix(uri, "sqlite:") {
			return "", fmt.Errorf("unsupported sqlite URI %q; expected %s<path>", uri, prefix)
		}
		return "", fmt.Errorf("this build supports only sqlite:/// URIs, got %q", uri)
	}
	path := strings.TrimPrefix(uri, prefix)
	if path == "" {
		return "", errors.New("sqlite URI has an empty path")
	}
	// sqlite:///C:/x.db and sqlite:////abs/path both yield a usable path once the
	// prefix is removed; on POSIX the result keeps its leading slash.
	return path, nil
}

// newLogger builds the structured logger. JSON in production would need a flag;
// text is what a developer reads during the rewrite, and the lines are already
// key=value so a switch is mechanical.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
