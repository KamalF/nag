# Icon sources

`web/icons/` holds exactly four PNGs (§9.3) and nothing else — it is embedded
into the binary. The SVGs they are rendered from live here, so the set can be
regenerated after an edit:

```
rsvg-convert -w 192 -h 192 docs/icons/icon.svg          -o web/icons/192.png
rsvg-convert -w 512 -h 512 docs/icons/icon.svg          -o web/icons/512.png
rsvg-convert -w 512 -h 512 docs/icons/icon-maskable.svg -o web/icons/512-maskable.png
rsvg-convert -w  32 -h  32 docs/icons/icon-badge.svg    -o web/icons/32.png
```

`icon-badge.svg` is a different drawing, not a smaller one: see the comment in
the file for why it carries no plate.
