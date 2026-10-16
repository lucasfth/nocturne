"""Small non-secret admin configuration capability; not a credential editor."""
import json
import os
from pathlib import Path
import tempfile
import stat
import threading
import uuid
from dataclasses import dataclass
from urllib.parse import urlsplit

from fastapi import HTTPException, Request


@dataclass(frozen=True)
class Setting:
    type: str
    default: object
    apply: str
    minimum: int | None = None
    maximum: int | None = None


# Only these existing runtime settings may be viewed or changed. Everything else
# (including future secret-bearing extensions) is deliberately absent.
FIELDS = {
    'service_name': Setting('string', 'inference', 'live'),
    'public_limit_concurrency': Setting('integer', 64, 'restart', 1, 4096),
}


# Shared with Go: a flat, strictly allowlisted overlay, never a copy of config.json.
# Single coordinator only; external edits require restart. Revisions are per-process.
OVERLAY_LIMIT = 4096


def overlay_path(base):
    return Path(base).parent / 'data' / 'settings' / 'config.json'


def validate_changes(changes):
    for key, value in changes.items():
        field = FIELDS.get(key)
        if field is None:
            raise ValueError('unsupported setting')
        if field.type == 'integer':
            if type(value) is not int or not field.minimum <= value <= field.maximum:
                raise ValueError('invalid setting value')
        elif type(value) is not str or not 1 <= len(value) <= 80 or any(not c.isprintable() or c in '<>&\"\'' for c in value):
            raise ValueError('invalid setting value')


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('duplicate key')
        result[key] = value
    return result


def check_directory(path, private=False):
    st = path.lstat()
    if not stat.S_ISDIR(st.st_mode) or (private and stat.S_IMODE(st.st_mode) != 0o700):
        raise ValueError('invalid settings directory')


def read_overlay(path):
    # Missing state is normal on first boot. Reject symlinks and permissive state.
    for directory in (path.parent.parent, path.parent):
        try:
            check_directory(directory, private=directory == path.parent)
        except FileNotFoundError:
            return None, {}
    try:
        st = path.lstat()
    except FileNotFoundError:
        return None, {}
    if not stat.S_ISREG(st.st_mode) or stat.S_IMODE(st.st_mode) != 0o600:
        raise ValueError('invalid settings file')
    with path.open('rb') as f:
        raw = f.read(OVERLAY_LIMIT + 1)
    if len(raw) > OVERLAY_LIMIT:
        raise ValueError('invalid settings file')
    try:
        values = json.loads(raw.decode('utf-8'), object_pairs_hook=unique_object)
    except RecursionError:
        raise ValueError('invalid settings file') from None
    if not isinstance(values, dict):
        raise ValueError('invalid settings file')
    validate_changes(values)
    return raw, values


