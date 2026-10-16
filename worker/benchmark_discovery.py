"""Offline GGUF v3 inspection of explicitly supplied local files (no traversal).

This verifies storage layout, not model provenance, inference correctness, or a
quantizer's `_M` recipe. Context is native declared capacity, never extrapolated
from RoPE metadata. Parameters count stored tensor elements, not active MoE
parameters. Results are snapshots: callers must revalidate before use. The legacy
benchmark reopens paths and cannot guarantee immutability after conversion.
"""
from __future__ import annotations

from contextlib import contextmanager
from dataclasses import dataclass
import hashlib
import math
import os
from pathlib import Path
import re
import stat
import struct


class Refused(ValueError):
    """Static, non-secret rejection code."""


# GGML type -> (name, elements per block, bytes per block).
TYPES = {0: ('F32', 1, 4), 1: ('F16', 1, 2), 2: ('Q4_0', 32, 18),
         3: ('Q4_1', 32, 20), 8: ('Q8_0', 32, 34),
         12: ('Q4_K', 256, 144), 13: ('Q5_K', 256, 176),
         14: ('Q6_K', 256, 210), 30: ('BF16', 1, 2)}
DEFAULT_QUALITY = frozenset({'F32', 'F16', 'BF16', 'Q5_K', 'Q6_K', 'Q8_0'})
CATALOG_QUANTIZATIONS = frozenset({'F16', 'BF16', 'Q6_K', 'Q8_0'})
MIN_CONTEXT = 32768
MAX_FILE_BYTES = 256 * 2**30
MAX_HEADER_BYTES = 64 * 2**20
MAX_ITEMS = 1_000_000
MAX_TENSORS = 100_000
MAX_STRING_BYTES = 2**20
MAX_PARAMETERS = 10**13
SCALARS = {0: 'B', 1: 'b', 2: 'H', 3: 'h', 4: 'I', 5: 'i',
           6: 'f', 7: 'B', 10: 'Q', 11: 'q', 12: 'd'}


def _identity(s):
    return (s.st_dev, s.st_ino, s.st_size, s.st_mtime_ns, s.st_ctime_ns)


@contextmanager
def _local_file(path):
    """Open every component relative to pinned directories, without symlinks."""
    if '..' in Path(path).parts:
        raise Refused('parent_path_unsupported')
    path = Path(os.path.abspath(path))
    fd = os.open(path.anchor, os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in path.parts[1:-1]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                            dir_fd=fd)
            os.close(fd)
            fd = child
        leaf = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                       dir_fd=fd)
        with os.fdopen(leaf, 'rb') as stream:
            before = os.fstat(stream.fileno())
            if not stat.S_ISREG(before.st_mode):
                raise Refused('not_regular')
            yield path, stream, before
            if (_identity(os.fstat(stream.fileno())) != _identity(before)
                    or _identity(os.stat(path.name, dir_fd=fd, follow_symlinks=False))
                    != _identity(before)):
                raise Refused('file_changed')
            # Detect renamed/replaced parent directories as well.
            with _reopen_identity(path) as current:
                if current != _identity(before):
                    raise Refused('file_changed')
    except OSError:
        raise Refused('local_file_unavailable') from None
    finally:
        os.close(fd)


@contextmanager
def _reopen_identity(path):
    fd = os.open(path.anchor, os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in path.parts[1:-1]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                            dir_fd=fd)
            os.close(fd)
            fd = child
        yield _identity(os.stat(path.name, dir_fd=fd, follow_symlinks=False))
    finally:
        os.close(fd)


class _Reader:
    def __init__(self, stream, size):
        self.stream = stream
        self.limit = min(size, MAX_HEADER_BYTES)
        self.items = 0

    def read(self, n):
        if n < 0 or self.stream.tell() + n > self.limit:
            raise Refused('header_limit')
        data = self.stream.read(n)
        if len(data) != n:
            raise Refused('truncated')
        return data

    def unpack(self, fmt):
        return struct.unpack('<' + fmt, self.read(struct.calcsize('<' + fmt)))[0]

    def string(self):
        n = self.unpack('Q')
        if n > MAX_STRING_BYTES:
            raise Refused('string_limit')
        try:
            result = self.read(n).decode('utf-8')
        except UnicodeError:
            raise Refused('invalid_utf8') from None
        if '\x00' in result:
            raise Refused('invalid_string')
        return result

    def value(self, kind, array=False):
        self.items += 1
        if self.items > MAX_ITEMS:
            raise Refused('metadata_limit')
        if kind in SCALARS:
            value = self.unpack(SCALARS[kind])
            if kind == 7 and value not in (0, 1):
                raise Refused('invalid_bool')
            if isinstance(value, float) and not math.isfinite(value):
                raise Refused('nonfinite')
            return value
        if kind == 8:
            return self.string()
        if kind == 9 and not array:
            subtype, count = self.unpack('I'), self.unpack('Q')
            if subtype not in {*SCALARS, 8} or count > MAX_ITEMS - self.items:
                raise Refused('unsupported_array')
            for _ in range(count):
                self.value(subtype, array=True)
            return None  # Validate tokenizer arrays without retaining them.
        raise Refused('unsupported_metadata_type')


@dataclass(frozen=True)
class Model:
    path: Path
    sha256: str
    size_bytes: int
    parameters: int
    context_length: int
    output_reserve: int
    quantization: str
    tensor_types: tuple[str, ...]
    identity: tuple[int, ...]

    @property
    def prompt_budget(self):
        return MIN_CONTEXT - self.output_reserve


