#!/usr/bin/env python3
"""
RTL-SDR Scanner Server
Streams live audio from an RTL-SDR dongle to web browsers,
with multi-frequency auto-scanning and squelch detection.
"""
import os
import json
import time
import subprocess
import threading
import hmac
import secrets
import base64
import logging
import tempfile
import select
from collections import defaultdict, deque
from functools import wraps

import numpy as np
from flask import Flask, request, jsonify, render_template
from flask_socketio import SocketIO, join_room, leave_room
from werkzeug.security import generate_password_hash, check_password_hash

logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s [%(levelname)s] %(message)s'
)
log = logging.getLogger(__name__)

# Valid rtl_fm demodulation modes passed straight to `rtl_fm -M`.
VALID_MODES = {'fm', 'am', 'usb', 'lsb', 'raw', 'wbfm'}

# App Setup
app = Flask(__name__)
app.config['SECRET_KEY'] = os.environ.get('SECRET_KEY', secrets.token_hex(32))

_origins_env = os.environ.get('ALLOWED_ORIGINS', '').strip()
if _origins_env == '*':
    _cors_origins = '*'
elif _origins_env:
    _cors_origins = [o.strip() for o in _origins_env.split(',') if o.strip()]
else:
    _cors_origins = []
    
socketio = SocketIO(
    app,
    cors_allowed_origins=_cors_origins,
    async_mode='threading',
    max_http_buffer_size=2 * 1024 * 1024,
    logger=False,
    engineio_logger=False
)

# Config
CONFIG_FILE = os.environ.get('CONFIG_FILE', '/data/config.json')

DEFAULTS = {
    'admin_username': 'admin',
    'admin_password_hash': generate_password_hash('changeme'),
    'must_change_password': True,
    'frequencies': [],
    'squelch_mode': 'audio',
    'squelch_db': -35.0,
    'rf_squelch': 0,
    'diff_squelch': 3.0,
    'dwell_time': 2.0,
    'sample_rate': 16000,
    'ppm': 0,
    'gain': 'auto',
}

_cfg_lock = threading.RLock()


def load_config() -> dict:
    if os.path.exists(CONFIG_FILE):
        try:
            with open(CONFIG_FILE) as f:
                data = json.load(f)
            for k, v in DEFAULTS.items():
                data.setdefault(k, v)
            ph = data.get('admin_password_hash', '')
            if (isinstance(ph, str) and len(ph) == 64 and
                    all(c in '0123456789abcdef' for c in ph.lower())):
                data['admin_password_hash'] = generate_password_hash(
                    secrets.token_hex(32))
                data['must_change_password'] = True
            return data
        except Exception as e:
            log.error(f'Failed to load config: {e}')
    return dict(DEFAULTS)


def save_config():
    with _cfg_lock:
        try:
            directory = os.path.dirname(CONFIG_FILE) or '.'
            os.makedirs(directory, exist_ok=True)
            fd, tmp_path = tempfile.mkstemp(
                prefix='.config.', suffix='.tmp', dir=directory)
            try:
                with os.fdopen(fd, 'w') as f:
                    json.dump(cfg, f, indent=2)
                    f.flush()
                    os.fsync(f.fileno())
                os.replace(tmp_path, CONFIG_FILE)
            except Exception:
                try:
                    os.unlink(tmp_path)
                except OSError:
                    pass
                raise
        except Exception as e:
            log.error(f'Failed to save config: {e}')


cfg = load_config()

# Session Auth
_sessions: dict[str, float] = {}
_sessions_lock = threading.Lock()
SESSION_TTL = 86400


def create_session() -> str:
    token = secrets.token_hex(32)
    with _sessions_lock:
        _sessions[token] = time.time() + SESSION_TTL
    return token


def is_valid_token(token: str) -> bool:
    if not token:
        return False
    with _sessions_lock:
        exp = _sessions.get(token)
        if exp and exp > time.time():
            return True
        _sessions.pop(token, None)
    return False


def drop_session(token: str):
    with _sessions_lock:
        _sessions.pop(token, None)


def admin_required(f):
    @wraps(f)
    def wrapper(*args, **kwargs):
        token = request.headers.get('X-Token', '')
        if not is_valid_token(token):
            return jsonify(error='Unauthorized'), 401
        return f(*args, **kwargs)
    return wrapper


