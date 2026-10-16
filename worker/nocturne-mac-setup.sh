#!/bin/bash
# Owner-consented preparation, using only standard macOS tools. No Python.
set -euo pipefail
set +x
umask 077
fail() { printf '%s\n' 'Setup refused: invalid/unsafe input or destination. No input values are logged.' >&2; exit 1; }
CONFIG=; CHECK=0; UNDO=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --config) [ "$#" -ge 2 ] || fail; CONFIG=$2; shift 2 ;;
    --check|--dry-run) CHECK=1; shift ;;
    --undo) UNDO=1; shift ;;
    --help|-h) printf '%s\n' 'Usage: bash nocturne-mac-setup.sh --config FILE [--check|--dry-run] [--undo]'; exit 0 ;;
    *) fail ;;
  esac
done
[ "$(uname -s)" = Darwin ] && [ "$(id -u)" != 0 ] || fail
[ -n "$CONFIG" ] && [ -f "$CONFIG" ] && [ ! -L "$CONFIG" ] || fail
[ "$(stat -f %z "$CONFIG")" -le 65536 ] || fail
BASE=$(cd -P -- "$(dirname -- "$CONFIG")" && pwd)
CONFIG="$BASE/$(basename -- "$CONFIG")"
/usr/bin/plutil -convert xml1 -o /dev/null "$CONFIG" >/dev/null 2>&1 || fail
# Exact schema: reject extra fields rather than accepting secret-bearing config.
SCHEMA=$(/usr/bin/plutil -convert xml1 -o - "$CONFIG" 2>/dev/null | awk '/<key>/{gsub(/^[ \t]*<key>|<\/key>.*$/, ""); print}' | LC_ALL=C sort)
[ "$SCHEMA" = "$(printf '%s\n' gateway_host gateway_host_public_key_file gateway_ssh_user operator_public_key_files worker_id)" ] || fail
value() {
  [ "$(/usr/bin/plutil -type "$1" "$CONFIG" 2>/dev/null)" = string ] || fail
  /usr/bin/plutil -extract "$1" raw -o - "$CONFIG" 2>/dev/null
}
ID=$(value worker_id); HOST=$(value gateway_host); USERNAME=$(value gateway_ssh_user)
[[ "$ID" =~ ^[a-z0-9][a-z0-9-]{0,63}$ ]] || fail
[[ "$HOST" =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]{0,252}$ ]] || fail
[[ "$USERNAME" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || fail
[ "$(/usr/bin/plutil -type operator_public_key_files "$CONFIG" 2>/dev/null)" = array ] || fail
COUNT=$(/usr/bin/plutil -extract operator_public_key_files raw -o - "$CONFIG" 2>/dev/null)
[[ "$COUNT" =~ ^[0-9]+$ ]] && [ "$COUNT" -ge 1 ] && [ "$COUNT" -le 16 ] || fail
# A read-only check does not create staging files, even for public-key validation.
key() {
  local path=$1 text type body extra
  case "$path" in /*) ;; *) path="$BASE/$path" ;; esac
  [ -f "$path" ] && [ ! -L "$path" ] && [ "$(stat -f %z "$path")" -le 65536 ] || fail
  text=$(cat -- "$path")
  [[ "$text" != *$'\n'* && "$text" != *$'\r'* ]] || fail
  read -r type body extra <<< "$text"
  case "$type" in ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp256|ecdsa-sha2-nistp384|ecdsa-sha2-nistp521) ;; *) fail ;; esac
  /usr/bin/ssh-keygen -l -f "$path" >/dev/null 2>&1 || fail
  printf '%s %s\n' "$type" "$body"
}
key "$(value gateway_host_public_key_file)" >/dev/null
KEYS=(); i=0
while [ "$i" -lt "$COUNT" ]; do KEYS+=("$(key "$(value "operator_public_key_files.$i")")"); i=$((i+1)); done
safe_path() {
  local p=$1 owner mode
  while :; do
    [ ! -L "$p" ] || fail
    if [ -e "$p" ]; then
      owner=$(stat -f %u "$p"); mode=$(stat -f %Lp "$p")
      [ "$owner" = 0 ] || [ "$owner" = "$(id -u)" ] || fail
      [ "$((8#$mode & 022))" -eq 0 ] || fail
    fi
    [ "$p" != / ] || break
    p=$(dirname -- "$p")
  done
}
TARGET="$HOME/.ssh/authorized_keys"
safe_path "$TARGET"
if [ -e "$TARGET" ]; then [ -f "$TARGET" ] && [ "$(stat -f %l "$TARGET")" = 1 ] || fail; fi
BEGIN='# BEGIN NOCTURNE OWNER ACCESS'; END='# END NOCTURNE OWNER ACCESS'
filter() {
  awk -v begin="$BEGIN" -v end="$END" '
    {marker=$0; sub(/\r$/, "", marker)}
    marker == begin {if (inside || seen++) exit 1; inside=1; next}
    marker == end {if (!inside) exit 1; inside=0; next}
    !inside {print}
    END {if (inside) exit 1}' "$@"
}
if [ -e "$TARGET" ]; then filter "$TARGET" >/dev/null || fail; fi
if [ "$CHECK" = 1 ]; then printf '%s\n' 'CHECK OK: no writes, network, sudo or Python.'; exit 0; fi
mkdir -p -- "$HOME/.ssh"
safe_path "$TARGET"
TMP=$(mktemp "$HOME/.ssh/.nocturne-XXXXXX")
trap 'rm -f -- "$TMP"' EXIT
if [ -e "$TARGET" ]; then filter "$TARGET" > "$TMP" || fail; fi
if [ "$UNDO" != 1 ]; then
  printf '%s\n' "$BEGIN" >> "$TMP"
  printf '%s\n' "${KEYS[@]}" | awk '!seen[$0]++' >> "$TMP"
  printf '%s\n' "$END" >> "$TMP"
fi
if [ ! -e "$TARGET" ] || ! cmp -s "$TARGET" "$TMP"; then
  BACKUP="$TARGET.nocturne-before"
  if [ -e "$TARGET" ] && [ ! -e "$BACKUP" ] && [ ! -L "$BACKUP" ]; then
    (set -o noclobber; cat -- "$TARGET" > "$BACKUP") || fail
  fi
  mv -f -- "$TMP" "$TARGET"
fi
chmod 600 "$TARGET"
printf '%s\n' 'Owner SSH managed block updated. No admin/sudo, sleep/update policy or worker services changed.'
printf '%s\n' 'Enable Remote Login for this owner in System Settings > General > Sharing.'
printf '%s\n' 'Verify this Mac host fingerprint locally: ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub'
