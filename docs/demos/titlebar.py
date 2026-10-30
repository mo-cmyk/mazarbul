#!/usr/bin/env python3
"""Render a transparent PNG that overlays a centered macOS-style window title.

VHS draws the colored traffic-light dots (Set WindowBar "Colorful") but has no
title text, and this ffmpeg build lacks the drawtext filter, so we composite the
title ourselves: a full-canvas transparent PNG with the title centered in the
window bar, overlaid onto every GIF frame in render.sh's polish step.

Usage:
  titlebar.py --width W --height H --bar-y Y --title TEXT --out PNG [--size N]
"""
import argparse
import sys

from PIL import Image, ImageDraw, ImageFont

# Tried in order; first that loads wins. SF Pro is the real macOS UI font.
FONT_CANDIDATES = [
    "/Library/Fonts/SF-Pro-Text-Medium.otf",
    "/System/Library/Fonts/HelveticaNeue.ttc",
    "/System/Library/Fonts/Helvetica.ttc",
    "/System/Library/Fonts/Supplemental/Arial.ttf",
]
TITLE_COLOR = (168, 168, 176, 255)  # neutral mac-titlebar gray on a dark bar


def load_font(size):
    for path in FONT_CANDIDATES:
        try:
            return ImageFont.truetype(path, size)
        except OSError:
            continue
    return ImageFont.load_default()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--width", type=int, required=True)
    ap.add_argument("--height", type=int, required=True)
    ap.add_argument("--bar-y", type=int, required=True, help="vertical center of the window bar, px")
    ap.add_argument("--title", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--size", type=int, default=22)
    args = ap.parse_args()

    img = Image.new("RGBA", (args.width, args.height), (0, 0, 0, 0))
    draw = ImageDraw.Draw(img)
    font = load_font(args.size)
    # Center horizontally on the whole window, vertically on the bar.
    draw.text(
        (args.width / 2, args.bar_y),
        args.title,
        font=font,
        fill=TITLE_COLOR,
        anchor="mm",
    )
    img.save(args.out)
    print(f"titlebar: {args.out} ({args.width}x{args.height}, '{args.title}')", file=sys.stderr)


if __name__ == "__main__":
    main()