# Login Rate Limiting
_login_attempts: dict[str, deque] = defaultdict(deque)
_login_lock = threading.Lock()
LOGIN_WINDOW_SECS = 300
LOGIN_MAX_ATTEMPTS = 8


def login_rate_limit_ok(ip: str) -> bool:
    now = time.time()
    cutoff = now - LOGIN_WINDOW_SECS
    with _login_lock:
        q = _login_attempts[ip]
        while q and q[0] < cutoff:
            q.popleft()
        if len(q) >= LOGIN_MAX_ATTEMPTS:
            return False
        q.append(now)
        if len(_login_attempts) > 1024:
            for k in list(_login_attempts.keys()):
                if not _login_attempts[k]:
                    del _login_attempts[k]
        return True


def client_ip() -> str:
    xff = request.headers.get('X-Forwarded-For', '')
    if xff:
        return xff.split(',')[0].strip()
    return request.remote_addr or '0.0.0.0'


# Connected Client Tracking
_connected = 0
_connected_lock = threading.Lock()


@socketio.on('connect')
def on_connect():
    global _connected
    with _connected_lock:
        _connected += 1
        count = _connected
    socketio.emit('system_stats', {'connected': count})


@socketio.on('disconnect')
def on_disconnect():
    global _connected
    with _connected_lock:
        _connected = max(0, _connected - 1)
        count = _connected
    socketio.emit('system_stats', {'connected': count})


@socketio.on('audio_subscribe')
def on_audio_subscribe():
    join_room('audio')


@socketio.on('audio_unsubscribe')
def on_audio_unsubscribe():
    leave_room('audio')


