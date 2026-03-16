# photo-organizer

Apple Photos and iPhoto libraries from macOS 10.15 and earlier are no longer supported by modern versions of macOS or Windows. If you have old `.photoslibrary` or `.migratedphotolibrary` backups sitting on an external drive, you can't just open them — the app that created them is gone.

This tool extracts all your photos and videos out of those libraries and organises them into a simple `YYYY/MM/DD/filename.ext` folder structure you can browse in any file explorer, import into Google Photos, or archive however you like.

It also handles a common problem with old Apple libraries: the same photo often exists in multiple places with different names (e.g. `IMG_0178.jpg` and a UUID-named copy like `8A198053-....jpeg`). The `dedup` tool finds these same-second duplicates and removes the smaller copy, so you don't end up with hundreds of gigabytes of duplicates.

## Install

Requires [Go 1.22+](https://go.dev/dl/).

```bash
git clone https://github.com/max-hill-4/photo-organizer
cd photo-organizer
go build -ldflags="-s -w" -o photo-organizer .
```

## Usage

```
./photo-organizer [flags] <source-dir> <dest-dir>
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--dry-run` | false | Print what would happen, no files copied |
| `--workers` | 4 | Parallel copy workers |
| `--buf-size` | 8192 | I/O buffer size in KB |
| `--log-file` | — | Write full JSON log to file |
| `--unknown-dir` | `<dest>/unknown` | Folder for files with no determinable date |
| `--verbose` | false | Log every file action |
| `--date-test` | — | Test date extraction on a single file and exit |

## Examples

**Dry run first (always recommended):**
```bash
./photo-organizer --dry-run --verbose /mnt/d/Pictures /mnt/c/Users/you/Pictures/Backup
```

**Full run:**
```bash
./photo-organizer --workers 4 --log-file run.json /mnt/d/Pictures /mnt/c/Users/you/Pictures/Backup
```

**Check what date a single file would get:**
```bash
./photo-organizer --date-test /mnt/d/Pictures/IMG_1234.JPG
# File:   /mnt/d/Pictures/IMG_1234.JPG
# Date:   2021-06-14
# Source: EXIF
```

**Tune workers for faster SSDs:**
```bash
./photo-organizer --workers 8 /source /dest
```

## How it works

### Date extraction (in priority order)

1. **EXIF** — reads `DateTimeOriginal` from JPEG, TIFF, RAW files
2. **HEIC** — parses EXIF from Apple HEIC files (pure Go, no C deps)
3. **Video mvhd** — reads creation time from MOV/MP4 container atom
4. **Filename** — matches patterns like `20210614_183000`, `2021-06-14`, `20210614`
5. **mtime** — falls back to file's last modified timestamp

Files where no reliable date can be found go into `<dest>/unknown/`.

### Duplicate handling

- Dest file exists, **same size** → skip (already copied)
- Dest file exists, **different size** → save as `filename_1.ext`, `filename_2.ext`, etc.

### Supported formats

`jpg jpeg png heic tiff tif cr2 nef arw dng mov mp4 m4v 3gp gif bmp webp aae`

macOS `._` resource fork files are automatically skipped.

## Output

```
=== Photo Organizer Summary ===
Files scanned:    13,097
  Copied:         12,077  (61.2 GB)
  Skipped (dup):  1,020
  Renamed:        3
  Errors:         0
  Unknown date:   0  (→ unknown/)

Date sources:
  EXIF:           12,049 ( 92%)
  HEIC:              421 (  3%)
  Video mvhd:         28 (  0%)
  Filename:            0 (  0%)
  mtime:             599 (  5%)

Elapsed: 47m 2s   Avg: 22.3 MB/s
```

Errors are logged line-by-line to `errors.log` in the working directory.

## Deduplication

Old Apple libraries frequently contain the same photo multiple times — once with its original camera name (`IMG_XXXX.jpg`) and again with a UUID name (`8A198053-441B-4B16-9DA0-3639CCE75AEC.jpeg`). These are created when Photos or iPhoto imports photos and re-encodes them internally.

The `dedup` tool detects these by comparing EXIF `DateTimeOriginal` timestamps. Two files in the same date folder taken at the exact same second are almost certainly the same photo. It keeps the largest version (highest quality) and deletes the smaller one.

**Build:**
```bash
go build -ldflags="-s -w" -o dedup ./cmd/dedup/
```

**Dry run first:**
```bash
./dedup --dry-run /mnt/c/Users/yourname/Pictures/Backup
```

**Delete duplicates:**
```bash
./dedup /mnt/c/Users/yourname/Pictures/Backup
```

The `photo-organizer` tool also applies this same-second dedup logic during copying, so if you run it fresh it won't create duplicates in the first place.

## Benchmarks

Tested on ~430 files (4.1 GB) from an Apple Photos library on a USB drive (WSL2, Windows 11).

| Tool | Files copied | Data | Time | Avg speed |
|---|---|---|---|---|
| **photo-organizer** | 357 | 4.1 GB | **4m 09s** | 16.7 MB/s |
| exiftool | 427 | 3.5 GB | 16m 38s | 3.5 MB/s |

**4x faster than exiftool.** photo-organizer also skips macOS `._` resource fork files that exiftool copies as junk, and organises everything into `YYYY/MM/DD` folders in one pass.

On a dry run (no I/O), photo-organizer processes the same files in under 1 second.
