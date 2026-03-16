package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rwcarlsen/goexif/exif"
)

var uuidRE = regexp.MustCompile(`(?i)^[A-F0-9]{8}-[A-F0-9]{4}-[A-F0-9]{4}-[A-F0-9]{4}-[A-F0-9]{12}\.[a-zA-Z0-9]+$`)

var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".heic": true, ".png": true,
	".tiff": true, ".tif": true, ".mov": true, ".mp4": true,
}

type fileEntry struct {
	path      string
	timestamp string
	isUUID    bool
}

func exifTimestamp(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	x, err := exif.Decode(f)
	if err != nil {
		return ""
	}
	t, err := x.DateTime()
	if err != nil || t.IsZero() {
		return ""
	}
	return t.Format("2006:01:02 15:04:05")
}

func printProgress(label string, done, total int) {
	pct := 0.0
	if total > 0 {
		pct = float64(done) / float64(total) * 100
	}
	fmt.Fprintf(os.Stderr, "\r%s: %d / %d (%.1f%%)   ", label, done, total, pct)
}

func main() {
	dryRun := flag.Bool("dry-run", false, "Print actions, do not delete")
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: dedup [--dry-run] <backup-dir>")
		os.Exit(1)
	}
	root := flag.Arg(0)

	// Phase 1: collect all image file paths
	fmt.Fprintf(os.Stderr, "Scanning files...\n")
	var allFiles []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if imageExts[strings.ToLower(filepath.Ext(path))] {
			allFiles = append(allFiles, path)
		}
		return nil
	})
	total := len(allFiles)
	fmt.Fprintf(os.Stderr, "Found %d image files\n", total)

	// Phase 2: read EXIF timestamps with progress
	byFolder := map[string][]fileEntry{}
	for i, path := range allFiles {
		if i%500 == 0 {
			printProgress("Reading EXIF", i, total)
		}
		entry := fileEntry{
			path:      path,
			timestamp: exifTimestamp(path),
			isUUID:    uuidRE.MatchString(filepath.Base(path)),
		}
		folder := filepath.Dir(path)
		byFolder[folder] = append(byFolder[folder], entry)
	}
	printProgress("Reading EXIF", total, total)
	fmt.Fprintln(os.Stderr)

	// Phase 3: find duplicates
	var toDelete []fileEntry
	for _, files := range byFolder {
		byTS := map[string][]fileEntry{}
		for _, f := range files {
			if f.timestamp != "" {
				byTS[f.timestamp] = append(byTS[f.timestamp], f)
			}
		}
		for _, group := range byTS {
			if len(group) < 2 {
				continue
			}
			// Keep the largest file, delete the rest
			largest := group[0]
			for _, f := range group[1:] {
				info, _ := os.Stat(f.path)
				linfo, _ := os.Stat(largest.path)
				if info != nil && linfo != nil && info.Size() > linfo.Size() {
					largest = f
				}
			}
			for _, f := range group {
				if f.path != largest.path {
					toDelete = append(toDelete, f)
				}
			}
		}
	}

	fmt.Fprintf(os.Stderr, "Found %d duplicates\n", len(toDelete))

	// Phase 4: delete with progress
	var deletedCount int
	var deletedBytes int64
	for i, f := range toDelete {
		if i%100 == 0 {
			printProgress("Deleting", i, len(toDelete))
		}
		info, err := os.Stat(f.path)
		if err != nil {
			continue
		}
		size := info.Size()
		if *dryRun {
			fmt.Printf("[dup] %s  ts=%s  (%.1f MB)\n", filepath.Base(f.path), f.timestamp, float64(size)/1024/1024)
		} else {
			if err := os.Remove(f.path); err != nil {
				fmt.Fprintf(os.Stderr, "\nerror: %s: %v\n", f.path, err)
				continue
			}
		}
		deletedCount++
		deletedBytes += size
	}
	printProgress("Deleting", len(toDelete), len(toDelete))
	fmt.Fprintln(os.Stderr)

	action := "Would delete"
	if !*dryRun {
		action = "Deleted"
	}
	fmt.Printf("\n%s: %d files  (%.2f GB freed)\n",
		action, deletedCount, float64(deletedBytes)/1024/1024/1024)
}
