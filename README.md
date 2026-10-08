# yggpeers

A webapp that tracks the reachability of public peers for the Yggdrasil network. Supports per-peer detail pages, JSON and CSV output, a peer world map, statistics, and more.

## Deploying

### Container (recommended)

```sh
docker run -p 8080:8080 gitfield.org/fsky/yggpeers:latest
```

A [Quadlet](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html) unit file is available at [`contrib/quadlet/yggpeers.container`](contrib/quadlet/yggpeers.container) for deploying with Podman and systemd.

### From source

Requires Go 1.26+.

```sh
make build
./build/yggpeers
```

Use `make run` to start directly with `go run`. `make test`, `make lint`, and `make fmt` run the common test, vet, and formatting commands; `make check` runs the full checks.

Then open http://localhost:8080 in your browser.

See `make help` for available commands.

## Peer details

Click an endpoint in the peer list to open `/peer/{id}`. Each page shows the full connection URI, destination host and port, protocol, listed country or overlay network, last observed status and check timestamp, check success percentage, related endpoints, and cached IP/ASN metadata. SOCKS destinations are displayed separately from their local proxy. Copy buttons directly beside URIs and IP addresses preserve the complete value, including connection options, on both secure and ordinary HTTP pages. Successful copies show a brief confirmation; unavailable copying controls stay hidden.

The history bar contains 168 hourly segments covering the preceding seven days: green for all recorded checks passing, red for all failing, amber for mixed results, and grey for no observations. Hover, tap, or use the arrow keys for sample counts; the expandable hourly table works without JavaScript. Missing samples are unknown, not downtime. Status becomes stale after three configured check intervals, and disabled overlay monitoring is explicitly labelled. Probe results do not verify a Yggdrasil handshake or continuous availability.

Raw checks retain a rolling seven days during normal operation. Daily aggregates use an inclusive UTC-date cutoff and may include eight date buckets; the displayed success percentage uses those aggregates, not time-weighted uptime. Pruning runs after checker cycles. Removing an endpoint from the upstream list deletes its history; a returning endpoint gets a new ID and history. Last-check timestamps are stored independently of raw-history pruning, with existing observations backfilled automatically when the database is upgraded.

Public destinations are enriched through background DNS and [Team Cymru's DNS IP-to-ASN service](https://www.team-cymru.com/ip-asn-mapping), with no API key or separate WHOIS/RDAP client. DNS snapshots are fresh for 15 minutes and ASN snapshots for 24 hours; the background worker checks for due entries every minute, independently of upstream list changes. Empty results and failed lookups are retried after a minute. These are application cache windows, not authoritative DNS TTLs. Metadata is persisted across restarts; page visits never trigger lookups. Public IPs are disclosed to the ASN lookup service, but overlay, local, private and special-use destinations are excluded.

Related endpoints share the same normalized destination hostname/IP or directly overlap in fresh public DNS addresses. Parent domains, subnets and ASNs do not establish a relationship, and stale DNS answers are excluded. Shared infrastructure is not proof of a shared Yggdrasil node; each endpoint keeps its own status and history.

Country mini-maps reuse the bundled map boundaries and aliases, initially fitting the main land component rather than the whole world. Zoom buttons and panning are available; cooperative gestures avoid capturing ordinary page scrolling. Country comes from the public-peers list, not ASN registration data or IP geolocation. Unknown country labels and overlay networks are not assigned an invented location.

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
