# yggpeers

A webapp that tracks the reachability of public peers for the Yggdrasil network using independently deployed probes. Supports per-peer detail pages, JSON and CSV output, a peer world map, statistics, and more. The main instance fetches the catalog, coordinates measurements, enriches metadata, and serves the site; it never probes peer connections.

## Deploying

### Container (recommended)

Build the main and probe images from this checkout:

```sh
podman build -f Containerfile -t yggpeers .
podman build -f Containerfile --target probe -t gitfield.org/fsky/yggpeers-probe:latest .
docker run -d --name yggpeers -p 8080:8080 \
  -e DB_PATH=/data/yggpeers.db -v yggpeers-data:/data yggpeers
docker exec yggpeers yggpeers probes add "Frankfurt"
```

The last command prints a probe ID and `PROBE_TOKEN` once. Save only the token value in a private `frankfurt.token` file on the probe machine, then run:

```sh
docker run -d --name yggpeers-probe \
  -e SERVER_URL=https://peers.example.org \
  -e PROBE_TOKEN_FILE=/run/secrets/probe.token \
  --mount type=bind,src="$PWD/frankfurt.token",dst=/run/secrets/probe.token,readonly \
  gitfield.org/fsky/yggpeers-probe:latest
```

Expose the main instance through an HTTPS reverse proxy with a trusted certificate. Probes need outbound HTTPS to it, and outbound TCP/UDP for peer transports; no inbound port or database volume is needed on probes. Credentials are not included in either image.

Quadlet units are available at [`contrib/quadlet/yggpeers.container`](contrib/quadlet/yggpeers.container) and [`contrib/quadlet/yggpeers-probe.container`](contrib/quadlet/yggpeers-probe.container). The probe Quadlet references `gitfield.org/fsky/yggpeers-probe:latest`; the build command above creates that tag locally without publishing it. Its unit uses `.env.probe` for the probe environment and has no published port. Supply overlay routing separately if needed.

### From source

Requires Go 1.26+.

```sh
make build                         # build/yggpeers and build/yggpeers-probe
./build/yggpeers probes add "Frankfurt"
./build/yggpeers
```

On each probe machine, deploy `build/yggpeers-probe` and its own provisioned token:

```sh
SERVER_URL=https://peers.example.org \
PROBE_TOKEN_FILE=/path/to/frankfurt.token \
./build/yggpeers-probe
```

For local use, `SERVER_URL=http://127.0.0.1:8080` is permitted. Other plain-HTTP origins are rejected. `make build-server` / `make build-probe` build individual roles; the probe builds without CGO or SQLite. `make run` / `make run-probe` start each role directly with `go run`. `make test`, `make lint`, and `make fmt` run the common test, vet, and formatting commands; `make check` runs the full checks.

Then open http://localhost:8080 in your browser.

See `make help` for available commands.

## Distributed measurements

Provision one named identity per probe machine. Authentication registers the probe on its first check-in; unused credentials remain pending and are not expected to report. New registrations and capability changes join the next scheduled round, so first measurements can take up to `CHECK_INTERVAL` to begin. The main instance by itself reports unknown reachability.

Each round snapshots the current catalog and every registered, enabled probe. Every eligible probe checks each assigned peer; probes are not merely splitting the catalog between them.

| Evidence in a round | Peer status |
| --- | --- |
| Any eligible probe succeeds | `up` immediately |
| All eligible probes explicitly fail | `down` |
| No success and any required report is missing when the deadline expires | `unknown` |
| No eligible probes | `unknown` |

Offline probes remain expected until explicitly disabled. Missing reports, cancelled work and unsupported overlay coverage are not failed connections. `CHECK_DARKNET` is a per-probe opt-in; a capable probe must actually have the required network access. Enabling it does not install proxy services or overlay routing.

Rounds and observations are persisted. Duplicate submissions are idempotent, conflicting replacements are rejected, and new results after the deadline cannot change a closed round. A server outage leaves a measurement gap, not fabricated availability samples. A previous status expires at the expected deadline of the next round (`round start + CHECK_INTERVAL + ROUND_TIMEOUT`).

