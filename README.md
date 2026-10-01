# Caddy Sandbox

A restricted API for publishing application routes through Caddy. Runs on a Linux VM with Docker/Caddy. No third-party Go dependencies; requires Go 1.22 or later. This is a standalone controller, not a plugin compiled into Caddy.

The sandbox's Docker port range must already be published. The API does not launch applications or manage sandbox containers. Any external application can use the API; it is not tied to a particular agent or sandbox provider.

## API contract

Base URL: `https://control.example.com`. `GET /` serves HTML API documentation that can be opened in a browser without a token. Caddy's IP restriction also applies to the documentation. All other requests, including the health check, require `Authorization: Bearer <token>`.

| Method and path | Result |
|---|---|
| `GET /` | HTML documentation with the current port range and IP; no token required |
| `PUT /apps/{name}` with `{"port":10003}` | Create (201) or update (200) a route |
| `GET /apps/{name}` | Name, port, and URL; 404 if not found |
| `GET /apps` | Array of applications sorted by name |
| `DELETE /apps/{name}` | Delete a route, 204; a missing route also returns 204 |
| `GET /healthz` | 200; 503 if rollback is incomplete and recovery is required |

Names contain 1–63 characters: lowercase ASCII letters, digits, and hyphens; leading and trailing hyphens are not allowed. The administrator sets the domain suffix using `CS_DOMAIN_SUFFIX` (default: `sandbox.example.com`). Only an integer `port` within the allowed range is accepted; additional fields are rejected. A port conflict with another registered application returns 409. An invalid token returns 401, invalid parameters return 400, and an update failure returns 503. Repeating an identical PUT does not trigger a reload.

Example response:

```json
{"name":"my-app","port":10003,"url":"https://my-app.sandbox.example.com"}
```

## How configuration is applied

`sandbox.caddy` serves as both the imported file and the application registry. The API loads it at startup and accepts only its restricted format: one domain and one `reverse_proxy` targeting the configured IPv4 address per block. An empty file is valid.

During an update, the API locks operations, checks for external changes, saves the previous file as `sandbox.caddy.pending`, atomically replaces `sandbox.caddy`, and runs:

```sh
docker exec caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
docker exec caddy caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
```

The entire configuration is validated, including other imports. On failure, the previous file is restored, validated, and reloaded. If rollback fails, further updates are blocked, the health check returns 503, and the journal is retained. Restore access to Docker/Caddy and restart the API: it will recover the previous version from the journal. An operation interrupted by a process crash is also rolled back at startup.

At every startup, the API validates and loads the configuration from disk before opening its HTTP port. `.pending` and `.lock` are internal files; do not import them. A file lock prevents a second API instance from using the same CS_FILE.

The API does not check application availability before publishing. An application that is not running may return 502 through Caddy. The health check reports the API's state; it does not continuously check Caddy or application availability. GET /apps shows the API's last confirmed configuration; after a failed rollback, Caddy's actual state must be recovered.

## Build

```sh
go test -race ./...
go vet ./...
go build -trimpath -o caddy-sandbox .
```

