# yggpeers

A webapp that tracks the reachability of public peers for the Yggdrasil network. Supports JSON and CSV output, a peer world map, statistics, and more.

## Deploying

### Container (recommended)

```sh
docker run -p 8080:8080 foundry.fsky.io/fsky/yggpeers:latest
```

A [Quadlet](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html) unit file is available at [`contrib/quadlet/yggpeers.container`](contrib/quadlet/yggpeers.container) for deploying with Podman and systemd.

### From source

Requires Go 1.26+.

```sh
go build -o yggpeers cmd/yggpeers/main.go
./yggpeers
```

Then open http://localhost:8080 in your browser.

## Map

Country and territory boundaries are bundled as vector tiles in a PMTiles archive. The browser downloads only the portions needed for the current view, with finer detail as you zoom in. No map tile service or API key is required. Hong Kong, Singapore, and other small places with peers have hoverable markers when zoomed out; clicking a marker zooms to its boundary.

The archive is about 4.9 MB on the server, not a full download per visitor. Peer counts are fetched separately, and switching Total/Online reuses the loaded geometry. The map uses Natural Earth's 1:10 million boundaries, which provide regional rather than street-level detail.

Map assets are embedded in the binary. Normal Go and container builds need no extra tools. See [map maintenance and verification](scripts/README.md) to regenerate the assets or run the browser checks. A reverse proxy must preserve HTTP byte-range responses for `/static/countries.pmtiles` and must not apply whole-file gzip/Brotli compression to that archive.

## Configuration

Configuration is handled through environment variables:

| Variable                | Default               | Description                                             |
| ----------------------- | --------------------- | ------------------------------------------------------- |
| `LISTEN_ADDR`           | `:8080`               | Address for the HTTP server to listen on.               |
| `DB_PATH`               | `./data/yggpeers.db`  | Path to the SQLite database file.                       |
| `FETCH_INTERVAL`        | `15m`                 | How often to refresh the public-peers snapshot.         |
| `CHECK_INTERVAL`        | `5m`                  | How often to run reachability checks.                   |
| `DIAL_TIMEOUT`          | `5s`                  | Timeout for individual network dials.                   |
| `MAX_CONCURRENT_CHECKS` | `50`                  | Maximum number of concurrent peer checks.               |
| `BATCH_DELAY`           |                       | Optional delay between check batches (duration string). |
| `CHECK_DARKNET`         |                       | Any non-empty value enables darknet peer checks.        |
| `DEBUG`                 |                       | Any non-empty value enables debug logging.              |

## License

The code of this project is released under the [Unlicense](https://unlicense.org/). See the `LICENSE` file for details.

Bundled map data is public-domain [Natural Earth](https://www.naturalearthdata.com/about/terms-of-use/) data. The PMTiles reader and its bundled dependency retain their [third-party licenses](internal/server/static/pmtiles-LICENSE.txt).