Manage identities on the main host, using the same `DB_PATH` as the running instance:

```sh
./build/yggpeers probes list
./build/yggpeers probes disable PROBE_ID
./build/yggpeers probes enable PROBE_ID
./build/yggpeers probes rotate PROBE_ID
```

Disabling rejects access immediately and removes the identity from subsequent round rosters; existing rosters remain fixed. Rotating prints a replacement token once and revokes the old token. Restart that probe with its new credential. Store token files privately, never commit them, and use a unique credential per machine. Registered probes are trusted: under the any-success rule, a dishonest probe could falsely report a peer up. Probe traffic scales with the number of eligible probes; choose concurrency, timeout and batch delay so a complete sweep fits inside `ROUND_TIMEOUT`.

### Upgrading an existing instance

Back up SQLite before upgrading. The schema migration preserves existing peer IDs, timestamps, raw observations, daily aggregates and metadata, but replaces boolean status with `up` / `down` / `unknown`. Old history remains single-location evidence with no invented probe attribution. Current reachability is unknown until fresh distributed evidence arrives. There is no embedded checker fallback: deploy at least one standalone probe, even for a single-machine installation.

**Public API breaking change:** `/peers.json`, `/peers.csv` and `/api/v1/peers` now use `status` instead of `is_up`. Use `?status=up`, `down` or `unknown`; the obsolete `is_up` parameter is rejected rather than treated as an alias. Exports include observation/validity timestamps, round/probe counts, and `coverage_pct`. CSV remains one row per peer. See `/api` for the current schema; update external consumers when deploying this release.


## Peer details

Click an endpoint in the peer list to open `/peer/{id}`. Each page shows the full connection URI, destination host and port, protocol, listed country or overlay network, fresh aggregate status and measurement timestamp, known-round success percentage and measurement coverage, related endpoints, and cached IP/ASN metadata. The distributed results panel shows successful probes out of the registered enabled probes in the published round, reporting/eligible counts, and each probe's successful, failed, missing or ineligible outcome. The roster denominator is fixed for that round, not silently reduced when a probe disconnects. SOCKS destinations are displayed separately from their local proxy. Copy buttons directly beside URIs and IP addresses preserve the complete value, including connection options, on both secure and ordinary HTTP pages. Successful copies show a brief confirmation; unavailable copying controls stay hidden.

The history bar contains 168 hourly segments covering the preceding seven days: green for all known rounds succeeding, red for all failing, amber for mixed known results, and grey for unknown/no observations. Hatching identifies hours containing unknown rounds, including incomplete coverage alongside known results. Hover, tap, or use the arrow keys for sample counts; the expandable hourly table works without JavaScript. Missing measurements are unknown, not downtime. Probe results preserve the existing transport reachability checks; they do not verify a Yggdrasil handshake or continuous availability.

Each completed round contributes one aggregate sample per peer, regardless of probe count. Unknown rounds are excluded from the success-percentage denominator and included in measurement coverage (`known / (known + unknown)`); coverage is a proportion of recorded rounds, not time-weighted coverage across a server outage. Raw checks and detailed probe rounds retain a rolling seven days during normal operation. Daily aggregates use an inclusive UTC-date cutoff and may include eight date buckets; the displayed percentages use those aggregates. Pruning runs during central round maintenance. Removing an endpoint from the upstream list deletes its history; a returning endpoint gets a new ID, so old in-flight results cannot affect it. Last-measurement timestamps survive raw-history pruning.

