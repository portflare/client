# Portflare Client

This repository contains the Portflare client.

It is split out from the original monorepo so the client image, release pipeline, and client-specific documentation can live in a dedicated repository.

## What is here

- `cmd/portflare`: the client binary and local API
- `internal/buildinfo`: version metadata helpers
- `Dockerfile`: production image for the client daemon
- `docs/usage.md`: client usage and discovery notes

## Build

```bash
make build
./dist/bin/portflare version
```

## Run

Register a user and print the environment variables needed by the daemon:

```bash
portflare register --server https://r.myw.io --user alice --email alice@example.com
```

Then export the printed values and start the daemon:

```bash
export PORTFLARE_SERVER_URL=https://r.myw.io
export PORTFLARE_CLIENT_KEY=pf_your_key_here
portflare daemon
```

Register an app manually:

```bash
portflare expose --app web --target http://127.0.0.1:3000
```

Request a server-side public port too:

```bash
portflare expose --app web --target http://127.0.0.1:3000 --public-port 13000
```

## Discovery mode

```bash
export PORTFLARE_CLIENT_DISCOVER=true
export PORTFLARE_CLIENT_DISCOVER_ALLOW=3000,8080,9000-9100
export PORTFLARE_CLIENT_DISCOVER_DENY=22,2375,2376
export PORTFLARE_CLIENT_DISCOVER_NAMES=3000=web,8080=admin
portflare daemon
```

By default, discovered apps are named `app-{port}`, for example `app-3000`. Exact per-port names in `PORTFLARE_CLIENT_DISCOVER_NAMES` still win.

For larger machines, add a descriptor prefix:

```bash
export PORTFLARE_CLIENT_DISCOVER_DESCRIPTOR=devbox
export PORTFLARE_CLIENT_DISCOVER_NAME_TEMPLATE=descriptor-port
```

This names port `3000` as `devbox-3000`.

You can also add explicit protocol labels for naming:

```bash
export PORTFLARE_CLIENT_DISCOVER_DESCRIPTOR=devbox
export PORTFLARE_CLIENT_DISCOVER_NAME_TEMPLATE=descriptor-proto-port
export PORTFLARE_CLIENT_DISCOVER_PROTOCOLS=3000=http,6379=redis,3306=mysql
```

This produces names such as `devbox-http-3000` and `devbox-redis-6379`. Protocol labels are naming metadata only; they do not by themselves add non-HTTP proxy support.

## Docker

Build the client image:

```bash
docker build -t ghcr.io/portflare/client:dev .
```

Run it locally:

```bash
docker run --rm \
  -e PORTFLARE_SERVER_URL=https://r.myw.io \
  -e PORTFLARE_CLIENT_KEY=pf_your_key_here \
  ghcr.io/portflare/client:dev
```

## FAQ

See [`docs/usage.md`](docs/usage.md#faq) for Docker sidecar, embedded client, discovery, lifecycle, and Compose notes.

## Embedded example repo

The embedded example now lives in its own repository:

- `github.com/portflare/client-embedded-example`

Use that repo when you want a sample application image that bundles `portflare` and an app in one container.
