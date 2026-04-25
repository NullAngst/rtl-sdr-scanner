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
import math
import hashlib
import secrets
import base64
import logging
from functools import wraps

import numpy as np
from flask import Flask, request, jsonify, render_template
from flask_socketio import SocketIO

logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s [%(levelname)s] %(message)s'
)
log = logging.getLogger(__name__)

# ── App Setup ─────────────────────────────────────────────────────────────────
app = Flask(__name__)
app.config['SECRET_KEY'] = os.environ.get('SECRET_KEY', secrets.token_hex(32))

socketio = SocketIO(
    app,
    cors_allowed_origins='*',
    async_mode='threading',
    max_http_buffer_size=2 * 1024 * 1024,
    logger=False,
    engineio_logger=False
)

# ── Config ────────────────────────────────────────────────────────────────────
CONFIG_FILE = os.environ.get('CONFIG_FILE', '/data/config.json')

DEFAULTS = {
    'admin_username': 'admin',
    'admin_password_hash': hashlib.sha256(b'changeme').hexdigest(),
    'frequencies': [],
    'squelch_db': -35.0,
    'dwell_time': 2.0,
    'sample_rate': 16000,
    'ppm': 0,
    'gain': 'auto',
}


def load_config() -> dict:
    if os.path.exists(CONFIG_FILE):
        try:
            with open(CONFIG_FILE) as f:
                data = json.load(f)
            for k, v in DEFAULTS.items():
                data.setdefault(k, v)
            return data
        except Exception as e:
            log.error(f'Failed to load config: {e}, using defaults')
    return dict(DEFAULTS)


def save_config():
    try:
        directory = os.path.dirname(CONFIG_FILE)
        if directory:
            os.makedirs(directory, exist_ok=True)
        with open(CONFIG_FILE, 'w') as f:
            json.dump(cfg, f, indent=2)
    except Exception as e:
        log.error(f'Failed to save config: {e}')


cfg = load_config()

# ── Session Auth ──────────────────────────────────────────────────────────────
_sessions: dict[str, float] = {}   # token -> expiry timestamp
SESSION_TTL = 86400                 # 24 hours


def create_session() -> str:
    token = secrets.token_hex(32)
    _sessions[token] = time.time() + SESSION_TTL
    return token


def is_valid_token(token: str) -> bool:
    exp = _sessions.get(token)
    if exp and exp > time.time():
        return True
    _sessions.pop(token, None)
    return False


def admin_required(f):
    @wraps(f)
    def wrapper(*args, **kwargs):
        token = request.headers.get('X-Token', '')
        if not is_valid_token(token):
            return jsonify(error='Unauthorized'), 401
        return f(*args, **kwargs)
    return wrapper


# ── Connected Client Tracking ─────────────────────────────────────────────────
_connected = 0


@socketio.on('connect')
def on_connect():
    global _connected
    _connected += 1
    socketio.emit('system_stats', {'connected': _connected})
    log.info(f'Client connected ({_connected} total)')


@socketio.on('disconnect')
def on_disconnect():
    global _connected
    _connected = max(0, _connected - 1)
    socketio.emit('system_stats', {'connected': _connected})
    log.info(f'Client disconnected ({_connected} total)')


