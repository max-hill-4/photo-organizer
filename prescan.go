package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// prescanDest walks destDir and pre-populates tsRegistry with files already
// present, so the copy run skips source files that are smaller than an
// already-present file with the same EXIF/HEIC timestamp. The largest file
// per timestamp wins. It also counts source files and returns that total so
// the copy progress bar can show a determinate fill.
func prescanDest(destDir, sourceDir string, workers int) int {
	// Initialise destIndex as a plain map — written only here (single goroutine),
	// then read-only for the entire copy phase.
	destIndex = make(map[string]struct{})

	var destFiles, srcFiles []string
	var destCount, srcCount atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		filepath.WalkDir(destDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if supportedExts[strings.ToLower(filepath.Ext(path))] {
				if info, err := os.Stat(path); err == nil {
					key := filepath.Base(path) + "|" + strconv.FormatInt(info.Size(), 10)
					destIndex[key] = struct{}{}
				}
				destFiles = append(destFiles, path)
				destCount.Add(1)
			}
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		filepath.WalkDir(sourceDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if supportedExts[strings.ToLower(filepath.Ext(path))] {
				srcFiles = append(srcFiles, path)
				srcCount.Add(1)
			}
			return nil
		})
	}()

	// Show live counts while walking.
	walkDone := make(chan struct{})
	go func() {
		for tick := 0; ; tick++ {
			select {
			case <-walkDone:
				return
			default:
			}
			time.Sleep(500 * time.Millisecond)
			fmt.Fprintf(os.Stderr, "\rWalking  %s  dest %s  source %s   ",
				drawBar(0, 0, tick), commaf(destCount.Load()), commaf(srcCount.Load()))
		}
	}()
	wg.Wait()
	close(walkDone)

	sourceTotal := len(srcFiles)
	total := len(destFiles)
	if total == 0 {
		return sourceTotal
	}

	// Phase 2: read EXIF from dest files in parallel and populate tsRegistry.
	var done atomic.Int64
	exifJobs := make(chan string, 256)
	var exifWg sync.WaitGroup

	for i := 0; i < workers; i++ {
		exifWg.Add(1)
		go func() {
			defer exifWg.Done()
			for path := range exifJobs {
				info, err := os.Stat(path)
				if err != nil {
					done.Add(1)
					continue
				}
				dateT, dateSrc := ExtractDate(path, info.ModTime())
				if dateSrc == DateSourceEXIF || dateSrc == DateSourceHEIC {
					tsKey := filepath.Dir(path) + "|" + dateT.Format("2006:01:02 15:04:05")
					entry := tsEntry{size: info.Size(), dest: path}
					if prev, loaded := tsRegistry.LoadOrStore(tsKey, entry); loaded {
						if entry.size > prev.(tsEntry).size {
							tsRegistry.Store(tsKey, entry)
						}
					}
				}
				done.Add(1)
			}
		}()
	}

	start := time.Now()
	go func() {
		for tick := 0; ; tick++ {
			time.Sleep(500 * time.Millisecond)
			n := int(done.Load())
			pct := float64(n) / float64(total) * 100
			elapsed := time.Since(start).Round(time.Second)
			fmt.Fprintf(os.Stderr, "\rPre-scan %s  %s / %s  %.1f%%  elapsed %s%s   ",
				drawBar(n, total, tick), commaf(int64(n)), commaf(int64(total)), pct, elapsed, etaStr(n, total, time.Since(start)))
			if n >= total {
				return
			}
		}
	}()

	for _, f := range destFiles {
		exifJobs <- f
	}
	close(exifJobs)
	exifWg.Wait()

	fmt.Fprintf(os.Stderr, "\rPre-scan %s  %s / %s  100.0%%  elapsed %s   ",
		drawBar(total, total, 0), commaf(int64(total)), commaf(int64(total)), time.Since(start).Round(time.Millisecond))

	return sourceTotal
}
