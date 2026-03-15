package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// Walk traverses sourceDir and sends jobs to the returned channel.
// It closes the channel when the walk is complete or ctx is done.
func Walk(sourceDir string, jobs chan<- Job) error {
	return filepath.WalkDir(sourceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil // skip dirs and symlinks
		}

		// Skip macOS AppleDouble resource fork files (._filename)
		if strings.HasPrefix(d.Name(), "._") {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if !supportedExts[ext] {
			return nil
		}

		info, err := os.Lstat(path)
		if err != nil || info.Size() == 0 {
			return nil // skip zero-byte or unreadable files
		}

		jobs <- Job{Path: path, Info: info}
		return nil
	})
}
