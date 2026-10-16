"""Fail-closed new-machine gate; never applies a recommendation or drains services."""
import argparse
import asyncio
import os
from pathlib import Path

try:
    from worker import model_benchmark as benchmark
except ModuleNotFoundError:  # Installer stages flat modules in /tmp/nocturne-pkg.
    import model_benchmark as benchmark


INSTALLER_MODEL = Path('/opt/nocturne/models/Ornith-1.5-9B-Q4_K_M.gguf')
INSTALLER_SERVER = Path('/opt/nocturne/bin/llama-server')
INSTALLER_PROFILE = {'context': 32768, 'parallel': 2, 'spec_n': 2}


def supported(c):
    return (c['server'] == INSTALLER_SERVER.resolve()
            and c['profiles'] == [INSTALLER_PROFILE]
            and len(c['candidates']) == 1
            and c['candidates'][0]['path'] == INSTALLER_MODEL.resolve()
            and c['candidates'][0]['quantization'] == 'Q4_K_M'
            and 'Q4_K_M' in c['allowed_quantizations'])


def compatible(c, report):
    selected = next((m for m in c['candidates'] if m['id'] == report.get('selected')), None)
    return (supported(c) and report['status'] == 'completed' and selected is not None
            and report.get('profiles') == [INSTALLER_PROFILE]
            and report.get('runtime_flags') == benchmark.RUNTIME_FLAGS
            and all(report.get('cleanup', {}).get(k) is True for k in
                    ('owned_processes_stopped', 'owned_temp_removed', 'final_safety_verified')))


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True)
    args = parser.parse_args(argv)
    report = {'schema': 1, 'status': 'refused', 'selected': None}
    approved = False
    try:
        if os.path.lexists(args.output):
            return 1
        if os.environ.get('NOCTURNE_SKIP_BENCHMARK') == '1':
            report['status'] = 'unbenchmarked'
            report['reason'] = 'operator_skip'
            report['setup_gate'] = 'operator_skip_not_evidence'
            report['policy'] = 'not_evaluated'
            report['installed_profile'] = INSTALLER_PROFILE
            report['runtime_flags'] = benchmark.RUNTIME_FLAGS
            report['quantization'] = 'Q4_K_M'
            report['quantization_verification'] = 'declared_not_verified'
            approved = True
        else:
            path = os.environ.get('NOCTURNE_BENCHMARK_CATALOG')
            if not path:
                raise benchmark.Refused('catalog_required')
            c = benchmark.catalog(path)
            report = benchmark.plan(c)
            if not supported(c):
                report['status'] = 'unsupported_configuration'
                raise benchmark.Refused('installer_settings_mismatch')
            with benchmark.Lock():
                asyncio.run(benchmark.session(c, report))
            if report['status'] != 'completed' or not report.get('selected'):
                report['setup_gate'] = 'recommendation_failed'
            elif not compatible(c, report):
                report['status'] = 'unsupported_recommendation'
                report['reason'] = 'manual_apply_required'
            else:
                approved = True
        benchmark.atomic_report(args.output, report)
    except Exception as exc:
        report['reason'] = benchmark.reason(exc)
        try:
            benchmark.atomic_report(args.output, report)
        except Exception:
            pass
        return 1
    return 0 if approved else 1


if __name__ == '__main__':
    raise SystemExit(main())