# Scanner
class Scanner:
    CHUNK_MS = 100

    def __init__(self):
        self.running = False
        self.paused = False
        self.force_skip = False
        self.current_idx = 0
        self.current_freq: dict | None = None
        self.signal_db = -100.0
        self._proc: subprocess.Popen | None = None
        self._proc_lock = threading.Lock()
        self._thread: threading.Thread | None = None
        self._stderr_thread: threading.Thread | None = None

    def _kill_proc(self):
        with self._proc_lock:
            proc = self._proc
            self._proc = None
        if proc and proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=3)
            except subprocess.TimeoutExpired:
                proc.kill()
                try:
                    proc.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    pass

    def _drain_stderr(self, proc: subprocess.Popen):
        try:
            for raw in iter(proc.stderr.readline, b''):
                line = raw.decode('utf-8', 'replace').rstrip()
                if line:
                    log.info(f'rtl_fm: {line}')
        except Exception:
            pass

    def _start_rtl(self, freq_hz: int, mode: str = 'fm') -> subprocess.Popen:
        with _cfg_lock:
            gain = cfg.get('gain', 'auto')
            sr = str(cfg.get('sample_rate', 16000))
            ppm = str(cfg.get('ppm', 0))
            sq_mode = cfg.get('squelch_mode', 'audio')
            
            # Only apply RF squelch limit if the mode is actually set to RF
            rf_sql = str(cfg.get('rf_squelch', 0)) if sq_mode == 'rf' else '0'

        cmd = [
            'rtl_fm',
            '-f', str(freq_hz),
            '-M', mode,
            '-s', '200000',
            '-r', sr,
            '-p', ppm,
            '-l', rf_sql,
            '-'
        ]
        if gain != 'auto':
            cmd += ['-g', str(gain)]

        log.info(f'Starting rtl_fm: {" ".join(cmd)}')
        proc = subprocess.Popen(
            cmd,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            bufsize=0
        )
        with self._proc_lock:
            self._proc = proc
        self._stderr_thread = threading.Thread(
            target=self._drain_stderr, args=(proc,),
            daemon=True, name='rtl_fm-stderr')
        self._stderr_thread.start()
        return proc

    @staticmethod
    def rms_db(raw: bytes) -> float:
        if len(raw) < 2:
            return -100.0
        samples = np.frombuffer(raw, dtype=np.int16).astype(np.float32)
        rms = float(np.sqrt(np.mean(samples ** 2)))
        if rms < 1.0:
            return -100.0
        return float(20.0 * np.log10(rms / 32768.0))

    def _loop(self):
        consecutive_failures = 0
        while self.running:
            with _cfg_lock:
                freqs = list(cfg.get('frequencies', []))
                sr = cfg.get('sample_rate', 16000)

            if not freqs:
                for _ in range(5):
                    if not self.running: break
                    time.sleep(0.1)
                continue

            idx = self.current_idx % len(freqs)
            fi = freqs[idx]
            chunk_bytes = int(sr * self.CHUNK_MS / 1000) * 2

            self.current_freq = fi
            self.current_idx = idx

            socketio.emit('scanner_update', {
                'running': True,
                'paused': self.paused,
                'idx': idx,
                'total': len(freqs),
                'freq': fi,
            })

            mode = fi.get('mode', 'fm')
            if mode not in VALID_MODES:
                self.current_idx = (idx + 1) % len(freqs)
                time.sleep(0.2)
                continue

            log.info(f'Tuning to {fi["freq"]/1e6:.3f} MHz ({fi.get("label","")}) [{mode.upper()}]')

            self._kill_proc()
            try:
                proc = self._start_rtl(fi['freq'], mode)
            except Exception as e:
                log.error(f'Failed to start rtl_fm: {e}')
                consecutive_failures += 1
                if consecutive_failures >= 5:
                    self.running = False
                    break
                time.sleep(1.0)
                continue

            silence_start: float | None = None
            chunks_read = 0
            buf = bytearray()
            db_history = []  # Used for rolling variance calculation in diff mode

            while self.running:
                if self.force_skip:
                    self.force_skip = False
                    self.current_idx = (idx + 1) % len(freqs)
                    break

                with _cfg_lock:
                    sq_mode = cfg.get('squelch_mode', 'audio')
                    sq_db = cfg.get('squelch_db', -35.0)
                    diff_sq = cfg.get('diff_squelch', 3.0)
                    dwell = cfg.get('dwell_time', 2.0)

                # NON-BLOCKING READ: Waits 50ms for data. If rtl_fm is blocked by 
                # RF squelch, this prevents the thread from freezing.
                try:
                    ready, _, _ = select.select([proc.stdout], [], [], 0.05)
                except Exception as e:
                    log.warning(f"select error: {e}")
                    break

                if proc.stdout in ready:
                    try:
                        # Grab whatever is instantly available
                        raw = os.read(proc.stdout.fileno(), 8192)
                    except Exception as e:
                        log.warning(f'rtl_fm read error: {e}')
                        break

                    if not raw:
                        log.warning('rtl_fm process ended unexpectedly')
                        break

                    buf.extend(raw)
                    chunks_read += 1

                    # Process full chunks as they buffer up
                    while len(buf) >= chunk_bytes:
                        chunk = bytes(buf[:chunk_bytes])
                        del buf[:chunk_bytes]

                        db = self.rms_db(chunk)
                        self.signal_db = db
                        
                        db_history.append(db)
                        # Keep a 1-second rolling window (approx 10 chunks at 100ms)
                        if len(db_history) > 10:
                            db_history.pop(0)

                        # Determine if this chunk is considered "Silence"
                        is_silence = False
                        if sq_mode == 'rf':
                            # If we are getting audio data in RF mode, the hardware gate is open.
                            is_silence = False
                        elif sq_mode == 'diff':
                            # Need a few chunks to establish a baseline
                            if len(db_history) < 3:
                                is_silence = True
                            else:
                                # A change in EITHER direction > limit breaks squelch
                                if max(db_history) - min(db_history) >= diff_sq:
                                    is_silence = False
                                else:
                                    is_silence = True
                        else:  # 'audio'
                            if db < sq_db:
                                is_silence = True

                        socketio.emit('signal', {'db': round(db, 1)})
                        socketio.emit('audio', {
                            'data': base64.b64encode(chunk).decode('ascii'),
                            'sr': sr,
                            'db': round(db, 1),
                            'sq': is_silence # Inform frontend so it can mute dead air
                        }, room='audio')

                        if is_silence:
                            if silence_start is None:
                                silence_start = time.time()
                        else:
                            silence_start = None

                        if silence_start is not None and not self.paused and (time.time() - silence_start) >= dwell:
                            break  # Breaks chunk loop
                else:
                    # Timeout triggered - hardware RF squelch is keeping the gate closed
                    self.signal_db = -100.0
                    socketio.emit('signal', {'db': -100.0})
                    if silence_start is None:
                        silence_start = time.time()

                if silence_start is not None and not self.paused and (time.time() - silence_start) >= dwell:
                    log.info(f'Silence for {dwell}s on {fi["freq"]/1e6:.3f} MHz, advancing')
                    self.current_idx = (idx + 1) % len(freqs)
                    break

            self._kill_proc()
            if chunks_read > 0:
                consecutive_failures = 0
            else:
                consecutive_failures += 1
                if consecutive_failures >= 5:
                    log.error('rtl_fm produced no audio 5 times in a row, stopping scanner')
                    self.running = False
                    break

        self.running = False
        self.current_freq = None
        socketio.emit('scanner_update', {'running': False})
        log.info('Scanner stopped')

    def start(self):
        if self.running: return
        self.running = True
        self.current_idx = 0
        self._thread = threading.Thread(target=self._loop, daemon=True, name='scanner')
        self._thread.start()

    def stop(self, join_timeout: float = 1.0):
        self.running = False
        self._kill_proc()
        if self._thread and self._thread is not threading.current_thread():
            self._thread.join(timeout=join_timeout)

    def notify_freqs_changed(self):
        with _cfg_lock:
            freqs = cfg.get('frequencies', [])
        if not freqs:
            self.current_idx = 0
            return
        cur = self.current_freq
        if cur is not None:
            for i, f in enumerate(freqs):
                if f is cur or (f.get('freq') == cur.get('freq') and f.get('mode') == cur.get('mode') and f.get('label') == cur.get('label')):
                    self.current_idx = i
                    return
        if self.current_idx >= len(freqs):
            self.current_idx = 0


