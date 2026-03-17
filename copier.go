package main

import (
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

	// Ensure destination directory exists (cached).
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
	// larger one arrives later.
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
		if info, err := os.Stat(destPath); err == nil && info.Size() == job.Info.Size() {
			action = "skipped"
		}
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

	// Hash-based global dedup: skip if we've already copied this exact file.
	if cfg.hashDedup {
		h, err := hashFile(job.Path)
		if err == nil {
			if _, seen := hashRegistry.LoadOrStore(h, job.Path); seen {
				return CopyResult{Src: job.Path, Dst: destPath, DateSource: dateSrc, Date: dateT, Action: "skipped"}
			}
		}
	}

	if prevDestToDelete != "" {
		os.Remove(prevDestToDelete)
	}

	action, finalDest, n, err := copyFile(cfg, job.Path, destPath, job.Info.Size())
	if err == nil && (action == "copied" || action == "renamed") {
		mtime := job.Info.ModTime()
		_ = os.Chtimes(finalDest, mtime, mtime)
	}
	if tsKey != "" && err == nil {
		tsRegistry.Store(tsKey, tsEntry{size: job.Info.Size(), dest: finalDest})
	}
	result := CopyResult{Src: job.Path, Dst: finalDest, DateSource: dateSrc, Date: dateT, Action: action, Bytes: n, Err: err}
	if !validDate && action == "copied" {
		result.Action = "unknown_date"
	}
	return result
}

// copyFile copies src to dest. Returns action, final dest path, bytes written, error.
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
//   - dest not exist           → (dest, false)
//   - dest exists, same size   → (dest, true)  skip
//   - dest exists, same hash   → (dest, true)  skip
//   - dest exists, different   → try _1.._99
func pickDest(dest, srcPath string, srcSize int64, hashDedup bool) (string, bool, error) {
	// Lazily compute source hash only when a collision actually occurs.
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
			return true, true, nil
		}
		// Sizes differ — always hash-compare so files with identical content but
		// different embedded metadata don't become both file.jpg and file_1.jpg.
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
		return true, false, nil
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
