"""Generate the shared vector master, Windows icon, preview and runtime pixels."""
from pathlib import Path
from io import BytesIO
import struct
from PIL import Image, ImageDraw

ROOT = Path(__file__).resolve().parents[1]
ASSETS = ROOT / "rust" / "assets"
SIZE = 1024
POINTS = [(62, 77), (88, 179), (128, 111), (168, 179), (194, 77)]


def gradient(top, bottom):
    image = Image.new("RGBA", (SIZE, SIZE))
    pixels = image.load()
    for y in range(SIZE):
        t = y / (SIZE - 1)
        color = tuple(round(a + (b - a) * t) for a, b in zip(top, bottom)) + (255,)
        for x in range(SIZE):
            pixels[x, y] = color
    return image


def generate():
    ASSETS.mkdir(parents=True, exist_ok=True)
    scale = SIZE / 256
    mask = Image.new("L", (SIZE, SIZE))
    drawing = ImageDraw.Draw(mask)
    drawing.rounded_rectangle([8 * scale, 8 * scale, 248 * scale, 248 * scale],
                              radius=58 * scale, fill=255)
    image = gradient((49, 58, 91), (25, 28, 46))
    image.putalpha(mask)

    border = Image.new("RGBA", image.size)
    drawing = ImageDraw.Draw(border)
    drawing.rounded_rectangle([9 * scale, 9 * scale, 247 * scale, 247 * scale],
                              radius=57 * scale, outline=(202, 212, 255, 40), width=round(2 * scale))
    image = Image.alpha_composite(image, border)

    glyph_mask = Image.new("L", image.size)
    drawing = ImageDraw.Draw(glyph_mask)
    points = [(round(x * scale), round(y * scale)) for x, y in POINTS]
    width = round(18 * scale)
    drawing.line(points, fill=255, width=width, joint="curve")
    for x, y in points:
        radius = width / 2
        drawing.ellipse([x - radius, y - radius, x + radius, y + radius], fill=255)
    glyph = gradient((223, 230, 255), (149, 169, 255))
    glyph.putalpha(glyph_mask)
    image = Image.alpha_composite(image, glyph)
    image = image.resize((256, 256), Image.Resampling.LANCZOS)
    image.putalpha(image.getchannel("A").point(lambda alpha: 0 if alpha < 4 else alpha))
    image.save(ASSETS / "WorkBuddy2API.png")
    sizes = (16, 20, 24, 32, 48, 64, 128, 256)
    frames = []
    for size in sizes:
        frame = image.resize((size, size), Image.Resampling.LANCZOS)
        frame.putalpha(frame.getchannel("A").point(lambda alpha: 0 if alpha < 4 else alpha))
        if size == 128:
            (ASSETS / "WorkBuddy2API-128.rgba").write_bytes(frame.tobytes())
        buffer = BytesIO()
        frame.save(buffer, format="PNG")
        frames.append(buffer.getvalue())
    directory = bytearray(struct.pack("<HHH", 0, 1, len(sizes)))
    offset = 6 + 16 * len(sizes)
    for size, frame in zip(sizes, frames):
        directory.extend(struct.pack("<BBBBHHII", size % 256, size % 256, 0, 0, 1, 32, len(frame), offset))
        offset += len(frame)
    (ASSETS / "WorkBuddy2API.ico").write_bytes(directory + b"".join(frames))
    (ASSETS / "WorkBuddy2API.svg").write_text('''<svg xmlns="http://www.w3.org/2000/svg" width="256" height="256" viewBox="0 0 256 256">
  <defs>
    <linearGradient id="tile" gradientUnits="userSpaceOnUse" x1="0" y1="0" x2="0" y2="256"><stop stop-color="#313A5B"/><stop offset="1" stop-color="#191C2E"/></linearGradient>
    <linearGradient id="glyph" gradientUnits="userSpaceOnUse" x1="0" y1="0" x2="0" y2="256"><stop stop-color="#DFE6FF"/><stop offset="1" stop-color="#95A9FF"/></linearGradient>
  </defs>
  <rect x="8" y="8" width="240" height="240" rx="58" fill="url(#tile)"/>
  <rect x="9" y="9" width="238" height="238" rx="57" fill="none" stroke="#CAD4FF" stroke-opacity=".16" stroke-width="2"/>
  <path d="M62 77 L88 179 L128 111 L168 179 L194 77" fill="none" stroke="url(#glyph)" stroke-width="18" stroke-linecap="round" stroke-linejoin="round"/>
</svg>
''', encoding="utf-8")
    print(ASSETS / "WorkBuddy2API.ico")


if __name__ == "__main__":
    generate()