scanner = Scanner()


# API Routes

@app.route('/')
def index():
    return render_template('index.html')


@app.route('/api/login', methods=['POST'])
def login():
    ip = client_ip()
    if not login_rate_limit_ok(ip):
        return jsonify(error='Too many attempts, try again in a few minutes'), 429

    d = request.get_json(force=True, silent=True) or {}
    username = d.get('username', '') or ''
    password = d.get('password', '') or ''
    if not isinstance(username, str) or not isinstance(password, str):
        return jsonify(error='Invalid credentials'), 401

    with _cfg_lock:
        expected_user = cfg['admin_username']
        stored_hash = cfg['admin_password_hash']
        must_change = bool(cfg.get('must_change_password', False))

    user_ok = hmac.compare_digest(username, expected_user)
    try:
        pw_ok = check_password_hash(stored_hash, password)
    except Exception:
        pw_ok = False

    if user_ok and pw_ok:
        token = create_session()
        return jsonify(token=token, username=expected_user, must_change_password=must_change)

    return jsonify(error='Invalid credentials'), 401


@app.route('/api/logout', methods=['POST'])
def logout():
    token = request.headers.get('X-Token', '')
    drop_session(token)
    return jsonify(ok=True)


@app.route('/api/verify', methods=['GET'])
def verify():
    token = request.headers.get('X-Token', '')
    if is_valid_token(token):
        with _cfg_lock:
            must_change = bool(cfg.get('must_change_password', False))
        return jsonify(valid=True, must_change_password=must_change)
    return jsonify(valid=False), 401


@app.route('/api/status')
def status():
    with _cfg_lock:
        freqs = list(cfg.get('frequencies', []))
        sq_mode = cfg.get('squelch_mode', 'audio')
    with _connected_lock:
        conn = _connected
    return jsonify(
        running=scanner.running,
        paused=scanner.paused,
        current_freq=scanner.current_freq,
        current_idx=scanner.current_idx,
        signal_db=round(scanner.signal_db, 1),
        squelch_mode=sq_mode,
        frequencies=freqs,
        connected=conn,
    )


@app.route('/api/frequencies', methods=['GET'])
def get_freqs():
    with _cfg_lock:
        return jsonify(list(cfg.get('frequencies', [])))


