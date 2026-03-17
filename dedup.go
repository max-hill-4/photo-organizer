package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// renamedRE matches files created by pickDest's rename logic: base_N.ext
var renamedRE = regexp.MustCompile(`^(.+)_(\d+)(\.[^.]+)$`)

// runDedupDest scans dir for files that were renamed to avoid collisions (e.g.
// photo_1.jpg) and removes any whose content matches the base file (photo.jpg)
// in the same folder. Supports --dry-run.
func runDedupDest(dir string, dryRun bool) error {
	var checked, removed int64
	var freed int64

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}

		name := d.Name()
		m := renamedRE.FindStringSubmatch(name)
		if m == nil {
			return nil
		}

		// Reconstruct what the base filename would be (without the _N suffix).
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
			return nil // genuinely different files
		}

		info, _ := d.Info()
		sz := int64(0)
		if info != nil {
			sz = info.Size()
		}

		if dryRun {
			fmt.Printf("[dry-run] would remove: %s  (duplicate of %s)\n", path, baseName)
		} else {
			if err := os.Remove(path); err != nil {
				fmt.Fprintf(os.Stderr, "error removing %s: %v\n", path, err)
				return nil
			}
			fmt.Printf("removed: %s  (duplicate of %s)\n", path, baseName)
			freed += sz
		}
		removed++
		return nil
	})

	fmt.Printf("\nChecked _N files: %d  Duplicates %s: %d  Freed: %.1f MB\n",
		checked,
		map[bool]string{true: "found", false: "removed"}[dryRun],
		removed,
		float64(freed)/(1024*1024),
	)
	return err
}