To cross-compile for Linux x86-64:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o caddy-sandbox .
```

For ARM64, use `GOARCH=arm64`. The repository contains source code only; build the binary for your server architecture before installation.

## Installation on the VM

The examples use documentation-only addresses: Caddy/API on `192.0.2.10`, and the sandbox and allowed client on `192.0.2.20`. Replace these with your actual addresses and replace the example domains. The allowed client and upstream may have different IP addresses. Set `CS_DOMAIN_SUFFIX` to your application domain and `CS_PUBLIC_URL` to your API URL; update `examples/api.caddy` accordingly. Ensure DNS records for the application domains resolve to Caddy.

1. Verify that ports 10000–10099 are available on the sandbox's Docker host. Publish the range when creating the sandbox container, for example:

   ```sh
   docker run -d -p 0.0.0.0:10000-10099:10000-10099 your-sandbox-image
   ```

   An existing sandbox must be recreated after its data has been saved. Applications listen on `0.0.0.0:<port>` inside the sandbox. These mappings apply to the Docker host that creates the sandboxes; with Docker-in-Docker, configure the outer port mappings separately.

2. Mount the entire configuration directory into Caddy rather than an individual `sandbox.caddy` file. Otherwise, the container may not see atomic file replacements. For example, in your existing Compose configuration:

   ```yaml
   services:
     caddy:
       volumes:
         - /opt/caddy/config:/etc/caddy:ro
   ```

   Preserve the other mounts, ports, and container settings. A read-only mount for Caddy is supported: the API writes to the directory on the VM.

3. Add the API block and `import sandbox.caddy` from `examples/api.caddy` to your existing main Caddyfile. Include the import exactly once. Do not replace the main file with the example. Create an empty `sandbox.caddy` using `examples/sandbox.caddy`. Do not overwrite an existing list of routes with the template.

4. Install the binary and create the service user:

   ```sh
   sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin caddy-sandbox
   sudo install -m 0755 caddy-sandbox /usr/local/bin/caddy-sandbox
   sudo install -m 0600 examples/caddy-sandbox.env /etc/caddy-sandbox.env
   sudo install -m 0644 examples/caddy-sandbox.service /etc/systemd/system/caddy-sandbox.service
   ```

   Skip useradd if the user already exists. The service uses the `docker` group; verify that this group exists and that `/usr/bin/docker` is the correct executable path. The user needs access to the Docker socket. This is a privileged management service: Docker access allows control over the entire VM.

5. Allow the service user to create and rename files in the configuration directory. For the example path:

   ```sh
   sudo chgrp caddy-sandbox /opt/caddy/config
   sudo chmod g+rwx /opt/caddy/config
   ```

   The user must also be able to read the existing `sandbox.caddy`. Files created by the API use mode 0644; the journal uses 0600. Caddy must be able to read the main Caddyfile and all imports. Treat the configuration directory as trusted; do not allow unprivileged applications to modify its contents.

6. Edit `/etc/caddy-sandbox.env`: set the paths, IP addresses, container name, and token. Generate a token with:

   ```sh
   openssl rand -hex 32
   ```

   CS_TOKEN is required and must contain at least 32 characters. If you change CS_FILE, also update ReadWritePaths in the systemd unit. CS_LISTEN must be a VM address reachable from the Caddy container; the container's `127.0.0.1` and the VM's `127.0.0.1` are separate loopback interfaces.

7. API port 9000 must be reachable from Caddy but blocked for direct connections from other clients. Configure the VM firewall using the actual container address or network. The remote_ip restriction applies only to requests through Caddy; the API checks the token for every API request. Sandbox application ports can also be restricted to traffic from Caddy. Do not expose Caddy's admin API port 2019 externally.

   If a proxy or NAT sits in front of Caddy, check the access log for the address seen by `remote_ip`; do not trust arbitrary X-Forwarded-For headers. Configure DNS and HTTPS connectivity for the API domain.

8. Apply the main Caddyfile with a normal reload, then start the service:

   ```sh
   docker exec caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
   docker exec caddy caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
   sudo systemctl daemon-reload
   sudo systemctl enable --now caddy-sandbox
   sudo journalctl -u caddy-sandbox -f
   ```

## Usage

On an allowed client, enter the token without saving it in shell history:

```sh
read -rs CS_CLIENT_TOKEN
export CS_CLIENT_TOKEN
curl --fail-with-body https://control.example.com/apps/my-app \
  -X PUT -H "Authorization: Bearer $CS_CLIENT_TOKEN" \
  -H 'Content-Type: application/json' --data '{"port":10003}'
curl --fail-with-body https://control.example.com/apps \
  -H "Authorization: Bearer $CS_CLIENT_TOKEN"
curl --fail-with-body https://control.example.com/apps/my-app \
  -X DELETE -H "Authorization: Bearer $CS_CLIENT_TOKEN"
unset CS_CLIENT_TOKEN
```

The example uses Bash's `read -rs` syntax. Deleting a route does not stop the application; your sandbox manager must stop it before reusing the port. The API tracks only registered applications, not arbitrary processes in the sandbox.

## Configuration

| Variable | Default |
|---|---|
| CS_LISTEN | 127.0.0.1:9000; for Caddy in Docker, set a reachable VM IP address |
| CS_TOKEN | Required |
| CS_UPSTREAM_IP | 192.0.2.20 |
| CS_DOMAIN_SUFFIX | sandbox.example.com; lowercase DNS domain, up to 189 characters |
| CS_PUBLIC_URL | https://control.example.com; absolute HTTP(S) URL for the documentation |
| CS_MIN_PORT / CS_MAX_PORT | 10000 / 10099 |
| CS_FILE | /opt/caddy/config/sandbox.caddy |
| CS_CONTAINER | caddy |
| CS_CONTAINER_CONFIG | /etc/caddy/Caddyfile |
| CS_DOCKER | /usr/bin/docker |

Run one instance and make all changes to the managed file through the API. Its lock serializes API requests, but not external editors or other Caddy administrators. Make main configuration changes separately from API requests. After a client timeout, use GET to check the result: the request may complete on the server even after the client disconnects.

## Verification

Tests cover authentication, input restrictions, rejection of extra directives/IP addresses/domains in the imported file, competing requests for the same port, idempotent PUT/DELETE, persistence, rollback after validate/reload failures, blocking updates after failed rollback, and recovery of interrupted transactions. Caddy calls are simulated in tests; verify the service against your actual container after installation.
