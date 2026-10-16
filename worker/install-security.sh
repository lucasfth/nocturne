#!/bin/bash
# Source only from an operator-reviewed, root-owned package. Never repair/chown
# an existing untrusted install. Requires Apple's system Python, not the venv.
export PATH=/usr/bin:/bin:/usr/sbin:/sbin
unset PYTHONPATH PYTHONHOME BASH_ENV ENV CDPATH
umask 077

security_check() {
  /usr/bin/python3 -I - "$@" <<'PY'
import os, stat, sys

INSTALL_ROOT = '/opt/nocturne'
MAX_LINK_HOPS = 40

def within(path, root):
    return path == root or path.startswith(root + '/')

def link_roots(path):
    python = INSTALL_ROOT + '/python'
    venv = INSTALL_ROOT + '/.venv'
    llama = INSTALL_ROOT + '/bin/llama'
    if within(path, python):
        return (python,)
    if within(path, venv):
        return (venv, python)
    if within(path, llama):
        return (llama,)
    raise ValueError('link outside runtime/release')

def check(path, links=False):
    import subprocess
    hops = 0

    def metadata(current):
        s = os.lstat(current)
        if s.st_uid != 0 or (not stat.S_ISLNK(s.st_mode) and s.st_mode & 0o022):
            raise ValueError('non-root owner or writable permissions')
        # Inspect the link itself, not just its eventual destination.
        listing = subprocess.check_output(['/bin/ls', '-lde', current], text=True)
        if len(listing.splitlines()) != 1:
            raise ValueError('ACL')
        return s

    def resolve(raw, roots=None):
        nonlocal hops
        current = '/'
        metadata(current)
        for part in raw.split('/')[1:]:
            if part in ('', '.'):
                continue
            if part == '..':
                current = os.path.dirname(current)
                # Never escape a boundary and then return through another hop.
                if roots and not any(within(current, r) for r in roots):
                    raise ValueError('link escapes boundary')
            else:
                current = os.path.join(current, part)
            if roots and not any(within(current, r) or within(r, current)
                                 for r in roots):
                raise ValueError('link outside boundary')
            s = metadata(current)
            if stat.S_ISLNK(s.st_mode):
                if not links:
                    raise ValueError('symlink')
                allowed = link_roots(current)
                if roots:
                    allowed = tuple(r for r in allowed if r in roots)
                    if not allowed:
                        raise ValueError('link crosses boundary')
                hops += 1
                if hops > MAX_LINK_HOPS:
                    raise ValueError('too many link hops')
                target = os.readlink(current)
                if not os.path.isabs(target):
                    target = os.path.dirname(current) + '/' + target
                current = resolve(target, allowed)
        if roots and not any(within(current, r) for r in roots):
            raise ValueError('link outside boundary')
        return current

    if '..' in path.split(os.sep):
        raise ValueError('parent traversal')
    return resolve(os.path.abspath(path))

try:
    mode, path = sys.argv[1:]
    check(path)
    if mode == 'tree':
        for base, dirs, files in os.walk(path, followlinks=False):
            for name in dirs + files:
                p = os.path.join(base, name)
                check(p, links=True)
                if not (os.path.isfile(p) or os.path.isdir(p)):
                    raise ValueError('special file')
except Exception:
    print('Unsafe installer path; refuse installation (reprovision legacy trees offline).', file=sys.stderr)
    sys.exit(1)
PY
}

security_init() {
  [ "$(id -u)" = 0 ] || { echo 'Run reviewed root-owned installer as root with a clean environment.' >&2; exit 1; }
  security_check path /opt
  security_check tree "$PACKAGE"
  cd /
  if [ -e "$R" ] || [ -L "$R" ]; then
    security_check path "$R"
    # Never walk worker-writable state as root, or treat it as trusted input.
    for p in bin models python .venv worker .install-cache; do
      if [ -e "$R/$p" ] || [ -L "$R/$p" ]; then
        security_check tree "$R/$p"
      fi
    done
  fi
}

worker_run() {
  /usr/bin/sudo -n -u nocturne-worker /usr/bin/env -i \
    PATH=/usr/bin:/bin:/usr/sbin:/sbin HOME="$R/home" "$@"
}

private_stage() {
  # Parent was checked before creation; never use a shared /tmp pathname.
  STAGE=$(mktemp -d "$R/.install-XXXXXXXX")
  trap 'rm -rf -- "$STAGE"' EXIT
}