# ── Scanner ───────────────────────────────────────────────────────────────────
class Scanner:
    """
    Manages the rtl_fm subprocess and frequency-scanning loop.
    Reads raw 16-bit PCM audio from rtl_fm, measures RMS power,
    and advances to the next frequency after `dwell_time` seconds
    of silence below `squelch_db`.
    """

    CHUNK_MS = 100  # Size of each read/emit chunk in milliseconds

    def __init__(self):
        self.running = False
        self.current_idx = 0
        self.current_freq: dict | None = None
        self.signal_db = -100.0
        self._proc: subprocess.Popen | None = None
        self._lock = threading.Lock()
        self._thread: threading.Thread | None = None

    # ── Process control ───────────────────────────────────────────────────────

    def _kill_proc(self):
        with self._lock:
            if self._proc and self._proc.poll() is None:
                self._proc.terminate()
                try:
                    self._proc.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    self._proc.kill()
                    self._proc.wait()
            self._proc = None

    def _start_rtl(self, freq_hz: int, mode: str = 'fm') -> subprocess.Popen:
        gain = cfg.get('gain', 'auto')
        sr = str(cfg.get('sample_rate', 16000))
        ppm = str(cfg.get('ppm', 0))

        cmd = [
            'rtl_fm',
            '-f', str(freq_hz),
            '-M', mode,
            '-s', '200000',   # capture sample rate (200 kHz)
            '-r', sr,         # output resample rate
            '-p', ppm,        # PPM frequency correction
            '-l', '0',        # hardware squelch off — we handle it in software
            '-'               # output to stdout
        ]
        if gain != 'auto':
            cmd += ['-g', str(gain)]

        log.info(f'Starting rtl_fm: {" ".join(cmd)}')
        with self._lock:
            self._proc = subprocess.Popen(
                cmd,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                bufsize=0
            )
        return self._proc

    # ── Signal analysis ───────────────────────────────────────────────────────

    @staticmethod
    def rms_db(raw: bytes) -> float:
        """Return RMS level in dBFS from raw signed 16-bit PCM bytes."""
        if len(raw) < 2:
            return -100.0
        samples = np.frombuffer(raw, dtype=np.int16).astype(np.float32)
        rms = np.sqrt(np.mean(samples ** 2))
        if rms < 1.0:
            return -100.0
        return float(20.0 * np.log10(rms / 32768.0))

    # ── Main scan loop ────────────────────────────────────────────────────────

    def _loop(self):
        while self.running:
            freqs = cfg.get('frequencies', [])
            if not freqs:
                time.sleep(0.5)
                continue

            idx = self.current_idx % len(freqs)
            fi = freqs[idx]
            sr = cfg.get('sample_rate', 16000)
            dwell = cfg.get('dwell_time', 2.0)
            squelch = cfg.get('squelch_db', -35.0)
            chunk_bytes = int(sr * self.CHUNK_MS / 1000) * 2  # 16-bit = 2 bytes/sample

            self.current_freq = fi
            self.current_idx = idx

            socketio.emit('scanner_update', {
                'running': True,
                'idx': idx,
                'total': len(freqs),
                'freq': fi,
            })

            log.info(f'Tuning to {fi["freq"]/1e6:.3f} MHz ({fi.get("label","")}) [{fi.get("mode","fm").upper()}]')

            self._kill_proc()
            proc = self._start_rtl(fi['freq'], fi.get('mode', 'fm'))

            silence_start: float | None = None

            while self.running:
                try:
                    chunk = proc.stdout.read(chunk_bytes)
                except Exception as e:
                    log.warning(f'rtl_fm read error: {e}')
                    break

                if not chunk:
                    log.warning('rtl_fm process ended unexpectedly')
                    break

                db = self.rms_db(chunk)
                self.signal_db = db

                # Emit signal level to all clients
                socketio.emit('signal', {'db': round(db, 1)})

                # Stream audio (base64-encoded raw PCM int16 LE)
                socketio.emit('audio', {
                    'data': base64.b64encode(chunk).decode('ascii'),
                    'sr': sr,
                })

                # Squelch / advance logic
                if db < squelch:
                    if silence_start is None:
                        silence_start = time.time()
                    elif (time.time() - silence_start) >= dwell:
                        log.info(f'Silence for {dwell}s on {fi["freq"]/1e6:.3f} MHz, advancing')
                        self.current_idx = (idx + 1) % len(freqs)
                        break
                else:
                    silence_start = None   # Active signal — reset silence timer

            self._kill_proc()

        # Scanner stopped
        self.running = False
        self.current_freq = None
        socketio.emit('scanner_update', {'running': False})
        log.info('Scanner stopped')

    def start(self):
        if self.running:
            return
        self.running = True
        self.current_idx = 0
        self._thread = threading.Thread(target=self._loop, daemon=True, name='scanner')
        self._thread.start()
        log.info('Scanner started')

    def stop(self):
        self.running = False
        self._kill_proc()
        if self._thread:
            self._thread.join(timeout=5)
        log.info('Scanner stop requested')


scanner = Scanner()


# ── API Routes ────────────────────────────────────────────────────────────────

@app.route('/')
def index():
    return render_template('index.html')


@app.route('/api/login', methods=['POST'])
def login():
    d = request.get_json(force=True, silent=True) or {}
    pw_hash = hashlib.sha256(d.get('password', '').encode()).hexdigest()
    if (d.get('username') == cfg['admin_username'] and
            pw_hash == cfg['admin_password_hash']):
        token = create_session()
        log.info(f'Admin login: {d["username"]}')
        return jsonify(token=token, username=cfg['admin_username'])
    log.warning(f'Failed login attempt for user: {d.get("username")}')
    return jsonify(error='Invalid credentials'), 401


@app.route('/api/logout', methods=['POST'])
def logout():
    token = request.headers.get('X-Token', '')
    _sessions.pop(token, None)
    return jsonify(ok=True)


@app.route('/api/verify', methods=['GET'])
def verify():
    """Check if a token is still valid — used on page load."""
    token = request.headers.get('X-Token', '')
    if is_valid_token(token):
        return jsonify(valid=True)
    return jsonify(valid=False), 401


