#!/bin/bash
# Installs nocturne launchd daemons on a safely bootstrapped Mac.
# Run as root with a clean environment from a reviewed root-owned package.
# Package includes agent.py, macos_sensors.py, __init__.py,
# benchmark_setup.py, model_benchmark.py and install-security.sh.
# See docs/model-benchmark.md for the opt-in Q4 catalog matching fixed flags.
set -euo pipefail
S=""; R=/opt/nocturne
PACKAGE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -L)
source "$PACKAGE/install-security.sh"
security_init
security_check path /Library/LaunchDaemons
for label in io.nocturne.tunnel io.nocturne.llama io.nocturne.worker; do
  p=/Library/LaunchDaemons/$label.plist
  if [ -e "$p" ] || [ -L "$p" ]; then security_check path "$p"; fi
done
ID=$(worker_run /bin/cat $R/data/worker.id)
# Coordinator host (LAN IP or name) and its SSH host key line, e.g. 'ssh-ed25519 AAAA...'.
COORD=${NOCTURNE_COORDINATOR_HOST:?set NOCTURNE_COORDINATOR_HOST}
COORD_HOSTKEY=${NOCTURNE_COORDINATOR_HOSTKEY:?set NOCTURNE_COORDINATOR_HOSTKEY}
COORD_USER=${NOCTURNE_COORDINATOR_USER:?set NOCTURNE_COORDINATOR_USER}
[[ "$COORD" =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]*$ ]] || { echo "Invalid coordinator host" >&2; exit 1; }
[[ "$COORD_USER" =~ ^[a-z_][a-z0-9_-]*$ ]] || { echo "Invalid coordinator SSH user" >&2; exit 1; }

# Validate the one supplied public host key before recording a known_hosts pin.
[[ "$COORD_HOSTKEY" != *$'\n'* && "$COORD_HOSTKEY" != *$'\r'* ]] || { echo 'Invalid coordinator host public key' >&2; exit 1; }
case "$COORD_HOSTKEY" in ssh-ed25519\ *|ssh-rsa\ *|ecdsa-sha2-nistp256\ *|ecdsa-sha2-nistp384\ *|ecdsa-sha2-nistp521\ *) ;; *) echo 'Invalid coordinator host public key' >&2; exit 1 ;; esac
printf '%s\n' "$COORD_HOSTKEY" | /usr/bin/ssh-keygen -l -f /dev/stdin >/dev/null 2>&1 || { echo 'Invalid coordinator host public key' >&2; exit 1; }

# Gate before any service/config mutation or readiness advertisement. Reports
# are private and unique; explicit skips remain recorded as unbenchmarked.
for module in agent macos_sensors __init__ benchmark_setup model_benchmark; do
  if ! test -r "$PACKAGE/$module.py"; then
    echo "Missing staged $module.py; stage all worker/installer benchmark modules (see docs/model-benchmark.md)." >&2
    exit 1
  fi
done
worker_run /usr/bin/env PYTHONPATH="$PACKAGE" \
  NOCTURNE_BENCHMARK_CATALOG="${NOCTURNE_BENCHMARK_CATALOG:-}" \
  NOCTURNE_SKIP_BENCHMARK="${NOCTURNE_SKIP_BENCHMARK:-}" \
  $R/.venv/bin/python -m benchmark_setup \
  --output "$R/data/benchmark-setup-$(date +%s)-$$.json"

$S mkdir -p $R/worker
$S chmod 755 $R/worker
$S install -m 644 "$PACKAGE/agent.py" "$PACKAGE/macos_sensors.py" "$PACKAGE/__init__.py" $R/worker/
printf '%s %s\n' "$COORD" "$COORD_HOSTKEY" | worker_run /usr/bin/tee $R/data/known_hosts >/dev/null

