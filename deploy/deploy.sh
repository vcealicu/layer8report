#!/bin/bash
# ─────────────────────────────────────────────────────────
# Deploy script — layer8report.com
# Run as: coder. Never asks for a password.
#
# 1. Tests and builds the API (server/, Go standard library only)
# 2. Syncs public/ and cache-busts CSS and JS
# 3. Generates the sitemap
# 4. Installs or restarts the layer8report user service (systemctl --user, no root)
# 5. Updates nginx if the config changed, with sudo -n only as a silent fallback
#
# SKIP_TESTS=1 skips go vet and go test.
# ─────────────────────────────────────────────────────────
set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$DEPLOY_DIR")"
SRC_DIR="$PROJECT_DIR/public"
SERVER_DIR="$PROJECT_DIR/server"
APP_DIR="/var/www/layer8report.com"
DEST_DIR="$APP_DIR/public"
BIN="$APP_DIR/bin/layer8d"
DATA_DIR="$APP_DIR/data"
API_ADDR="127.0.0.1:48808"
SERVICE="layer8report"
UNIT_SRC="$DEPLOY_DIR/layer8report.service"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
UNIT_DEST="$UNIT_DIR/$SERVICE.service"
PID_FILE="$APP_DIR/run/layer8d.pid"
LOG_FILE="$APP_DIR/run/layer8d.log"
NGINX_SRC="$DEPLOY_DIR/nginx.conf"
NGINX_DEST="/etc/nginx/sites-available/www.layer8report.com"
SITE_URL="https://www.layer8report.com"
GO="${GO:-$(command -v go || echo /usr/local/go/bin/go)}"

echo ""
echo "  ╔═══════════════════════════════════════╗"
echo "  ║  🚀 DEPLOY — layer8report.com"
echo "  ╚═══════════════════════════════════════╝"
echo ""

# Try a command as this user, then with sudo -n (which fails instead of asking
# for a password). Nothing in this script ever prompts.
try_priv() {
    "$@" >/dev/null 2>&1 && return 0
    sudo -n "$@" >/dev/null 2>&1
}

# Build the API first, so a broken build stops before anything changes
echo "> BUILDING API..."
[ -x "$GO" ] || { echo "  [✗] go not found, set GO=/path/to/go"; exit 1; }
if [ "${SKIP_TESTS:-0}" != "1" ]; then
    (cd "$SERVER_DIR" && "$GO" vet ./... && "$GO" test ./...) | sed 's/^/  /'
fi
mkdir -p "$APP_DIR/bin" "$DATA_DIR" "$DEST_DIR"
chmod 750 "$DATA_DIR"
(cd "$SERVER_DIR" && CGO_ENABLED=0 "$GO" build -trimpath -ldflags="-s -w" -o "$BIN.new" .)
BIN_CHANGED=0
if cmp -s "$BIN.new" "$BIN" 2>/dev/null; then
    rm -f "$BIN.new"
    echo "  [—] layer8d unchanged"
else
    mv "$BIN.new" "$BIN"
    BIN_CHANGED=1
    echo "  [⚙] layer8d built"
fi

# Sync public/ → /var/www/
echo ""
echo "> SYNCING FILES..."
rsync -av --checksum --delete \
    --exclude='sitemap.xml' \
    --out-format="  [↑] %n" \
    "$SRC_DIR"/ "$DEST_DIR"/

# Cache-bust CSS and JS, which nginx caches for a year
ASSET_V=$(cat "$SRC_DIR"/css/*.css "$SRC_DIR"/js/*.js | sha256sum | cut -c1-10)
find "$DEST_DIR" -name '*.html' -exec sed -i "s/?v=[A-Za-z0-9]*\"/?v=$ASSET_V\"/g" {} +
echo "  [#] assets v=$ASSET_V"

# Generate sitemap
echo ""
echo "> GENERATING SITEMAP..."
LASTMOD=$(date +%Y-%m-%d)

cat > "$DEST_DIR/sitemap.xml" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url>
    <loc>${SITE_URL}/</loc>
    <lastmod>${LASTMOD}</lastmod>
    <changefreq>hourly</changefreq>
    <priority>1.0</priority>
  </url>
EOF

while IFS= read -r -d '' html_file; do
    rel="${html_file#$SRC_DIR/}"
    [ "$rel" = "index.html" ] && continue
    [[ "$rel" == error/* ]] && continue
    # r.html and h.html are shells for /r/{id} and /h/{id}, not pages
    [ "$rel" = "r.html" ] || [ "$rel" = "h.html" ] && continue
    if [[ "$rel" == */index.html ]]; then
        url="/${rel%index.html}"
    else
        url="/${rel%.html}"
    fi
    cat >> "$DEST_DIR/sitemap.xml" << EOF
  <url>
    <loc>${SITE_URL}${url}</loc>
    <lastmod>${LASTMOD}</lastmod>
    <changefreq>daily</changefreq>
    <priority>0.8</priority>
  </url>
EOF
done < <(find "$SRC_DIR/" -name "*.html" -type f -print0)

for doc in /agents.md /llms.txt; do
    cat >> "$DEST_DIR/sitemap.xml" << EOF
  <url>
    <loc>${SITE_URL}${doc}</loc>
    <lastmod>${LASTMOD}</lastmod>
    <changefreq>weekly</changefreq>
    <priority>0.6</priority>
  </url>