def prepare_directory(path):
    for directory in (path.parent.parent, path.parent):
        try:
            directory.mkdir(mode=0o700)
        except FileExistsError:
            pass
        else:
            fd = os.open(directory.parent, os.O_RDONLY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
        check_directory(directory, private=directory == path.parent)


def load_config(path):
    """Load immutable operator config plus typed nonsecret settings before wiring."""
    try:
        config = json.loads(Path(path).read_bytes())
        _, overlay = read_overlay(overlay_path(path))
        return {**config, **overlay}
    except (OSError, ValueError, AttributeError):
        raise ValueError('configuration settings startup failed') from None


class Settings:
    def __init__(self, path, runtime):
        self.base_path = Path(path)
        self.path = overlay_path(path)
        self.runtime = runtime
        self.lock = threading.RLock()
        try:
            self.base_raw = self.base_path.read_bytes()
            base = json.loads(self.base_raw)
            self.saved = {k: v for k, v in base.items() if k in FIELDS}
            prepare_directory(self.path)
            self.raw, self.overlay = read_overlay(self.path)
            self.saved.update(self.overlay)
        except (OSError, ValueError, AttributeError):
            raise ValueError('configuration settings startup failed') from None
        self.revision = uuid.uuid4().hex
        self.uncertain = False

    def view(self):
        with self.lock:
            if self.uncertain:
                raise HTTPException(503, 'configuration durability uncertain; restart required')
            fields = {k: dict(type=v.type, apply=v.apply, minimum=v.minimum,
                              maximum=v.maximum, default=v.default) for k, v in FIELDS.items()}
            values = {k: self.saved.get(k, v.default) for k, v in FIELDS.items()}
            effective = {k: self.runtime.get(k, v.default) for k, v in FIELDS.items()}
            return dict(schema_version=1, runtime='python', revision=self.revision,
                        fields=fields, values=values, effective=effective,
                        restart_required=[k for k in FIELDS if values[k] != effective[k]])

    def patch(self, body):
        with self.lock:
            self.view()
            if set(body) != {'revision', 'changes'} or not isinstance(body['changes'], dict) or not body['changes']:
                raise HTTPException(400, 'expected revision and nonempty changes object')
            if body['revision'] != self.revision:
                raise HTTPException(409, 'configuration changed; reload or restart before editing')
            try:
                current, _ = read_overlay(self.path)
                base = self.base_path.read_bytes()
            except (OSError, ValueError):
                raise HTTPException(503, 'configuration persistence failed; reload or restart required') from None
            if current != self.raw or base != self.base_raw:
                raise HTTPException(409, 'configuration changed; reload or restart before editing')
            changes = body['changes']
            try:
                validate_changes(changes)
            except ValueError as e:
                raise HTTPException(400, str(e)) from None
            saved = {**self.saved, **changes}
            overlay = {**self.overlay, **changes}
            raw = (json.dumps(overlay, indent=2, ensure_ascii=True) + '\n').encode()
            name = None
            renamed = False
            try:
                fd, name = tempfile.mkstemp(prefix='.nocturne-settings-', dir=self.path.parent)
                with os.fdopen(fd, 'wb') as f:
                    os.fchmod(f.fileno(), 0o600)
                    f.write(raw)
                    f.flush()
                    os.fsync(f.fileno())
                os.replace(name, self.path)
                renamed = True
                directory = os.open(self.path.parent, os.O_RDONLY)
                try:
                    os.fsync(directory)
                finally:
                    os.close(directory)
            except OSError:
                self.uncertain = renamed
                raise HTTPException(503, 'configuration persistence failed; reload or restart required') from None
            finally:
                if name:
                    try:
                        os.unlink(name)
                    except OSError:
                        # Best-effort private temp cleanup must not expose filesystem errors.
                        pass
            self.saved, self.overlay, self.raw, self.revision = saved, overlay, raw, uuid.uuid4().hex
            for key, value in changes.items():
                if FIELDS[key].apply == 'live':
                    self.runtime[key] = value
            return self.view()


def browser_guard(request):
    """Bearer auth remains mandatory; reject browser cross-origin mutations.

    No forwarded host trust: configure the proxy to preserve Host, or use the
    same-origin browser fetch metadata. CLI clients may omit Origin/metadata.
    """
    site = request.headers.get('sec-fetch-site')
    origin = request.headers.get('origin')
    if site and site not in {'same-origin', 'none'}:
        raise HTTPException(403, 'cross-origin configuration access denied')
    if origin:
        parsed = urlsplit(origin)
        if parsed.scheme not in {'http', 'https'} or parsed.netloc != request.headers.get('host') or parsed.path or parsed.query or parsed.fragment:
            raise HTTPException(403, 'cross-origin configuration access denied')


def install_settings(coordinator):
    store = Settings(coordinator.root / 'config.json', coordinator.config)
    coordinator.settings = store

    @coordinator.admin.get('/admin/config')
    async def get_config(request: Request):
        coordinator.admin_auth(request)
        browser_guard(request)
        from fastapi.responses import JSONResponse
        return JSONResponse(store.view(), headers={'Cache-Control': 'no-store'})

    @coordinator.admin.patch('/admin/config')
    async def patch_config(request: Request):
        coordinator.admin_auth(request)
        browser_guard(request)
        if request.headers.get('content-type', '').split(';')[0].strip().lower() != 'application/json':
            raise HTTPException(415, 'application/json required')
        raw = bytearray()
        async for chunk in request.stream():
            raw.extend(chunk)
            if len(raw) > 4096:
                raise HTTPException(413, 'configuration patch too large')
        def unique(pairs):
            result = {}
            for key, value in pairs:
                if key in result:
                    raise ValueError('duplicate key')
                result[key] = value
            return result
        try:
            body = json.loads(raw, object_pairs_hook=unique)
        except (ValueError, UnicodeError):
            raise HTTPException(400, 'invalid JSON') from None
        if not isinstance(body, dict):
            raise HTTPException(400, 'expected object')
        from fastapi.responses import JSONResponse
        return JSONResponse(store.patch(body), headers={'Cache-Control': 'no-store'})
