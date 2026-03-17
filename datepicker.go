package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

// DateSource indicates how the date was determined.
type DateSource int

const (
	DateSourceEXIF DateSource = iota
	DateSourceHEIC
	DateSourceVideo
	DateSourceFilename
	DateSourceMtime
)

func (d DateSource) String() string {
	switch d {
	case DateSourceEXIF:
		return "EXIF"
	case DateSourceHEIC:
		return "HEIC"
	case DateSourceVideo:
		return "Video mvhd"
	case DateSourceFilename:
		return "Filename"
	default:
		return "mtime"
	}
}

var filenamePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(\d{4})(\d{2})(\d{2})_(\d{2})(\d{2})(\d{2})`),
	regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})`),
	regexp.MustCompile(`(\d{4})(\d{2})(\d{2})`),
}

var exifExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".tiff": true, ".tif": true,
	".cr2": true, ".nef": true, ".arw": true, ".dng": true,
	".png": true,
}

// mvhdEpoch: seconds between 1904-01-01 and 1970-01-01
const mvhdEpoch int64 = -2082844800

func ExtractDate(path string, mtime time.Time) (time.Time, DateSource) {
	ext := strings.ToLower(filepath.Ext(path))

	if exifExts[ext] {
		if t, ok := exifDate(path); ok {
			return t, DateSourceEXIF
		}
	}

	if ext == ".heic" {
		if t, ok := heicDate(path); ok {
			return t, DateSourceHEIC
		}
	}

	if ext == ".mov" || ext == ".mp4" || ext == ".m4v" || ext == ".3gp" {
		if t, ok := videoDate(path); ok {
			return t, DateSourceVideo
		}
	}

	if t, ok := filenameDate(filepath.Base(path)); ok {
		return t, DateSourceFilename
	}

	return mtime, DateSourceMtime
}

func exifDate(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	x, err := exif.Decode(f)
	if err != nil {
		return time.Time{}, false
	}
	t, err := x.DateTime()
	if err != nil || t.IsZero() || t.Year() < 1990 {
		return time.Time{}, false
	}
	return t, true
}

// heicDate extracts EXIF from an HEIC/HEIF file using a pure-Go ISOBMFF parser.
// It scans for the Exif item inside the meta box and passes raw EXIF bytes to goexif.
func heicDate(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()

	// Read up to 512 KB — EXIF is always in the first few hundred bytes
	data, err := io.ReadAll(io.LimitReader(f, 512*1024))
	if err != nil {
		return time.Time{}, false
	}

	// Apple iPhone HEIC: EXIF bytes are preceded by the 6-byte "Exif\x00\x00" marker.
	// After that marker comes the TIFF header (II or MM).
	marker := []byte("Exif\x00\x00")
	idx := bytes.Index(data, marker)
	if idx < 0 {
		return time.Time{}, false
	}
	raw := data[idx+len(marker):]
	if len(raw) < 8 {
		return time.Time{}, false
	}

	x, err := exif.Decode(bytes.NewReader(raw))
	if err != nil {
		return time.Time{}, false
	}
	t, err := x.DateTime()
	if err != nil || t.IsZero() || t.Year() < 1990 {
		return time.Time{}, false
	}
	return t, true
}

func videoDate(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()

	// mvhd atom is always within the first few KB of a well-formed file
	data, err := io.ReadAll(io.LimitReader(f, 512*1024))
	if err != nil {
		return time.Time{}, false
	}

	idx := bytes.Index(data, []byte("mvhd"))
	if idx < 0 || idx+28 > len(data) {
		return time.Time{}, false
	}

	box := data[idx+4:]
	version := box[0]
	var secs int64
	if version == 1 {
		if len(box) < 28 {
			return time.Time{}, false
		}
		secs = int64(binary.BigEndian.Uint64(box[4:12]))
	} else {
		if len(box) < 12 {
			return time.Time{}, false
		}
		secs = int64(binary.BigEndian.Uint32(box[4:8]))
	}

	t := time.Unix(secs+mvhdEpoch, 0).UTC()
	if t.Year() < 1990 || t.Year() > 2100 {
		return time.Time{}, false
	}
	return t, true
}

func filenameDate(name string) (time.Time, bool) {
	for _, re := range filenamePatterns {
		m := re.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		var t time.Time
		var err error
		if len(m) == 7 {
			t, err = time.Parse("20060102150405", m[1]+m[2]+m[3]+m[4]+m[5]+m[6])
		} else {
			t, err = time.Parse("20060102", m[1]+m[2]+m[3])
		}
		if err == nil && t.Year() >= 1990 && t.Year() <= 2100 {
			return t, true
		}
	}
	return time.Time{}, false
}
