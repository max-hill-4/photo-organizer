# How photo-organizer works

## Overview

The tool copies photos and videos from a source directory into a clean
`YYYY/MM/DD/filename.ext` folder structure, detecting and removing duplicates
along the way. It is designed to handle large libraries (500k+ files) quickly
by doing as much work in parallel as possible and skipping already-processed
files with minimal overhead on re-runs.

---

## Stage 1 — Pre-scan

Before any copying begins, the tool runs a pre-scan of both the source and
destination directories in parallel.

**Destination walk**

Every file already in the destination is registered in two in-memory
structures:

- **`destIndex`** — a fast lookup table keyed by `filename|size`. If the
  exact same filename and size is already in the destination, the file can be
  skipped instantly without reading any metadata. This is the primary
  short-circuit for re-runs where most files are already present.

- **`tsRegistry`** — a table keyed by `folder|timestamp`. For each file that
  has a reliable date (EXIF, HEIC, or video mvhd), the file is registered
  under its timestamp so that duplicate files with the same creation time
  can be caught during the copy phase. Each file is registered under up to
  two keys — its primary extracted date **and** its filesystem mtime — to
  handle the case where a duplicate has lost its original metadata but
  preserved the timestamp as mtime.

**Pre-scan cache**

Populating `tsRegistry` requires reading EXIF/metadata from every file in
the destination — which on a large library means opening every file on every
run. To eliminate this on re-runs, the tool maintains a cache file at
`<dest>/.photo-organizer.cache`.

Each entry maps an absolute path to `{size, mtime, extracted date, date
source}`. On pre-scan, the worker checks the cache first:

- **Cache hit** (size + mtime unchanged) — date is used directly; the file
  is never opened.
- **Cache miss** (new or modified file) — `ExtractDate` runs as normal and
  the result is written to the new cache.

The cache is rewritten after every pre-scan, containing only files currently
present in the destination — so deleted files are pruned automatically. On a
stable destination re-run, pre-scan cost drops from N full file reads to N
`stat` calls plus one gob decode.

**Source walk**

The source directory is counted so the progress bar has a known total to work
with.

---

## Stage 2 — Parallel directory walk

Once pre-scan is complete, the tool walks the source directory using up to 64
concurrent goroutines — each one reading a different subdirectory at the same
time. When a new subdirectory is found, it is handed off to a free goroutine.
If all goroutines are busy, the current goroutine scans it inline so there is
never a deadlock.

For every file found, the walker applies quick filters before doing anything
else:

1. Skip symlinks, devices, and macOS `._` resource fork files
2. Skip unsupported file extensions
3. Skip zero-byte files
4. **Check `destIndex`** — if `filename|size` is already in the destination,
   count the file as skipped immediately without sending it to any worker.
   This avoids all channel and goroutine overhead for already-present files,
   which on a re-run is typically 80%+ of all files.

Files that pass all filters are sent to the worker pool as jobs.

---

## Stage 3 — Worker pool

A configurable number of workers (default: one per CPU core) pick jobs off the
queue and process them concurrently. Each worker calls `Process()` on its file.

### Date extraction

The date is extracted in priority order, stopping at the first success:

| Priority | Source | Method |
|---|---|---|
| 1 | **EXIF** | Reads `DateTimeOriginal` from JPEG, TIFF, RAW files |
| 2 | **HEIC** | Finds the `Exif\x00\x00` marker in the first 512 KB of the file and decodes the embedded TIFF block |
| 3 | **Video mvhd** | Reads the creation timestamp from the `mvhd` atom in the first 512 KB of MOV/MP4/M4V/3GP files |
| 4 | **Filename** | Matches patterns like `20210614_183000`, `2021-06-14`, `20210614` |
| 5 | **mtime** | Falls back to the file's last-modified timestamp |

Dates outside the range 1990–2100 are rejected and the next source is tried.

### Destination path

The extracted date determines the destination folder:
```
<dest>/2021/06/14/IMG_1234.JPG
```

If no valid date can be found, the file goes to:
```
<dest>/unknown/<original-parent-folder>/filename.ext
```

### Duplicate detection (same-folder)

