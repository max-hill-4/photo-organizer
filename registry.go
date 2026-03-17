package main

import "sync"

// dirCache avoids redundant MkdirAll syscalls.
var dirCache sync.Map

// hashRegistry stores MD5 hashes of copied source files for global dedup.
var hashRegistry sync.Map

// tsRegistry stores "destDir|timestamp" → tsEntry to detect same-folder duplicates.
// The largest file seen for a given timestamp wins.
var tsRegistry sync.Map

// destIndex is populated by prescan: "basename|size" → struct{}.
// Written once before any workers start, then read-only — plain map is safe and fast.
var destIndex map[string]struct{}

// tsEntry tracks the largest file copied for a given timestamp key.
type tsEntry struct {
	size int64
	dest string
}

// bufPool holds reusable I/O buffers. Size is set once at startup via initBufPool.
var bufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 8*1024*1024)
		return &buf
	},
}

// initBufPool reconfigures bufPool to allocate buffers of the given size.
// Must be called before any workers start.
func initBufPool(size int) {
	if size > 0 {
		bufPool.New = func() any {
			buf := make([]byte, size)
			return &buf
		}
	}
}
