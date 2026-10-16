# Nocturne visual identity

Nocturne is an independent project. Its visual identity is documented here without
claiming rights in unrelated artwork or third-party code.

## Selected production icon

The selected artwork is option 2, the middle icon of the first three-option sheet.
`docs/brand-source/selection.json` records its crop, pixel verification and hashes.

The selected source sheet is cropped at left 442/top 442, size 368 × 368, then
rotated exactly 90 degrees counterclockwise. The crop retains the forest badge and
symbol pixels without redesign or recoloring.

`docs/brand-source/option-2-original-crop.png` preserves the unrotated crop;
`web/src/assets/nocturne-selected.png` is the rotated artwork. The selected image
SHA-256 is `35957fdac390a8754998942c0df0ae4a2750ff78cee6540419fa2436701b7c01`.
The selection record includes objective crop bounds and pixel verification.

The interface palette uses mint `#1bfc9e`, forest `#051111`, surface `#121d1d`,
teal `#075555`, and light grey `#f3f3f3`. Dark mode uses mint on forest; light
mode uses teal on off-white for readable text and controls. Muted colors and
borders are adapted for contrast. System fonts remain local; no external font,
analytics, or tracking requests were added.
- `web/src/assets/nocturne.svg`: self-contained SVG embedding selected PNG pixels.
- `web/src/components/BrandMark.vue`: selected raster on its original forest badge.
- PNG fallbacks: 16, 32 and 180 pixels, downsampled from the rotated original.
- Public header and favicon embed the same selected image without new routes.
- Admin assets are Vite content-hashed and served through existing admin routes.


Both public and admin default to the system color scheme and respond to OS theme
changes without reload. Admin retains its existing explicit manual preference;
no preference means system. CSS supplies the correct initial dark surface before
Vue mounts, and an explicit light class preserves manual light mode. Browser
chrome theme-color metadata follows prefers-color-scheme. The selected icon
retains its forest badge and mint symbol for visibility in both themes.
