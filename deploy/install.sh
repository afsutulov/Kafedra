#!/bin/sh
# Установка ПК «Кафедра» на сервер Linux (Astra Linux, РЕД ОС, Альт и др.). Запускать от root:
#   sh install.sh [имя-узла-или-IP] ["Название организации"]
set -eu
HOST="${1:-$(hostname -f 2>/dev/null || hostname)}"
ORG="${2:-}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64) ARCH=arm64 ;;
  *) echo "Архитектура $(uname -m): соберите исполняемый файл из исходных текстов (build.sh)"; exit 1 ;;
esac
BIN="$HERE/dist/kafedra-linux-$ARCH"
[ -f "$BIN" ] || { echo "Не найден $BIN — выполните сборку: sh build.sh"; exit 1; }
[ "$(id -u)" = 0 ] || { echo "Требуются права root"; exit 1; }
if [ -f "$HERE/dist/SHA256SUMS" ]; then
  (cd "$HERE/dist" && grep "kafedra-linux-$ARCH" SHA256SUMS | sha256sum -c -) || { echo "Контрольная сумма не совпала"; exit 1; }
fi

id kafedra >/dev/null 2>&1 || useradd --system --home-dir /var/lib/kafedra --shell /usr/sbin/nologin kafedra
install -d -m 0755 /opt/kafedra
install -d -m 0750 -o root -g kafedra /etc/kafedra
install -d -m 0700 -o kafedra -g kafedra /var/lib/kafedra
install -m 0755 "$BIN" /opt/kafedra/kafedra

if [ ! -f /etc/kafedra/cert.pem ]; then
  /opt/kafedra/kafedra gencert -host "$HOST" -out /etc/kafedra
  chown root:kafedra /etc/kafedra/cert.pem /etc/kafedra/key.pem
  chmod 0640 /etc/kafedra/key.pem
fi
if [ ! -f /etc/kafedra/config.json ]; then
  cat > /etc/kafedra/config.json <<CFG
{
  "listen": ":443",
  "data_dir": "/var/lib/kafedra",
  "tls_cert": "/etc/kafedra/cert.pem",
  "tls_key": "/etc/kafedra/key.pem",
  "timezone": "Europe/Moscow",
  "org_name": "$ORG",
  "system_name": "Кафедра",
  "trusted_proxy": "",
  "backup_hour": 3,
  "backup_keep": 7
}
CFG
  chown root:kafedra /etc/kafedra/config.json
  chmod 0640 /etc/kafedra/config.json
fi
if [ ! -d /var/lib/kafedra/db ]; then
  runuser -u kafedra -- /opt/kafedra/kafedra init -config /etc/kafedra/config.json
fi
install -m 0644 "$HERE/deploy/kafedra.service" /etc/systemd/system/kafedra.service
systemctl daemon-reload
systemctl enable --now kafedra
sleep 1
systemctl --no-pager --lines=5 status kafedra || true
echo
echo "Готово. Откройте https://$HOST/ и войдите под учётной записью администратора (пароль показан выше)."
echo "Проверьте часовой пояс в /etc/kafedra/config.json и замените самоподписанный сертификат на выданный УЦ."
