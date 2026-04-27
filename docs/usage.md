# Portflare Client usage

## Daemon

```bash
portflare daemon
```

The client exposes a local API on `REVERSE_CLIENT_LISTEN_ADDR`.

## Manual registration

```bash
portflare expose --app web --target http://127.0.0.1:3000
portflare expose --app web --target http://127.0.0.1:3000 --public-port 13000
```

## Listing apps

```bash
portflare list
curl http://127.0.0.1:9901/apps
```

## Discovery settings

```bash
export REVERSE_CLIENT_DISCOVER=true
export REVERSE_CLIENT_DISCOVER_ALLOW=3000,8080,9000-9100
export REVERSE_CLIENT_DISCOVER_DENY=22,2375,2376
export REVERSE_CLIENT_DISCOVER_NAMES=3000=web,8080=admin
export REVERSE_CLIENT_DISCOVER_INTERVAL=5s
export REVERSE_CLIENT_DISCOVER_GRACE=10m
```

## FAQ

### Why did `docker compose config` fail?

`docker compose config` requires Docker Compose v2, usually installed as the Docker CLI plugin. If Docker itself is installed but the plugin is missing, validation fails before Portflare is involved. Install the Compose plugin or validate on a host that has `docker compose` available.

### Where should I put `REVERSE_CLIENT_KEY`?

For Compose deployments, put it in a local `.env` file and do not commit it:

```env
REVERSE_CLIENT_KEY=pf_your_key_here
```

If a real key is pasted into chat, logs, or a public issue, treat it as compromised and rotate it from the Portflare user page.

### Does `docker run my-app` also start a Portflare sidecar?

No. `docker run` starts exactly one container. A Compose sidecar only starts when you run the Compose project:

```bash
docker compose up -d --build
```

To use `docker run`, either run an embedded client inside the app container or start a separate Portflare container manually.

### What is the difference between embedded mode and sidecar mode?

Embedded mode runs the app and `portflare daemon` in the same container. This gives the client the same `localhost` and process/network view as the app, so discovery is straightforward.

Sidecar mode runs Portflare in a separate container. If you want localhost-style discovery, start the sidecar with the app container's network namespace:

```bash
docker run -d \
  --name my-app-portflare \
  --network container:my-app \
  -e REVERSE_SERVER_URL=https://r.myw.io \
  -e REVERSE_CLIENT_KEY \
  -e REVERSE_CLIENT_LISTEN_ADDR=127.0.0.1:9901 \
  -e REVERSE_CLIENT_DISCOVER=true \
  -e REVERSE_CLIENT_DISCOVER_ALLOW=3000,8080,9000-9100 \
  -e REVERSE_CLIENT_DISCOVER_NAMES=3000=web,8080=admin \
  -v "$HOME/.config/portflare-client:/state" \
  ghcr.io/portflare/client:latest
```

### Should my app listen on `localhost` or `0.0.0.0`?

If Portflare is embedded in the same container or shares the app container network namespace with `--network container:<app>`, `localhost` / `127.0.0.1` is fine.

If Portflare is a separate container on a Docker bridge network, the app must listen on `0.0.0.0:<port>` so other containers can reach it. This does not publish the port to the host unless you also use Docker `ports:` or `-p`.

### Why did discovery not find my app in another container?

Discovery reads listening ports from the client's own network namespace and registers targets such as `http://127.0.0.1:<port>`. It does not inspect every other Docker container. For automatic discovery, run the client embedded or with `--network container:<app>`.

For a shared Portflare client, use explicit targets on a shared Docker network instead:

```bash
docker network create portflare-shared

docker run -d --network portflare-shared --name app-a my-app-a
docker run -d --network portflare-shared --name portflare \
  -e REVERSE_SERVER_URL=https://r.myw.io \
  -e REVERSE_CLIENT_KEY \
  -e REVERSE_CLIENT_LISTEN_ADDR=0.0.0.0:9901 \
  ghcr.io/portflare/client:latest

portflare expose --app app-a-web --target http://app-a:3000
```

### Can one sidecar serve all my Docker containers?

Not when using `--network container:<app>`. That mode shares exactly one container's network namespace.

Use one sidecar per app container if you want automatic discovery and `localhost` targets. Use one shared Portflare client only when all apps are reachable over a shared Docker network and you register explicit targets like `http://app-name:3000`.

### Will Docker stop the sidecar when the app exits?

Not automatically in plain Docker or Compose. `depends_on` controls startup order, not shutdown coupling. Stop the Compose project with `docker compose down`, run app and client in one embedded container, or add your own supervisor/health-check logic if the sidecar must exit when the app exits.

### What should I set in Compose when the app already embeds Portflare?

If you move Portflare into a Compose sidecar, disable the embedded client in the app container if the image supports it, for example:

```env
PORTFLARE_EMBEDDED_CLIENT=false
```

Keep the sidecar's state persistent, for example:

```yaml
volumes:
  - ./data/portflare-client:/state
environment:
  REVERSE_CLIENT_STATE_PATH: /state/state.json
```
