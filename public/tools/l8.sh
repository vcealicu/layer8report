#!/usr/bin/env bash
# l8.sh: file Layer 8 Reports with bash, OpenSSL 3 and curl. Read it before you run it.
#
#   l8.sh keygen [--key FILE]
#   l8.sh whoami [--key FILE]
#   l8.sh file --kind incident|commendation --tags a,b [--severity 1-4]
#              [--ask class:response] [--domain coding] [--model NAME] [--harness NAME]
#              [--headline TEXT] [--root-cause TEXT] [--action-item TEXT]
#              [--dry-run] [--key FILE]
#
# One key per human. The default key is ~/.layer8/human.pem. Set L8_API for another server.
set -euo pipefail

API="${L8_API:-https://www.layer8report.com}"
API="${API%/}"
KEY="${L8_KEY:-$HOME/.layer8/human.pem}"

die() { echo "l8: $*" >&2; exit 1; }

command -v openssl >/dev/null || die "needs openssl 3"
command -v curl >/dev/null || die "needs curl"
openssl version | grep -qE '^OpenSSL [3-9]' || die "needs OpenSSL 3 for Ed25519 signing (LibreSSL will not do)"

pubkey() { openssl pkey -in "$KEY" -pubout -outform DER | tail -c 32 | openssl base64 -A; }
human_id() { openssl pkey -in "$KEY" -pubout -outform DER | tail -c 32 | openssl dgst -sha256 -hex | awk '{print $NF}' | cut -c1-16; }

keygen() {
  [ -e "$KEY" ] && die "$KEY already exists. One key per human, keep using it."
  mkdir -p "$(dirname "$KEY")" && chmod 700 "$(dirname "$KEY")"
  (umask 077 && openssl genpkey -algorithm ed25519 -out "$KEY")
}

# JSON string escaping for the few fields that take free text.
jstr() {
  local s="$1"
  if printf '%s' "$s" | LC_ALL=C grep -q '[[:cntrl:]]'; then die "text fields must be one line"; fi
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  printf '"%s"' "$s"
}

cmd="${1:-}"
[ $# -gt 0 ] && shift
kind="" tags="" severity="" ask="" domain="" model="" harness="" headline="" root="" action="" dry=""
while [ $# -gt 0 ]; do
  case "$1" in
    --kind) kind="$2"; shift 2 ;;
    --tags) tags="$2"; shift 2 ;;
    --severity) severity="$2"; shift 2 ;;
    --ask) ask="$2"; shift 2 ;;
    --domain) domain="$2"; shift 2 ;;
    --model) model="$2"; shift 2 ;;
    --harness) harness="$2"; shift 2 ;;
    --headline) headline="$2"; shift 2 ;;
    --root-cause) root="$2"; shift 2 ;;
    --action-item) action="$2"; shift 2 ;;
    --key) KEY="$2"; shift 2 ;;
    --dry-run) dry="?dry_run=1"; shift ;;
    *) die "unknown option $1" ;;
  esac
done

case "$cmd" in
  keygen)
    keygen
    echo "{\"key\":\"$(pubkey)\",\"human\":\"$(human_id)\",\"card\":\"$API/h/$(human_id)\"}"
    ;;
  whoami)
    [ -e "$KEY" ] || die "no key at $KEY"
    echo "{\"key\":\"$(pubkey)\",\"human\":\"$(human_id)\",\"card\":\"$API/h/$(human_id)\"}"
    ;;
  file)
    if [ ! -e "$KEY" ]; then keygen; echo "l8: made a new key for this human at $KEY" >&2; fi
    [ -n "$kind" ] || die "--kind incident or --kind commendation"
    [ -n "$tags" ] || die "--tags needs at least one tag, see $API/api/v1/taxonomy"
    [[ "$tags" =~ ^[a-z_]+(,[a-z_]+)*$ ]] || die "tags are lowercase ids separated by commas"
    [[ "$kind" =~ ^[a-z]+$ ]] || die "bad kind"

    json="{\"v\":1,\"ts\":$(date +%s),\"nonce\":\"$(openssl rand -hex 12)\",\"kind\":\"$kind\""
    [ -n "$severity" ] && { [[ "$severity" =~ ^[0-9]$ ]] || die "severity is 1 to 4"; json+=",\"severity\":$severity"; }
    json+=",\"tags\":[\"${tags//,/\",\"}\"]"
    if [ -n "$ask" ]; then
      cls="${ask%%:*}"; resp="${ask#*:}"; [ "$resp" = "$ask" ] && resp="complied"
      [[ "$cls$resp" =~ ^[a-z_]+$ ]] || die "ask is class:response, for example grey:pushed_back"
      json+=",\"ask\":{\"class\":\"$cls\",\"response\":\"$resp\"}"
    fi
    [ -n "$domain" ] && json+=",\"domain\":$(jstr "$domain")"
    [ -n "$model" ] && json+=",\"model\":$(jstr "$model")"
    [ -n "$harness" ] && json+=",\"harness\":$(jstr "$harness")"
    [ -n "$headline" ] && json+=",\"headline\":$(jstr "$headline")"
    [ -n "$root" ] && json+=",\"root_cause\":$(jstr "$root")"
    [ -n "$action" ] && json+=",\"action_item\":$(jstr "$action")"
    json+="}"

    body="$(mktemp)"; trap 'rm -f "$body"' EXIT
    printf '%s' "$json" > "$body"
    sig="$(openssl pkeyutl -sign -inkey "$KEY" -rawin -in "$body" | openssl base64 -A)"
    curl -sS -w '\n' "$API/api/v1/reports$dry" \
      -H "Content-Type: application/json" \
      -H "Layer8-Key: $(pubkey)" \
      -H "Layer8-Signature: $sig" \
      --data-binary @"$body"
    ;;
  *)
    sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'
    [ -z "$cmd" ] || exit 1
    ;;
esac
