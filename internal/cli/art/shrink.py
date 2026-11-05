#!/usr/bin/env python3
"""Derive the launch-view art from a full-size drawing.

The view's art is generated rather than drawn at its final size, so a
replacement drawing can be re-derived instead of hand-shrunk. Density in
each 2x2 block picks one of four ink levels.

    python3 internal/cli/art/shrink.py source.txt > internal/cli/art/spov.txt

@decision(DL-024)
"""
import sys

LEVELS = ((0.15, " "), (0.33, "."), (0.55, "*"), (1.01, "@"))
SCALE_Y = SCALE_X = 2


def crop(lines):
    """Trim blank rows and the common left margin."""
    while lines and not lines[-1].strip():
        lines.pop()
    ink = [i for i, l in enumerate(lines) if l.strip()]
    if not ink:
        return []
    lines = lines[ink[0]: ink[-1] + 1]
    left = min(len(l) - len(l.lstrip()) for l in lines if l.strip())
    return [l[left:] for l in lines]


def shrink(lines):
    width = max(len(l) for l in lines)
    lines = [l.ljust(width) for l in lines]
    out = []
    for y in range(0, len(lines), SCALE_Y):
        row = ""
        for x in range(0, width, SCALE_X):
            block = [
                lines[yy][xx]
                for yy in range(y, min(y + SCALE_Y, len(lines)))
                for xx in range(x, min(x + SCALE_X, width))
            ]
            density = sum(1 for c in block if not c.isspace()) / max(1, len(block))
            row += next(glyph for limit, glyph in LEVELS if density < limit)
        out.append(row.rstrip())
    return crop(out)


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    with open(sys.argv[1], encoding="utf-8") as f:
        source = crop(f.read().replace("\t", "    ").split("\n"))
    if not source:
        sys.exit("the source drawing is empty")
    print("\n".join(shrink(source)))


if __name__ == "__main__":
    main()
