// Command server runs the HTTP API: it loads Lean once, then serves jobs.
//
//	go run ./cmd/server -addr 127.0.0.1:8080
//	curl -s localhost:8080/v1/jobs -d '{"input":"For all real x, x^2 >= 0."}'
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mathprover/internal/auth"
	"mathprover/internal/dotenv"
	"mathprover/internal/jobs"
	"mathprover/internal/lean"
	"mathprover/internal/llm"
	"mathprover/internal/pipeline"
	"mathprover/internal/server"
)

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:8080", "listen address")
		leanDir     = flag.String("lean-dir", "lean_env", "Lake project with Mathlib")
		envFile     = flag.String("env", ".env", "dotenv file (DEEPSEEK_API_KEY)")
		authMode    = flag.String("auth", "keys", "client authentication: keys (server-issued access keys) | none (development only)")
		dbPath      = flag.String("db", "quod.db", "SQLite database with access keys (manage with: quod-admin keys ...)")
		workers     = flag.Int("workers", 2, "warm Lean REPL workers; each job leases one and it is discarded afterwards (~0.5-1GB private RAM each + ~5GB shared)")
		sandboxKind = flag.String("sandbox", "bwrap", "Lean process isolation: bwrap | none (none only for trusted input)")
		sandboxMem  = flag.String("sandbox-mem", "10G", "memory limit per Lean worker (systemd scope); empty = none")
		sandboxPids = flag.Int("sandbox-tasks", 256, "process/thread limit per Lean worker; 0 = none")
		maxRunning  = flag.Int("max-running", 2, "jobs running at once")
		maxQueued   = flag.Int("max-queued", 32, "jobs waiting before 429")
		model       = flag.String("model", llm.ModelFlash, "prover model")
		effort      = flag.String("effort", "", "single stage at this effort (none|low|high|max) instead of the fast→think→deep ladder")
		noAuto      = flag.Bool("no-auto", false, "skip trying automation tactics before the LLM")
		jobBudget   = flag.Float64("job-budget", 0.25, "default USD budget per job")
		maxBudget   = flag.Float64("max-job-budget", 1.00, "max USD budget a client may request")
		dailyBudget = flag.Float64("daily-budget", 5.00, "USD cap across all jobs per UTC day (<0 = unlimited)")
		jobTimeout  = flag.Duration("job-timeout", 30*time.Minute, "max duration of one job")
		grace       = flag.Duration("grace", 30*time.Second, "on shutdown, wait this long for running jobs")
		verify      = flag.Bool("verify", true, "independently re-check every proof in fresh sandboxes (kernel + axiom audit)")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	if err := dotenv.Load(*envFile); err != nil {
		fatal(log, "loading env file", err)
	}
	var authn auth.Authenticator
	switch *authMode {
	case "keys":
		store, err := auth.OpenKeyStore(*dbPath)
		if err != nil {
			fatal(log, "access keys", err)
		}
		defer store.Close()
		if n, err := store.ActiveKeys(); err != nil {
			fatal(log, "access keys", err)
		} else if n == 0 {
			log.Warn("no access keys yet: every API request will be rejected; create one with `quod-admin keys create --name NAME`", "db", *dbPath)
		}
		authn = store
	case "none":
		if !isLoopback(*addr) {
			log.Warn("-auth none on a non-loopback address: anyone who can reach it can spend your LLM budget")
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown -auth %q (want keys or none)\n", *authMode)
		os.Exit(2)
	}

	client, err := llm.NewClientFromEnv()
	if err != nil {
		fatal(log, "llm client", err)
	}
	models := llm.DefaultModels()
	models.Prove = llm.ModelSpec{Model: *model, Effort: llm.EffortHigh}
	stages := pipeline.DefaultStages(*model)
	if *effort != "" {
		stages = []pipeline.Stage{{Name: *effort, Spec: llm.ModelSpec{Model: *model, Effort: llm.Effort(*effort)}, Sessions: 1, Attempts: 6}}
	}

	// Listen first so /healthz answers while Lean loads; job endpoints
	// return 503 until the pool is ready.
	srv := &server.Server{Auth: authn, Log: log}
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: SSE streams are long-lived; handlers set
		// per-write deadlines instead.
	}
	baseCtx := server.ShutdownContext(httpSrv)
	httpSrv.BaseContext = func(net.Listener) context.Context { return baseCtx }

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fatal(log, "listen", err)
	}
	log.Info("listening", "addr", ln.Addr().String(), "auth", *authMode)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal(log, "http server", err)
		}
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting Lean workers", "workers", *workers, "sandbox", *sandboxKind)
	t0 := time.Now()
	sb := sandboxConfig(*sandboxKind, *sandboxMem, *sandboxPids)
	if sb.Kind == lean.SandboxNone {
		log.Warn("Lean sandbox disabled: model-written Lean can read files and secrets on this host")
	}
	pool, err := lean.NewPool(sigCtx, lean.PoolConfig{
		Config:      lean.Config{ProjectDir: *leanDir, Sandbox: sb},
		Size:        *workers,
		MaxCommands: 500,
		Logger:      log,
	})
	if err != nil {
		fatal(log, "lean pool", err)
	}
	log.Info("Lean ready", "startup", time.Since(t0).Round(100*time.Millisecond))

	var verifier pipeline.Verifier
	if *verify {
		v, err := lean.NewVerifier(sigCtx, lean.VerifierConfig{Config: lean.Config{ProjectDir: *leanDir, Sandbox: sb}})
		if err != nil {
			fatal(log, "verifier", err)
		}
		verifier = v
	} else {
		log.Warn("independent verification disabled: results rely on the in-job check only")
	}

	mgr := jobs.NewManager(jobs.Deps{
		Chatter:     client,
		Checkers:    pipeline.PoolSource{Pool: pool},
		Verifier:    verifier,
		Models:      models,
		LeanVersion: leanVersion(*leanDir),
	}, jobs.Config{
		MaxRunning:       *maxRunning,
		MaxQueued:        *maxQueued,
		DefaultBudgetUSD: *jobBudget,
		MaxBudgetUSD:     *maxBudget,
		DailyBudgetUSD:   *dailyBudget,
		JobTimeout:       *jobTimeout,
		Stages:           stages,
		NoAutomation:     *noAuto,
	})
	srv.SetManager(mgr)

	<-sigCtx.Done()
	stop() // a second Ctrl-C kills immediately
	log.Info("shutting down", "grace", *grace)

	gctx, cancel := context.WithTimeout(context.Background(), *grace)
	defer cancel()
	if err := mgr.Shutdown(gctx); err != nil {
		log.Warn("canceled unfinished jobs", "err", err)
	}
	hctx, hcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer hcancel()
	_ = httpSrv.Shutdown(hctx)
	pool.Close()
	log.Info("bye")
}

func sandboxConfig(kind, mem string, tasks int) lean.Sandbox {
	switch lean.SandboxKind(kind) {
	case lean.SandboxNone:
		return lean.Sandbox{Kind: lean.SandboxNone}
	case lean.SandboxBwrap:
		return lean.Sandbox{Kind: lean.SandboxBwrap, MemoryMax: mem, TasksMax: tasks}
	default:
		fmt.Fprintf(os.Stderr, "unknown -sandbox %q (want bwrap or none)\n", kind)
		os.Exit(2)
		return lean.Sandbox{}
	}
}

func leanVersion(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "lean-toolchain"))
	if err != nil {
		return "Lean 4 + Mathlib"
	}
	v := strings.TrimSpace(string(b))
	v = v[strings.LastIndex(v, ":")+1:]
	return fmt.Sprintf("Lean %s, Mathlib %s", v, v)
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func fatal(log *slog.Logger, what string, err error) {
	log.Error(what, "err", err)
	os.Exit(1)
}
