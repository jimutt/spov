#!/usr/bin/env python3
"""Fail on comment blocks longer than a limit.

A comment says what the code does, plus anything local and
non-obvious, then @decision(DL-NNN). The rationale lives in the
decision record, so a long block is usually a record being restated.
Put `comment-block: allow` anywhere in a block that has a reason to be
long, such as a spike's findings in an integration test.

Usage: scripts/comment-blocks.py [--max N] [--top K]
  --max N  longest block allowed (default 20)
  --top K  list the K longest blocks and exit 0, as a report
"""
import argparse
import os
import re
import subprocess
import sys

ALLOW = "comment-block: allow"

# Generated or append-only files, and prose.
SKIP = re.compile(
    r"^(decisions/|docs/|.*\.md$"
    r"|web/src/lib/api/types\.ts$"
    r"|internal/store/postgres/(sqlcgen|migrations|queries)/)"
)

# Line comment markers per extension; block comments are tracked separately.
LINE = {
    ".go": "//", ".ts": "//", ".js": "//", ".mjs": "//",
    ".svelte": "//", ".astro": "//",
    ".sh": "#", ".py": "#", ".sql": "--",
}
BLOCK = {
    ".go": [("/*", "*/")],
    ".ts": [("/*", "*/")], ".js": [("/*", "*/")], ".mjs": [("/*", "*/")],
    ".css": [("/*", "*/")],
    ".svelte": [("/*", "*/"), ("<!--", "-->")],
    ".astro": [("/*", "*/"), ("<!--", "-->")],
}


def comment_lines(ext, lines):
    """Yield (index, is_comment) for each line."""
    marker = LINE.get(ext)
    pairs = BLOCK.get(ext, [])
    close = None
    for i, raw in enumerate(lines):
        s = raw.strip()
        if close:
            yield i, True
            if close in s:
                close = None
            continue
        opened = next((p for p in pairs if s.startswith(p[0])), None)
        if opened:
            yield i, True
            if opened[1] not in s[len(opened[0]):]:
                close = opened[1]
            continue
        if marker and s.startswith(marker) and not s.startswith("#!"):
            yield i, True
            continue
        yield i, False


def blocks(path):
    ext = os.path.splitext(path)[1]
    if ext not in LINE and ext not in BLOCK:
        return
    try:
        with open(path, encoding="utf-8") as f:
            lines = f.read().split("\n")
    except (OSError, UnicodeDecodeError):
        return
    start = None
    for i, is_comment in comment_lines(ext, lines):
        if is_comment and start is None:
            start = i
        elif not is_comment and start is not None:
            yield start, i, lines[start:i]
            start = None
    if start is not None:
        yield start, len(lines), lines[start:]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--max", type=int, default=20)
    ap.add_argument("--top", type=int, default=0)
    args = ap.parse_args()

    os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
    files = subprocess.run(
        ["git", "ls-files"], capture_output=True, text=True, check=True
    ).stdout.split()

    found = []
    for path in files:
        if SKIP.match(path):
            continue
        for a, b, text in blocks(path):
            if any(ALLOW in t for t in text):
                continue
            found.append((b - a, path, a + 1))

    if args.top:
        for n, path, line in sorted(found, reverse=True)[: args.top]:
            print(f"{n:4}  {path}:{line}")
        return 0

    over = sorted((f for f in found if f[0] > args.max), reverse=True)
    for n, path, line in over:
        print(f"{path}:{line}: {n}-line comment block (max {args.max})")
    if over:
        print(
            f"\n{len(over)} comment block(s) over {args.max} lines. Keep what the code "
            "does and its local constraints; move the why into the decision record. "
            f"If a block must stay long, add `{ALLOW}` to it.",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
