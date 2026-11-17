// Command prove runs the full pipeline on natural-language input from the
// command line, printing progress to stderr and the result to stdout.
//
//	go run ./cmd/prove "For all real x, x^2 - 2x + 1 >= 0."
//	echo "..." | go run ./cmd/prove -confirm=false
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"mathprover/internal/dotenv"
	"mathprover/internal/lean"
	"mathprover/internal/llm"
	"mathprover/internal/pipeline"
)

func main() {
	var (
		leanDir   = flag.String("lean-dir", "lean_env", "Lake project with Mathlib (see scripts/bootstrap.sh)")
		envFile   = flag.String("env", ".env", "dotenv file with DEEPSEEK_API_KEY")
		workers   = flag.Int("workers", 1, "Lean REPL workers (each ~7GB RAM)")
		model     = flag.String("model", llm.ModelFlash, "prover model: deepseek-flash | deepseek-v4-pro")
		effort    = flag.String("effort", "high", "prover reasoning effort: low | high | max")
		attempts  = flag.Int("attempts", 6, "proof attempts per session")
		parallel  = flag.Int("parallel", 1, "parallel proof sessions per statement")
		budget    = flag.Float64("budget", 0.50, "max USD to spend (0 = unlimited)")
		timeout   = flag.Duration("timeout", 30*time.Minute, "overall time limit")
		confirm   = flag.Bool("confirm", false, "ask before proving each formalized statement")
		noExplain = flag.Bool("no-explain", false, "skip plain-language explanations")
		noSplit   = flag.Bool("no-split", false, "treat input as a single statement")
		asJSON    = flag.Bool("json", false, "print the result as JSON")
		verbose   = flag.Bool("v", false, "print Lean feedback for failed attempts")
		interact  = flag.Bool("i", false, "interactive: keep Lean loaded and prompt for statements repeatedly")
	)
	flag.Parse()

	if err := dotenv.Load(*envFile); err != nil {
		fatal("loading %s: %v", *envFile, err)
	}
	var (
		input     string
		fromStdin bool
		stdin     = bufio.NewReader(os.Stdin)
	)
	if *interact {
		input = strings.Join(flag.Args(), " ") // optional first job
	} else {
		input, fromStdin = readInput()
		if strings.TrimSpace(input) == "" {
			fatal("no input: pass statements as arguments or on stdin (or use -i)")
		}
	}

	// Startup can be interrupted with Ctrl-C.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client, err := llm.NewClientFromEnv()
	if err != nil {
		fatal("%v", err)
	}
	models := llm.DefaultModels()
	models.Prove = llm.ModelSpec{Model: *model, Effort: llm.Effort(*effort)}
	leanVersion := "Lean 4 + Mathlib"
	if tc, err := os.ReadFile(filepath.Join(*leanDir, "lean-toolchain")); err == nil {
		v := strings.TrimSpace(string(tc))
		v = v[strings.LastIndex(v, ":")+1:]
		leanVersion = fmt.Sprintf("Lean %s, Mathlib %s", v, v)
	}
	agent := llm.NewAgent(client, llm.Options{Models: models, LeanVersion: leanVersion, BudgetUSD: *budget})

	progress("starting %d Lean worker(s) (imports Mathlib)...", *workers)
	t0 := time.Now()
	pool, err := lean.NewPool(ctx, lean.PoolConfig{
		Config:      lean.Config{ProjectDir: *leanDir},
		Size:        *workers,
		MaxCommands: 500,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		fatal("lean: %v", err)
	}
	defer pool.Close()
	progress("Lean ready in %s", time.Since(t0).Round(100*time.Millisecond))

	p := pipeline.New(agent, pipeline.PoolChecker{Pool: pool}, pipeline.Options{
		MaxProofAttempts: *attempts,
		ParallelSessions: *parallel,
		SkipSplit:        *noSplit,
		NoExplain:        *noExplain,
	})
	hooks := pipeline.Hooks{OnEvent: func(e pipeline.Event) { printEvent(e, *verbose) }}
	if *confirm {
		hooks.Confirm = terminalConfirm(stdin, fromStdin)
	}
	stop() // from here on, each job installs its own Ctrl-C handler

	// runJob runs one input; Ctrl-C cancels just this job.
	runJob := func(input string) error {
		jctx, jstop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer jstop()
		jctx, cancel := context.WithTimeout(jctx, *timeout)
		defer cancel()
		res, err := p.Run(jctx, input, hooks)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(res)
		} else {
			printResult(res)
		}
		return err
	}

	if !*interact {
		if err := runJob(input); err != nil {
			fatal("%v", err)
		}
		return
	}

	fmt.Fprintln(os.Stderr, "Interactive mode. One input per line (end a line with \\ to continue it).")
	fmt.Fprintln(os.Stderr, "Ctrl-C cancels the running job; :q or Ctrl-D quits.")
	for {
		if strings.TrimSpace(input) != "" {
			if err := runJob(input); err != nil {
				progress("job ended: %v", err)
			}
			usd, calls := agent.Spent()
			progress("session total: $%.4f over %d LLM calls (budget $%.2f)", usd, calls, *budget)
		}
		var ok bool
		input, ok = promptInput(stdin)
		if !ok {
			fmt.Fprintln(os.Stderr)
			return
		}
	}
}

