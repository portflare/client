# Portflare Client usage

## Daemon

```bash
reverse-client daemon
```

The client exposes a local API on `REVERSE_CLIENT_LISTEN_ADDR`.

## Manual registration

```bash
reverse-client expose --app web --target http://127.0.0.1:3000
reverse-client expose --app web --target http://127.0.0.1:3000 --public-port 13000
```

## Listing apps

```bash
reverse-client list
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
