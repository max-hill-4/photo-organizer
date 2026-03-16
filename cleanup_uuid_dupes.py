#!/usr/bin/env python3
"""
cleanup_uuid_dupes.py — finds UUID-named files that are perceptual duplicates
of normally-named files in the same folder and deletes them.

Usage:
  python3 cleanup_uuid_dupes.py --dry-run /mnt/c/Users/maxwe/Pictures/Backup
  python3 cleanup_uuid_dupes.py /mnt/c/Users/maxwe/Pictures/Backup
"""

import argparse
import os
import re
import sys
from PIL import Image, ImageOps

UUID_RE = re.compile(
    r'^[A-F0-9]{8}-[A-F0-9]{4}-[A-F0-9]{4}-[A-F0-9]{4}-[A-F0-9]{12}\.[a-zA-Z0-9]+$',
    re.I
)
THRESHOLD = 10  # hamming distance <= this = duplicate

def dhash(path, size=16):
    try:
        img = Image.open(path)
        img = ImageOps.exif_transpose(img)
        img = img.convert('L').resize((size + 1, size), Image.LANCZOS)
        pixels = list(img.getdata())
        bits = []
        for row in range(size):
            for col in range(size):
                bits.append('1' if pixels[row * (size + 1) + col] > pixels[row * (size + 1) + col + 1] else '0')
        return ''.join(bits)
    except Exception:
        return None

def hamming(a, b):
    return sum(x != y for x, y in zip(a, b))

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('root')
    ap.add_argument('--dry-run', action='store_true')
    args = ap.parse_args()

    deleted_count = 0
    deleted_bytes = 0
    unique_count = 0
    error_count = 0
    folders_checked = 0

    for dirpath, _, files in os.walk(args.root):
        uuid_files = [f for f in files if UUID_RE.match(f)]
        normal_files = [f for f in files if not UUID_RE.match(f)]

        if not uuid_files or not normal_files:
            continue

        folders_checked += 1

        # Hash all normal files in this folder
        normal_hashes = []
        for f in normal_files:
            h = dhash(os.path.join(dirpath, f))
            if h:
                normal_hashes.append((f, h))

        if not normal_hashes:
            continue

        for uf in uuid_files:
            upath = os.path.join(dirpath, uf)
            uh = dhash(upath)
            if uh is None:
                error_count += 1
                continue

            best = min(normal_hashes, key=lambda x: hamming(uh, x[1]))
            dist = hamming(uh, best[1])

            if dist <= THRESHOLD:
                size = os.path.getsize(upath)
                deleted_count += 1
                deleted_bytes += size
                if args.dry_run:
                    print(f"[dup] {uf}  =~  {best[0]}  (dist={dist}, {size/1024/1024:.1f} MB)")
                else:
                    os.remove(upath)
                    if deleted_count % 500 == 0:
                        print(f"  ... deleted {deleted_count:,} files, freed {deleted_bytes/1024/1024/1024:.1f} GB", file=sys.stderr)
            else:
                unique_count += 1
                print(f"[unique] {uf}  best_match={best[0]}  dist={dist}")

    print()
    action = "Would delete" if args.dry_run else "Deleted"
    print(f"{action}:  {deleted_count:,} files  ({deleted_bytes/1024/1024/1024:.2f} GB)")
    print(f"Unique UUID files kept: {unique_count:,}")
    print(f"Errors: {error_count:,}")
    print(f"Folders checked: {folders_checked:,}")

if __name__ == '__main__':
    main()
