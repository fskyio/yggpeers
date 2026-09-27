# Map assets

`internal/server/static/countries.pmtiles` contains Mapbox Vector Tiles (MVT) in a PMTiles v3 archive. It is served by the existing Go process with byte-range support, a content-derived ETag, and one-hour cache freshness. No separate tile server is needed.

`countries.json` is a small catalog joining the API's country names to stable tile codes. ISO alpha-2 codes are used where available; other Natural Earth features use `NE:<ADM0_A3>`. Some remote features with the same ISO code are grouped together. Small places also have label coordinates and a zoom threshold: their marker disappears once the main land component spans roughly 14 pixels. Only places with peers in the selected Total/Online mode get markers.

The boundary source is Natural Earth v5.1.2, `ne_10m_admin_0_countries.geojson`, pinned to commit `f1890d9f152c896d250a77557a5751a93d494776` and verified by SHA-256 in `build-map.py`. It is public-domain data from [Natural Earth](https://www.naturalearthdata.com/), at 1:10 million scale (not 10-metre accuracy). The archive contains zooms 0–7; MapLibre reuses the most detailed tiles above zoom 7, up to the UI's zoom limit of 12. This avoids storing redundant higher-zoom tiles for the same source detail.

## Regenerate boundaries

Requires Python 3 (standard library only) and [Tippecanoe 2.79.0](https://github.com/felt/tippecanoe/tree/2.79.0). Neither is needed for normal application builds. With Tippecanoe on `PATH`:

```sh
make map-build
```

Use `MAP_ARGS="--tippecanoe /path/to/tippecanoe"` for a local build, or `MAP_ARGS="--source /path/to/ne_10m_admin_0_countries.geojson"` to reuse the pinned source without downloading it again. Commit the generated archive and catalog together. Rebuild the Go binary/container to include them.

The generator retains separate country identities, simplifies shared borders consistently, and does not merge small polygons into their neighbours. Maximum-zoom geometry is not simplified beyond tile-coordinate quantization. The data source and generation options are also recorded in the archive metadata.

The vendored `pmtiles-4.5.0.js` comes from `pmtiles@4.5.0/dist/pmtiles.js` on npm; the BSD-3-Clause license and the bundled fflate MIT license are in `pmtiles-LICENSE.txt`. When updating the reader, keep the pinned filename, template reference, and licenses in sync.

## Verify

```sh
make test
make lint
```

Go checks exercise the production HTTP route's actual archive bytes, range headers, invalid ranges, ETag revalidation, and country/territory aliases and markers.

For browser checks, start the app with `make run` in one terminal, then install the temporary Playwright tools and run the check in another:

```sh
make map-tools
make map-check
```

Optional variables: `MAP_URL` (default `http://127.0.0.1:8080`), `MAP_QA_DIR` (default `/tmp/yggpeers-map-qa`), and `CHROMIUM_PATH` (an existing Chromium executable).

The browser check substitutes a fixed country-count fixture, verifies small-place discovery and zooming, Hong Kong/Singapore polygons and hover details, toggling before new tiles load, empty data, mobile layout, and graceful API failure. It asserts that archive requests are partial responses and writes screenshots plus `transfers.json`. Transfer figures count compressed archive response bodies at a 1600×900 viewport, excluding HTTP headers, the country catalog, scripts, and styles. The Hong Kong sample includes intermediate tiles loaded during the zoom animation; later views reuse already fetched archive data.

Example measured session with the bundled archive (decimal KB):

| View | Additional archive transfer |
| --- | ---: |
| Initial world view, zoom 2, cold cache | 224 KB |
| Click Hong Kong marker, animate to zoom 8 | 263 KB |
| Move from Hong Kong to Singapore, zoom 8 | 3 KB |
| Switch Total/Online without moving | 0 KB |

The full archive is 4.93 MB on disk. The separate country catalog is 24 KB and the PMTiles reader is 20 KB; MapLibre's existing CDN assets are unchanged. Transfer sizes depend on viewport, cache state, and animation timing, so use the browser check to measure your deployment.
