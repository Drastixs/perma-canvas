#!/usr/bin/env bash
# Run as root on Proxmox with a built release directory supplied as PAYLOAD_DIR.
set -euo pipefail
: "${CTID:?Choose an unused container ID}"
: "${TEMPLATE:?Proxmox template volume, e.g. local:vztmpl/debian-13-standard_amd64.tar.zst}"
: "${ADDRESS:?Unused IPv4 address with CIDR}"
: "${GATEWAY:?IPv4 gateway}"
: "${DNS_SERVER:?IPv4 DNS resolver}"
: "${TRAEFIK_IP:?Only permitted inbound Traefik source IPv4}"
: "${PAYLOAD_DIR:?Directory containing perma-canvas, deploy/, and login/}"
BRIDGE=${BRIDGE:-vmbr0}
STORAGE=${STORAGE:-local-lvm}
[[ $EUID == 0 ]] || { echo 'Run on Proxmox as root' >&2; exit 1; }
[[ $CTID =~ ^[0-9]+$ ]] || exit 1
for value in "$GATEWAY" "$DNS_SERVER" "$TRAEFIK_IP"; do
    [[ $value =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || { echo 'IPv4 addresses required' >&2; exit 1; }
done
[[ $ADDRESS =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$ ]] || exit 1
[[ $BRIDGE =~ ^[a-zA-Z0-9_-]+$ && $STORAGE =~ ^[a-zA-Z0-9_-]+$ ]] || exit 1
[[ -f "$PAYLOAD_DIR/perma-canvas" && -f "$PAYLOAD_DIR/deploy/perma-canvas.service" ]] || exit 1
if pct config "$CTID" >/dev/null 2>&1 || qm config "$CTID" >/dev/null 2>&1; then
    echo "Guest $CTID already exists; refusing to overwrite" >&2; exit 1
fi
pct create "$CTID" "$TEMPLATE" --hostname perma-canvas --unprivileged 1 \
    --features nesting=1 --cores 1 --memory 512 --swap 256 --rootfs "$STORAGE:4" \
    --net0 "name=eth0,bridge=$BRIDGE,ip=$ADDRESS,gw=$GATEWAY,firewall=1" \
    --nameserver "$DNS_SERVER" --onboot 1 --start 0
cat > "/etc/pve/firewall/$CTID.fw" <<PVEFW
[OPTIONS]
enable: 1
policy_in: DROP
policy_out: DROP

[RULES]
IN ACCEPT -source $TRAEFIK_IP -p tcp -dport 80 # Traefik ingress
OUT ACCEPT -dest $TRAEFIK_IP -p tcp -sport 80 # Traefik replies
OUT ACCEPT -dest $DNS_SERVER -p udp -dport 53 # DNS
OUT ACCEPT -dest $DNS_SERVER -p tcp -dport 53 # DNS TCP
OUT DROP -dest 10.0.0.0/8
OUT DROP -dest 100.64.0.0/10
OUT DROP -dest 169.254.0.0/16
OUT DROP -dest 172.16.0.0/12
OUT DROP -dest 192.168.0.0/16
OUT ACCEPT -dest 0.0.0.0/0 -p tcp -dport 80,443 # Canvas and packages
PVEFW
pct start "$CTID"
# An interrupted deployment leaves the new guest for inspection; never destroys it.
pct exec "$CTID" -- bash -c 'apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates nftables curl nginx'
pct exec "$CTID" -- systemctl disable --now ssh ssh.socket postfix
pct exec "$CTID" -- useradd --system --home-dir /var/lib/perma-canvas --shell /usr/sbin/nologin perma-canvas
pct exec "$CTID" -- install -d -m 0755 /opt/perma-canvas/login
pct exec "$CTID" -- install -d -o perma-canvas -g perma-canvas -m 0700 /var/lib/perma-canvas
pct push "$CTID" "$PAYLOAD_DIR/perma-canvas" /opt/perma-canvas/perma-canvas --perms 0755
pct push "$CTID" "$PAYLOAD_DIR/login/capture_code.py" /opt/perma-canvas/login/capture_code.py --perms 0644
pct push "$CTID" "$PAYLOAD_DIR/login/requirements.txt" /opt/perma-canvas/login/requirements.txt --perms 0644
pct push "$CTID" "$PAYLOAD_DIR/deploy/perma-canvas.service" /etc/systemd/system/perma-canvas.service --perms 0644
# No nested container runtime or Chromium is required in the service LXC.
pct exec "$CTID" -- bash -c 'cat > /etc/perma-canvas.env' <<ENV
CANVAS_DOMAIN=canvas.institution.example
PUBLIC_URL=https://canvas.example.com
LISTEN_ADDR=127.0.0.1:8080
DATA_DIR=/var/lib/perma-canvas
ENV
pct exec "$CTID" -- bash -c 'cat > /etc/nginx/sites-available/perma-canvas' <<NGINX
server {
 listen 80 default_server;
 server_name canvas.example.com;
 client_max_body_size 2g;
 location / {
  proxy_pass http://127.0.0.1:8080;
  proxy_http_version 1.1;
  proxy_set_header Host canvas.example.com;
  proxy_set_header Connection "";
  proxy_buffering off;
  proxy_request_buffering off;
  proxy_read_timeout 300s;
  proxy_send_timeout 300s;
 }
}
NGINX
pct exec "$CTID" -- unlink /etc/nginx/sites-enabled/default
pct exec "$CTID" -- ln -s /etc/nginx/sites-available/perma-canvas /etc/nginx/sites-enabled/perma-canvas
pct exec "$CTID" -- nginx -t
# Guest firewall is self-contained and does not change host/other guest policies.
pct exec "$CTID" -- bash -c 'cat > /etc/nftables.conf' <<NFT
#!/usr/sbin/nft -f
flush ruleset
table inet perma_canvas {
 chain input {
  type filter hook input priority 0; policy drop;
  iifname "lo" accept
  ct state established,related accept
  ip saddr $TRAEFIK_IP tcp dport 80 accept
 }
 chain forward { type filter hook forward priority 0; policy drop; }
 chain output {
  type filter hook output priority 0; policy drop;
  oifname "lo" accept
  ct state established,related accept
  ip daddr $DNS_SERVER udp dport 53 accept
  ip daddr $DNS_SERVER tcp dport 53 accept
  ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16 } reject
  ip6 daddr { fc00::/7, fe80::/10 } reject
  tcp dport { 80, 443 } accept
 }
}
NFT
pct exec "$CTID" -- nft -c -f /etc/nftables.conf
pct exec "$CTID" -- systemctl enable --now nftables
pct exec "$CTID" -- systemctl restart nginx
pct exec "$CTID" -- systemctl daemon-reload
pct exec "$CTID" -- systemctl enable --now perma-canvas
pct exec "$CTID" -- curl --fail --silent "http://127.0.0.1:8080/_perma/health"
echo
echo "LXC $CTID installed. Enroll a separate Canvas grant, then configure Traefik upstream http://${ADDRESS%/*}:80 behind Authelia."
