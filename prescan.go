package main

import (
	"encoding/gob"
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

// prescanCacheEntry stores the extracted date for one dest file.  The cache
// is keyed by absolute path; an entry is valid only when Size and Mtime still
// match the file on disk, so a changed file is automatically re-extracted.
type prescanCacheEntry struct {
	Size    int64
	Mtime   int64 // Unix seconds
	Date    int64 // Unix seconds
	DateSrc DateSource
}

func cacheFilePath(destDir string) string {
	return filepath.Join(destDir, ".photo-organizer.cache")
}

func loadPrescanCache(destDir string) map[string]prescanCacheEntry {
	f, err := os.Open(cacheFilePath(destDir))
	if err != nil {
		return make(map[string]prescanCacheEntry)
	}
	defer f.Close()
	var m map[string]prescanCacheEntry
	if err := gob.NewDecoder(f).Decode(&m); err != nil || m == nil {
		return make(map[string]prescanCacheEntry)
	}
	return m
}

func savePrescanCache(destDir string, entries map[string]prescanCacheEntry) {
	f, err := os.Create(cacheFilePath(destDir))
	if err != nil {
		return
	}
	defer f.Close()
	_ = gob.NewEncoder(f).Encode(entries)
}

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

	// Phase 2: populate tsRegistry from dest files, using a disk cache to skip
	// EXIF extraction for files whose size+mtime have not changed since the
	// last run.
	oldCache := loadPrescanCache(destDir)
	var newCacheMu sync.Mutex
	newCache := make(map[string]prescanCacheEntry, len(destFiles))

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
				mtime := info.ModTime()
				var dateT time.Time
				var dateSrc DateSource
				if ce, ok := oldCache[path]; ok && ce.Size == info.Size() && ce.Mtime == mtime.Unix() {
					// Cache hit: no need to open or parse the file.
					dateT = time.Unix(ce.Date, 0).UTC()
					dateSrc = ce.DateSrc
				} else {
					dateT, dateSrc = ExtractDate(path, mtime)
				}
				newCacheMu.Lock()
				newCache[path] = prescanCacheEntry{
					Size:    info.Size(),
					Mtime:   mtime.Unix(),
					Date:    dateT.Unix(),
					DateSrc: dateSrc,
				}
				newCacheMu.Unlock()
				entry := tsEntry{size: info.Size(), dest: path}
				for _, k := range buildTsKeys(filepath.Dir(path), dateT, dateSrc, mtime) {
					if prev, loaded := tsRegistry.LoadOrStore(k, entry); loaded {
						if entry.size > prev.(tsEntry).size {
							tsRegistry.Store(k, entry)
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
	savePrescanCache(destDir, newCache)

	fmt.Fprintf(os.Stderr, "\rPre-scan %s  %s / %s  100.0%%  elapsed %s   ",
		drawBar(total, total, 0), commaf(int64(total)), commaf(int64(total)), time.Since(start).Round(time.Millisecond))

	return sourceTotal
}