@app.route('/api/frequencies', methods=['POST'])
@admin_required
def add_freq():
    d = request.get_json(force=True, silent=True) or {}
    raw_freq = d.get('freq')
    if raw_freq is None or isinstance(raw_freq, bool):
        return jsonify(error='freq is required'), 400
    try:
        freq = int(raw_freq)
    except (ValueError, TypeError):
        return jsonify(error='freq must be an integer (Hz)'), 400
    if freq < 500_000 or freq > 1_750_000_000:
        return jsonify(error='freq out of RTL-SDR range (0.5 MHz - 1750 MHz)'), 400

    mode = str(d.get('mode', 'fm')).lower().strip()
    if mode not in VALID_MODES:
        return jsonify(error=f'mode must be one of: {", ".join(sorted(VALID_MODES))}'), 400

    label = d.get('label')
    if label is not None:
        label = str(label).strip()[:80]
    if not label:
        label = f'{freq / 1e6:.3f} MHz'

    entry = {'freq': freq, 'label': label, 'mode': mode}

    with _cfg_lock:
        cfg.setdefault('frequencies', []).append(entry)
        freqs_snapshot = list(cfg['frequencies'])
        save_config()

    socketio.emit('frequencies_updated', freqs_snapshot)
    return jsonify(entry), 201


@app.route('/api/frequencies/<int:idx>', methods=['PUT'])
@admin_required
def update_freq(idx):
    d = request.get_json(force=True, silent=True) or {}
    with _cfg_lock:
        freqs = cfg.get('frequencies', [])
        if not (0 <= idx < len(freqs)):
            return jsonify(error='Not found'), 404
        if 'label' in d:
            freqs[idx]['label'] = str(d['label']).strip()[:80]
        if 'mode' in d:
            mode = str(d['mode']).lower().strip()
            if mode not in VALID_MODES:
                return jsonify(error='Invalid mode'), 400
            freqs[idx]['mode'] = mode
        updated = dict(freqs[idx])
        freqs_snapshot = list(freqs)
        save_config()

    socketio.emit('frequencies_updated', freqs_snapshot)
    return jsonify(updated)


@app.route('/api/frequencies/<int:idx>', methods=['DELETE'])
@admin_required
def del_freq(idx):
    with _cfg_lock:
        freqs = cfg.get('frequencies', [])
        if not (0 <= idx < len(freqs)):
            return jsonify(error='Not found'), 404
        removed = freqs.pop(idx)
        freqs_snapshot = list(freqs)
        save_config()

    scanner.notify_freqs_changed()
    socketio.emit('frequencies_updated', freqs_snapshot)
    return jsonify(removed)


@app.route('/api/scanner/start', methods=['POST'])
@admin_required
def start_scanner():
    with _cfg_lock:
        has_freqs = bool(cfg.get('frequencies'))
    if not has_freqs:
        return jsonify(error='No frequencies configured'), 400
    scanner.start()
    return jsonify(running=True)


@app.route('/api/scanner/stop', methods=['POST'])
@admin_required
def stop_scanner():
    scanner.stop(join_timeout=0.5)
    return jsonify(running=False)


@app.route('/api/scanner/pause', methods=['POST'])
@admin_required
def pause_scanner():
    scanner.paused = not scanner.paused
    socketio.emit('scanner_update', {
        'running': scanner.running,
        'paused': scanner.paused,
        'idx': scanner.current_idx,
        'total': len(cfg.get('frequencies', [])),
        'freq': scanner.current_freq,
    })
    return jsonify(paused=scanner.paused)


@app.route('/api/scanner/skip', methods=['POST'])
@admin_required
def skip_scanner():
    scanner.force_skip = True
    return jsonify(ok=True)


# Settings
SETTINGS_KEYS = (
    'squelch_mode', 'squelch_db', 'rf_squelch', 'diff_squelch',
    'dwell_time', 'sample_rate', 'ppm', 'gain'
)


