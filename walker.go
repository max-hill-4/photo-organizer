package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Job represents a single file to be processed.
type Job struct {
	Path string
	Info fs.FileInfo
}

// supportedExts is the set of file extensions we handle.
var supportedExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".heic": true,
	".tiff": true, ".tif": true, ".cr2": true, ".nef": true,
	".arw": true, ".dng": true, ".mov": true, ".mp4": true,
	".m4v": true, ".aae": true, ".gif": true, ".bmp": true,
	".webp": true, ".3gp": true,
}

// Walk traverses sourceDir concurrently and sends jobs to the jobs channel.
// Up to 64 directories are scanned in parallel; when all goroutines are busy
// the work falls back to inline recursion so there is no deadlock.
func Walk(sourceDir string, jobs chan<- Job) error {
	var wg sync.WaitGroup
	// sem limits the number of concurrent directory-scan goroutines.
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
					// spare capacity: scan this subdirectory in a new goroutine
					go func(p string) {
						defer func() { <-sem }()
						walk(p)
					}(path)
				default:
					// all slots busy: recurse inline to avoid blocking
					walk(path)
				}
				continue
			}

			// skip symlinks, devices, pipes, etc.
			if !d.Type().IsRegular() {
				continue
			}
			// skip macOS AppleDouble resource fork files (._filename)
			if strings.HasPrefix(name, "._") {
				continue
			}

			ext := strings.ToLower(filepath.Ext(name))
			if !supportedExts[ext] {
				continue
			}

			info, err := d.Info()
			if err != nil || info.Size() == 0 {
				continue
			}

			jobs <- Job{Path: path, Info: info}
		}
	}

	wg.Add(1)
	walk(sourceDir)
	wg.Wait()
	return nil
}