EOF
done

echo "</urlset>" >> "$DEST_DIR/sitemap.xml"
echo "  [📍] sitemap.xml"

# API service, run as this user so nothing needs root.
echo ""
echo "> API SERVICE..."
mkdir -p "$APP_DIR/run"
UNIT_TMP="$(mktemp)"
trap 'rm -f "$UNIT_TMP"' EXIT
sed -e "s#@BIN@#$BIN#g" -e "s#@ADDR@#$API_ADDR#g" -e "s#@DATA@#$DATA_DIR#g" "$UNIT_SRC" > "$UNIT_TMP"

if command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
    # systemd user service: survives logout and starts at boot once lingering is on.
    mkdir -p "$UNIT_DIR"
    UNIT_CHANGED=0
    if ! cmp -s "$UNIT_TMP" "$UNIT_DEST" 2>/dev/null; then
        cp "$UNIT_TMP" "$UNIT_DEST"
        systemctl --user daemon-reload
        systemctl --user enable "$SERVICE" >/dev/null 2>&1 || true
        UNIT_CHANGED=1
        echo "  [⚙] $SERVICE.service installed for $(id -un)"
    fi
    if [ "$BIN_CHANGED" = "1" ] || [ "$UNIT_CHANGED" = "1" ] || ! systemctl --user is-active --quiet "$SERVICE"; then
        systemctl --user restart "$SERVICE"
        echo "  [⚡] $SERVICE restarted"
    else
        echo "  [—] $SERVICE unchanged"
    fi
    LOGS_HINT="journalctl --user -u $SERVICE -n 50"
    if [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null)" != "yes" ]; then
        if loginctl enable-linger "$(id -un)" 2>/dev/null; then
            echo "  [⚙] lingering enabled, the service now starts at boot"
        else
            echo "  [!] run once as root so the service starts at boot: loginctl enable-linger $(id -un)"
        fi
    fi
else
    # No systemd user session (cron, a container, an odd box): run it in the background.
    if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
        if [ "$BIN_CHANGED" = "1" ]; then
            kill "$(cat "$PID_FILE")" && sleep 1
            echo "  [⚡] layer8d stopped for upgrade"
        else
            echo "  [—] layer8d already running (pid $(cat "$PID_FILE"))"
        fi
    fi
    if ! { [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; }; then
        GOMEMLIMIT=48MiB nohup "$BIN" -addr "$API_ADDR" -data "$DATA_DIR" -site "$SITE_URL" >> "$LOG_FILE" 2>&1 &
        echo $! > "$PID_FILE"
        echo "  [⚡] layer8d started in the background (pid $!), log at $LOG_FILE"
        echo "  [!] no systemd user session here, so it will not restart itself; add to crontab: @reboot $DEPLOY_DIR/deploy.sh"
    fi
    LOGS_HINT="tail -50 $LOG_FILE"
fi

HEALTHY=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
    if curl -fsS "http://$API_ADDR/api/v1/health" >/dev/null 2>&1; then HEALTHY=1; break; fi
    sleep 0.5
done
if [ "$HEALTHY" = "1" ]; then
    echo "  [✓] API healthy on $API_ADDR"
else
    echo "  [✗] API not answering on $API_ADDR. Check: $LOGS_HINT"
fi

# Nginx config (if changed). Writes the file directly when it can, tests with
# nginx -t as this user or with sudo -n, and reloads the same way. Never prompts.
if [ -f "$NGINX_SRC" ]; then
    if ! cmp -s "$NGINX_SRC" "$NGINX_DEST" 2>/dev/null; then
        echo ""
        echo "> NGINX CONFIG..."
        HAD_OLD=0
        if [ -f "$NGINX_DEST" ]; then
            try_priv cp "$NGINX_DEST" "$NGINX_DEST.bak" && HAD_OLD=1
        fi
        if ! try_priv cp "$NGINX_SRC" "$NGINX_DEST"; then
            echo "  [✗] cannot write $NGINX_DEST as $(id -un) and sudo -n is not allowed"
            echo "      copy it yourself: sudo cp $NGINX_SRC $NGINX_DEST && sudo nginx -t && sudo systemctl reload nginx"
        else
            NGINX_TEST="$(nginx -t 2>&1 || true)"
            if ! echo "$NGINX_TEST" | grep -q "successful"; then
                NGINX_TEST="$(sudo -n nginx -t 2>&1 || true)"
            fi
            if echo "$NGINX_TEST" | grep -q "successful"; then
                if try_priv systemctl reload nginx || try_priv nginx -s reload; then
                    echo "  [⚡] nginx deployed & reloaded"
                else
                    echo "  [!] nginx config installed but not reloaded; run: sudo systemctl reload nginx"
                fi
            else
                # Put the working config back so a later reload or reboot cannot pick up the broken one
                if [ "$HAD_OLD" = "1" ]; then
                    try_priv cp "$NGINX_DEST.bak" "$NGINX_DEST"
                else
                    try_priv rm -f "$NGINX_DEST"
                fi
                echo "  [✗] nginx config did not pass nginx -t — previous config restored, not reloaded"
                echo "$NGINX_TEST" | sed 's/^/      /'
            fi
        fi
    else
        echo ""
        echo "  [—] nginx unchanged"
    fi
fi

echo ""
echo "  ✅ DEPLOYED"
echo ""
