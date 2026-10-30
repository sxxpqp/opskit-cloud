#!/usr/bin/env bash
set -euo pipefail
# Requires root or passwordless sudo. No Docker is installed or used.
if [[ "$(id -u)" != 0 ]]; then exec sudo -n bash "$0"; fi
target=/opt/opskit-cloud
mkdir -p "$target"
backup="$(mktemp -d /opt/opskit-cloud-backup.XXXXXX)"
had_old=false
if [[ -f "$target/opskit-cloud" && -f "$target/config.yaml" ]]; then
  cp "$target"/{opskit-cloud,config.yaml} "$backup/"
  had_old=true
fi
if [[ -f /etc/systemd/system/opskit-cloud.service ]]; then cp /etc/systemd/system/opskit-cloud.service "$backup/"; fi
rollback() {
  if [[ "$had_old" == true ]]; then
    cp "$backup"/{opskit-cloud,config.yaml} "$target/"
    if [[ -f "$backup/opskit-cloud.service" ]]; then cp "$backup/opskit-cloud.service" /etc/systemd/system/; fi
    systemctl daemon-reload
    systemctl restart opskit-cloud || true
  fi
}
trap rollback ERR
# Validate staged executable/config before stopping the old service.
chmod +x opskit-cloud
./opskit-cloud serve --config config.yaml --check
systemctl stop opskit-cloud 2>/dev/null || true
install -m 755 opskit-cloud "$target/opskit-cloud"
install -m 644 config.yaml "$target/config.yaml"
cat > /etc/systemd/system/opskit-cloud.service <<'UNIT'
[Unit]
Description=opskit-cloud Go download proxy
After=network-online.target
Wants=network-online.target

[Service]
DynamicUser=true
WorkingDirectory=/opt/opskit-cloud
ExecStart=/opt/opskit-cloud/opskit-cloud serve --config /opt/opskit-cloud/config.yaml
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable opskit-cloud
systemctl start opskit-cloud
# CI uses a fixed HTTP listener so no server-side language tools or YAML parser are required.
for i in $(seq 1 15); do
  if curl --fail --silent http://127.0.0.1:8080/healthz >/dev/null; then
    trap - ERR
    echo "Go proxy deployed successfully"
    exit 0
  fi
  sleep 2
done
echo "Proxy failed readiness check; restoring previous deployment" >&2
false
