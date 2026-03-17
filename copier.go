package main

import (
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// CopyResult describes the outcome of processing one file.
type CopyResult struct {
	Src        string
	Dst        string
	DateSource DateSource
	Date       time.Time
	Action     string // "copied", "skipped", "renamed", "error", "unknown_date"
	Bytes      int64
	Err        error
}

// dirCache avoids redundant MkdirAll syscalls.
var dirCache sync.Map

// hashRegistry stores MD5 hashes of all copied source files for global dedup.
var hashRegistry sync.Map

// tsRegistry stores "destDir|timestamp" keys to detect same-folder duplicates.
// Values are tsEntry, tracking the largest file seen so far for that key.
var tsRegistry sync.Map

// destIndex is populated by prescan: "basename|size" -> struct{}.
// Written once before any workers start, then read-only — plain map is safe and fast.
var destIndex map[string]struct{}

type tsEntry struct {
	size int64
	dest string
}

var uuidRE = regexp.MustCompile(`(?i)^[A-F0-9]{8}-[A-F0-9]{4}-[A-F0-9]{4}-[A-F0-9]{4}-[A-F0-9]{12}\.[a-zA-Z0-9]+$`)

// bufPool holds reusable I/O buffers. Size is set once at startup via initBufPool.
var bufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 8*1024*1024)
		return &buf
	},
}

func initBufPool(size int) {
	if size > 0 {
		bufPool.New = func() any {
			buf := make([]byte, size)
			return &buf
		}
	}
}

// Process handles one file: extract date, build dest path, copy/skip/rename.
// destIndex hits are pre-filtered by the walker and never reach here.
func Process(cfg *Config, job Job) CopyResult {
	dateT, dateSrc := ExtractDate(job.Path, job.Info.ModTime())

	validDate := !dateT.IsZero() && dateT.Year() >= 1990 && dateT.Year() <= 2100

	var destDir string
	if !validDate {
		destDir = filepath.Join(cfg.unknownDir, filepath.Base(filepath.Dir(job.Path)))
	} else {
		destDir = filepath.Join(cfg.destDir,
			fmt.Sprintf("%04d", dateT.Year()),
			fmt.Sprintf("%02d", int(dateT.Month())),
			fmt.Sprintf("%02d", dateT.Day()))
	}

	// Ensure destination directory exists (cached)
	if _, loaded := dirCache.LoadOrStore(destDir, true); !loaded {
		if !cfg.dryRun {
			if err := os.MkdirAll(destDir, 0755); err != nil {
				return CopyResult{Src: job.Path, Err: err, Action: "error", DateSource: dateSrc}
			}
		}
	}

	destPath := filepath.Join(destDir, filepath.Base(job.Path))

	// Same-folder dedup: for files with a reliable timestamp (EXIF/HEIC), keep only
	// the largest. We track the dest path so the smaller file can be removed if a
	// larger one arrives later, and store job.Info.Size() (not bytes written) so
	// re-runs correctly recognise already-present files as their real size.
	var tsKey string
	var prevDestToDelete string
	if dateSrc == DateSourceEXIF || dateSrc == DateSourceHEIC {
		tsKey = destDir + "|" + dateT.Format("2006:01:02 15:04:05")
		cur := tsEntry{size: job.Info.Size()}
		if prev, loaded := tsRegistry.LoadOrStore(tsKey, cur); loaded {
			prevE := prev.(tsEntry)
			if job.Info.Size() <= prevE.size {
				return CopyResult{Src: job.Path, Dst: destPath, DateSource: dateSrc, Date: dateT, Action: "skipped"}
			}
			// Current is larger — remove the previously copied smaller file.
			prevDestToDelete = prevE.dest
			tsRegistry.Store(tsKey, cur)
		}
	}

	if cfg.dryRun {
		action := "copied"
		if !validDate {
			action = "unknown_date"
		}
		// Check if dest already exists with same size — same logic as the real copy.
		if info, err := os.Stat(destPath); err == nil && info.Size() == job.Info.Size() {
			action = "skipped"
		}
		// Update tsRegistry so subsequent files with the same timestamp benefit.
		if tsKey != "" {
			tsRegistry.Store(tsKey, tsEntry{size: job.Info.Size(), dest: destPath})
		}
		return CopyResult{
			Src:        job.Path,
			Dst:        destPath,
			DateSource: dateSrc,
			Date:       dateT,
			Action:     action,
			Bytes:      job.Info.Size(),
		}
	}

	// Hash-based global dedup: skip if we've already copied this exact file
	if cfg.hashDedup {
		h, err := hashFile(job.Path)
		if err == nil {
			if _, seen := hashRegistry.LoadOrStore(h, job.Path); seen {
				return CopyResult{Src: job.Path, Dst: destPath, DateSource: dateSrc, Date: dateT, Action: "skipped", Bytes: 0}
			}
		}
	}

	if prevDestToDelete != "" {
		os.Remove(prevDestToDelete)
	}

	action, finalDest, n, err := copyFile(cfg, job.Path, destPath, job.Info.Size())
	// Preserve original timestamps on newly copied files
	if err == nil && (action == "copied" || action == "renamed") {
		mtime := job.Info.ModTime()
		_ = os.Chtimes(finalDest, mtime, mtime)
	}
	// Update registry with actual dest path and source size (not bytes written,
	// so skipped files still register at their real size for future comparisons).
	if tsKey != "" && err == nil {
		tsRegistry.Store(tsKey, tsEntry{size: job.Info.Size(), dest: finalDest})
	}
	result := CopyResult{Src: job.Path, Dst: finalDest, DateSource: dateSrc, Date: dateT, Action: action, Bytes: n, Err: err}
	if !validDate && action == "copied" {
		result.Action = "unknown_date"
	}
	return result
}

