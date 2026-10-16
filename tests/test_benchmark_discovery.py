"""Synthetic GGUF only: no model downloads, services, or production data."""
import hashlib
import struct

import pytest

from worker import benchmark_discovery as d


def pack(fmt, *values):
    return struct.pack('<' + fmt, *values)


def string(value):
    value = value.encode()
    return pack('Q', len(value)) + value


def gguf(*, tensors=((14, (256, 2)),), context=32768, extra=(), version=3):
    metadata = [('general.architecture', 8, string('llama')),
                ('llama.context_length', 4, pack('I', context)), *extra]
    header = b'GGUF' + pack('IQQ', version, len(tensors), len(metadata))
    for key, kind, value in metadata:
        header += string(key) + pack('I', kind) + value
    data = b''
    for i, (kind, dims) in enumerate(tensors):
        data += b'\0' * (-len(data) % 32)
        header += (string(f'tensor.{i}') + pack('I', len(dims))
                   + b''.join(pack('Q', n) for n in dims)
                   + pack('IQ', kind, len(data)))
        _, block, size = d.TYPES.get(kind, ('unknown', 1, 1))
        import math
        data += b'\0' * (math.prod(dims) // block * size)
    return header + b'\0' * (-len(header) % 32) + data


def put(tmp_path, data=None):
    # resolve only the temporary fixture root (macOS /var may be a symlink).
    path = tmp_path.resolve() / 'claimed-Q8_0.gguf'
    path.write_bytes(gguf() if data is None else data)
    return path


def inspect(path, **kwargs):
    return d.inspect_gguf(path, output_reserve=256, **kwargs)


def test_evidence_and_conversion(tmp_path):
    path = put(tmp_path)
    model = inspect(path)
    assert model.parameters == 512
    assert model.quantization == 'Q6_K'
    assert model.context_length == 32768
    assert model.prompt_budget == 32512
    assert model.sha256 == hashlib.sha256(path.read_bytes()).hexdigest()
    assert d.catalog_candidates([model]) == [dict(path=str(path), parameters=512,
                                                 quantization='Q6_K')]
    assert d.discover([path], output_reserve=256) == [model]


@pytest.mark.parametrize('kind', [2, 3, 12])
def test_q4_never_hidden_by_name_or_declaration(tmp_path, kind):
    path = put(tmp_path, gguf(tensors=((14, (256, 2)), (kind, (256, 2))),
                             extra=[('general.file_type', 4, pack('I', 7))]))
    with pytest.raises(d.Refused, match='quantization_policy'):
        inspect(path)
    model = inspect(path, allowed_quantizations=set(d.DEFAULT_QUALITY) | {d.TYPES[kind][0]})
    assert model.quantization == 'MIXED'


@pytest.mark.parametrize('kind,label', [(0, 'F32'), (1, 'F16'), (8, 'Q8_0'),
                                       (13, 'Q5_K'), (14, 'Q6_K'), (30, 'BF16')])
def test_storage_types(tmp_path, kind, label):
    model = inspect(put(tmp_path, gguf(tensors=((kind, (256, 2)), (0, (256,))))))
    assert model.quantization == label
    assert model.parameters == 768
    if label in ('F32', 'Q5_K'):
        with pytest.raises(d.Refused, match='unrepresentable'):
            d.catalog_candidates([model])


@pytest.mark.parametrize('context', [0, 8192, 32767])
def test_no_context_extension(tmp_path, context):
    path = put(tmp_path, gguf(context=context, extra=[
        ('llama.rope.scaling.factor', 6, pack('f', 8.0))]))
    with pytest.raises(d.Refused, match='context_length'):
        inspect(path)


@pytest.mark.parametrize('reserve', [None, True, 0, -1, 32768, 1.5])
def test_explicit_reserve(tmp_path, reserve):
    with pytest.raises(d.Refused, match='output_reserve'):
        d.inspect_gguf(put(tmp_path), output_reserve=reserve)


def test_reserve_required(tmp_path):
    with pytest.raises(TypeError):
        d.inspect_gguf(put(tmp_path))


@pytest.mark.parametrize('data', [gguf(version=2), gguf(version=4), b'NOPE' * 8,
    gguf()[:-1], gguf() + b'x' * 32,
    gguf(tensors=((99, (256, 2)),)), gguf(tensors=((14, (255, 2)),)),
    gguf(tensors=((14, (0, 2)),)),
    gguf(extra=[('general.architecture', 8, string('llama'))]),
    gguf(extra=[('split.count', 2, pack('H', 1))]),
    gguf(extra=[('bad', 7, b'\x02')]),
    gguf(extra=[('bad', 9, pack('IQ', 9, 1))]),
    gguf(extra=[('bad', 9, pack('IQ', 0, d.MAX_ITEMS + 1))]),
    gguf(extra=[('bad', 6, pack('f', float('nan')))]),
    gguf(extra=[('general.alignment', 4, pack('I', 3))]),
    b'GGUF' + pack('IQQ', 3, d.MAX_TENSORS + 1, 0),
])
def test_malformed_unsupported(tmp_path, data):
    with pytest.raises(d.Refused):
        inspect(put(tmp_path, data))


def test_arrays_validated_without_retention(tmp_path):
    path = put(tmp_path, gguf(extra=[('tokenizer.ggml.tokens', 9,
                                     pack('IQ', 8, 2) + string('a') + string('b'))]))
    assert inspect(path).parameters == 512


def test_symlink_leaf_parent_and_dotdot(tmp_path):
    path = put(tmp_path)
    leaf = path.parent / 'link.gguf'
    leaf.symlink_to(path)
    parent = path.parent / 'linkdir'
    parent.symlink_to(path.parent, target_is_directory=True)
    for candidate in (leaf, parent / path.name, parent / '..' / path.parent.name / path.name):
        with pytest.raises(d.Refused):
            inspect(candidate)


def test_changed_after_discovery(tmp_path):
    path = put(tmp_path)
    model = inspect(path)
    data = path.read_bytes()
    path.write_bytes(data[:-1] + b'\x01')
    with pytest.raises(d.Refused, match='file_changed'):
        d.catalog_candidates([model])


def test_changed_during_hash(tmp_path, monkeypatch):
    path = put(tmp_path)
    original = d.hashlib.sha256

    class MutatingHash:
        def __init__(self):
            self.digest = original()

        def update(self, chunk):
            self.digest.update(chunk)
            with path.open('r+b') as f:
                f.seek(-1, 2)
                f.write(b'\x01')

        def hexdigest(self):
            return self.digest.hexdigest()

    monkeypatch.setattr(d.hashlib, 'sha256', MutatingHash)
    with pytest.raises(d.Refused, match='file_changed'):
        inspect(path)


def test_duplicate_and_limits(tmp_path, monkeypatch):
    path = put(tmp_path)
    alias = path.parent / 'hardlink.gguf'
    alias.hardlink_to(path)
    with pytest.raises(d.Refused, match='duplicate_file'):
        d.discover([path, alias], output_reserve=128)
    monkeypatch.setattr(d, 'MAX_FILE_BYTES', 32)
    with pytest.raises(d.Refused, match='file_size'):
        inspect(path)


def test_overlap_and_out_of_bounds(tmp_path):
    data = gguf(tensors=((14, (256, 2)), (14, (256, 2))))
    marker = string('tensor.1') + pack('IQQI', 2, 256, 2, 14)
    position = data.index(marker) + len(marker)
    for offset in (0, 2**40):
        corrupted = data[:position] + pack('Q', offset) + data[position + 8:]
        with pytest.raises(d.Refused, match='tensor_bounds'):
            inspect(put(tmp_path, corrupted))


def test_f32_matrices_are_not_vector_exception(tmp_path):
    model = inspect(put(tmp_path, gguf(tensors=((14, (256, 2)), (0, (256, 2))))))
    assert model.quantization == 'MIXED'
    with pytest.raises(d.Refused, match='unrepresentable'):
        d.catalog_candidates([model])
