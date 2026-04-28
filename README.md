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

```bash
export REVERSE_SERVER_URL=https://r.myw.io
export REVERSE_CLIENT_KEY=pf_your_key_here
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
export REVERSE_CLIENT_DISCOVER=true
export REVERSE_CLIENT_DISCOVER_ALLOW=3000,8080,9000-9100
export REVERSE_CLIENT_DISCOVER_DENY=22,2375,2376
export REVERSE_CLIENT_DISCOVER_NAMES=3000=web,8080=admin
portflare daemon
```

## Docker

Build the client image:

```bash
docker build -t ghcr.io/portflare/client:dev .
```

Run it locally:

```bash
docker run --rm \
  -e REVERSE_SERVER_URL=https://r.myw.io \
  -e REVERSE_CLIENT_KEY=pf_your_key_here \
  ghcr.io/portflare/client:dev
```

## Embedded example repo

The embedded example now lives in its own repository:

- `github.com/portflare/client-embedded-example`

Use that repo when you want a sample application image that bundles `portflare` and an app in one container.
