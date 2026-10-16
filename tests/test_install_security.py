"""No sudo, downloads, installs or service activation; filesystem policy fixtures only."""
import os
from pathlib import Path
import stat
import subprocess

import pytest

ROOT = Path(__file__).resolve().parents[1]
HELPER = (ROOT / 'worker/install-security.sh').read_text()


@pytest.fixture
def policy(tmp_path, monkeypatch):
    # Exercise the actual embedded validator with real temporary files. Model
    # root ownership/safe ancestors without requiring root or chmod of hosts.
    source = HELPER.split("<<'PY'\n", 1)[1].split('\nPY\n', 1)[0]
    ns = {}
    exec(source.split('\ntry:\n', 1)[0], ns)
    ns['INSTALL_ROOT'] = str(tmp_path)
    real_stat = os.lstat
    owners = {}

    def metadata(path):
        path = Path(path)
        s = real_stat(path)
        fields = list(s)
        fields[4] = owners.get(path, 0)
        if path != tmp_path and tmp_path not in path.parents:
            fields[0] = stat.S_IFDIR | 0o755
        return os.stat_result(fields)

    monkeypatch.setattr(os, 'lstat', metadata)
    monkeypatch.setattr(subprocess, 'check_output', lambda *a, **kw: 'no ACL\n')
    return ns['check'], owners


def test_root_owned_private_tree(policy, tmp_path):
    check, _ = policy
    p = tmp_path / 'code.py'
    p.write_text('trusted')
    p.chmod(0o644)
    check(str(p))


@pytest.mark.parametrize('mode', [0o666, 0o664, 0o777])
def test_writable_code_rejected(policy, tmp_path, mode):
    check, _ = policy
    p = tmp_path / 'code.py'
    p.write_text('attacker')
    p.chmod(mode)
    with pytest.raises(ValueError):
        check(str(p))


def test_worker_owned_code_is_not_repaired(policy, tmp_path):
    check, owners = policy
    p = tmp_path / 'code.py'
    p.write_text('attacker')
    p.chmod(0o444)
    owners[p] = 550
    with pytest.raises(ValueError):
        check(str(p))
    assert p.read_text() == 'attacker'


def test_writable_parent_rejected(policy, tmp_path):
    check, _ = policy
    parent = tmp_path / 'unsafe'
    parent.mkdir(mode=0o755)
    p = parent / 'code.py'
    p.touch()
    parent.chmod(0o777)
    with pytest.raises(ValueError):
        check(str(p))


def test_symlink_and_dangling_link_rejected(policy, tmp_path):
    check, _ = policy
    target = tmp_path / 'target'
    target.write_text('unchanged')
    link = tmp_path / 'link'
    link.symlink_to(target)
    with pytest.raises(ValueError):
        check(str(link))
    target.unlink()
    with pytest.raises(ValueError):
        check(str(link))


def test_acl_rejected(policy, tmp_path, monkeypatch):
    check, _ = policy
    monkeypatch.setattr(subprocess, 'check_output', lambda *a, **kw: 'mode\n 0: user allow write\n')
    with pytest.raises(ValueError):
        check(str(tmp_path))


@pytest.fixture
def generated_runtime(policy, tmp_path):
    check, owners = policy
    python = tmp_path / 'python/cpython-3.12-macos-aarch64-none/bin'
    venv = tmp_path / '.venv/bin'
    llama = tmp_path / 'bin/llama/llama-b11588'
    for directory in (python, venv, llama):
        directory.mkdir(parents=True, mode=0o755)
    (python / 'python3.12').write_text('interpreter fixture')
    (python / 'python3').symlink_to('python3.12')
    (python / 'python').symlink_to('python3')
    (venv / 'python').symlink_to(python / 'python3.12')
    (venv / 'python3').symlink_to('python')
    (venv / 'python3.12').symlink_to('python')
    (llama / 'libllama.0.0.0.dylib').write_text('library fixture')
    (llama / 'libllama.0.dylib').symlink_to('libllama.0.0.0.dylib')
    (llama / 'libllama.dylib').symlink_to('libllama.0.dylib')
    return check, owners, python, venv, llama


def test_generated_runtime_links(generated_runtime):
    check, _, python, venv, llama = generated_runtime
    for directory in (python, venv, llama):
        for path in directory.iterdir():
            check(str(path), links=True)


