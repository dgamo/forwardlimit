// SPDX-License-Identifier: Apache-2.0

// Command forwardlimit answers Traefik ForwardAuth calls with 200 (allow) or the
// configured rejection, counting in Redis so limits hold across replicas.
//
// Limiters come from a YAML file, infrastructure and secrets from the environment.
// See README.md and docs/ for the reference and the rationale.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dgamo/forwardlimit/internal/config"
	"github.com/dgamo/forwardlimit/internal/httpapi"
	"github.com/dgamo/forwardlimit/internal/limiter"
	"github.com/dgamo/forwardlimit/internal/observability"
	"github.com/dgamo/forwardlimit/internal/store"
	"github.com/dgamo/forwardlimit/internal/store/blockcache"
	"github.com/dgamo/forwardlimit/internal/store/breaker"
	"github.com/dgamo/forwardlimit/internal/store/redisstore"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		// The logger may not exist yet, so report configuration failures plainly.
		fmt.Fprintf(os.Stderr, "forwardlimit: %v\n", err)
		os.Exit(1)
	}
}

// flags holds what the command line asked for.
type flags struct {
	configPath  string
	showVersion bool
	healthcheck bool
	validate    bool
}

func parseFlags(argv []string, defaultConfig string) (flags, error) {
	var f flags

	fs := flag.NewFlagSet("forwardlimit", flag.ContinueOnError)
	// Defaults to whatever the environment resolved, so it overrides CONFIG_PATH
	// when given and is invisible when not.
	fs.StringVar(&f.configPath, "config", defaultConfig, "path to the limiter configuration file")
	fs.BoolVar(&f.showVersion, "version", false, "print the version and exit")
	fs.BoolVar(&f.healthcheck, "healthcheck", false,
		"probe a running instance's /healthz and exit 0 if it is healthy")
	fs.BoolVar(&f.validate, "validate", false,
		"check the configuration and exit; non-zero if it would not start")

	return f, fs.Parse(argv)
}

func run(argv []string) error {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}

	f, err := parseFlags(argv, cfg.ConfigPath)
	if err != nil {
		return err
	}
	switch {
	case f.showVersion:
		fmt.Println(version)
		return nil
	case f.healthcheck:
		return probeHealth(cfg.ListenAddr)
	}
	cfg.ConfigPath = f.configPath

	file, err := config.LoadFile(cfg.ConfigPath)
	if err != nil {
		return err
	}
	built, err := file.Build(cfg.HashSecret)
	if err != nil {
		return fmt.Errorf("%s: %w", cfg.ConfigPath, err)
	}

	// A pre-flight check that exercises the whole start-up path: parse, validate,
	// build every keyer.
	if f.validate {
		return report(cfg, built)
	}

	log, err := observability.NewLogger(os.Stdout, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		return err
	}
	log = log.With(slog.String("service", "forwardlimit"), slog.String("version", version))

	metrics := observability.NewMetrics(cfg.MetricsNamespace)

	st, bc, br := buildStore(cfg, log)
	defer func() {
		if cerr := st.Close(); cerr != nil {
			log.Warn("closing store", slog.String("error", cerr.Error()))
		}
	}()

	log.Info("starting",
		slog.String("listen", cfg.ListenAddr),
		slog.String("config", cfg.ConfigPath),
		slog.Any("limiters", built.Names()),
		slog.Any("dry_run", built.DryRunNames()),
		slog.Bool("store_enabled", cfg.Redis.Enabled),
		slog.Bool("block_cache", cfg.BlockCache.Enabled),
		slog.Int64("max_body_bytes", cfg.MaxBodyBytes))

	// Warnings describe configurations that are valid but will not limit
	// anything. Each is a fail-open path, so it must be loud at startup.
	for _, w := range append(cfg.Warnings(), built.Warnings...) {
		log.Warn("configuration warning", slog.String("detail", w))
	}

	handler := httpapi.New(httpapi.Options{
		Engine:       limiter.NewEngine(st, log, built.Limiters...),
		Metrics:      metrics,
		Logger:       log,
		MaxBodyBytes: cfg.MaxBodyBytes,
		Response:     built.Default,
		Responses:    built.Responses,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go publishStoreMetrics(ctx, metrics, bc, br)

	return serve(ctx, cfg, handler, log)
}

// serve listens until the context is cancelled, then drains.
func serve(ctx context.Context, cfg *config.Config, handler http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if lerr := srv.ListenAndServe(); lerr != nil && !errors.Is(lerr, http.ErrServerClosed) {
			errCh <- lerr
		}
	}()

	select {
	case lerr := <-errCh:
		return fmt.Errorf("http server: %w", lerr)
	case <-ctx.Done():
		log.Info("shutdown signal received", slog.Duration("timeout", cfg.ShutdownTimeout))
	}

	// Finish in-flight checks first. As a native sidecar this container stops only
	// after the proxy, so the proxy's drain period is already over by now.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	log.Info("stopped")
	return nil
}

