# Battery policy: serve on AC or on battery; on battery stop at <=20% and only (re)start above 25% (hysteresis).
STOP_PCT = 20
START_PCT = 25

def battery_ok(w, starting):
    if w.get('on_ac'): return w.get('battery_pct', 0) >= 5
    return w.get('battery_pct', 0) > (START_PCT if starting else STOP_PCT)

def eligible(w, now):
    return bool(battery_ok(w, starting=not w.get('jobs')) and w.get('thermal_state') in {'nominal', 'fair'} and not w.get('user_active', True) and now-w.get('seen', 0)<45 and w.get('ready'))

def unsafe(w):
    return not battery_ok(w, starting=False) or w.get('user_active', True) or w.get('thermal_state') in {'serious','critical'}
