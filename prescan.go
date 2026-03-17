package main

import (
	"encoding/gob"
	"fmt"
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

// destScanEntry holds the filesystem metadata collected for a dest file during
// the initial directory walk. Workers receive it directly so no re-stat is needed.
type destScanEntry struct {
	path  string
	size  int64
	mtime time.Time
}

// prescanDest walks destDir and pre-populates tsRegistry with files already
// present, so the copy run skips source files that are smaller than an
// already-present file with the same EXIF/HEIC timestamp. The largest file
// per timestamp wins.
func prescanDest(destDir string, workers int) {
	var destCount atomic.Int64

	// Show live count while walking.
	walkDone := make(chan struct{})
	go func() {
		for tick := 0; ; tick++ {
			select {
			case <-walkDone:
				return
			default:
			}
			time.Sleep(500 * time.Millisecond)
			fmt.Fprintf(os.Stderr, "\rWalking  %s  dest %s   ",
				drawBar(0, 0, tick), commaf(destCount.Load()))
		}
	}()

	// Concurrent walk — same pattern as Walk() in walker.go.
	// Entries are collected under a mutex; most time is spent in os.ReadDir
	// (I/O bound) so contention is negligible.
	var mu sync.Mutex
	var destFiles []destScanEntry

	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)

	var walk func(dir string)
	walk = func(dir string) {
		defer wg.Done()
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, d := range entries {
			name := d.Name()
			path := filepath.Join(dir, name)
			if d.IsDir() {
				wg.Add(1)
				select {
				case sem <- struct{}{}:
					go func(p string) {
						defer func() { <-sem }()
						walk(p)
					}(path)
				default:
					walk(path)
				}
				continue
			}
			if !d.Type().IsRegular() {
				continue
			}
			if !supportedExts[strings.ToLower(filepath.Ext(name))] {
				continue
			}
			info, err := d.Info()
			if err != nil || info.Size() == 0 {
				continue
			}
			entry := destScanEntry{
				path:  path,
				size:  info.Size(),
				mtime: info.ModTime(),
			}
			mu.Lock()
			destFiles = append(destFiles, entry)
			mu.Unlock()
			destCount.Add(1)
		}
	}

	wg.Add(1)
	walk(destDir)
	wg.Wait()
	close(walkDone)

	// Build destIndex from collected entries (single goroutine — plain map is safe).
	destIndex = make(map[string]struct{}, len(destFiles))
	for _, entry := range destFiles {
		key := filepath.Base(entry.path) + "|" + strconv.FormatInt(entry.size, 10)
		destIndex[key] = struct{}{}
	}

	total := len(destFiles)
	if total == 0 {
		return
	}

	// Phase 2: populate tsRegistry from dest files, using a disk cache to skip
	// EXIF extraction for files whose size+mtime have not changed since the
	// last run. Workers receive destScanEntry directly — no re-stat required.
	oldCache := loadPrescanCache(destDir)
	var newCacheMu sync.Mutex
	newCache := make(map[string]prescanCacheEntry, len(destFiles))

	var done atomic.Int64
	exifJobs := make(chan destScanEntry, 256)
	var exifWg sync.WaitGroup

	for i := 0; i < workers; i++ {
		exifWg.Add(1)
		go func() {
			defer exifWg.Done()
			for entry := range exifJobs {
				var dateT time.Time
				var dateSrc DateSource
				if ce, ok := oldCache[entry.path]; ok && ce.Size == entry.size && ce.Mtime == entry.mtime.Unix() {
					// Cache hit: no need to open or parse the file.
					dateT = time.Unix(ce.Date, 0).UTC()
					dateSrc = ce.DateSrc
				} else {
					dateT, dateSrc = ExtractDate(entry.path, entry.mtime)
				}
				newCacheMu.Lock()
				newCache[entry.path] = prescanCacheEntry{
					Size:    entry.size,
					Mtime:   entry.mtime.Unix(),
					Date:    dateT.Unix(),
					DateSrc: dateSrc,
				}
				newCacheMu.Unlock()
				e := tsEntry{size: entry.size, dest: entry.path}
				for _, k := range buildTsKeys(filepath.Dir(entry.path), dateT, entry.mtime) {
					if prev, loaded := tsRegistry.LoadOrStore(k, e); loaded {
						if e.size > prev.(tsEntry).size {
							tsRegistry.Store(k, e)
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

	for _, entry := range destFiles {
		exifJobs <- entry
	}
	close(exifJobs)
	exifWg.Wait()
	savePrescanCache(destDir, newCache)

	fmt.Fprintf(os.Stderr, "\rPre-scan %s  %s / %s  100.0%%  elapsed %s   ",
		drawBar(total, total, 0), commaf(int64(total)), commaf(int64(total)), time.Since(start).Round(time.Millisecond))
}
