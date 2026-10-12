# perma-canvas

Serve the original Canvas web UI at your own HTTPS origin (for example, `https://canvas.example.com`), using a separate Canvas mobile OAuth grant to renew web sessions. No MCP server or MCP dependency. Designed for a single account behind an authenticated reverse proxy, such as Traefik → Authelia → nginx → application.

## How it works

A one-shot Playwright script logs in through Microsoft SSO and Duo. Go exchanges the captured code and saves the grant in a mode-0600 file. During normal use the proxy refreshes the access token on demand, calls Canvas's `/login/session_token`, consumes the returned URL on the original Canvas host, and holds the web cookies in memory. Restarting remints the session from the persisted grant.

The browser receives the original HTML, JavaScript, CSS, and API responses with the original Canvas origin rewritten to the public origin. Account cookies remain on the VM; the CSRF cookie is exposed for Canvas's own JavaScript. Each visitor admitted by Authelia accesses the same Canvas account. There is no additional application login.

## Build and enroll

Requires Go 1.27.1 or newer. Python and Chromium are needed only for enrollment.

```sh
go build -o perma-canvas ./cmd/perma-canvas
python3 -m venv .venv
.venv/bin/pip install -r login/requirements.txt
.venv/bin/playwright install chromium
export CANVAS_DOMAIN=canvas.institution.example
export PUBLIC_URL=https://canvas.example.com
export DATA_DIR="$PWD/data"
PYTHON=.venv/bin/python ./perma-canvas login
./perma-canvas serve
```

The helper prompts for email/password and waits for your Duo approval. It runs headless, blocks images/media/fonts, closes after capturing the code, and does not save passwords or a browser profile. `LOGIN_HEADED=1` opens a visible browser on machines with a display. `CANVAS_EMAIL` and `CANVAS_PASSWORD` can optionally supply credentials through the environment. Never put them in the runtime service configuration.

For manual enrollment, use `LOGIN_BROWSER=manual ./perma-canvas login`, open the printed authorization URL, and paste the callback URL. Full callback URLs must contain the matching OAuth state. A bare code is also accepted for local enrollment. Python/browser installation is unnecessary in manual mode.

The grant can be revoked by Canvas or university policy; persistence does not guarantee it will last forever. Run enrollment again if revoked. Stop the service before replacing its token file, then restart so it reloads the grant. `./perma-canvas status` reports grant metadata without token values.

## VM deployment

Use [the systemd service](deploy/perma-canvas.service), [environment example](deploy/perma-canvas.env.example), and [nginx example](deploy/nginx.conf.example). No live infrastructure is changed by these examples.

1. Create a dedicated `perma-canvas` service user. Install the binary and login helper under `/opt/perma-canvas`.
2. Create `/var/lib/perma-canvas` owned by that user with mode 0700. Enroll as that user with `DATA_DIR=/var/lib/perma-canvas` (or securely copy its separately enrolled `tokens.json`, mode 0600).
3. Install `/etc/perma-canvas.env` and the unit. For a separate nginx LXC, bind `LISTEN_ADDR` to the VM's private IP. Set nginx's upstream to that IP.
4. Allow inbound proxy traffic only from the nginx LXC. Keep the public route behind your existing Authelia policy. Keep nginx reachable only through your intended ingress.
5. Start the service. `GET /_perma/health` reports whether a grant is present; it does not contact Canvas or prove the grant still works.

The runtime Docker image contains only the Go binary and CA certificates. Enroll outside it and mount the data directory owned by UID 65532. This avoids shipping a browser in the always-running container.

## Validation and current limits

```sh
go test -race ./...
go vet ./...
```

Tests cover concurrent refresh, refresh-token rotation and disk permissions, upstream errors without token disclosure, URL/origin rewriting, server-only account cookies, session recovery, and avoiding submission replay. Reads without bodies retry once when Canvas redirects to login or returns 401. Submissions/uploads encountering an expired session return 409 after renewal: reload and submit again. Upload bodies are streamed; textual responses are rewritten with a 32 MiB bound. Assets/downloads are streamed.

This is an initial implementation, **not comprehensively validated across Canvas institutions or Firefox**. Before relying on it, check dashboard, modules, assignments, grades, inbox, uploads/downloads, session expiry, and browser restart.

External LTI tools, Microsoft/third-party SSO, file/CDN hosts, signed URLs, websocket services, embedded videos, CSP rules containing host-only references, and URLs dynamically assembled from hostnames need live validation. Only the configured Canvas origin is rewritten; unrelated hosts remain direct. There is no promise that every external tool works through a changed origin. Normal Canvas session cookies are intentionally withheld from the browser, which may affect external file/tool integrations. The proxy does not inspect or rewrite uploaded bodies. Signed LTI payloads are never altered. Logout returns to the dashboard because this service intentionally maintains the account session; use Authelia to end access and revoke the Canvas grant to disconnect the service.

The Playwright helper is adapted from `Drastixs/mcp-canvas-lms` under its MIT license, included in [LICENSE](LICENSE). It assumes the original Microsoft/Duo flow and may need adjustment if university login screens change.

## Proxmox LXC

[deploy/proxmox-lxc.sh](deploy/proxmox-lxc.sh) provisions a new unprivileged Debian LXC (1 core, 512 MiB RAM, 256 MiB swap, 4 GiB disk). Run it on Proxmox as root with a locally built Linux/amd64 binary and the `deploy/` and `login/` directories staged in `PAYLOAD_DIR`. It refuses existing guest IDs; it does not alter other guests or host networking. Confirm the unused CTID/IP, bridge, storage, cached template volume, DNS resolver, gateway, and actual Traefik source address from live inventory before execution.

Required variables: `CTID`, `TEMPLATE`, `ADDRESS` (IPv4/CIDR), `GATEWAY`, `DNS_SERVER`, `TRAEFIK_IP`, and `PAYLOAD_DIR`. Optional: `BRIDGE` (default `vmbr0`) and `STORAGE` (default `local-lvm`). Enrollment runs elsewhere; securely install its separate mode-0600 token file owned by `perma-canvas` at `/var/lib/perma-canvas/tokens.json`, then restart the service. No browser is installed in the LXC. Nginx runs in the LXC and forwards to the Go service on loopback. The guest firewall permits only the Traefik source on port 80, DNS to the selected resolver, and public HTTP(S) egress; private destination ranges are blocked. Management uses `pct exec` from Proxmox. Use the observed Traefik source IP; reserve the LXC address outside dynamic DHCP allocation.

The script installs an active but unenrolled service; health reports `has_grant: false` until enrollment. It leaves a partially created guest in place if interrupted. Ingress configuration and DNS remain separate operations requiring verification against the current Traefik/Authelia/nginx setup. Traefik must forward to this LXC on port 80, behind Authelia.