@pytest.mark.parametrize('location', ['python', 'llama'])
@pytest.mark.parametrize('unsafe', ['owner', 'mode', 'acl', 'link-owner'])
def test_intermediate_redirect_rejected(generated_runtime, monkeypatch, location, unsafe):
    check, owners, python, _, llama = generated_runtime
    base = python if location == 'python' else llama
    redirect = base / 'redirect'
    redirect.mkdir()
    target = base / 'protected'
    target.write_text('safe final destination')
    hop = redirect / 'hop'
    hop.symlink_to(target)
    entry = base / 'entry'
    entry.symlink_to('redirect/hop')
    assert entry.resolve() == target
    if unsafe == 'owner':
        owners[redirect] = 550
    elif unsafe == 'mode':
        redirect.chmod(0o777)
    elif unsafe == 'link-owner':
        owners[hop] = 550
    else:
        monkeypatch.setattr(subprocess, 'check_output',
                            lambda args, **kw: 'mode\n 0: allow write\n'
                            if Path(args[-1]) == redirect else 'no ACL\n')
    with pytest.raises(ValueError):
        check(str(entry), links=True)


@pytest.mark.parametrize('state', ['home', 'data', 'logs'])
def test_state_hop_rejected_even_with_protected_final_target(generated_runtime, tmp_path, state):
    check, owners, python, venv, _ = generated_runtime
    directory = tmp_path / state
    directory.mkdir()
    owners[directory] = 550
    (directory / 'redirect').symlink_to(python / 'python3.12')
    entry = venv / 'bad'
    entry.symlink_to(directory / 'redirect')
    assert entry.resolve() == python / 'python3.12'
    with pytest.raises(ValueError):
        check(str(entry), links=True)


@pytest.mark.parametrize('kind', ['cycle', 'long', 'dangling', 'cross', 'escape-return'])
def test_invalid_release_links(generated_runtime, kind):
    check, _, python, _, llama = generated_runtime
    entry = llama / 'bad'
    if kind == 'cycle':
        entry.symlink_to('other')
        (llama / 'other').symlink_to('bad')
    elif kind == 'long':
        entry.symlink_to('hop0')
        for i in range(41):
            (llama / f'hop{i}').symlink_to(f'hop{i+1}' if i < 40 else 'libllama.dylib')
    elif kind == 'dangling':
        entry.symlink_to('missing')
    elif kind == 'cross':
        entry.symlink_to(python / 'python3.12')
    else:
        entry.symlink_to('../../../bin/llama/llama-b11588/libllama.dylib')
    with pytest.raises((ValueError, FileNotFoundError)):
        check(str(entry), links=True)


def test_protected_directory_link(generated_runtime):
    check, _, python, venv, _ = generated_runtime
    alias = python.parent / 'current-bin'
    alias.symlink_to('bin', target_is_directory=True)
    entry = venv / 'directory-linked-python'
    entry.symlink_to(alias / 'python3')
    check(str(entry), links=True)


def test_relative_parent_link_within_release(generated_runtime):
    check, _, _, _, llama = generated_runtime
    child = llama / 'lib'
    child.mkdir()
    entry = child / 'libllama.dylib'
    entry.symlink_to('../libllama.0.dylib')
    check(str(entry), links=True)


def test_installer_privilege_boundaries():
    bootstrap = (ROOT / 'worker/bootstrap-remote.sh').read_text()
    services = (ROOT / 'worker/install-services.sh').read_text()
    for text in (bootstrap, services):
        assert '/tmp/' not in text
        assert 'chown -R nocturne-worker:staff $R\n' not in text
        assert 'security_init' in text
    assert 'fetch-model.sh' not in bootstrap
    assert 'private_stage' in bootstrap
    assert 'UV_CACHE_DIR=$R/.install-cache' not in bootstrap
    assert '$R/.install-cache' not in bootstrap
    assert bootstrap.count('UV_CACHE_DIR="$STAGE/uv-cache"') == 2
    assert bootstrap.count('UV_PYTHON_PREFERENCE=only-managed') == 2
    assert bootstrap.count('UV_PYTHON_INSTALL_DIR=$R/python') == 2
    assert 'UV_LINK_MODE=copy' in bootstrap
    assert 'mktemp -d "$R/.install-XXXXXXXX"' in HELPER
    assert "trap 'rm -rf -- \"$STAGE\"' EXIT" in HELPER
    assert 'security_check tree "$R/.venv"' in bootstrap
    assert 'security_check tree "$R/bin/llama"' in bootstrap
    assert 'worker_run /usr/bin/env PYTHONPATH="$PACKAGE"' in services
    assert '/usr/bin/python3 -I -' in services
    assert '$S $R/.venv/bin/python' not in services
    gate = services.index('-m benchmark_setup')
    for mutation in ('$S mkdir', '$S install', '$S tee', 'launchctl bootout', 'launchctl bootstrap'):
        assert gate < services.index(mutation)
    assert 'NOCTURNE_SKIP_BENCHMARK' in services
    assert 'NOCTURNE_BENCHMARK_CATALOG' in services


@pytest.mark.parametrize('name', ['bootstrap-remote.sh', 'install-services.sh', 'install-security.sh'])
def test_shell_syntax(name):
    subprocess.run(['/bin/bash', '-n', str(ROOT / 'worker' / name)], check=True)