// report prints what the configuration resolved to, for -validate.
//
// Warnings go to stderr and are not fatal - a limiter left disabled is the
// operator's call, not a syntax error - so this is usable in a pipeline.
func report(cfg *config.Config, built *config.Built) error {
	fmt.Printf("config:  %s\n", cfg.ConfigPath)
	fmt.Printf("valid:   yes\n")

	if len(built.Limiters) == 0 {
		fmt.Println("active:  none")
	} else {
		fmt.Printf("active:  %s\n", strings.Join(built.Names(), ", "))
	}
	if names := built.DryRunNames(); len(names) > 0 {
		fmt.Printf("dry run: %s\n", strings.Join(names, ", "))
	}

	for _, w := range append(cfg.Warnings(), built.Warnings...) {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	return nil
}

// probeHealth requests /healthz from an instance listening on addr.
//
// It exists because the published image is distroless: no shell, no curl, so a
// container healthcheck has nothing else to call. A wildcard host becomes 127.0.0.1,
// since the probe runs beside the server.
func probeHealth(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("healthcheck: cannot parse LISTEN_ADDR %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	url := "http://" + net.JoinHostPort(host, port) + "/healthz"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s returned %d", url, resp.StatusCode)
	}
	return nil
}

// buildStore assembles the store chain, outermost first:
//
//	blockcache -> breaker -> redisstore
//
// The cache is outermost so a known-blocked bucket costs nothing at all, and the
// breaker sits above Redis so an outage stops producing connection attempts.
func buildStore(cfg *config.Config, log *slog.Logger) (store.Store, *blockcache.Store, *breaker.Store) {
	if !cfg.Redis.Enabled {
		log.Warn("store disabled, no limiting will occur")
		return store.Noop{}, nil, nil
	}

	rs, err := redisstore.New(redisstore.Config{
		Addrs:         cfg.Redis.Addrs,
		Mode:          cfg.Redis.Mode,
		TLSEnabled:    cfg.Redis.TLSEnabled,
		TLSSkipVerify: cfg.Redis.TLSSkipVerify,
		TLSCAFile:     cfg.Redis.TLSCAFile,
		TLSCertFile:   cfg.Redis.TLSCertFile,
		TLSKeyFile:    cfg.Redis.TLSKeyFile,
		PoolSize:      cfg.Redis.PoolSize,
		MaxRetries:    cfg.Redis.MaxRetries,
		DialTimeout:   cfg.Redis.DialTimeout,
		ReadTimeout:   cfg.Redis.ReadTimeout,
		WriteTimeout:  cfg.Redis.WriteTimeout,
		KeyPrefix:     cfg.Redis.KeyPrefix,
	})
	if err != nil {
		// Misconfiguration rather than unreachability; fail open loudly instead
		// of crash-looping.
		log.Error("cannot build store, falling back to no limiting",
			slog.String("error", err.Error()))
		return store.Noop{}, nil, nil
	}

	// Connectivity is logged once, for diagnosis only. It never gates readiness:
	// see internal/httpapi for why.
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if perr := rs.Ping(pingCtx); perr != nil {
		log.Warn("store unreachable at startup; requests will fail open until it recovers",
			slog.String("error", perr.Error()))
	} else {
		log.Info("store connected",
			slog.String("mode", string(cfg.Redis.Mode)),
			slog.Bool("tls", cfg.Redis.TLSEnabled))
	}

	var st store.Store = rs

	var br *breaker.Store
	if cfg.Breaker.Threshold > 0 {
		br = breaker.New(st, breaker.Options{
			Threshold: cfg.Breaker.Threshold,
			Cooldown:  cfg.Breaker.Cooldown,
		})
		st = br
	}

	var bc *blockcache.Store
	if cfg.BlockCache.Enabled {
		bc = blockcache.New(st, blockcache.Options{
			MaxEntries: cfg.BlockCache.MaxEntries,
			MaxTTL:     cfg.BlockCache.MaxTTL,
		})
		st = bc
	}

	return st, bc, br
}

// publishStoreMetrics mirrors internal store state into gauges so a degraded
// backend is observable without affecting readiness.
func publishStoreMetrics(ctx context.Context, m *observability.Metrics, bc *blockcache.Store, br *breaker.Store) {
	publish := func() {
		if bc != nil {
			m.SetBlockCacheHits(bc.Hits())
		}
		if br != nil {
			m.SetBreakerOpen(br.Open())
		}
	}

	// Publish immediately so the gauges are never unset, then refresh often
	// enough that an open breaker is actionable for alerting.
	publish()

	t := time.NewTicker(2 * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			publish()
		}
	}
}
