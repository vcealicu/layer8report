#!/bin/bash
# ─────────────────────────────────────────────────────────
# Deploy script — layer8report.com
# Run as: coder
# ─────────────────────────────────────────────────────────
set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$DEPLOY_DIR")"
SRC_DIR="$PROJECT_DIR/public"
DEST_DIR="/var/www/layer8report.com/public"
NGINX_SRC="$DEPLOY_DIR/nginx.conf"
NGINX_DEST="/etc/nginx/sites-available/www.layer8report.com"
SITE_URL="https://www.layer8report.com"

echo ""
echo "  ╔═══════════════════════════════════════╗"
echo "  ║  🚀 DEPLOY — layer8report.com"
echo "  ╚═══════════════════════════════════════╝"
echo ""

# Sync public/ → /var/www/
echo "> SYNCING FILES..."
rsync -av --checksum --delete \
    --exclude='sitemap.xml' \
    --out-format="  [↑] %n" \
    "$SRC_DIR"/ "$DEST_DIR"/

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
    <changefreq>weekly</changefreq>
    <priority>1.0</priority>
  </url>
EOF

while IFS= read -r -d '' html_file; do
    rel="${html_file#$SRC_DIR/}"
    [ "$rel" = "index.html" ] && continue
    [[ "$rel" == error/* ]] && continue
    if [[ "$rel" == */index.html ]]; then
        url="/${rel%index.html}"
    else
        url="/${rel%.html}"
    fi
    cat >> "$DEST_DIR/sitemap.xml" << EOF
  <url>
    <loc>${SITE_URL}${url}</loc>
    <lastmod>${LASTMOD}</lastmod>
    <changefreq>weekly</changefreq>
    <priority>0.8</priority>
  </url>
EOF
done < <(find "$SRC_DIR" -name "*.html" -type f -print0)

echo "</urlset>" >> "$DEST_DIR/sitemap.xml"
echo "  [📍] sitemap.xml"

# Nginx config (if changed)
if [ -f "$NGINX_SRC" ]; then
    if ! cmp -s "$NGINX_SRC" "$NGINX_DEST" 2>/dev/null; then
        echo ""
        echo "> NGINX CONFIG..."
        sudo cp "$NGINX_SRC" "$NGINX_DEST"
        if sudo nginx -t 2>&1 | grep -q "successful"; then
            sudo systemctl reload nginx
            echo "  [⚡] nginx deployed & reloaded"
        else
            echo "  [✗] nginx config invalid — not reloaded"
        fi
    else
        echo ""
        echo "  [—] nginx unchanged"
    fi
fi

echo ""
echo "  ✅ DEPLOYED"
echo ""
