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
import shutil
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
    now = time.time()
    with _sessions_lock:
        # Expired tokens were only dropped when someone happened to present
        # them, so the table grew forever.
        for t, exp in list(_sessions.items()):
            if exp <= now:
                del _sessions[t]
        _sessions[token] = now + SESSION_TTL
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

# sids currently in the 'audio' room. Base64-encoding and emitting every
# 100 ms chunk when nobody is listening is pure waste, and it used to happen
# on every scan.
_audio_sids: set[str] = set()
_audio_lock = threading.Lock()


def has_audio_listeners() -> bool:
    with _audio_lock:
        return bool(_audio_sids)


@socketio.on('connect')
def on_connect():
    global _connected
    with _connected_lock:
        _connected += 1
        count = _connected
    socketio.emit('system_stats', {'connected': count})


@socketio.on('disconnect')
def on_disconnect(*args):
    # Flask-SocketIO >= 5.5 passes a disconnect reason. Accept it either way.
    global _connected
    with _connected_lock:
        _connected = max(0, _connected - 1)
        count = _connected
    with _audio_lock:
        _audio_sids.discard(request.sid)
    socketio.emit('system_stats', {'connected': count})


@socketio.on('audio_subscribe')
def on_audio_subscribe():
    join_room('audio')
    with _audio_lock:
        _audio_sids.add(request.sid)


@socketio.on('audio_unsubscribe')
def on_audio_unsubscribe():
    leave_room('audio')
    with _audio_lock:
        _audio_sids.discard(request.sid)


# Scanner
RTL_FM_BIN = shutil.which('rtl_fm') or 'rtl_fm'

SIGNAL_EMIT_INTERVAL = 0.1  # seconds; meter updates are capped at 10 Hz