// promptInput reads one (possibly \-continued) input; ok=false on EOF or :q.
func promptInput(rd *bufio.Reader) (string, bool) {
	var parts []string
	prompt := "\nprove> "
	for {
		fmt.Fprint(os.Stderr, prompt)
		line, err := rd.ReadString('\n')
		if err != nil && line == "" {
			return "", false
		}
		line = strings.TrimRight(line, "\r\n")
		if len(parts) == 0 {
			switch strings.TrimSpace(line) {
			case ":q", ":quit", "exit", "quit":
				return "", false
			}
		}
		if cont, found := strings.CutSuffix(line, "\\"); found {
			parts = append(parts, cont)
			prompt = "  ...> "
			continue
		}
		parts = append(parts, line)
		return strings.Join(parts, "\n"), true
	}
}

func readInput() (string, bool) {
	if flag.NArg() > 0 {
		return strings.Join(flag.Args(), " "), false
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		fatal("reading stdin: %v", err)
	}
	return string(b), true
}

var start = time.Now()

func progress(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%6.1fs] %s\n", time.Since(start).Seconds(), fmt.Sprintf(format, args...))
}

func printEvent(e pipeline.Event, verbose bool) {
	tag := ""
	if e.Stmt > 0 {
		tag = fmt.Sprintf("stmt_%d: ", e.Stmt)
	}
	switch e.Type {
	case pipeline.EvFormalized:
		s := e.Statement
		progress("%sformalized as  theorem %s %s", tag, s.Name, oneLine(s.Signature))
		progress("%s  reads back as: %s", tag, oneLine(s.BackTranslation))
	case pipeline.EvStatementError:
		progress("%sLean rejected statement: %s", tag, firstLine(e.Message))
	case pipeline.EvAttempt:
		a := e.Attempt
		status := "✗ " + strings.Join(a.ErrorKinds, ", ")
		if a.Verified {
			status = "✓ verified"
		}
		progress("%sattempt %d.%d %s  (llm %s, lean %s, $%.4f)", tag, a.Session, a.N, status,
			a.LLMTime.Round(100*time.Millisecond), a.LeanTime.Round(time.Millisecond), a.CostUSD)
		if verbose && !a.Verified && a.Feedback != "" {
			fmt.Fprintln(os.Stderr, indent(a.Feedback, "           │ "))
		}
	case pipeline.EvProved:
		progress("%sPROVED", tag)
	case pipeline.EvFailed:
		progress("%sFAILED: %s", tag, e.Statement.Error)
	case pipeline.EvExplained:
		// shown in the final report
	default:
		msg := e.Message
		if msg != "" {
			msg = ": " + firstLine(msg)
		}
		progress("%s%s%s", tag, e.Type, msg)
	}
}

func printResult(res *pipeline.Result) {
	if res == nil {
		return
	}
	fmt.Println()
	for _, s := range res.Statements {
		fmt.Printf("── %s [%s] ──\n%s\n", s.Name, s.Status, s.Text)
		if s.Signature != "" {
			fmt.Printf("\nLean:  theorem %s %s\nMeans: %s\n", s.Name, oneLine(s.Signature), oneLine(s.BackTranslation))
		}
		if s.Proof != "" {
			fmt.Printf("\nProof:\n%s\n", indent(s.Proof, "  "))
		}
		if s.Explanation != "" {
			fmt.Printf("\nExplanation:\n%s\n", indent(s.Explanation, "  "))
		}
		if s.Error != "" && s.Status != pipeline.StatusProved {
			fmt.Printf("\nError: %s\n", s.Error)
		}
		fmt.Println()
	}
	fmt.Printf("── Lean file ──\n%s\n", res.LeanFile)
	fmt.Printf("%s, $%.4f over %d LLM calls\n", res.Duration.Round(100*time.Millisecond), res.CostUSD, res.LLMCalls)
}

// terminalConfirm asks on the terminal, sharing the prompt's reader. If
// stdin carried the input, it reads answers from /dev/tty instead.
func terminalConfirm(stdin *bufio.Reader, stdinUsed bool) pipeline.ConfirmFunc {
	rd := stdin
	if stdinUsed {
		tty, err := os.Open("/dev/tty")
		if err != nil {
			fatal("-confirm needs a terminal: %v", err)
		}
		rd = bufio.NewReader(tty)
	}
	return func(ctx context.Context, p pipeline.Proposal) (pipeline.Decision, error) {
		fmt.Fprintf(os.Stderr, "\n  %s: %s\n  Lean:  theorem %s %s\n  Means: %s\n", p.Name, oneLine(p.Text), p.Name, oneLine(p.Signature), oneLine(p.BackTranslation))
		if p.Notes != "" {
			fmt.Fprintf(os.Stderr, "  Notes: %s\n", indent(p.Notes, "         ")[9:])
		}
		fmt.Fprint(os.Stderr, "  Prove this? [Enter=yes, n=skip, or type a correction]: ")
		line, err := rd.ReadString('\n')
		if err != nil && line == "" {
			return pipeline.Decision{}, err
		}
		switch ans := strings.TrimSpace(line); strings.ToLower(ans) {
		case "", "y", "yes":
			return pipeline.Decision{Accept: true}, nil
		case "n", "no":
			return pipeline.Decision{}, nil
		default:
			return pipeline.Decision{Correction: ans}, nil
		}
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

func indent(s, p string) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range ls {
		ls[i] = p + ls[i]
	}
	return strings.Join(ls, "\n")
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "prove: "+format+"\n", args...)
	os.Exit(1)
}
