#!/usr/bin/env python3
"""
cleanup_dupes.py — finds _1/_2/etc renamed files that are identical to their
original and deletes them. Run with --dry-run first to preview.

Usage:
  python3 cleanup_dupes.py --dry-run /mnt/c/Users/maxwe/Pictures/Backup
  python3 cleanup_dupes.py /mnt/c/Users/maxwe/Pictures/Backup
"""

import argparse
import hashlib
import os
import re
import sys

SUFFIX_RE = re.compile(r'^(.+)_(\d+)(\.[^.]+)$')

def md5(path, chunk=8 * 1024 * 1024):
    h = hashlib.md5()
    with open(path, 'rb') as f:
        while chunk := f.read(chunk):
            h.update(chunk)
    return h.hexdigest()

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('root', help='Backup root directory to scan')
    ap.add_argument('--dry-run', action='store_true', help='Print only, do not delete')
    args = ap.parse_args()

    deleted_count = 0
    deleted_bytes = 0
    checked = 0

    for dirpath, _, files in os.walk(args.root):
        for fname in files:
            m = SUFFIX_RE.match(fname)
            if not m:
                continue

            base, _num, ext = m.group(1), m.group(2), m.group(3)
            original = os.path.join(dirpath, base + ext)
            duplicate = os.path.join(dirpath, fname)

            if not os.path.exists(original):
                continue

            checked += 1
            try:
                h1 = md5(original)
                h2 = md5(duplicate)
            except Exception as e:
                print(f"ERROR hashing {duplicate}: {e}", file=sys.stderr)
                continue

            if h1 == h2:
                size = os.path.getsize(duplicate)
                deleted_count += 1
                deleted_bytes += size
                if args.dry_run:
                    print(f"[dry-run] would delete {duplicate}  ({size/1024/1024:.1f} MB)")
                else:
                    os.remove(duplicate)
                    print(f"deleted {duplicate}  ({size/1024/1024:.1f} MB)")
            else:
                print(f"kept {duplicate}  (different content)")

            if checked % 100 == 0:
                print(f"  ... checked {checked} files, freed {deleted_bytes/1024/1024/1024:.2f} GB so far", file=sys.stderr)

    print()
    action = "Would delete" if args.dry_run else "Deleted"
    print(f"{action} {deleted_count:,} files, freeing {deleted_bytes/1024/1024/1024:.2f} GB")
    print(f"Checked {checked:,} suffixed files total")

if __name__ == '__main__':
    main()
