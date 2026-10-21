"""Build the pinned, offline login-browser payload from the official release.

The Windows build uses the machine's Windows fonts; the optional cross-platform
font collection is omitted. Executables and other runtime files are unmodified.
"""
import hashlib
import json
from pathlib import Path
import urllib.request
import zipfile

ROOT = Path(__file__).resolve().parents[1]
TAG = "v156.0.1-beta.36"
ASSET = "camoufox-156.0.1-beta.36-win.x86_64.zip"
SHA256 = "096fd4832a7a32f2aa7f7bdd07fe6253dda296cbe783acd7bc2c86cf5ae62031"
SOURCE = f"https://github.com/daijro/camoufox/releases/download/{TAG}/{ASSET}"


def prepare():
    archive = ROOT / ".workbuddy" / ASSET
    if not archive.is_file():
        archive.parent.mkdir(parents=True, exist_ok=True)
        urllib.request.urlretrieve(SOURCE, archive)
    with archive.open("rb") as file:
        actual = hashlib.file_digest(file, "sha256").hexdigest()
    if actual != SHA256:
        raise RuntimeError("Official Camoufox release SHA-256 mismatch")
    target = ROOT / "browser" / "camoufox-win-x64.zip"
    target.parent.mkdir(parents=True, exist_ok=True)
    licenses = {}
    for name, path in {
        "MPL-2.0.txt": "LICENSE",
        "Cursory-LGPL.txt": "additions/juggler/input/cursory/LICENSE",
    }.items():
        with urllib.request.urlopen(f"https://raw.githubusercontent.com/daijro/camoufox/{TAG}/{path}", timeout=30) as response:
            licenses[name] = response.read()
    with zipfile.ZipFile(archive) as source, zipfile.ZipFile(target, "w", zipfile.ZIP_DEFLATED, compresslevel=6) as output:
        for item in source.infolist():
            if item.is_dir() or item.filename.startswith(("fonts/", "fontconfig/", "fontconfigs/")):
                continue
            if item.filename.startswith("/") or ".." in Path(item.filename).parts:
                raise RuntimeError("Unsafe upstream archive path")
            output.writestr(item.filename, source.read(item))
        for name, content in licenses.items():
            output.writestr(f"licenses/{name}", content)
        output.writestr("licenses/SOURCE.txt", f"Camoufox {TAG}\nSource and corresponding source code: https://github.com/daijro/camoufox/tree/{TAG}\nOfficial binary: {SOURCE}\nOfficial SHA-256: {SHA256}\nOptional cross-platform font data omitted; runtime binaries unmodified.\n")
    manifest = {
        "engine": "Camoufox", "version": TAG[1:], "source": SOURCE,
        "source_sha256": SHA256,
        "payload_sha256": hashlib.sha256(target.read_bytes()).hexdigest(),
        "payload_bytes": target.stat().st_size,
        "source_code": f"https://github.com/daijro/camoufox/tree/{TAG}",
        "fonts": "Native Windows fonts; optional cross-platform fonts omitted",
    }
    (target.parent / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(manifest, indent=2))


if __name__ == "__main__":
    prepare()
