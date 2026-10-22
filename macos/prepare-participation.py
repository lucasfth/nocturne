"""Generate inert pilot launchd manifests. Does not install/start/stop anything.

Root service code/Python/executable MUST be administrator-owned, not the existing
worker-writable /opt/nocturne tree. Review all output before an approved pilot.
"""
import argparse
import json
from pathlib import Path
import plistlib


def manifests(python, root, app, uid, gid, worker_uid, worker_gid, model_argv, enabled=False, automatic_participation=False):
    if any(type(v) is not int or v <= 0 for v in (uid, gid, worker_uid, worker_gid)):
        raise ValueError('non-root identities required')
    if not all(Path(p).is_absolute() for p in (python, root, app, model_argv[0])):
        raise ValueError('absolute paths required')
    ipc = '/private/var/db/nocturne-participation-ipc'
    arguments = [python, '-m', 'worker.participation_service',
                 '--state-path', '/private/var/db/nocturne-participation/state.json',
                 '--socket-path', ipc + '/control.sock', '--status-socket', ipc + '/worker.sock',
                 '--permitted-uid', str(uid), '--worker-uid', str(worker_uid),
                 '--worker-gid', str(worker_gid)]
    if enabled:
        arguments.append('--enabled')
    if automatic_participation:
        if not enabled:
            raise ValueError('automatic participation requires explicit enabled enrollment')
        arguments.append('--automatic-participation')
    arguments += ['--model-argv', *model_argv]
    common = dict(RunAtLoad=True, KeepAlive=True, ThrottleInterval=30)
    daemon = dict(common, Label='io.nocturne.participation', UserName='root',
                  WorkingDirectory=root, ProgramArguments=arguments,
                  EnvironmentVariables={'PYTHONDONTWRITEBYTECODE': '1', 'PYTHONNOUSERSITE': '1', 'PATH': '/usr/bin:/bin:/usr/sbin:/sbin'})
    companion = dict(common, Label='io.nocturne.companion',
                     ProgramArguments=[app + '/Contents/MacOS/Nocturne', ipc + '/control.sock', '0'],
                     LimitLoadToSessionType='Aqua')
    worker_patch = {'NOCTURNE_PARTICIPATION_SOCKET': ipc + '/worker.sock',
                    'NOCTURNE_PARTICIPATION_OWNER_UID': '0'}
    return daemon, companion, worker_patch


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for key in ('python', 'root', 'app', 'output'):
        p.add_argument('--' + key, required=True)
    for key in ('uid', 'gid', 'worker-uid', 'worker-gid'):
        p.add_argument('--' + key, required=True, type=int)
    p.add_argument('--enabled', action='store_true')
    p.add_argument('--automatic-participation', action='store_true')
    p.add_argument('--model-argv', nargs=argparse.REMAINDER, required=True)
    a = p.parse_args()
    out = Path(a.output); out.mkdir(parents=True, exist_ok=False)
    daemon, companion, patch = manifests(a.python, a.root, a.app, a.uid, a.gid,
                                         a.worker_uid, a.worker_gid, a.model_argv, a.enabled, a.automatic_participation)
    for name, value in [('io.nocturne.participation.plist', daemon), ('io.nocturne.companion.plist', companion)]:
        (out / name).write_bytes(plistlib.dumps(value))
    (out / 'worker-environment-merge.json').write_text(json.dumps(patch, indent=2))

if __name__ == '__main__': main()
