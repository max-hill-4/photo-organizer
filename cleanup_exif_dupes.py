#!/usr/bin/env python3
"""
cleanup_exif_dupes.py — single-pass exiftool scan to find files sharing the
same folder + DateTimeOriginal. UUID-named files that match a normally-named
file are deleted.

Usage:
  python3 cleanup_exif_dupes.py --dry-run /mnt/c/Users/maxwe/Pictures/Backup
  python3 cleanup_exif_dupes.py /mnt/c/Users/maxwe/Pictures/Backup
"""

import argparse
import os
import re
import subprocess
import sys
from collections import defaultdict

UUID_RE = re.compile(
    r'^[A-F0-9]{8}-[A-F0-9]{4}-[A-F0-9]{4}-[A-F0-9]{4}-[A-F0-9]{12}\.[a-zA-Z0-9]+$',
    re.I
)

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('root')
    ap.add_argument('--dry-run', action='store_true')
    args = ap.parse_args()

    print("Running exiftool scan (single pass)...", file=sys.stderr)

    # Single exiftool call across entire tree
    result = subprocess.run(
        ['exiftool', '-r', '-T', '-Directory', '-FileName', '-DateTimeOriginal', args.root],
        capture_output=True, text=True, timeout=7200
    )

    print(f"Parsing results...", file=sys.stderr)

    # Group by (folder, timestamp) -> list of filenames
    groups = defaultdict(list)
    for line in result.stdout.splitlines():
        parts = line.split('\t')
        if len(parts) != 3:
            continue
        folder, fname, ts = parts[0].strip(), parts[1].strip(), parts[2].strip()
        if ts and ts != '-':
            groups[(folder, ts)].append(fname)

    deleted_count = 0
    deleted_bytes = 0

    for (folder, ts), files in groups.items():
        if len(files) < 2:
            continue

        keep = [f for f in files if not UUID_RE.match(f)]
        delete = [f for f in files if UUID_RE.match(f)]

        if not keep or not delete:
            continue

        for f in delete:
            path = os.path.join(folder, f)
            if not os.path.exists(path):
                continue
            size = os.path.getsize(path)
            deleted_count += 1
            deleted_bytes += size
            if args.dry_run:
                print(f"[dup] {f}  =  {keep[0]}  ts={ts}  ({size/1024/1024:.1f} MB)")
            else:
                os.remove(path)
                if deleted_count % 500 == 0:
                    print(f"  ... {deleted_count:,} deleted, {deleted_bytes/1024/1024/1024:.1f} GB freed", file=sys.stderr)

    print()
    action = "Would delete" if args.dry_run else "Deleted"
    print(f"{action}: {deleted_count:,} files  ({deleted_bytes/1024/1024/1024:.2f} GB freed)")

if __name__ == '__main__':
    main()