def _coerce_settings(d: dict) -> tuple[dict, str | None]:
    out: dict = {}
    if 'squelch_mode' in d:
        v = str(d['squelch_mode']).lower()
        if v in ('audio', 'rf', 'diff'):
            out['squelch_mode'] = v
        else:
            return {}, 'Invalid squelch mode'
    if 'squelch_db' in d:
        try:
            v = float(d['squelch_db'])
            if not -120 <= v <= 0: return {}, 'squelch_db must be between -120 and 0'
            out['squelch_db'] = v
        except (ValueError, TypeError): return {}, 'squelch_db must be a number'
    if 'rf_squelch' in d:
        try:
            v = int(d['rf_squelch'])
            if not 0 <= v <= 1000: return {}, 'rf_squelch must be between 0 and 1000'
            out['rf_squelch'] = v
        except (ValueError, TypeError): return {}, 'rf_squelch must be an integer'
    if 'diff_squelch' in d:
        try:
            v = float(d['diff_squelch'])
            if not 0.1 <= v <= 50.0: return {}, 'diff_squelch must be between 0.1 and 50'
            out['diff_squelch'] = v
        except (ValueError, TypeError): return {}, 'diff_squelch must be a number'
    if 'dwell_time' in d:
        try:
            v = float(d['dwell_time'])
            if not 0.1 <= v <= 600: return {}, 'dwell_time must be between 0.1 and 600'
            out['dwell_time'] = v
        except (ValueError, TypeError): return {}, 'dwell_time must be a number'
    if 'sample_rate' in d:
        try:
            v = int(d['sample_rate'])
            if v not in (8000, 16000, 22050, 24000, 32000, 44100, 48000): return {}, 'Invalid sample_rate'
            out['sample_rate'] = v
        except (ValueError, TypeError): return {}, 'sample_rate must be an integer'
    if 'ppm' in d:
        try:
            v = int(d['ppm'])
            if not -200 <= v <= 200: return {}, 'ppm must be between -200 and 200'
            out['ppm'] = v
        except (ValueError, TypeError): return {}, 'ppm must be an integer'
    if 'gain' in d:
        g = str(d['gain']).strip().lower()
        if g == 'auto':
            out['gain'] = 'auto'
        else:
            try:
                gv = float(g)
                if not 0 <= gv <= 100: return {}, 'gain must be between 0 and 100 dB'
                out['gain'] = g
            except ValueError: return {}, 'gain must be "auto" or a number'
    return out, None


@app.route('/api/settings', methods=['GET'])
@admin_required
def get_settings():
    with _cfg_lock:
        return jsonify({k: cfg.get(k) for k in SETTINGS_KEYS})


@app.route('/api/settings', methods=['POST'])
@admin_required
def update_settings():
    d = request.get_json(force=True, silent=True) or {}
    clean, err = _coerce_settings(d)
    if err:
        return jsonify(error=err), 400

    sample_rate_changed = False
    with _cfg_lock:
        old_sr = cfg.get('sample_rate')
        for k, v in clean.items():
            cfg[k] = v
        if 'sample_rate' in clean and clean['sample_rate'] != old_sr:
            sample_rate_changed = True
        if clean:
            save_config()

    needs_restart = sample_rate_changed or 'gain' in clean or 'ppm' in clean or 'rf_squelch' in clean or 'squelch_mode' in clean
    if needs_restart and scanner.running:
        scanner.stop(join_timeout=0.5)
        time.sleep(0.4)
        scanner.start()

    return jsonify(ok=True)


@app.route('/api/change_password', methods=['POST'])
@admin_required
def change_password():
    d = request.get_json(force=True, silent=True) or {}
    pw = d.get('password', '')
    if not isinstance(pw, str) or len(pw) < 8:
        return jsonify(error='Password must be at least 8 characters'), 400

    new_hash = generate_password_hash(pw)
    with _cfg_lock:
        cfg['admin_password_hash'] = new_hash
        cfg['must_change_password'] = False
        save_config()

    cur_token = request.headers.get('X-Token', '')
    with _sessions_lock:
        for t in list(_sessions.keys()):
            if t != cur_token:
                _sessions.pop(t, None)

    return jsonify(ok=True)


if __name__ == '__main__':
    port = int(os.environ.get('PORT', 8073))
    log.info(f'RTL-SDR Scanner starting on 0.0.0.0:{port}')
    if cfg.get('must_change_password'):
        log.warning('Default credentials in use: admin / changeme - CHANGE PASSWORD ON FIRST LOGIN')
    socketio.run(
        app,
        host='0.0.0.0',
        port=port,
        allow_unsafe_werkzeug=True,
    )
