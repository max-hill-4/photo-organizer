package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/schollz/progressbar/v3"
)

// Config holds all runtime settings.
type Config struct {
	sourceDir  string
	destDir    string
	dryRun     bool
	workers    int
	bufSize    int // bytes
	logFile    string
	unknownDir string
	verbose    bool
	hashDedup  bool
}

func main() {
	var (
		dryRun     = flag.Bool("dry-run", false, "Print actions, no copying")
		workers    = flag.Int("workers", 4, "Parallel workers")
		bufSizeKB  = flag.Int("buf-size", 8192, "I/O buffer in KB")
		logFile    = flag.String("log-file", "", "Write JSON summary to file")
		unknownDir = flag.String("unknown-dir", "", "Dir for undatable files (default: <dest>/unknown)")
		verbose    = flag.Bool("verbose", false, "Log each file action")
		hashDedup  = flag.Bool("hash-dedup", false, "Use MD5 hash to detect duplicates across different filenames/sizes")
		dateTest   = flag.String("date-test", "", "Test date extraction on a single file and exit")
	)
	flag.Parse()

	// --date-test mode
	if *dateTest != "" {
		info, err := os.Lstat(*dateTest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		t, src := ExtractDate(*dateTest, info.ModTime())
		fmt.Printf("File:   %s\nDate:   %s\nSource: %s\n", *dateTest, t.Format("2006-01-02"), src)
		return
	}

	args := flag.Args()
	if len(args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: photo-organizer [flags] <source-dir> <dest-dir>\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	cfg := &Config{
		sourceDir:  args[0],
		destDir:    args[1],
		dryRun:     *dryRun,
		workers:    *workers,
		bufSize:    *bufSizeKB * 1024,
		logFile:    *logFile,
		unknownDir: *unknownDir,
		verbose:    *verbose,
		hashDedup:  *hashDedup,
	}
	if cfg.unknownDir == "" {
		cfg.unknownDir = cfg.destDir + "/unknown"
	}
	if cfg.workers < 1 {
		cfg.workers = 1
	}
	if cfg.workers > runtime.NumCPU()*4 {
		cfg.workers = runtime.NumCPU() * 4
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle SIGINT/SIGTERM for clean shutdown
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		fmt.Fprintln(os.Stderr, "\nInterrupt received, shutting down...")
		cancel()
	}()

	if err := run(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *Config) error {
	start := time.Now()

	jobs := make(chan Job, 1000)
	results := make(chan CopyResult, 1000)

	// Open errors log
	errLog, err := os.OpenFile("errors.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open errors.log: %w", err)
	}
	defer errLog.Close()
	errWriter := bufio.NewWriter(errLog)
	defer errWriter.Flush()

	// Start walker in background
	walkDone := make(chan error, 1)
	go func() {
		defer close(jobs)
		walkDone <- Walk(cfg.sourceDir, jobs)
	}()

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < cfg.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}
					results <- Process(cfg, job)
				}
			}
		}()
	}

	// Close results when all workers done
	go func() {
		wg.Wait()
		close(results)
	}()

	// Progress bar (indeterminate)
	bar := progressbar.NewOptions(-1,
		progressbar.OptionSetDescription("Processing"),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionThrottle(200*time.Millisecond),
		progressbar.OptionSpinnerType(14),
		progressbar.OptionSetRenderBlankState(true),
	)

	// Aggregate results
	stats := &Stats{}
	var jsonRecords []map[string]any

	for r := range results {
		stats.Record(r)
		bar.Add(1) //nolint

		if r.Err != nil {
			fmt.Fprintf(errWriter, "%s\t%v\n", r.Src, r.Err)
		}
		if cfg.verbose {
			fmt.Fprintf(os.Stderr, "[%s] %s → %s (%s)\n", r.Action, r.Src, r.Dst, r.DateSource)
		}
		if cfg.logFile != "" {
			rec := map[string]any{
				"src":    r.Src,
				"dst":    r.Dst,
				"action": r.Action,
				"date":   r.Date.Format("2006-01-02"),
				"source": r.DateSource.String(),
				"bytes":  r.Bytes,
			}
			if r.Err != nil {
				rec["error"] = r.Err.Error()
			}
			jsonRecords = append(jsonRecords, rec)
		}
	}
	bar.Finish() //nolint

	// Check walk error
	if walkErr := <-walkDone; walkErr != nil {
		fmt.Fprintf(os.Stderr, "walk error: %v\n", walkErr)
	}

	elapsed := time.Since(start)
	stats.PrintSummary(elapsed)

	// Write JSON log
	if cfg.logFile != "" {
		f, err := os.Create(cfg.logFile)
		if err != nil {
			return fmt.Errorf("create log file: %w", err)
		}
		defer f.Close()
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		summary := map[string]any{
			"elapsed_s": elapsed.Seconds(),
			"scanned":   stats.Scanned.Load(),
			"copied":    stats.Copied.Load(),
			"skipped":   stats.Skipped.Load(),
			"renamed":   stats.Renamed.Load(),
			"errors":    stats.Errors.Load(),
			"bytes":     stats.Bytes.Load(),
			"files":     jsonRecords,
		}
		if err := enc.Encode(summary); err != nil {
			return fmt.Errorf("write log: %w", err)
		}
	}

	return nil
}
