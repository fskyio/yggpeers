# yggpeers

A webapp that tracks the reachability of public peers for the Yggdrasil network. Supports JSON and CSV output, a peer world map, statistics, and more.

## Deploying

### Container (recommended)

```sh
docker run -p 8080:8080 foundry.fsky.io/fsky/yggpeers:latest
```

A [Quadlet](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html) unit file is available at [`contrib/quadlet/yggpeers.container`](contrib/quadlet/yggpeers.container) for deploying with Podman and systemd.

### From source

Requires Go 1.26+ and [Git](https://git-scm.com/).

```sh
go build -o yggpeers cmd/yggpeers/main.go
./yggpeers
```

Then open http://localhost:8080 in your browser.

## Configuration

Configuration is handled through environment variables:

| Variable                | Default               | Description                                             |
| ----------------------- | --------------------- | ------------------------------------------------------- |
| `LISTEN_ADDR`           | `:8080`               | Address for the HTTP server to listen on.               |
| `DB_PATH`               | `./data/yggpeers.db`  | Path to the SQLite database file.                       |
| `REPO_DIR`              | `./data/public-peers` | Path where the public-peers repo is cloned.             |
| `FETCH_INTERVAL`        | `15m`                 | How often to pull and re-parse the public-peers repo.   |
| `CHECK_INTERVAL`        | `5m`                  | How often to run reachability checks.                   |
| `DIAL_TIMEOUT`          | `5s`                  | Timeout for individual network dials.                   |
| `MAX_CONCURRENT_CHECKS` | `50`                  | Maximum number of concurrent peer checks.               |
| `BATCH_DELAY`           |                       | Optional delay between check batches (duration string). |
| `CHECK_DARKNET`         |                       | Any non-empty value enables darknet peer checks.        |
| `DEBUG`                 |                       | Any non-empty value enables debug logging.              |

## License

The code of this project is released under the [Unlicense](https://unlicense.org/). See the `LICENSE` file for details.
