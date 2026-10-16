"""Disposable HOME only: execute embedded entry point, never launchd/sudo."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

import pytest

ROOT = Path(__file__).resolve().parents[1]
pytestmark = pytest.mark.skipif(sys.platform != 'darwin', reason='Native macOS shell/plutil contract')

@pytest.fixture
def protected_temp():
    if os.getuid() == 0:
        pytest.skip('Owner setup tests require a non-root account')
    with tempfile.TemporaryDirectory(prefix='.nocturne-test-', dir=Path.home()) as directory:
        yield Path(directory)

@pytest.fixture
def kit(protected_temp):
    # /var is a macOS symlink; use physical path for destination safety policy.
    tmp_path = protected_temp.resolve()
    home = tmp_path / 'Owner space'
    home.mkdir(mode=0o700)
    pub = tmp_path / 'operator key'
    subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(pub)], check=True)
    config = tmp_path / 'setup.json'
    data = dict(worker_id='mac-example', gateway_host='gateway.example.invalid', gateway_ssh_user='operator', gateway_host_public_key_file=pub.name+'.pub', operator_public_key_files=[pub.name+'.pub'])
    config.write_text(json.dumps(data))
    def run(*args):
        # Tests may run on any CI OS: simulate only platform, not file/key validation.
        return subprocess.run(['/bin/bash', str(ROOT/'worker/nocturne-mac-setup.sh'), '--config', str(config), *args], env={**os.environ, 'HOME': str(home)}, text=True, capture_output=True)
    return home, config, data, pub, run

def test_check_no_write_and_idempotent_preservation(kit):
    home, _, _, _, run = kit
    assert run('--check').returncode == 0
    assert not (home/'.ssh').exists()
    ssh = home/'.ssh'
    ssh.mkdir(mode=0o700)
    target = ssh/'authorized_keys'
    target.write_text('# unmanaged\n')
    target.chmod(0o600)
    assert run().returncode == 0
    content = target.read_text()
    assert content.startswith('# unmanaged\n')
    assert run().returncode == 0
    assert target.read_text() == content
    assert target.stat().st_mode & 0o777 == 0o600
    assert run('--undo').returncode == 0
    assert target.read_text() == '# unmanaged\n'
    assert (ssh/'authorized_keys.nocturne-before').read_text() == '# unmanaged\n'

@pytest.mark.parametrize('field,value', [('gateway_host', '-oProxyCommand=bad'), ('worker_id', 'BAD ID'), ('gateway_ssh_user', 'user@host'), ('operator_public_key_files', []), ('unexpected', 'secret-marker-never-log')])
def test_invalid_config_redacted(kit, field, value):
    home, config, data, _, run = kit
    data[field] = value
    config.write_text(json.dumps(data))
    result = run()
    assert result.returncode != 0
    assert 'secret-marker-never-log' not in result.stdout + result.stderr
    assert not (home/'.ssh').exists()

@pytest.mark.parametrize('text', ['ssh-ed25519 AAAA_REPLACE', '-----BEGIN OPENSSH PRIVATE KEY-----\nsecret-marker-never-log', 'command="bad" ssh-ed25519 AAAA', 'ssh-rsa AAAAC3NzaC1lZDI1NTE5AAAAIbad'])
def test_invalid_public_key(kit, text):
    home, _, _, pub, run = kit
    Path(str(pub)+'.pub').write_text(text)
    result = run()
    assert result.returncode != 0
    assert 'secret-marker-never-log' not in result.stdout + result.stderr
    assert not (home/'.ssh').exists()

@pytest.mark.parametrize('unsafe', ['symlink', 'shared', 'malformed', 'hardlink'])
def test_unsafe_destination(kit, unsafe):
    home, _, _, _, run = kit
    ssh = home/'.ssh'
    ssh.mkdir(mode=0o700)
    target = ssh/'authorized_keys'
    if unsafe == 'symlink':
        target.symlink_to(home/'elsewhere')
    else:
        target.write_text('# BEGIN NOCTURNE OWNER ACCESS\n' if unsafe == 'malformed' else '# unmanaged\n')
        target.chmod(0o666 if unsafe == 'shared' else 0o600)
        if unsafe == 'hardlink':
            os.link(target, home/'hardlink')
    assert run().returncode != 0

def test_service_ssh_identity_is_explicit():
    script = (ROOT/'worker/install-services.sh').read_text()
    assert 'NOCTURNE_COORDINATOR_USER:?' in script
    assert "U+'@'+F" in script
    assert 'StrictHostKeyChecking=yes' in script
    assert 'GlobalKnownHostsFile=/dev/null' in script
    assert "'-F','/dev/null'" in script
    assert "'lucas@'" not in script


def test_no_python_owner_runtime():
    script = (ROOT/'worker/nocturne-mac-setup.sh').read_text()
    assert 'python3' not in script
    assert '/usr/bin/plutil' in script


def test_crlf_managed_block_removed(kit):
    home, _, _, _, run = kit
    ssh = home/'.ssh'
    ssh.mkdir(mode=0o700)
    target = ssh/'authorized_keys'
    target.write_bytes(b'# unmanaged\r\n# BEGIN NOCTURNE OWNER ACCESS\r\nold-key\r\n# END NOCTURNE OWNER ACCESS\r\n')
    target.chmod(0o600)
    assert run('--undo').returncode == 0
    assert target.read_bytes() == b'# unmanaged\r\n'