Public destinations are enriched through background DNS and [Team Cymru's DNS IP-to-ASN service](https://www.team-cymru.com/ip-asn-mapping), with no API key or separate WHOIS/RDAP client. DNS snapshots are fresh for 15 minutes and ASN snapshots for 24 hours; the background worker checks for due entries every minute, independently of upstream list changes. Empty results and failed lookups are retried after a minute. These are application cache windows, not authoritative DNS TTLs. Metadata is persisted across restarts; page visits never trigger lookups. Public IPs are disclosed to the ASN lookup service, but overlay, local, private and special-use destinations are excluded.

Related endpoints share the same normalized destination hostname/IP or directly overlap in fresh public DNS addresses. Parent domains, subnets and ASNs do not establish a relationship, and stale DNS answers are excluded. Shared infrastructure is not proof of a shared Yggdrasil node; each endpoint keeps its own status and history.

Country mini-maps reuse the bundled map boundaries and aliases, initially fitting the main land component rather than the whole world. Zoom buttons and panning are available; cooperative gestures avoid capturing ordinary page scrolling. Country comes from the public-peers list, not ASN registration data or IP geolocation. Unknown country labels and overlay networks are not assigned an invented location.

## Map

Country and territory boundaries are bundled as vector tiles in a PMTiles archive. The browser downloads only the portions needed for the current view, with finer detail as you zoom in. No map tile service or API key is required. Hong Kong, Singapore, and other small places with peers have hoverable markers when zoomed out; clicking a marker zooms to its boundary.

The archive is about 4.9 MB on the server, not a full download per visitor. Peer counts are fetched separately, and switching Total/Online reuses the loaded geometry. The map uses Natural Earth's 1:10 million boundaries, which provide regional rather than street-level detail.

Map popups and statistics distinguish online, offline and unknown peers. `total - online` is not an offline count. The map is still peer-country geography, not probe geography; country counts are a page-load snapshot.

Map assets are embedded in the binary. Normal Go and container builds need no extra tools. See [map maintenance and verification](scripts/README.md) to regenerate the assets or run the browser checks. A reverse proxy must preserve HTTP byte-range responses for `/static/countries.pmtiles` and must not apply whole-file gzip/Brotli compression to that archive.

## Configuration

Configuration is handled through environment variables. Main-instance intervals must be positive, and `ROUND_TIMEOUT` must be positive and shorter than `CHECK_INTERVAL`.

### Main instance

| Variable | Default | Description |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8080` | HTTP listener, normally behind an HTTPS reverse proxy. |
| `DB_PATH` | `./data/yggpeers.db` | Central SQLite database; also used by management commands. |
| `FETCH_INTERVAL` | `15m` | Public-peer catalog refresh interval. |
| `CHECK_INTERVAL` | `5m` | Interval between centrally scheduled measurement rounds. |
| `ROUND_TIMEOUT` | 80% of `CHECK_INTERVAL` (`4m` by default) | Deadline for a round's results. |

### Standalone probe

| Variable | Default | Description |
| --- | --- | --- |
| `SERVER_URL` | required | Main instance's HTTPS origin, without a path/query; loopback HTTP is allowed. |
| `PROBE_TOKEN` | | Provisioned bearer token; set exactly one token source. |
| `PROBE_TOKEN_FILE` | | File containing only the token value; surrounding whitespace is trimmed. |
| `DIAL_TIMEOUT` | `5s` | Positive timeout for an individual peer check. |
| `MAX_CONCURRENT_CHECKS` | `50` | Positive maximum number of parallel checks. |
| `BATCH_DELAY` | `0s` | Nonnegative delay between check batches. |
| `CHECK_DARKNET` | | Any non-empty value opts this probe into overlay checks. |
| `DEBUG` | | Any non-empty value enables probe debug logging. |

Workers validate required URL/credentials and checking limits before starting. API TLS always verifies the server certificate, independently of peer reachability TLS settings. The existing non-empty flag convention means `CHECK_DARKNET=false` still enables overlay checking; omit the variable to disable it.

## License

The code of this project is released under the [Unlicense](https://unlicense.org/). See the `LICENSE` file for details.

Bundled map data is public-domain [Natural Earth](https://www.naturalearthdata.com/about/terms-of-use/) data. The PMTiles reader and its bundled dependency retain their [third-party licenses](internal/server/static/pmtiles-LICENSE.txt).
