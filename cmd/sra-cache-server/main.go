// Command sra-cache-server is the SRACache control-plane (JSONRPC, later
// phases) and data-plane (plain HTTP, later phases) service. Phase 1 of the
// staged implementation (see PLAN-sra-cache-service-implementation.md in the
// dev_container modules tree) only proves the skeleton: config loading, the
// dual-listener/graceful-shutdown plumbing everything else is built on, and
// a /ping endpoint on the control-plane listener.
//
// Config comes from the same deployment INI the rest of this codebase's
// services read:
//
//	sra-cache-server --config /kb/deployment/deployment.cfg
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/BV-BRC/BV-BRC-Go-SDK/sra"
	"github.com/BV-BRC/sra_cache_service/internal/config"
	"github.com/BV-BRC/sra_cache_service/internal/jsonrpc"
	"github.com/BV-BRC/sra_cache_service/internal/metadata"
	"github.com/BV-BRC/sra_cache_service/internal/rpcapi"
)

// version is stamped at build time with -X main.version=... A future phase
// wires this up to BV-BRC-Go-SDK/version's stamping machinery; for now a
// literal default is enough to make --version meaningful.
var version = "dev"

func main() {
	var (
		configPath = pflag.StringP("config", "c", "", "deployment INI to read (defaults to $KB_DEPLOYMENT_CONFIG)")
		section    = pflag.String("section", "", "INI section to read (defaults to $KB_SERVICE_NAME, else SRACache)")
		listen     = pflag.StringP("listen", "l", ":7141", "control-plane (JSONRPC) address to listen on")
		dataListen = pflag.String("data-listen", ":7142", "data-plane (HTTP) address to listen on")
		logLevel   = pflag.String("log-level", "info", "debug, info, warn, or error")
		pidFile    = pflag.String("pid-file", "", "write the process id to this file at startup (removed on clean shutdown)")
		showVer    = pflag.BoolP("version", "v", false, "print the version and exit")
	)

	pflag.Usage = func() {
		fmt.Fprintf(os.Stderr, "sra-cache-server - BV-BRC SRA cache service\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n  sra-cache-server [options]\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  sra-cache-server --config /kb/deployment/deployment.cfg\n")
		fmt.Fprintf(os.Stderr, "  sra-cache-server -c ./test.cfg -l :7141 --data-listen :7142 --log-level debug\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		pflag.PrintDefaults()
	}
	pflag.Parse()

	if *showVer {
		fmt.Printf("sra-cache-server %s\n", version)
		return
	}

	if *configPath == "" {
		*configPath = os.Getenv("KB_DEPLOYMENT_CONFIG")
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "sra-cache-server: no config given: pass --config or set KB_DEPLOYMENT_CONFIG")
		os.Exit(1)
	}
	if *section == "" {
		*section = os.Getenv("KB_SERVICE_NAME")
	}

	// Config-file values fill in whatever the corresponding flag wasn't
	// explicitly given, mirroring the precedence deploy.cfg / CLI flags have
	// everywhere else in this tree: an explicit flag always wins.
	cfg, err := config.Load(*configPath, *section)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sra-cache-server: loading config: %s\n", err)
		os.Exit(1)
	}
	if !pflag.CommandLine.Changed("listen") && cfg.Listen != "" {
		*listen = cfg.Listen
	}
	if !pflag.CommandLine.Changed("data-listen") && cfg.DataListen != "" {
		*dataListen = cfg.DataListen
	}
	if !pflag.CommandLine.Changed("log-level") && cfg.LogLevel != "" {
		*logLevel = cfg.LogLevel
	}

	log := newLogger(*logLevel)
	log.Info("config loaded", "path", *configPath, "section", sectionOrDefault(*section),
		"listen", *listen, "data_listen", *dataListen)

	if *pidFile != "" {
		if err := os.WriteFile(*pidFile, fmt.Appendf(nil, "%d\n", os.Getpid()), 0o644); err != nil {
			fatal(log, "writing pid file", "path", *pidFile, "err", err)
		}
		defer os.Remove(*pidFile)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// TODO(Phase 3+ / after BV-BRC-Go-SDK p3auth PR merges): replace
	// NoopValidator with a real p3auth.Validator once this module's go.mod
	// replace directive points at a BV-BRC-Go-SDK checkout that has the
	// promoted p3auth package on disk (today it points at
	// ../BV-BRC-Go-SDK-sra-prereqs, a worktree on a branch that predates
	// that promotion -- see go.mod's own comment). Until then every token
	// is rejected, which still exercises AuthRequired's and AuthOptional's
	// real gating logic correctly (see internal/jsonrpc/auth.go) -- only
	// "a valid token was presented" is unreachable.
	rpcServer := jsonrpc.NewServer(NoopValidator{}, log)

	// Phase 4: SRACache.get_metadata. Stateless -- sra.New() is all the
	// setup it needs, constructed the same way BV-BRC-Go-SDK/sra's own
	// tests do; it defaults its NCBI API key from $NCBI_API_KEY and its
	// User-Agent from BV-BRC-Go-SDK/version. Phases 6/7 register
	// start_precache / precache_status / get_sra_data here, once the
	// catalog/cache/fetcher pieces those need exist.
	svc := &rpcapi.Service{Metadata: metadata.New(sra.New())}
	svc.RegisterGetMetadata(rpcServer)

	// The data-plane listener serves nothing yet (Phase 7 adds
	// /sra/{key}/{accession}.sra); it exists at this phase purely to prove
	// the dual-server/dual-port startup and shutdown plumbing that later
	// phases build on.
	dataMux := http.NewServeMux()

	mainSrv := &http.Server{
		Addr: *listen,
		// jsonrpc.Server serves /ping and /auth_ping itself (see
		// internal/jsonrpc/ping.go) in addition to dispatching RPC calls,
		// replacing Phase 1's ad-hoc mux+handlePing wiring. handlePing
		// below is kept only so its pre-existing test keeps compiling and
		// passing unchanged; it now just delegates to jsonrpc.Ping.
		Handler:           rpcServer,
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	dataSrv := &http.Server{
		Addr:    *dataListen,
		Handler: dataMux,
		// No WriteTimeout: the data plane will stream large .sra files in
		// later phases, and a deadline here would truncate a long transfer.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErrs := make(chan error, 2)
	go func() {
		log.Info("control plane listening", "addr", *listen, "version", version)
		if err := mainSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrs <- fmt.Errorf("control plane: %w", err)
			return
		}
		serveErrs <- nil
	}()
	go func() {
		log.Info("data plane listening", "addr", *dataListen, "version", version)
		if err := dataSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrs <- fmt.Errorf("data plane: %w", err)
			return
		}
		serveErrs <- nil
	}()

	select {
	case <-ctx.Done():
		log.Info("signal received; shutting down")
	case err := <-serveErrs:
		if err != nil {
			fatal(log, "serving", "err", err)
		}
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := mainSrv.Shutdown(shutCtx); err != nil {
			log.Warn("control plane shutdown did not complete cleanly", "err", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := dataSrv.Shutdown(shutCtx); err != nil {
			log.Warn("data plane shutdown did not complete cleanly", "err", err)
		}
	}()
	wg.Wait()

	log.Info("stopped")
}

// handlePing is Phase 1's original /ping handler. Production traffic no
// longer reaches it directly -- mainSrv.Handler is now the jsonrpc.Server
// built in main, which serves /ping via jsonrpc.Ping -- but it's kept,
// delegating to that same function, so main_test.go's pre-existing
// TestHandlePing/TestHandlePingHead/TestHandlePingRejectsOtherMethods
// keep compiling and passing unchanged.
func handlePing(w http.ResponseWriter, r *http.Request) {
	jsonrpc.Ping(w, r)
}

// NoopValidator is a placeholder jsonrpc.Validator used until the real
// p3auth-backed one is available (see the TODO at rpcServer's
// construction site in main, above). It rejects every token. That's
// sufficient to exercise both AuthRequired's and AuthOptional's real code
// paths -- a rejected token behaves, from the dispatcher's perspective,
// exactly like an untrusted or expired caller's -- while being honest
// that no real validation is wired up yet: nothing can ever successfully
// authenticate against this server until it's replaced.
type NoopValidator struct{}

// Validate implements jsonrpc.Validator.
func (NoopValidator) Validate(token string) (string, error) {
	return "", errors.New("token validation not yet configured")
}

func sectionOrDefault(section string) string {
	if section == "" {
		return config.DefaultSection
	}
	return section
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func fatal(log *slog.Logger, msg string, args ...any) {
	log.Error(msg, args...)
	os.Exit(1)
}