@app.route('/api/status')
def status():
    return jsonify(
        running=scanner.running,
        current_freq=scanner.current_freq,
        current_idx=scanner.current_idx,
        signal_db=round(scanner.signal_db, 1),
        frequencies=cfg.get('frequencies', []),
        connected=_connected,
    )


# ── Frequency Management ──────────────────────────────────────────────────────

@app.route('/api/frequencies', methods=['GET'])
def get_freqs():
    return jsonify(cfg.get('frequencies', []))


@app.route('/api/frequencies', methods=['POST'])
@admin_required
def add_freq():
    d = request.get_json(force=True, silent=True) or {}
    freq = d.get('freq')
    if not freq:
        return jsonify(error='freq is required'), 400
    try:
        freq = int(freq)
    except ValueError:
        return jsonify(error='freq must be an integer (Hz)'), 400
    if freq < 500_000 or freq > 1_750_000_000:
        return jsonify(error='freq out of RTL-SDR range (0.5 MHz - 1750 MHz)'), 400

    entry = {
        'freq': freq,
        'label': d.get('label') or f'{freq / 1e6:.3f} MHz',
        'mode': d.get('mode', 'fm'),
    }
    cfg.setdefault('frequencies', []).append(entry)
    save_config()
    socketio.emit('frequencies_updated', cfg['frequencies'])
    log.info(f'Added frequency: {entry}')
    return jsonify(entry), 201


@app.route('/api/frequencies/<int:idx>', methods=['PUT'])
@admin_required
def update_freq(idx):
    freqs = cfg.get('frequencies', [])
    if not (0 <= idx < len(freqs)):
        return jsonify(error='Not found'), 404
    d = request.get_json(force=True, silent=True) or {}
    if 'label' in d:
        freqs[idx]['label'] = d['label']
    if 'mode' in d:
        freqs[idx]['mode'] = d['mode']
    save_config()
    socketio.emit('frequencies_updated', cfg['frequencies'])
    return jsonify(freqs[idx])


@app.route('/api/frequencies/<int:idx>', methods=['DELETE'])
@admin_required
def del_freq(idx):
    freqs = cfg.get('frequencies', [])
    if not (0 <= idx < len(freqs)):
        return jsonify(error='Not found'), 404
    removed = freqs.pop(idx)
    # Keep scanner index in range
    if scanner.current_idx >= len(freqs) and freqs:
        scanner.current_idx = 0
    save_config()
    socketio.emit('frequencies_updated', cfg['frequencies'])
    log.info(f'Removed frequency: {removed}')
    return jsonify(removed)


# ── Scanner Control ───────────────────────────────────────────────────────────

@app.route('/api/scanner/start', methods=['POST'])
@admin_required
def start_scanner():
    if not cfg.get('frequencies'):
        return jsonify(error='No frequencies configured'), 400
    scanner.start()
    return jsonify(running=True)


@app.route('/api/scanner/stop', methods=['POST'])
@admin_required
def stop_scanner():
    scanner.stop()
    return jsonify(running=False)


# ── Settings ──────────────────────────────────────────────────────────────────

@app.route('/api/settings', methods=['GET'])
@admin_required
def get_settings():
    return jsonify({k: cfg.get(k) for k in
                    ['squelch_db', 'dwell_time', 'sample_rate', 'ppm', 'gain']})


@app.route('/api/settings', methods=['POST'])
@admin_required
def update_settings():
    d = request.get_json(force=True, silent=True) or {}
    changed = False
    for k in ['squelch_db', 'dwell_time', 'sample_rate', 'ppm', 'gain']:
        if k in d:
            cfg[k] = d[k]
            changed = True
    if changed:
        save_config()
        # Restart scanner if running so new settings take effect
        if scanner.running:
            scanner.stop()
            time.sleep(0.4)
            scanner.start()
    return jsonify(ok=True)


@app.route('/api/change_password', methods=['POST'])
@admin_required
def change_password():
    d = request.get_json(force=True, silent=True) or {}
    pw = d.get('password', '')
    if len(pw) < 6:
        return jsonify(error='Password must be at least 6 characters'), 400
    cfg['admin_password_hash'] = hashlib.sha256(pw.encode()).hexdigest()
    save_config()
    log.info('Admin password changed')
    return jsonify(ok=True)


# ── Entry Point ───────────────────────────────────────────────────────────────

if __name__ == '__main__':
    port = int(os.environ.get('PORT', 8073))
    log.info(f'RTL-SDR Scanner starting on 0.0.0.0:{port}')
    log.info(f'Config file: {CONFIG_FILE}')
    log.info(f'Default credentials: admin / changeme  <-- CHANGE THIS')
    socketio.run(
        app,
        host='0.0.0.0',
        port=port,
        allow_unsafe_werkzeug=True
    )
