# Favicon provenance

The current PNG icons are Nocturne artwork derived from the selected image at
`docs/brand-source/selection.json` and its original crop. The selected artwork hash
is `35957fdac390a8754998942c0df0ae4a2750ff78cee6540419fa2436701b7c01`. Its
selection record documents crop and pixel verification. This documents provenance
only and makes no rights claim about unrelated artwork.

| Current file | SHA-256 |
| --- | --- |
| `favicon-16x16.png` | `c29855045a1f5c3f31e132b131bac2bd18236e8a68cda8d01f532dfbe6acb5e1` |
| `favicon-32x32.png` | `e3a41df7136a838e642c601608c2f74f3fffc297e3072050b3fa5e8f0b16397d` |
| `apple-touch-icon.png` | `70ec797347b64df351d51a2467ff5faba939d175a1c834a8cc68b21e2e6edbb1` |

Regenerated from `web/src/assets/nocturne-selected.png` with Pillow 12.3.0:

```python
from PIL import Image

source = Image.open("web/src/assets/nocturne-selected.png").convert("RGBA")
for name, size in [("favicon-16x16.png", 16), ("favicon-32x32.png", 32),
                   ("apple-touch-icon.png", 180)]:
    source.resize((size, size), Image.Resampling.LANCZOS).save(
        "web/src/assets/" + name, format="PNG", optimize=False, compress_level=9)
```

Each decoded RGBA output was checked pixel-for-pixel against that resize.
Pillow is a regeneration tool, not an application dependency.

PNG assets are bundled with content-hashed names under `/admin/assets/`, the
existing Python/Go static route. ICO is not shipped because that route does not
serve its image MIME type. No backend route or authentication changes are needed.