def inspect_gguf(path, *, output_reserve, allowed_quantizations=DEFAULT_QUALITY):
    """Inspect one file. Reserve is mandatory and inside the 32768 total context.

    Any disallowed tensor type rejects the model, including minority Q4 tensors.
    F32 vectors may accompany a single matrix storage type. Mixed quantizations
    are reported honestly and cannot be converted to the legacy catalog.
    """
    if type(output_reserve) is not int or not 1 <= output_reserve < MIN_CONTEXT:
        raise Refused('output_reserve')
    allowed = frozenset(allowed_quantizations)
    if not allowed or not allowed <= {v[0] for v in TYPES.values()}:
        raise Refused('quantization_policy')
    with _local_file(path) as (path, stream, before):
        if not 24 <= before.st_size <= MAX_FILE_BYTES:
            raise Refused('file_size')
        r = _Reader(stream, before.st_size)
        if r.read(4) != b'GGUF' or r.unpack('I') != 3:
            raise Refused('unsupported_gguf')
        count, metadata_count = r.unpack('Q'), r.unpack('Q')
        if not 1 <= count <= MAX_TENSORS or metadata_count > MAX_ITEMS:
            raise Refused('count_limit')
        metadata = {}
        for _ in range(metadata_count):
            key = r.string()
            if not key or key in metadata:
                raise Refused('metadata_key')
            kind = r.unpack('I')
            metadata[key] = (kind, r.value(kind))
        architecture = metadata.get('general.architecture', (None, None))
        if architecture[0] != 8 or not re.fullmatch(r'[a-z0-9_]{1,64}', architecture[1]):
            raise Refused('architecture')
        context_kind, context = metadata.get(architecture[1] + '.context_length', (None, None))
        if context_kind not in (4, 10) or not MIN_CONTEXT <= context <= 2**31:
            raise Refused('context_length')
        alignment_kind, alignment = metadata.get('general.alignment', (4, 32))
        if (alignment_kind != 4 or not 1 <= alignment <= 4096
                or alignment & (alignment - 1)):
            raise Refused('alignment')
        if any(key.startswith('split.') for key in metadata):
            raise Refused('split_unsupported')
        names, spans, types, matrix_types = set(), [], set(), set()
        parameters = 0
        for _ in range(count):
            name, ndim = r.string(), r.unpack('I')
            if not name or name in names or not 1 <= ndim <= 4:
                raise Refused('tensor_descriptor')
            names.add(name)
            dims = [r.unpack('Q') for _ in range(ndim)]
            elements = math.prod(dims)
            if not all(dims) or not 1 <= elements <= MAX_PARAMETERS:
                raise Refused('tensor_dimensions')
            kind, offset = r.unpack('I'), r.unpack('Q')
            if kind not in TYPES:
                raise Refused('unsupported_tensor_type')
            label, block, size = TYPES[kind]
            if label not in allowed:
                raise Refused('quantization_policy')
            if dims[0] % block or offset % alignment:
                raise Refused('tensor_alignment')
            types.add(label)
            if ndim > 1:
                matrix_types.add(label)
            spans.append((offset, offset + elements // block * size))
            parameters += elements
            if parameters > MAX_PARAMETERS:
                raise Refused('parameter_limit')
        data_start = (stream.tell() + alignment - 1) // alignment * alignment
        end = 0
        for start, stop in sorted(spans):
            if start < end or data_start + stop > before.st_size:
                raise Refused('tensor_bounds')
            end = stop
        # Only alignment padding may follow the last tensor.
        if not 0 <= before.st_size - (data_start + end) < alignment:
            raise Refused('trailing_data')
        if not matrix_types:
            raise Refused('no_matrix_tensors')
        primary = types - {'F32'} if 'F32' not in matrix_types and types != {'F32'} else types
        quantization = next(iter(primary)) if len(primary) == 1 else 'MIXED'
        stream.seek(0)
        digest = hashlib.sha256()
        remaining = before.st_size
        while remaining:
            chunk = stream.read(min(2**20, remaining))
            if not chunk:
                raise Refused('file_changed')
            digest.update(chunk)
            remaining -= len(chunk)
        result = Model(path, digest.hexdigest(), before.st_size, parameters,
                       context, output_reserve, quantization, tuple(sorted(types)),
                       _identity(before))
    return result


def discover(paths, *, output_reserve, allowed_quantizations=DEFAULT_QUALITY):
    """All-or-nothing discovery of at most 64 explicit paths; never downloads."""
    models, seen = [], set()
    for path in paths:
        if len(models) >= 64:
            raise Refused('discovery_limit')
        model = inspect_gguf(path, output_reserve=output_reserve,
                             allowed_quantizations=allowed_quantizations)
        if model.identity[:2] in seen:
            raise Refused('duplicate_file')
        seen.add(model.identity[:2])
        models.append(model)
    return models


def catalog_candidates(models):
    """Return legacy candidate dictionaries after full reinspection.

    No `_M` label is synthesized. SHA/context evidence remains in Model, since
    model_benchmark.catalog rejects additional candidate fields. This does not
    fix the legacy runner's later path-reopen race or declaration-only reports.
    """
    models = list(models)
    if not 1 <= len(models) <= 3:
        raise Refused('catalog_candidate_count')
    rows, seen = [], set()
    for model in models:
        current = inspect_gguf(model.path, output_reserve=model.output_reserve)
        if current != model:
            raise Refused('file_changed')
        if model.quantization not in CATALOG_QUANTIZATIONS:
            raise Refused('catalog_quantization_unrepresentable')
        if model.identity[:2] in seen:
            raise Refused('duplicate_file')
        seen.add(model.identity[:2])
        rows.append(dict(path=str(model.path), parameters=model.parameters,
                         quantization=model.quantization))
    return rows
