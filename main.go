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
		workers    = flag.Int("workers", runtime.NumCPU(), "Parallel workers")
		bufSizeKB  = flag.Int("buf-size", 8192, "I/O buffer in KB")
		logFile    = flag.String("log-file", "", "Write JSON summary to file")
		unknownDir = flag.String("unknown-dir", "", "Dir for undatable files (default: <dest>/unknown)")
		verbose    = flag.Bool("verbose", false, "Log each file action")
		hashDedup  = flag.Bool("hash-dedup", false, "Use MD5 hash to detect duplicates across different filenames/sizes")
		dateTest   = flag.String("date-test", "", "Test date extraction on a single file and exit")
		dedupDest  = flag.String("dedup-dest", "", "Scan a dest dir for _1/_2 duplicates and remove them")
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

	// --dedup-dest mode
	if *dedupDest != "" {
		if err := runDedupDest(*dedupDest, *dryRun); err != nil {
			fmt.Fprintln(os.Stderr, "fatal:", err)
			os.Exit(1)
		}
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
	initBufPool(cfg.bufSize)

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

	// Pre-populate tsRegistry from dest so smaller duplicates are skipped.
	prescanDest(cfg.destDir, cfg.workers)

	stats := &Stats{}
	var jsonRecords []map[string]any

	// Start walker in background
	walkDone := make(chan error, 1)
	go func() {
		defer close(jobs)
		walkDone <- Walk(cfg.sourceDir, jobs, stats)
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

	// Progress ticker
	go func() {
		for tick := 0; ; tick++ {
			time.Sleep(500 * time.Millisecond)
			scanned := int(stats.Scanned.Load())
			discovered := int(stats.Discovered.Load())
			skipped := int(stats.Skipped.Load())
			elapsed := time.Since(start)
			skipPct := 0.0
			if scanned > 0 {
				skipPct = float64(skipped) / float64(scanned) * 100
			}
			pct := 0.0
			if discovered > 0 {
				pct = float64(scanned) / float64(discovered) * 100
			}
			fmt.Fprintf(os.Stderr, "\rCopying  %s  %s / %s  %.1f%%  skipped %.0f%%  elapsed %s%s   ",
				drawBar(scanned, discovered, tick),
				commaf(int64(scanned)), commaf(int64(discovered)),
				pct,
				skipPct,
				elapsed.Round(time.Second),
				etaStr(scanned, discovered, elapsed),
			)
		}
	}()

	for r := range results {
		stats.Record(r)

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
	fmt.Fprintln(os.Stderr)

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
