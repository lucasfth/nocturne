#!/bin/bash
# Nocturne worker bootstrap: fresh installs only, root-owned reviewed package.
# Usage: sudo env -i PATH=/usr/bin:/bin:/usr/sbin:/sbin /bin/bash /trusted/package/bootstrap-remote.sh <worker-id>
# See docs/installer-security.md; never run a worker-writable installer as root.
# Prints only: secret sha256 digest, tunnel public key. Never prints secrets.
set -euo pipefail
ID=${1:?worker id}
R=/opt/nocturne
LLAMA_TAG=b11588
LLAMA_SHA=95039180209cb8b9a2fadedd03e061c5131fed17e1eb423b3dcf8c7848d12b9d
UV_VER=0.13.0
UV_SHA=a9c1b29002cf3c83f07fa9cd8a887a3be0107d90e23189721221e7257db8e3d6
MODEL=Ornith-1.5-9B-Q4_K_M.gguf
MODEL_SHA=70c112196e0b7023803c9762752e46d29e612a92c83f995bc3ba1ceb07e8fab6
MODEL_URL=https://huggingface.co/ornith-ai/Ornith-1.5-9B-GGUF/resolve/main/$MODEL
# Invoke with a clean root environment from a reviewed root-owned package.
PACKAGE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -L)
source "$PACKAGE/install-security.sh"
security_init
# Never reopen worker-controlled legacy state, even if ownership was repaired.
[ ! -e "$R" ] && [ ! -L "$R" ] || { echo 'Bootstrap requires a fresh installation.' >&2; exit 1; }
S=""
[[ $(uname -m) == arm64 ]] || { echo "not arm64"; exit 1; }

# 1. dedicated hidden non-login service account
if ! id nocturne-worker >/dev/null 2>&1; then
  uid=550; while dscl . -list /Users UniqueID | awk '{print $2}' | grep -qx "$uid"; do uid=$((uid+1)); done
  $S dscl . -create /Users/nocturne-worker
  $S dscl . -create /Users/nocturne-worker UniqueID "$uid"
  $S dscl . -create /Users/nocturne-worker PrimaryGroupID 20
  $S dscl . -create /Users/nocturne-worker UserShell /usr/bin/false
  $S dscl . -create /Users/nocturne-worker NFSHomeDirectory $R/home
  $S dscl . -create /Users/nocturne-worker RealName "nocturne worker"
  $S dscl . -create /Users/nocturne-worker IsHidden 1
  $S dscl . -create /Users/nocturne-worker Password '*'
fi
echo "user: nocturne-worker uid=$(id -u nocturne-worker)"
$S mkdir -m 755 $R
$S mkdir -m 755 $R/bin $R/models
$S mkdir -m 700 $R/data $R/home $R/logs
private_stage
T=$STAGE

# 2. uv (pinned) -> managed Python 3.12 venv with httpx
if [ ! -x $R/bin/uv ]; then
  curl -fsSL -o "$T/uv.tgz" "https://github.com/astral-sh/uv/releases/download/$UV_VER/uv-aarch64-apple-darwin.tar.gz"
  echo "$UV_SHA  $T/uv.tgz" | shasum -a 256 -c - >/dev/null
  tar -xzf "$T/uv.tgz" -C "$T"; $S install -m 755 "$T/uv-aarch64-apple-darwin/uv" $R/bin/uv
fi
if [ ! -x $R/.venv/bin/python ]; then
  $S env -i HOME=/var/root PATH="$PATH" UV_PYTHON_INSTALL_DIR=$R/python UV_CACHE_DIR="$STAGE/uv-cache" UV_PYTHON_PREFERENCE=only-managed $R/bin/uv --no-config venv -q --python 3.12 $R/.venv
fi
$S env -i HOME=/var/root PATH="$PATH" UV_PYTHON_INSTALL_DIR=$R/python UV_CACHE_DIR="$STAGE/uv-cache" UV_PYTHON_PREFERENCE=only-managed UV_LINK_MODE=copy $R/bin/uv --no-config pip install -q --python $R/.venv/bin/python 'httpx==0.28.1'
security_check tree "$R/python"
security_check tree "$R/.venv"
echo 'python: environment installed'

# 3. llama.cpp (pinned release + sha256)
if [ ! -f $R/bin/llama/.tag ] || [ "$(cat $R/bin/llama/.tag)" != $LLAMA_TAG ]; then
  curl -fsSL -o "$T/llama.tgz" "https://github.com/ggml-org/llama.cpp/releases/download/$LLAMA_TAG/llama-$LLAMA_TAG-bin-macos-arm64.tar.gz"
  echo "$LLAMA_SHA  $T/llama.tgz" | shasum -a 256 -c - >/dev/null
  $S rm -rf $R/bin/llama; $S mkdir -m 755 $R/bin/llama
  $S tar -xzf "$T/llama.tgz" -C $R/bin/llama
  echo $LLAMA_TAG | $S tee $R/bin/llama/.tag >/dev/null
  $S xattr -dr com.apple.quarantine $R/bin/llama 2>/dev/null || true
fi
security_check tree "$R/bin/llama"
$S chmod -R go+rX $R/bin/llama
SERVER=$($S find $R/bin/llama -type f -name llama-server | head -1)
[ -n "$SERVER" ] || { echo "llama-server missing"; exit 1; }
# Keep release-relative dylib lookup intact without a destination symlink.
printf '#!/bin/bash\nexec %q "$@"\n' "$SERVER" > "$R/bin/llama-server"
chmod 755 "$R/bin/llama-server"
echo "llama: $LLAMA_TAG $SERVER"

# 4. secrets generated locally; only digest/pubkey leave the Mac
if ! $S test -f $R/data/worker.secret; then
  openssl rand -base64 48 | tr -d '\n=+/' | cut -c1-48 | $S tee $R/data/worker.secret >/dev/null
fi
if ! $S test -f $R/data/tunnel_key; then
  $S ssh-keygen -q -t ed25519 -N '' -C "nocturne-tunnel-$ID" -f $R/data/tunnel_key
fi
echo "$ID" | $S tee $R/data/worker.id >/dev/null
# Fresh root-private state only; never transfer code or environments.
$S chmod 600 $R/data/worker.secret $R/data/tunnel_key
# Transfer files while their parent is still root-private, then directories.
$S chown nocturne-worker:staff $R/data/worker.secret $R/data/tunnel_key $R/data/tunnel_key.pub $R/data/worker.id
$S chown nocturne-worker:staff $R/data $R/home $R/logs
$S chmod -R go+rX $R/python $R/.venv

# 5. Synchronous verified data download; never execute worker-writable scripts.
curl -fL -sS -o "$STAGE/model.part" "$MODEL_URL"
echo "$MODEL_SHA  $STAGE/model.part" | shasum -a 256 -c - >/dev/null
install -m 644 "$STAGE/model.part" "$R/models/$MODEL"
echo 'model: verified'

echo "DIGEST $ID $(worker_run /bin/cat $R/data/worker.secret | tr -d '\n' | shasum -a 256 | awk '{print $1}')"
echo "PUBKEY $ID $(worker_run /bin/cat $R/data/tunnel_key.pub)"
