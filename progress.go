package main

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Stats tracks atomic counters across workers.
type Stats struct {
	Scanned  atomic.Int64
	Copied   atomic.Int64
	Skipped  atomic.Int64
	Renamed  atomic.Int64
	Errors      atomic.Int64
	UnknownDate atomic.Int64
	Bytes       atomic.Int64

	// Date source counters
	SrcEXIF     atomic.Int64
	SrcHEIC     atomic.Int64
	SrcVideo    atomic.Int64
	SrcFilename atomic.Int64
	SrcMtime    atomic.Int64
}

func (s *Stats) Record(r CopyResult) {
	s.Scanned.Add(1)
	switch r.Action {
	case "copied":
		s.Copied.Add(1)
		s.Bytes.Add(r.Bytes)
	case "skipped":
		s.Skipped.Add(1)
	case "renamed":
		s.Renamed.Add(1)
		s.Bytes.Add(r.Bytes)
	case "error":
		s.Errors.Add(1)
	case "unknown_date":
		s.UnknownDate.Add(1)
		s.Bytes.Add(r.Bytes)
	}
	switch r.DateSource {
	case DateSourceEXIF:
		s.SrcEXIF.Add(1)
	case DateSourceHEIC:
		s.SrcHEIC.Add(1)
	case DateSourceVideo:
		s.SrcVideo.Add(1)
	case DateSourceFilename:
		s.SrcFilename.Add(1)
	case DateSourceMtime:
		s.SrcMtime.Add(1)
	}
}

func (s *Stats) PrintSummary(elapsed time.Duration) {
	total := s.Scanned.Load()
	copied := s.Copied.Load()
	skipped := s.Skipped.Load()
	renamed := s.Renamed.Load()
	errors := s.Errors.Load()
	unknown := s.UnknownDate.Load()
	bytes := s.Bytes.Load()

	srcEXIF := s.SrcEXIF.Load()
	srcHEIC := s.SrcHEIC.Load()
	srcVideo := s.SrcVideo.Load()
	srcFile := s.SrcFilename.Load()
	srcMtime := s.SrcMtime.Load()

	gbCopied := float64(bytes) / (1024 * 1024 * 1024)
	var avgMBs float64
	if elapsed.Seconds() > 0 {
		avgMBs = float64(bytes) / (1024 * 1024) / elapsed.Seconds()
	}

	pct := func(n, total int64) string {
		if total == 0 {
			return "  0%"
		}
		return fmt.Sprintf("%3.0f%%", float64(n)/float64(total)*100)
	}

	fmt.Println()
	fmt.Println("=== Photo Organizer Summary ===")
	fmt.Printf("Files scanned:    %s\n", commaf(total))
	fmt.Printf("  Copied:         %s  (%.1f GB)\n", commaf(copied), gbCopied)
	fmt.Printf("  Skipped (dup):  %s\n", commaf(skipped))
	fmt.Printf("  Renamed:        %s\n", commaf(renamed))
	fmt.Printf("  Errors:         %s\n", commaf(errors))
	fmt.Printf("  Unknown date:   %s  (→ unknown/)\n", commaf(unknown))
	fmt.Println()
	fmt.Println("Date sources:")
	fmt.Printf("  EXIF:           %s (%s)\n", commaf(srcEXIF), pct(srcEXIF, total))
	fmt.Printf("  HEIC:           %s (%s)\n", commaf(srcHEIC), pct(srcHEIC, total))
	fmt.Printf("  Video mvhd:     %s (%s)\n", commaf(srcVideo), pct(srcVideo, total))
	fmt.Printf("  Filename:       %s (%s)\n", commaf(srcFile), pct(srcFile, total))
	fmt.Printf("  mtime:          %s (%s)\n", commaf(srcMtime), pct(srcMtime, total))
	fmt.Println()
	fmt.Printf("Elapsed: %s   Avg: %.1f MB/s\n", fmtDuration(elapsed), avgMBs)
}

// spinnerFrames cycles for indeterminate progress (unknown total).
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// drawBar renders a 20-char filled bar when total > 0, or a spinner frame when total == 0.
func drawBar(done, total, tick int) string {
	if total <= 0 {
		return fmt.Sprintf("[%s]", spinnerFrames[tick%len(spinnerFrames)])
	}
	pct := float64(done) / float64(total)
	filled := int(pct * 20)
	if filled > 20 {
		filled = 20
	}
	return fmt.Sprintf("[%s%s]", repeatRune('█', filled), repeatRune('░', 20-filled))
}

// etaStr returns an ETA string when calculable, otherwise "".
func etaStr(done, total int, elapsed time.Duration) string {
	if done <= 0 || total <= 0 || done >= total {
		return ""
	}
	remaining := time.Duration(float64(elapsed) / float64(done) * float64(total-done))
	return fmt.Sprintf("  ETA %s", remaining.Round(time.Second))
}

func repeatRune(c rune, n int) string {
	if n <= 0 {
		return ""
	}
	s := make([]rune, n)
	for i := range s {
		s[i] = c
	}
	return string(s)
}

func commaf(n int64) string {
	s := fmt.Sprintf("%d", n)
	out := make([]byte, 0, len(s)+len(s)/3)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(c))
	}
	return string(out)
}

func fmtDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
