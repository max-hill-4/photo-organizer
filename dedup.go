package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// renamedRE matches files created by pickDest's rename logic: base_N.ext
var renamedRE = regexp.MustCompile(`^(.+)_(\d+)(\.[^.]+)$`)

// runDedupDest runs two dedup passes over dir:
//  1. Timestamp pass — groups files in each folder that share any timestamp
//     (primary date or mtime), keeps the largest, removes the rest.
//  2. Rename pass — finds *_N.ext files whose content is identical to the
//     base file and removes them.
func runDedupDest(dir string, dryRun bool) error {
	fmt.Fprintln(os.Stderr, "Pass 1: timestamp-based dedup...")
	r1, f1, err := dedupByTimestamp(dir, dryRun)
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "\nPass 2: rename-collision dedup (_1, _2 ...)...")
	r2, f2, err := dedupByRename(dir, dryRun)
	if err != nil {
		return err
	}

	verb := "removed"
	if dryRun {
		verb = "would remove"
	}
	fmt.Printf("\nTotal %s: %d  Freed: %.1f MB\n", verb, r1+r2, float64(f1+f2)/(1024*1024))
	return nil
}

// dedupByTimestamp collects all files per directory, groups files that share
// any timestamp key using union-find, then removes all but the largest in
// each group.
func dedupByTimestamp(dir string, dryRun bool) (removed, freed int64, err error) {
	type fileEntry struct {
		path string
		size int64
		keys []string
	}

	// Collect all files grouped by their parent directory.
	cache := loadPrescanCache(dir)
	dirFiles := make(map[string][]fileEntry)
	var scanned int64

	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, "._") {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		if !supportedExts[ext] {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() == 0 {
			return nil
		}

		mtime := info.ModTime()
		var dateT time.Time
		if ce, ok := cache[path]; ok && ce.Size == info.Size() && ce.Mtime == mtime.Unix() {
			dateT = time.Unix(ce.Date, 0).UTC()
		} else {
			dateT, _ = ExtractDate(path, mtime)
		}
		keys := buildTsKeys(filepath.Dir(path), dateT, mtime)

		dirPath := filepath.Dir(path)
		dirFiles[dirPath] = append(dirFiles[dirPath], fileEntry{
			path: path,
			size: info.Size(),
			keys: keys,
		})
		scanned++
		if scanned%5000 == 0 {
			fmt.Fprintf(os.Stderr, "\r  scanned %d files...", scanned)
		}
		return nil
	})
	fmt.Fprintf(os.Stderr, "\r  scanned %d files\n", scanned)
	if walkErr != nil {
		return 0, 0, walkErr
	}

	// For each directory, union-find files that share any timestamp key.
	for _, files := range dirFiles {
		if len(files) <= 1 {
			continue
		}

		// Map timestamp key → indices of files that have it.
		keyToIdx := make(map[string][]int)
		for i, f := range files {
			for _, k := range f.keys {
				keyToIdx[k] = append(keyToIdx[k], i)
			}
		}

		// Union-Find.
		parent := make([]int, len(files))
		for i := range parent {
			parent[i] = i
		}
		var find func(int) int
		find = func(x int) int {
			if parent[x] != x {
				parent[x] = find(parent[x])
			}
			return parent[x]
		}
		union := func(x, y int) {
			parent[find(x)] = find(y)
		}
		for _, group := range keyToIdx {
			for i := 1; i < len(group); i++ {
				union(group[0], group[i])
			}
		}

		// Group by component root.
		components := make(map[int][]int)
		for i := range files {
			components[find(i)] = append(components[find(i)], i)
		}

		// Within each component keep the largest file, remove the rest.
		for _, comp := range components {
			if len(comp) <= 1 {
				continue
			}
			maxIdx := comp[0]
			for _, idx := range comp[1:] {
				if files[idx].size > files[maxIdx].size {
					maxIdx = idx
				}
			}
			for _, idx := range comp {
				if idx == maxIdx {
					continue
				}
				f := files[idx]
				if dryRun {
					fmt.Printf("[dry-run] would remove: %s  (dup of %s)\n", f.path, filepath.Base(files[maxIdx].path))
				} else {
					if rmErr := os.Remove(f.path); rmErr != nil {
						fmt.Fprintf(os.Stderr, "error removing %s: %v\n", f.path, rmErr)
						continue
					}
					fmt.Printf("removed: %s  (dup of %s)\n", f.path, filepath.Base(files[maxIdx].path))
					freed += f.size
				}
				removed++
			}
		}
	}
	return removed, freed, nil
}

// dedupByRename finds *_N.ext files whose content is identical to the base
// file in the same folder and removes them.
func dedupByRename(dir string, dryRun bool) (removed, freed int64, err error) {
	var checked int64
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		m := renamedRE.FindStringSubmatch(name)
		if m == nil {
			return nil
		}
		ext := m[3]
		if !supportedExts[strings.ToLower(ext)] {
			return nil
		}

		baseName := m[1] + ext
		basePath := filepath.Join(filepath.Dir(path), baseName)
		if _, err := os.Stat(basePath); err != nil {
			return nil // base file doesn't exist
		}

		checked++
		h1, err := hashFile(path)
		if err != nil {
			return nil
		}
		h2, err := hashFile(basePath)
		if err != nil {
			return nil
		}
		if h1 != h2 {
			return nil // genuinely different
		}

		info, _ := d.Info()
		sz := int64(0)
		if info != nil {
			sz = info.Size()
		}
		if dryRun {
			fmt.Printf("[dry-run] would remove: %s  (same content as %s)\n", path, baseName)
		} else {
			if rmErr := os.Remove(path); rmErr != nil {
				fmt.Fprintf(os.Stderr, "error removing %s: %v\n", path, rmErr)
				return nil
			}
			fmt.Printf("removed: %s  (same content as %s)\n", path, baseName)
			freed += sz
		}
		removed++
		return nil
	})
	fmt.Fprintf(os.Stderr, "  checked %d _N files\n", checked)
	return removed, freed, err
}