# llama supervisor: model server runs inside the 17:25-08:00 window, on AC or on battery above 20%, with a verified model.
$S tee $R/bin/llama-supervisor.sh >/dev/null <<'EOF'
#!/bin/bash
R=/opt/nocturne; M=$R/models/Ornith-1.5-9B-Q4_K_M.gguf; PID=
stop() { [ -n "$PID" ] && kill "$PID" 2>/dev/null && wait "$PID" 2>/dev/null; PID=; }
trap 'stop; exit 0' TERM INT
while true; do
  hm=$(TZ=Europe/Copenhagen date +%H%M); hm=$((10#$hm))
  want=0
  b=$(pmset -g batt); pct=$(echo "$b" | grep -oE '[0-9]+%' | head -1 | tr -d %); pct=${pct:-0}
  # On AC always; on battery only above 20% (start needs >25% to avoid flapping).
  if echo "$b" | grep -q "'AC Power'"; then pw=1; elif [ -n "$PID" ] && [ $pct -gt 20 ]; then pw=1; elif [ $pct -gt 25 ]; then pw=1; else pw=0; fi
  if { [ $hm -ge 1725 ] || [ $hm -lt 800 ]; } && [ $pw = 1 ] && [ -f "$M" ]; then want=1; fi
  [ -f $R/data/disabled ] && want=0
  if [ $want = 1 ] && { [ -z "$PID" ] || ! kill -0 "$PID" 2>/dev/null; }; then
    $R/bin/llama-server --host 127.0.0.1 --port 8080 --model "$M" --alias Ornith-1.5-9B \
      --ctx-size 65536 --parallel 2 -ctk q8_0 -ctv q8_0 -fa on -ngl 999 --reasoning-format deepseek --spec-type draft-mtp --spec-draft-n-max 2 --no-webui >>$R/logs/llama.log 2>&1 &
    PID=$!
    # Keep the Mac awake (also on battery) only while the model server runs.
    /usr/bin/caffeinate -i -m -w $PID &
  elif [ $want = 0 ] && [ -n "$PID" ]; then stop; fi
  sleep 30
done
EOF
$S chmod 755 $R/bin/llama-supervisor.sh

/usr/bin/python3 -I - "$R" "$ID" "$COORD" "$COORD_USER" <<'PY'
import plistlib,sys
from pathlib import Path
R,ID,F,U=sys.argv[1:]
base={'UserName':'nocturne-worker','GroupName':'staff','WorkingDirectory':R,'RunAtLoad':True,'KeepAlive':True,'ProcessType':'Background','LowPriorityIO':True,'Nice':10,'ThrottleInterval':30,'EnvironmentVariables':{'HOME':R+'/home','PATH':'/usr/bin:/bin:/usr/sbin:/sbin'}}
jobs=[
 dict(base,Label='io.nocturne.tunnel',StandardErrorPath=R+'/logs/tunnel.log',ProgramArguments=['/usr/bin/ssh','-F','/dev/null','-o','GlobalKnownHostsFile=/dev/null','-N','-i',R+'/data/tunnel_key','-o','IdentitiesOnly=yes','-o','BatchMode=yes','-o','ExitOnForwardFailure=yes','-o','ServerAliveInterval=15','-o','ServerAliveCountMax=3','-o','StrictHostKeyChecking=yes','-o','UserKnownHostsFile='+R+'/data/known_hosts','-L','127.0.0.1:18701:127.0.0.1:8701',U+'@'+F]),
 dict(base,Label='io.nocturne.llama',StandardErrorPath=R+'/logs/supervisor.log',ProgramArguments=['/bin/bash',R+'/bin/llama-supervisor.sh']),
 dict(base,Label='io.nocturne.worker',StandardErrorPath=R+'/logs/worker.log',ProgramArguments=[R+'/.venv/bin/python','-m','worker.agent'],EnvironmentVariables=dict(base['EnvironmentVariables'],COORDINATOR_URL='http://127.0.0.1:18701',WORKER_ID=ID,WORKER_SECRET_FILE=R+'/data/worker.secret')),
]
for j in jobs:
    p=Path('/Library/LaunchDaemons')/(j['Label']+'.plist'); p.write_bytes(plistlib.dumps(j)); p.chmod(0o644)
print('plists written')
PY
# No privileged chmod/chown/read/write through worker-controlled state paths.
for l in io.nocturne.tunnel io.nocturne.llama io.nocturne.worker; do
  $S launchctl bootout system/$l 2>/dev/null || true
  $S launchctl bootstrap system /Library/LaunchDaemons/$l.plist
done
sleep 5
for l in io.nocturne.tunnel io.nocturne.llama io.nocturne.worker; do
  echo "$l: $($S launchctl print system/$l 2>/dev/null | awk '/^\tstate =/{print $3}')"
done
# Reviewed source package is retained; no deletion of caller-supplied paths.
