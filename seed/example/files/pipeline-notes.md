# Harbour district — art pipeline notes

Example document asset. Nothing here is real; it exists so the worked
example covers `asset_type: "document"` end to end.

## Blockout

Greybox the waterfront first, silhouette only. Nothing gets a texture
pass until the camera path is locked, because the camera decides which
faces are ever seen and half the district never is.

## Texture budget

- Hero buildings: 2048x2048, one material each.
- Background flats: 512x512, shared atlas.
- Water: procedural, no source texture.

## Review

Ping the reviewer when the blockout lands, not when it is finished.