// copyFile copies src to dest handling dedup. Returns action, final dest path, bytes written, error.
func copyFile(cfg *Config, src, dest string, srcSize int64) (string, string, int64, error) {
	finalDest, skip, err := pickDest(dest, src, srcSize, cfg.hashDedup)
	if err != nil {
		return "error", dest, 0, err
	}
	if skip {
		return "skipped", finalDest, 0, nil
	}

	action := "copied"
	if finalDest != dest {
		action = "renamed"
	}

	f, err := os.OpenFile(finalDest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return "error", finalDest, 0, err
	}
	defer f.Close()

	in, err := os.Open(src)
	if err != nil {
		os.Remove(finalDest)
		return "error", dest, 0, err
	}
	defer in.Close()

	bufPtr := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufPtr)

	n, err := io.CopyBuffer(f, in, *bufPtr)
	if err != nil {
		os.Remove(finalDest)
		return "error", dest, 0, err
	}
	return action, finalDest, n, nil
}

// pickDest resolves the final destination path:
// - dest not exist → (dest, false)
// - dest exists, same size → (dest, true) skip
// - dest exists, same hash → (dest, true) skip (always checked to prevent _1 duplicates)
// - dest exists, different content → try _1.._99
func pickDest(dest, srcPath string, srcSize int64, hashDedup bool) (string, bool, error) {
	// Lazily compute source hash only if we actually need it.
	var srcHash string
	getSrcHash := func() string {
		if srcHash == "" {
			srcHash, _ = hashFile(srcPath)
		}
		return srcHash
	}

	check := func(path string) (exists bool, skip bool, err error) {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			return false, false, nil
		}
		if err != nil {
			return false, false, err
		}
		if info.Size() == srcSize {
			return true, true, nil // same size → skip
		}
		// Sizes differ — always hash-compare so files with the same content but
		// different embedded metadata (e.g. one copy stripped of EXIF) don't
		// end up as both file.jpg and file_1.jpg.
		if h := getSrcHash(); h != "" {
			if dh, err := hashFile(path); err == nil && dh == h {
				return true, true, nil
			}
		}
		if hashDedup {
			if _, seen := hashRegistry.Load(getSrcHash()); seen {
				return true, true, nil
			}
		}
		return true, false, nil // exists but different → need rename
	}

	exists, skip, err := check(dest)
	if err != nil {
		return dest, false, err
	}
	if !exists {
		return dest, false, nil
	}
	if skip {
		return dest, true, nil
	}

	ext := filepath.Ext(dest)
	base := strings.TrimSuffix(dest, ext)
	for i := 1; i <= 99; i++ {
		candidate := fmt.Sprintf("%s_%d%s", base, i, ext)
		exists, skip, err := check(candidate)
		if err != nil {
			return dest, false, err
		}
		if !exists {
			return candidate, false, nil
		}
		if skip {
			return candidate, true, nil
		}
	}
	return dest, false, fmt.Errorf("too many name collisions for %s", dest)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	bufPtr := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufPtr)
	if _, err := io.CopyBuffer(h, f, *bufPtr); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