Before copying, the worker checks `tsRegistry` for any file already present in
the same destination folder with the same timestamp. Each incoming file is
checked against up to two keys — its primary date and its mtime:

```
File A (original):   EXIF = 05:58:50,  mtime = 06:13:05
  → registered under keys: ["2011/02/08|05:58:50", "2011/02/08|06:13:05"]

File B (degraded duplicate, lost EXIF):  mtime = 05:58:50
  → checks key "2011/02/08|05:58:50" → finds File A → duplicate!
```

The rule is always **keep the largest file** (highest quality). If the
incoming file is smaller than what is already registered, it is skipped. If it
is larger, the previously copied smaller file is deleted and the new one takes
its place.

### Destination collision handling

Even after timestamp dedup, two files can legitimately have the same filename
in the same folder (e.g. photos from different cameras). `pickDest` resolves
this:

1. Destination does not exist → copy as-is
2. Destination exists, **same size** → skip (already there)
3. Destination exists, **same content** (hash match) → skip, even if sizes
   differ (handles files with different embedded metadata but identical pixels)
4. Destination exists, **different content** → copy as `filename_1.ext`,
   `filename_2.ext`, up to `_99`

The hash for step 3 is only computed when a collision actually occurs, so
there is no overhead for the common case.

### Hash-based global dedup (`--hash-dedup`)

When enabled, every file's MD5 hash is checked against a global registry
before copying. If the exact same bytes have already been copied anywhere in
the destination (regardless of filename or folder), the file is skipped. This
is the most thorough dedup mode but requires reading every file in full.

The computed hash is retained and passed through to the destination collision
check (`pickDest`), so a file that triggers a name collision is never hashed
twice.

---

## Stage 4 — Results and summary

Results flow back through a channel to the main goroutine, which:

- Records stats (copied, skipped, renamed, errors, unknown date)
- Writes errors to `errors.log`
- Optionally writes a full JSON log (`--log-file`)
- Updates the live progress bar every 500 ms

At the end, a summary is printed showing file counts, date source breakdown,
total data copied, and average throughput.

---

## Cleanup tool — `--dedup-dest`

For cleaning up duplicates already present in a destination directory, the
`--dedup-dest` flag runs two passes:

### Pass 1 — Timestamp-based (union-find)

For each folder in the destination:

1. Every file's timestamps (primary date + mtime) are extracted using the same
   date extraction logic as the copy phase
2. Files that share any timestamp key are grouped together using a
   [union-find](https://en.wikipedia.org/wiki/Disjoint-set_data_structure)
   algorithm — so if File A and File B share one timestamp, and File B and
   File C share another, all three are considered the same group
3. Within each group, the largest file is kept and all smaller files are
   removed

This correctly handles the case where a duplicate has lost its creation date
and only has mtime left, which matches the original's EXIF date.

### Pass 2 — Rename collision (`_N` files)

Any file matching the `filename_N.ext` pattern (created by `pickDest` when
two files had the same name) is hash-compared against the base file. If the
content is identical, the `_N` copy is removed.

Always run with `--dry-run` first to preview what would be deleted:

```bash
./photo-organizer --dry-run --dedup-dest /path/to/Backup
./photo-organizer --dedup-dest /path/to/Backup
```

---

## Performance notes

- On a native filesystem (Linux ext4, Windows NTFS from Windows), expect
  10–25 MB/s per worker for copy-heavy runs
- On WSL2 accessing a Windows drive (`/mnt/d/...`), every filesystem call
  crosses the 9P bridge and performance drops significantly — build a native
  Windows `.exe` with `GOOS=windows go build` and run from PowerShell instead
- Re-runs where most files are already in the destination are much faster —
  the `destIndex` pre-filter means skipped files cost two atomic counter
  increments, no file I/O at all
- Pre-scan on a stable destination is equally fast: the cache (`<dest>/.photo-organizer.cache`)
  reduces EXIF extraction to a `stat` call per file; only new or changed
  destination files are opened and parsed
- The HEIC and video date readers are limited to 512 KB per file; EXIF data
  is always within the first few hundred bytes so this is more than sufficient
