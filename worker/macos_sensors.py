import ctypes, re, subprocess, time

_THERMAL = {0: 'nominal', 1: 'fair', 2: 'serious', 3: 'critical'}

def command(*args):
    return subprocess.check_output(args, text=True, timeout=5)

def _thermal_from_processinfo():
    # NSProcessInfo.thermalState via the ObjC runtime; works without Xcode/CLT (swift is a stub on stock macOS).
    objc = ctypes.cdll.LoadLibrary('/usr/lib/libobjc.A.dylib')
    ctypes.cdll.LoadLibrary('/System/Library/Frameworks/Foundation.framework/Foundation')
    objc.objc_getClass.restype = ctypes.c_void_p; objc.objc_getClass.argtypes = [ctypes.c_char_p]
    objc.sel_registerName.restype = ctypes.c_void_p; objc.sel_registerName.argtypes = [ctypes.c_char_p]
    addr = ctypes.cast(objc.objc_msgSend, ctypes.c_void_p).value
    send_obj = ctypes.CFUNCTYPE(ctypes.c_void_p, ctypes.c_void_p, ctypes.c_void_p)(addr)
    send_long = ctypes.CFUNCTYPE(ctypes.c_long, ctypes.c_void_p, ctypes.c_void_p)(addr)
    info = send_obj(objc.objc_getClass(b'NSProcessInfo'), objc.sel_registerName(b'processInfo'))
    if not info: raise RuntimeError('no NSProcessInfo')
    return _THERMAL.get(int(send_long(info, objc.sel_registerName(b'thermalState'))), 'critical')

_METRICS = {'t': 0, 'v': None}

def _sysctl(name):
    return command('/usr/sbin/sysctl', '-n', name).strip()

def read_metrics():
    # Load/pressure numbers for the admin view only; never used for scheduling. No sudo needed. Cached 10 s.
    now = time.monotonic()
    if _METRICS['v'] is not None and now - _METRICS['t'] < 10: return _METRICS['v']
    m = {}
    try:
        free = int(_sysctl('kern.memorystatus_level'))  # % of memory macOS considers available
        m['mem_pressure_pct'] = max(0, min(100, 100 - free))
    except Exception: pass
    try: m['vm_pressure_level'] = int(_sysctl('kern.memorystatus_vm_pressure_level'))  # 1 normal, 2 warn, 4 critical
    except Exception: pass
    try: m['mem_total_gb'] = round(int(_sysctl('hw.memsize')) / 2**30, 1)
    except Exception: pass
    try:
        sw = re.search(r'used = ([\d.]+)M', _sysctl('vm.swapusage'))
        if sw: m['swap_used_mb'] = round(float(sw.group(1)))
    except Exception: pass
    try:
        load = float(_sysctl('vm.loadavg').strip('{} ').split()[0]); n = int(_sysctl('hw.ncpu'))
        m['load1'] = round(load, 2); m['cpu_count'] = n; m['cpu_pct'] = round(min(100.0, 100.0 * load / n), 1)
    except Exception: pass
    try:
        g = re.search(r'"Device Utilization %"=(\d+)', command('/usr/sbin/ioreg', '-r', '-d', '1', '-c', 'IOAccelerator'))
        if g: m['gpu_pct'] = int(g.group(1))
    except Exception: pass
    _METRICS.update(t=now, v=m)
    return m

def read_sensors():
    try:
        batt = command('/usr/bin/pmset', '-g', 'batt'); match = re.search(r'(\d+)%', batt)
        on_ac = "'AC Power'" in batt; pct = int(match.group(1)) if match else 0
        idle_raw = command('/usr/sbin/ioreg', '-c', 'IOHIDSystem'); idle_match = re.search(r'"HIDIdleTime"\s*=\s*(\d+)', idle_raw)
        idle = int(idle_match.group(1)) / 1e9 if idle_match else 0
        therm = command('/usr/bin/pmset', '-g', 'therm')
        m = re.search(r'(?:Thermal_Level|ThermalLevel)\s*=\s*(\d+)', therm)
        if m: thermal = _THERMAL.get(int(m.group(1)), 'critical')
        else:
            try: thermal = _thermal_from_processinfo()
            except Exception: thermal = 'critical'
        out = dict(on_ac=on_ac, battery_pct=pct, thermal_state=thermal, user_active=idle < 120, idle_seconds=idle)
        try: out['metrics'] = read_metrics()
        except Exception: pass
        return out
    except Exception:
        return dict(on_ac=False, battery_pct=0, thermal_state='critical', user_active=True)

if __name__ == '__main__':
    print(read_sensors())
