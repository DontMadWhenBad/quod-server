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
		envFile     = flag.String("env", ".env", "dotenv file (DEEPSEEK_API_KEY, API_TOKEN)")
		workers     = flag.Int("workers", 1, "Lean REPL workers (each ~7GB RAM)")
		maxRunning  = flag.Int("max-running", 2, "jobs running at once")
		maxQueued   = flag.Int("max-queued", 32, "jobs waiting before 429")
		model       = flag.String("model", llm.ModelFlash, "prover model")
		effort      = flag.String("effort", "high", "prover reasoning effort: low | high | max")
		jobBudget   = flag.Float64("job-budget", 0.25, "default USD budget per job")
		maxBudget   = flag.Float64("max-job-budget", 1.00, "max USD budget a client may request")
		dailyBudget = flag.Float64("daily-budget", 5.00, "USD cap across all jobs per UTC day (<0 = unlimited)")
		jobTimeout  = flag.Duration("job-timeout", 30*time.Minute, "max duration of one job")
		grace       = flag.Duration("grace", 30*time.Second, "on shutdown, wait this long for running jobs")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	if err := dotenv.Load(*envFile); err != nil {
		fatal(log, "loading env file", err)
	}
	token := strings.TrimSpace(os.Getenv("API_TOKEN"))
	if token == "" && !isLoopback(*addr) {
		log.Warn("listening on a non-loopback address without API_TOKEN: anyone who can reach it can spend your LLM budget")
	}

	client, err := llm.NewClientFromEnv()
	if err != nil {
		fatal(log, "llm client", err)
	}
	models := llm.DefaultModels()
	models.Prove = llm.ModelSpec{Model: *model, Effort: llm.Effort(*effort)}

	// Listen first so /healthz answers while Lean loads; job endpoints
	// return 503 until the pool is ready.
	srv := &server.Server{Token: token, Log: log}
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
	log.Info("listening", "addr", ln.Addr().String(), "auth", token != "")
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal(log, "http server", err)
		}
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting Lean workers", "workers", *workers)
	t0 := time.Now()
	pool, err := lean.NewPool(sigCtx, lean.PoolConfig{
		Config:      lean.Config{ProjectDir: *leanDir},
		Size:        *workers,
		MaxCommands: 500,
		Logger:      log,
	})
	if err != nil {
		fatal(log, "lean pool", err)
	}
	log.Info("Lean ready", "startup", time.Since(t0).Round(100*time.Millisecond))

	mgr := jobs.NewManager(jobs.Deps{
		Chatter:     client,
		Checker:     pipeline.PoolChecker{Pool: pool},
		Models:      models,
		LeanVersion: leanVersion(*leanDir),
	}, jobs.Config{
		MaxRunning:       *maxRunning,
		MaxQueued:        *maxQueued,
		DefaultBudgetUSD: *jobBudget,
		MaxBudgetUSD:     *maxBudget,
		DailyBudgetUSD:   *dailyBudget,
		JobTimeout:       *jobTimeout,
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
