#!/bin/sh
# Установка ПК «Кафедра» на сервер Linux (Astra Linux, РЕД ОС, Альт и др.). Запускать от root:
#   sh install.sh [имя-узла-или-IP] ["Название организации"] ["Название портала"]
# Всё размещается в /opt/kafedra (программа, config.json, сертификат, данные); журнал — в /var/log/kafedra.
set -eu
HOST="${1:-$(hostname -f 2>/dev/null || hostname)}"
ORG="${2:-}"
PORTAL="${3:-Учебный портал}"
HOME_DIR=/opt/kafedra
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

id kafedra >/dev/null 2>&1 || useradd --system --home-dir "$HOME_DIR" --shell /usr/sbin/nologin kafedra
# каталог и программа принадлежат root (служба не может подменить исполняемый файл);
# службе принадлежат только данные, файл конфигурации, ключ TLS и журнал
install -d -m 0755 -o root -g root "$HOME_DIR"
install -d -m 0700 -o kafedra -g kafedra "$HOME_DIR/data"
install -d -m 0750 -o kafedra -g kafedra /var/log/kafedra
install -m 0755 -o root -g root "$BIN" "$HOME_DIR/kafedra"

if [ ! -f "$HOME_DIR/cert.pem" ]; then
  "$HOME_DIR/kafedra" gencert -host "$HOST" -out "$HOME_DIR"
  chown root:kafedra "$HOME_DIR/cert.pem" "$HOME_DIR/key.pem"
  chmod 0644 "$HOME_DIR/cert.pem"
  chmod 0640 "$HOME_DIR/key.pem"
fi
if [ ! -f "$HOME_DIR/config.json" ]; then
  "$HOME_DIR/kafedra" init -config-only -listen ":443" \
    -tls-cert "$HOME_DIR/cert.pem" -tls-key "$HOME_DIR/key.pem" -portal "$PORTAL" -org "$ORG"
  chown kafedra:kafedra "$HOME_DIR/config.json"   # служба записывает сюда настройки, изменённые в веб-интерфейсе
  chmod 0600 "$HOME_DIR/config.json"
fi
if [ ! -d "$HOME_DIR/data/db" ]; then
  runuser -u kafedra -- "$HOME_DIR/kafedra" init
fi
install -m 0644 "$HERE/deploy/kafedra.service" /etc/systemd/system/kafedra.service
systemctl daemon-reload
systemctl enable --now kafedra
sleep 1
systemctl --no-pager --lines=5 status kafedra || true
echo
echo "Готово. Откройте https://$HOST/ и войдите под учётной записью администратора (пароль показан выше)."
echo "Настройки: $HOME_DIR/config.json. Проверьте часовой пояс и замените самоподписанный сертификат на выданный УЦ."