class Scanner:
    CHUNK_MS = 100
    TUNE_GRACE = 3.0   # seconds allowed for rtl_fm/dongle startup before the silence clock may run
    NO_DATA_GAP = 0.4  # seconds without any bytes before the channel counts as dead air

    def __init__(self):
        self.running = False
        self.paused = False
        self.force_skip = False
        self.current_idx = 0
        self.current_freq: dict | None = None
        self.signal_db = -100.0
        self.last_error: str | None = None
        self._proc: subprocess.Popen | None = None
        self._proc_lock = threading.Lock()
        self._thread: threading.Thread | None = None
        self._stderr_thread: threading.Thread | None = None
        self._last_stderr: str = ''
        # Bumped on every start/stop. A loop whose generation no longer
        # matches is a leftover and must exit without touching shared state.
        self._gen = 0
        self._life_lock = threading.Lock()

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
                    low = line.lower()
                    if any(k in low for k in (
                            'error', 'failed', 'no supported', 'not found',
                            'busy', 'cannot', 'unable', 'usb_')):
                        self._last_stderr = line
        except Exception:
            pass

    # Demodulation bandwidth we aim for, per mode, in Hz.
    #
    # The old command hardcoded `-s 200000` for every mode. That is the
    # broadcast-FM recipe and it is wrong for everything else here: it
    # demodulates a 200 kHz slice for a 12.5/25 kHz NBFM channel, so the
    # wanted signal is buried under ~10x its own bandwidth of noise and the
    # audio comes out weak and hissy. On AM/USB/LSB it is useless.
    #
    # It also broke the resampler. rtl_fm's `low_pass_real` averages with
    # `rate_in / rate_out` using *integer* division, so 200000 -> 16000
    # (12.5) divides sums of 12.5 samples by 12: wrong gain plus distortion.
    # Rates below are always an exact integer multiple of the output rate.
    TARGET_BW = {
        'fm': 16000,
        'am': 16000,
        'usb': 16000,
        'lsb': 16000,
        'raw': 16000,
        'wbfm': 170000,
    }

    @classmethod
    def rates_for(cls, mode: str, out_rate: int) -> tuple[int, int]:
        target = cls.TARGET_BW.get(mode, 16000)
        mult = max(1, round(target / out_rate))
        return out_rate * mult, out_rate

    def _start_rtl(self, freq_hz: int, mode: str = 'fm') -> subprocess.Popen:
        with _cfg_lock:
            gain = cfg.get('gain', 'auto')
            out_rate = int(cfg.get('sample_rate', 16000))
            ppm = str(cfg.get('ppm', 0))
            sq_mode = cfg.get('squelch_mode', 'audio')

            # Only apply RF squelch limit if the mode is actually set to RF
            rf_sql = str(cfg.get('rf_squelch', 0)) if sq_mode == 'rf' else '0'

        demod_rate, out_rate = self.rates_for(mode, out_rate)

        # -M must come before -s: rtl_fm's wbfm preset sets its own rate_in,
        # and we want our value to win.
        cmd = [
            RTL_FM_BIN,
            '-f', str(freq_hz),
            '-M', mode,
            '-s', str(demod_rate),
            '-p', ppm,
            '-l', rf_sql,
        ]
        if demod_rate != out_rate:
            cmd += ['-r', str(out_rate)]
        if gain != 'auto':
            cmd += ['-g', str(gain)]
        cmd.append('-')

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

    def _alive(self, gen: int) -> bool:
        return self.running and self._gen == gen

    def _loop(self, gen: int):
        consecutive_failures = 0
        last_sig_emit = 0.0
        while self._alive(gen):
            with _cfg_lock:
                freqs = list(cfg.get('frequencies', []))
                sr = cfg.get('sample_rate', 16000)

            if not freqs:
                for _ in range(5):
                    if not self._alive(gen): break
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
                self.last_error = f'Failed to start rtl_fm: {e}'
                consecutive_failures += 1
                if consecutive_failures >= 5:
                    break
                time.sleep(1.0)
                continue

            silence_start: float | None = None
            chunks_read = 0
            tuned_at = time.time()
            last_data_at = tuned_at
            buf = bytearray()
            db_history = []  # Used for rolling variance calculation in diff mode

            while self._alive(gen):
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
                    last_data_at = time.time()

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

                        # Determine if this chunk is considered "Silence".
                        # NOTE: the absolute dBFS floor applies ONLY in
                        # 'audio' mode. Applying it to all modes breaks
                        # 'diff' mode completely: diff mode exists for FM
                        # quieting, where real voice on a strong carrier is
                        # QUIETER than open static. A -35 dBFS floor there
                        # flags every chunk of real audio as silence, so the
                        # frontend mutes everything and the scanner advances
                        # every dwell period.
                        if sq_mode == 'rf':
                            # Data flowing at all means the hardware gate is
                            # open (rtl_fm -l blocks output when squelched).
                            is_silence = False
                        elif sq_mode == 'diff':
                            if len(db_history) < 3:
                                # Warm-up: not enough history to judge.
                                # Treat as signal so a freshly tuned channel
                                # is never skipped before it can be measured.
                                is_silence = False
                            else:
                                # A swing in EITHER direction >= limit breaks squelch
                                is_silence = (max(db_history) - min(db_history)) < diff_sq
                        else:  # 'audio'
                            is_silence = db < sq_db

                        now = time.time()
                        if now - last_sig_emit >= SIGNAL_EMIT_INTERVAL:
                            last_sig_emit = now
                            socketio.emit('signal', {'db': round(db, 1)})

                        if has_audio_listeners():
                            socketio.emit('audio', {
                                'data': base64.b64encode(chunk).decode('ascii'),
                                'sr': sr,
                                'db': round(db, 1),
                                'sq': is_silence  # Inform frontend so it can mute dead air
                            }, room='audio')

                        if is_silence:
                            if silence_start is None:
                                silence_start = time.time()
                        else:
                            silence_start = None

                        if silence_start is not None and not self.paused and (time.time() - silence_start) >= dwell:
                            break  # Breaks chunk loop
                else:
                    # Nothing within the select timeout. That is NOT by itself
                    # dead air: rtl_fm writes in bursts, so gaps of one or two
                    # timeouts happen constantly on a perfectly live channel.
                    # Treating each one as silence pinned the meter to -100
                    # between bursts and, in diff mode, wiped the rolling
                    # window every time.
                    #
                    # A sustained gap means one of two things: rtl_fm is still
                    # starting up (dongle init and tune take 1-3s), or the
                    # hardware RF squelch is holding the gate closed. Only the
                    # second one is dead air, hence the startup grace: without
                    # it the dwell timer expires before the first sample ever
                    # arrives and the scanner hops forever.
                    now = time.time()
                    gap = now - last_data_at
                    if gap >= self.NO_DATA_GAP:
                        self.signal_db = -100.0
                        db_history.clear()
                        if now - last_sig_emit >= SIGNAL_EMIT_INTERVAL:
                            last_sig_emit = now
                            socketio.emit('signal', {'db': -100.0})
                        if silence_start is None and (
                                chunks_read > 0 or
                                (now - tuned_at) >= self.TUNE_GRACE):
                            silence_start = now

                if silence_start is not None and not self.paused and (time.time() - silence_start) >= dwell:
                    log.info(f'Silence for {dwell}s on {fi["freq"]/1e6:.3f} MHz, advancing')
                    self.current_idx = (idx + 1) % len(freqs)
                    break

            # Did rtl_fm die on its own, or are we killing it to retune?
            # poll() right after EOF often still reports None (the pipe closes
            # before the child is reaped), which made the failure counter miss
            # most real failures. Only worth a short wait when the channel
            # produced nothing at all.
            proc_exited = False
            if chunks_read == 0:
                try:
                    proc.wait(timeout=0.25)
                    proc_exited = True
                except subprocess.TimeoutExpired:
                    proc_exited = False
            self._kill_proc()
            if chunks_read > 0:
                consecutive_failures = 0
            elif proc_exited:
                # Process died without ever producing audio - a real failure
                # (no dongle, device busy, bad args, ...).
                consecutive_failures += 1
                detail = self._last_stderr.strip()
                self.last_error = (
                    'rtl_fm exited without producing audio'
                    + (f': {detail}' if detail else
                       ' - check that the dongle is attached and not claimed '
                       'by another process')
                )
                if consecutive_failures >= 5:
                    log.error('rtl_fm produced no audio 5 times in a row, stopping scanner')
                    break
            # else: rtl_fm was alive but hardware-squelched the whole dwell.
            # A quiet channel is not a failure; don't count it, or scanning a
            # list of idle channels in RF mode kills the scanner after five.

        # Only the current generation owns the shared state. A superseded
        # thread must not flip `running` off under a scanner that has already
        # been restarted.
        with self._life_lock:
            if self._gen == gen:
                self.running = False
                self.current_freq = None
                socketio.emit('scanner_update', {'running': False})
        log.info('Scanner loop exited')

    def start(self) -> tuple[bool, str | None]:
        with self._life_lock:
            if self.running and self._thread and self._thread.is_alive():
                return True, None

            # Two live scanner threads means two rtl_fm processes fighting
            # over one dongle: the second one fails with a device-busy error
            # and the whole thing goes quiet. Make sure the old thread is
            # really gone first. The previous code only waited 0.5s on stop
            # and then started a second thread regardless.
            self.running = False
            self._gen += 1
            self._kill_proc()
            prev = self._thread
        if prev and prev.is_alive() and prev is not threading.current_thread():
            prev.join(timeout=5.0)
            if prev.is_alive():
                log.error('Previous scanner thread did not exit; refusing to start')
                return False, 'Previous scan did not shut down; try again'

        if not shutil.which(RTL_FM_BIN) and not os.path.exists(RTL_FM_BIN):
            return False, 'rtl_fm not found in the container'

        with self._life_lock:
            self._gen += 1
            gen = self._gen
            self.running = True
            self.paused = False
            self.force_skip = False
            self.last_error = None
            self.current_idx = 0
            self._thread = threading.Thread(
                target=self._loop, args=(gen,), daemon=True, name='scanner')
            self._thread.start()
        return True, None

    def stop(self, join_timeout: float = 5.0):
        with self._life_lock:
            self.running = False
            self._gen += 1
            self._kill_proc()
            t = self._thread
        if t and t is not threading.current_thread():
            t.join(timeout=join_timeout)
        self.current_freq = None
        self.signal_db = -100.0
        # stop() bumped the generation, so the exiting loop will not announce
        # this. Announce it here or the UI sits on a stale SCANNING state.
        socketio.emit('scanner_update', {'running': False})

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

    # compare_digest raises TypeError on non-ASCII str input, which would
    # turn a junk username into a 500 instead of a 401.
    user_ok = hmac.compare_digest(
        username.encode('utf-8'), str(expected_user).encode('utf-8'))
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
        last_error=scanner.last_error,
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

    scanner.notify_freqs_changed()
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

    scanner.notify_freqs_changed()
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
    ok, err = scanner.start()
    if not ok:
        return jsonify(error=err or 'Failed to start scanner'), 500
    return jsonify(running=True)


@app.route('/api/scanner/stop', methods=['POST'])
@admin_required
def stop_scanner():
    scanner.stop()
    return jsonify(running=False)


@app.route('/api/scanner/pause', methods=['POST'])
@admin_required
def pause_scanner():
    scanner.paused = not scanner.paused
    with _cfg_lock:
        total = len(cfg.get('frequencies', []))
    socketio.emit('scanner_update', {
        'running': scanner.running,
        'paused': scanner.paused,
        'idx': scanner.current_idx,
        'total': total,
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
        # stop() now joins the worker, so start() cannot race a thread that is
        # still holding the dongle open.
        scanner.stop()
        ok, err = scanner.start()
        if not ok:
            return jsonify(error=err or 'Settings saved but scanner restart failed'), 500

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
    if not shutil.which('rtl_fm'):
        log.error('rtl_fm not found on PATH - the scanner will not produce audio. '
                  'Install the rtl-sdr package (or rebuild the image).')
    if cfg.get('must_change_password'):
        log.warning('Default credentials in use: admin / changeme - CHANGE PASSWORD ON FIRST LOGIN')
    socketio.run(
        app,
        host='0.0.0.0',
        port=port,
        allow_unsafe_werkzeug=True,
    )
