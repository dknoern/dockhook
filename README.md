# dockhook

![DockHook](dockhook-banner.png)

A lightweight Docker webhook service written in Go. Send a POST request to trigger a `docker pull` + container recreation for a configured image, then receive a Slack notification with the result.

## How it works

1. Receives a `POST /webhook` request with a shared secret.
2. Pulls the configured Docker image from its registry.
3. Finds all running containers using that image.
4. Stops, removes, and recreates each container with the freshly pulled image.
5. Posts a success or failure message to Slack (optional).

The container is recreated with its original name, environment, volumes, ports, and networks — functionally equivalent to `docker compose up -d --pull always`.

## Quick start

Create a `.env` file alongside your `docker-compose.yml`:

```
DOCKHOOK_SECRET=my-super-secret
DOCKHOOK_IMAGE=myapp:latest
DOCKHOOK_SLACK_WEBHOOK=https://hooks.slack.com/services/XXX/YYY/ZZZ
```

Add dockhook to your compose stack:

```yaml
# docker-compose.yml
services:
  dockhook:
    image: dknoern/dockhook:latest
    ports:
      - "8080:8080"
    env_file: .env
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    restart: unless-stopped

  myapp:
    image: myapp:latest
    # ... rest of your service config
```

Start it:

```bash
docker compose up -d
```

Trigger a deploy:

```bash
curl -X POST http://your-host:8080/webhook \
  -H "X-Webhook-Secret: my-super-secret"
```

## Configuration

| Variable | Required | Default | Description |
|---|---|---|---|
| `DOCKHOOK_SECRET` | yes | — | Shared secret for webhook authentication |
| `DOCKHOOK_IMAGE` | yes | — | Full image name+tag to watch (e.g. `myapp:latest`) |
| `DOCKHOOK_SLACK_WEBHOOK` | no | — | Slack incoming webhook URL for notifications |
| `DOCKHOOK_PORT` | no | `8080` | Port for the HTTP server |

## Sending the webhook

Pass the secret in either the `X-Webhook-Secret` header or as a `?secret=` query parameter:

```bash
# Header (recommended)
curl -X POST http://host:8080/webhook -H "X-Webhook-Secret: my-super-secret"

# Query param
curl -X POST "http://host:8080/webhook?secret=my-super-secret"
```

The endpoint returns `202 Accepted` immediately; the pull + restart happens asynchronously.

## Health check

```bash
curl http://host:8080/health
# → ok
```

## Setting up the GitHub Action (Docker Hub publish)

The workflow at `.github/workflows/docker-publish.yml` builds and pushes `dknoern/dockhook` to Docker Hub on every push to `main` and on version tags.

Add two repository secrets in **Settings → Secrets and variables → Actions**:

| Secret | Value |
|---|---|
| `DOCKERHUB_USERNAME` | Your Docker Hub username |
| `DOCKERHUB_TOKEN` | A Docker Hub access token (not your password) |

Tag a release to push a versioned image:

```bash
git tag v1.0.0
git push origin v1.0.0
```

This produces `dknoern/dockhook:v1.0.0` and `dknoern/dockhook:latest` (multi-arch: amd64 + arm64).

## Triggering dockhook from your app's GitHub Action

After your CI pipeline builds and pushes a new image, add a final step that calls your dockhook endpoint. Store the URL and secret as repository secrets so they aren't exposed in the workflow file.

**Add these secrets to your app's repo** (Settings → Secrets and variables → Actions):

| Secret | Value |
|---|---|
| `DEPLOY_WEBHOOK_URL` | `http://your-host:8080/webhook` |
| `DEPLOY_WEBHOOK_SECRET` | The same value as `DOCKHOOK_SECRET` on your server |

**Add a deploy step at the end of your build workflow:**

```yaml
# .github/workflows/build.yml (in your app's repo)
name: Build and Deploy

on:
  push:
    branches: [main]

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Log in to Docker Hub
        uses: docker/login-action@v3
        with:
          username: ${{ secrets.DOCKERHUB_USERNAME }}
          password: ${{ secrets.DOCKERHUB_TOKEN }}

      - name: Build and push
        uses: docker/build-push-action@v6
        with:
          push: true
          tags: yourname/myapp:latest

      - name: Trigger deploy via dockhook
        run: |
          curl -f -X POST "${{ secrets.DEPLOY_WEBHOOK_URL }}" \
            -H "X-Webhook-Secret: ${{ secrets.DEPLOY_WEBHOOK_SECRET }}"
```

The `-f` flag makes `curl` exit with a non-zero status if dockhook returns an error (e.g. wrong secret), which will fail the workflow step. The actual pull and restart happen asynchronously on your server after the 202 response — check Slack or the server logs to confirm the deploy completed.

**If your server isn't publicly reachable** (e.g. behind a firewall), you can use a tunnel like [Tailscale](https://tailscale.com) or [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) to expose the endpoint, or trigger the webhook from a self-hosted GitHub Actions runner on the same network.

## Building locally

dockhook has no external dependencies — it communicates with Docker via the REST API over the Unix socket using only the Go standard library.

```bash
go build -o dockhook .
./dockhook           # set env vars first
```

Or with Docker:

```bash
docker build -t dockhook .
```

## Security notes

- Mount the Docker socket (`/var/run/docker.sock`) only on trusted hosts. Any process with socket access has full control over the Docker daemon.
- Use a long random secret: `openssl rand -hex 32`.
- Place dockhook behind a reverse proxy with TLS in production.
- The secret comparison uses `crypto/subtle.ConstantTimeCompare` to prevent timing attacks.
