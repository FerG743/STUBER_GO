#!/usr/bin/env python3
"""
SPDH Sim Agent — stdlib-only HTTP control plane for STUBER_GO.

Runs on this machine (wherever STUBER_GO is checked out). Oms_Automation's
HiperBackend/app3 — which can live on a completely different host — calls
this agent's HTTP endpoints as a webhook to start/stop TCP stub servers and
tail their traffic. No browser ever talks to this agent directly, so no
third-party dependency (Flask, CORS, ...) is needed here — stdlib only, so
this drops onto a bare machine with just Python 3 and Go installed.

Traffic seen by a stub — from the built-in test button, or from a real
POS/B24 device pointed at this machine — is captured the same way either
way: STUBER_GO logs every connection's source address, and that's tracked
per run below (see `clients`) so it survives past a scrolling log view.

Multiple message types can run at once (each on its own port), but this
machine is underpowered — MAX_CONCURRENT_STUBS caps how many stay up
together; starting a new one past the cap is rejected, not auto-evicted.

Each run remembers who started it (an opaque `ownerId` the caller supplies —
no real auth here, just enough to stop a second tester from silently
restarting someone else's in-progress run out from under them). Only that
owner can stop or restart it; the TTL timer is still a universal safety net
regardless of owner, so an abandoned run can't get stuck forever.

Usage:
    python3 agent/sim_agent.py --port 8090
"""
import argparse
import json
import os
import re
import signal
import socket
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

_HERE = os.path.dirname(os.path.abspath(__file__))
PROGRAM_DIR = os.path.normpath(os.path.join(_HERE, "..", "Program"))
DATA_DIR = os.path.join(_HERE, "data")
# Config a run uses when the caller sends no YAML: the repo's own stub definitions.
# The binary's -port/-outcome flags then cover the two things that vary per run.
STATIC_CONFIG = os.path.join(PROGRAM_DIR, "STUBS.yaml")
# The stub's own HTTP server (for http_stubs) needs a port too, even though
# our generated configs are TCP-only. "0" asks the OS for a free ephemeral
# port — required once more than one stub can run at a time: a fixed port
# here would make the 2nd+ concurrent process collide and exit(1).
UNUSED_HTTP_PORT = "0"

MIN_TTL_MINUTES = 60
MAX_TTL_MINUTES = 180
MAX_CONCURRENT_STUBS = 3

# The IP/hostname this machine tells callers to point a real POS/B24 device
# at. Set once at startup (see main()) — either from --host, or auto-detected
# below. A machine's address doesn't change mid-session, so there's no need
# to redetect this on every /status call.
ADVERTISE_HOST = "127.0.0.1"


def _detect_local_ip():
    # Doesn't actually send a packet — connecting a UDP socket just asks the
    # OS which local interface/address it would route through, which is
    # exactly "what's my real IP on this network" without needing a
    # reachable peer at the other end.
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        s.connect(("8.8.8.8", 80))
        return s.getsockname()[0]
    except OSError:
        return "127.0.0.1"
    finally:
        s.close()

_CONNECTION_RE = re.compile(r"Connection from (\S+)")
# main.go logs a bind failure ("[TCP] Stub tcp-protocol error: failed to
# listen on port 8001: ...") as a plain line and keeps the process alive —
# it does NOT exit non-zero. Without catching this, a run whose port never
# actually bound would sit reported as "running" forever, and nothing would
# ever explain why no traffic — real or test — reaches it.
_LISTEN_ERROR_RE = re.compile(r"\[TCP\] Stub (\S+) error: (.+)")

_LOCK = threading.Lock()
# Keyed by stubName, which maps to one running process built from a YAML
# config. That config can bundle several tcp_stubs entries (e.g. Liverpool +
# Suburbia variants of the same message on their own ports) so one run/build
# covers both instead of spending a process per variant on this machine.
_RUNS = {}
_LOGS = {}       # stubName -> [{seq, ts, line}]
_LOG_SEQ = {}    # stubName -> int
_LOGS_CAP = 5000

_NEW_RUN = {
    "process": None,
    "timer": None,
    "run_id": 0,
    "status": "stopped",
    "ports": [],
    "bridgeProcesses": [],
    "wsPorts": [],
    "outcomeName": None,
    "pinnedCode": None,
    "startedAt": None,
    "expiresAt": None,
    "ttlMinutes": None,
    "stoppedReason": None,
    "error": None,
    "clients": [],
    "ownerId": None,
}


def _run_slot(stub_name):
    if stub_name not in _RUNS:
        _RUNS[stub_name] = dict(_NEW_RUN)
    return _RUNS[stub_name]


def _append_log(stub_name, line):
    with _LOCK:
        seq = _LOG_SEQ.get(stub_name, 0) + 1
        _LOG_SEQ[stub_name] = seq
        bucket = _LOGS.setdefault(stub_name, [])
        bucket.append({"seq": seq, "ts": time.time(), "line": line})
        if len(bucket) > _LOGS_CAP:
            del bucket[: len(bucket) - _LOGS_CAP]

        match = _CONNECTION_RE.search(line)
        if match and stub_name in _RUNS:
            addr = match.group(1)
            clients = _RUNS[stub_name]["clients"]
            if addr not in clients:
                clients.append(addr)

        # Every line here came from this run's own process, so a bind failure in it is
        # this run's failure - no need for the YAML's stub name to equal the caller's
        # stubName (they legitimately differ when running the static STUBS.yaml).
        listen_error = _LISTEN_ERROR_RE.search(line)
        if listen_error and stub_name in _RUNS:
            run = _RUNS[stub_name]
            if run["status"] == "running":
                run["status"] = "error"
                run["error"] = listen_error.group(2)


def _reader_thread(stub_name, proc, run_id):
    for raw_line in proc.stdout:
        _append_log(stub_name, raw_line.rstrip("\n"))
    proc.wait()
    with _LOCK:
        run = _RUNS.get(stub_name)
        # A stale reader from a run that's since been replaced shouldn't
        # clobber the newer run's state.
        if run and run["run_id"] == run_id and run["process"] is proc:
            run["process"] = None
            if proc.returncode == 0:
                if run["status"] == "running":
                    run["status"] = "stopped"
            else:
                run["status"] = "error"
                run["error"] = f"El proceso terminó con código {proc.returncode}"


def _stop_locked(stub_name):
    """Caller must hold _LOCK for the whole call. Blocks other requests for
    up to ~5s on a slow shutdown — acceptable at this scale (a handful of
    QA testers, not a stop storm); simpler and safer than juggling the lock."""
    run = _RUNS.get(stub_name)
    if not run:
        return
    proc = run["process"]
    bridge_procs = run["bridgeProcesses"]
    timer = run["timer"]
    run["process"] = None
    run["bridgeProcesses"] = []
    run["wsPorts"] = []
    run["timer"] = None
    if timer:
        timer.cancel()
    for p in [proc, *bridge_procs]:
        if p and p.poll() is None:
            p.terminate()
    for p in [proc, *bridge_procs]:
        if p and p.poll() is None:
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                p.kill()
    run["status"] = "stopped"


def _expire(stub_name, run_id):
    with _LOCK:
        run = _RUNS.get(stub_name)
        if not run or run["run_id"] != run_id or run["status"] != "running":
            return
        run["stoppedReason"] = "expired"
        ttl = run["ttlMinutes"]
        _stop_locked(stub_name)
    _append_log(stub_name, f"[agent] Límite de tiempo alcanzado ({ttl} min) — deteniendo automáticamente")


WSBRIDGE_DIR = os.path.join(PROGRAM_DIR, "wsbridge")


def _ws_port_for(tcp_port):
    # Deterministic so a caller can work out where to connect without us
    # having to report it separately - good enough at this scale (a
    # handful of stub ports, all in the 8000s).
    return tcp_port + 1000


def _bridge_reader_thread(stub_name, proc):
    """Pipes a wsbridge process's own output into the run's log, tagged so it's
    obviously not stub traffic. Doesn't touch run/status - a bridge dying doesn't
    mean the stub itself is down, just that WS/HTTP callers can't reach it until
    the next start."""
    for raw_line in proc.stdout:
        line = raw_line.rstrip("\n")
        _append_log(stub_name, f"[bridge] {line}")
    proc.wait()
    if proc.returncode not in (0, None):
        _append_log(stub_name, f"[bridge] process exited with code {proc.returncode}")


def _kill_port_squatter(port):
    """Best-effort: kill whatever's still bound to `port` before we start a fresh
    process there. _stop_locked only knows about processes we're tracking in _RUNS -
    an orphan (started outside that lifecycle, e.g. manually from a terminal, or a
    sim_agent restart that lost track of its child) would otherwise make our new
    process fail to bind and die immediately, leaving the orphan to keep answering
    with whatever stale code it was built from."""
    try:
        found = subprocess.run(
            ["lsof", "-ti", f"tcp:{port}", "-sTCP:LISTEN"],
            capture_output=True, text=True, timeout=3,
        )
    except (OSError, subprocess.TimeoutExpired):
        return
    for pid in found.stdout.split():
        try:
            os.kill(int(pid), signal.SIGKILL)
        except (ValueError, ProcessLookupError, PermissionError):
            pass


def start(stub_name, yaml_text, ports, outcome_name, pinned_code, ttl_minutes, owner_id):
    try:
        ttl_minutes = int(ttl_minutes)
    except (TypeError, ValueError):
        ttl_minutes = MIN_TTL_MINUTES
    ttl_minutes = max(MIN_TTL_MINUTES, min(ttl_minutes, MAX_TTL_MINUTES))

    # With yaml_text: legacy path, the caller's YAML is the whole config. Without it: run
    # the static STUBS.yaml and pass what varies as flags - one port (-port only takes
    # one) and an optional variant name to pin (-outcome). pinned_code has no flag, so
    # asking for it without a YAML is rejected rather than silently ignored.
    if not yaml_text:
        if len(ports) != 1:
            raise ValueError("Sin YAML solo se admite un puerto (ports debe tener 1 elemento)")
        if pinned_code:
            raise ValueError("pinnedCode requiere enviar el YAML; sin YAML solo se puede fijar outcomeName")

    if not os.path.exists(os.path.join(PROGRAM_DIR, "main.go")):
        raise RuntimeError(f"No se encontró main.go en {PROGRAM_DIR}")

    with _LOCK:
        existing = _RUNS.get(stub_name)
        already_running = existing is not None and existing["status"] == "running"
        # Someone else already has this exact message type up: block, don't
        # silently kill their run and replace it out from under them. The
        # TTL timer is still the universal safety net regardless of owner.
        if already_running and existing.get("ownerId") != owner_id:
            minutes_up = int((time.time() - existing["startedAt"]) / 60)
            raise PermissionError(
                f"Ya está corriendo como {existing['outcomeName']} (iniciado hace {minutes_up} min por otra "
                "persona). Solo quien lo inició puede detenerlo o reiniciarlo — espera a que lo liberen o "
                "a que expire."
            )
        active_others = sum(
            1 for name, run in _RUNS.items() if run["status"] == "running" and name != stub_name
        )
        if not already_running and active_others >= MAX_CONCURRENT_STUBS:
            raise RuntimeError(
                f"Ya hay {MAX_CONCURRENT_STUBS} stubs activos (máximo). Detén uno antes de iniciar otro."
            )
        _stop_locked(stub_name)
        for port in ports:
            _kill_port_squatter(port)
            _kill_port_squatter(_ws_port_for(port))

    stub_dir = os.path.join(DATA_DIR, stub_name)
    os.makedirs(stub_dir, exist_ok=True)
    config_path = os.path.join(stub_dir, "current.yaml")
    bin_path = os.path.join(stub_dir, "stuber_bin")
    if yaml_text:
        with open(config_path, "w", encoding="utf-8") as f:
            f.write(yaml_text)
        run_args = ["-config", config_path]
    else:
        run_args = ["-config", STATIC_CONFIG, "-port", str(ports[0])]
        if outcome_name:
            run_args += ["-outcome", outcome_name]

    build = subprocess.run(
        ["go", "build", "-o", bin_path, "."], cwd=PROGRAM_DIR, capture_output=True, text=True
    )
    if build.returncode != 0:
        raise RuntimeError(f"go build falló: {build.stderr}")

    proc = subprocess.Popen(
        [bin_path, *run_args, "-http-port", UNUSED_HTTP_PORT],
        cwd=PROGRAM_DIR,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        encoding="utf-8",
        errors="replace",
        bufsize=1,
    )

    # Callers that can't open a raw TCP socket (a browser, the portal) go through
    # wsbridge instead - one instance per TCP port, each proxying WS/HTTP traffic
    # to that port. Built fresh alongside the stub itself so it can't go stale the
    # way the deleted-then-restored copy did.
    bridge_bin_path = os.path.join(stub_dir, "wsbridge_bin")
    bridge_build = subprocess.run(
        ["go", "build", "-o", bridge_bin_path, "./wsbridge"], cwd=PROGRAM_DIR, capture_output=True, text=True
    )
    if bridge_build.returncode != 0:
        proc.kill()
        raise RuntimeError(f"go build (wsbridge) falló: {bridge_build.stderr}")

    ws_ports = []
    bridge_procs = []
    try:
        for port in ports:
            ws_port = _ws_port_for(port)
            bproc = subprocess.Popen(
                [bridge_bin_path, "-ws-port", str(ws_port), "-tcp-host", "127.0.0.1", "-tcp-port", str(port)],
                cwd=PROGRAM_DIR,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                encoding="utf-8",
                errors="replace",
                bufsize=1,
            )
            bridge_procs.append(bproc)
            ws_ports.append(ws_port)
    except OSError as exc:
        proc.kill()
        for bproc in bridge_procs:
            bproc.kill()
        raise RuntimeError(f"No se pudo iniciar wsbridge: {exc}")

    now = time.time()
    with _LOCK:
        _LOGS[stub_name] = []
        _LOG_SEQ[stub_name] = 0
        run = _run_slot(stub_name)
        run["run_id"] += 1
        run_id = run["run_id"]
        timer = threading.Timer(ttl_minutes * 60, _expire, args=(stub_name, run_id))
        timer.daemon = True
        run.update(
            {
                "process": proc,
                "bridgeProcesses": bridge_procs,
                "wsPorts": ws_ports,
                "timer": timer,
                "status": "running",
                "ports": ports,
                "outcomeName": outcome_name,
                "pinnedCode": pinned_code,
                "startedAt": now,
                "expiresAt": now + ttl_minutes * 60,
                "ttlMinutes": ttl_minutes,
                "stoppedReason": None,
                "error": None,
                "clients": [],
                "ownerId": owner_id,
            }
        )
    timer.start()
    threading.Thread(target=_reader_thread, args=(stub_name, proc, run_id), daemon=True).start()
    for bproc in bridge_procs:
        threading.Thread(target=_bridge_reader_thread, args=(stub_name, bproc), daemon=True).start()
    return status()


def stop(stub_name, owner_id):
    with _LOCK:
        run = _RUNS.get(stub_name)
        if run and run["status"] == "running" and run.get("ownerId") != owner_id:
            raise PermissionError("Solo quien inició este stub puede detenerlo.")
        if run:
            run["stoppedReason"] = "manual"
        _stop_locked(stub_name)
    return status()


def status():
    with _LOCK:
        runs = {
            name: {k: v for k, v in run.items() if k not in ("process", "timer", "bridgeProcesses")}
            for name, run in _RUNS.items()
        }
        active_count = sum(1 for r in runs.values() if r["status"] == "running")
    return {
        "runs": runs,
        "maxConcurrent": MAX_CONCURRENT_STUBS,
        "activeCount": active_count,
        "host": ADVERTISE_HOST,
    }


def logs_since(stub_name, seq):
    with _LOCK:
        return [entry for entry in _LOGS.get(stub_name, []) if entry["seq"] > seq]


def send_test_request(stub_name, hex_payload, port=None, timeout=5):
    with _LOCK:
        run = _RUNS.get(stub_name)
        ports = run["ports"] if run else []
        running = run["status"] == "running" if run else False
    if not running or not ports:
        raise RuntimeError("El stub no está corriendo")
    if port is None:
        port = ports[0]
    elif port not in ports:
        raise RuntimeError(f"Este stub no escucha en el puerto {port} (usa uno de {ports})")
    data = bytes.fromhex(hex_payload)
    with socket.create_connection(("127.0.0.1", port), timeout=timeout) as sock:
        sock.sendall(data)
        sock.settimeout(timeout)
        response = sock.recv(4096)
    return response.hex()


class Handler(BaseHTTPRequestHandler):
    def _send_json(self, code, payload):
        body = json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _read_json(self):
        length = int(self.headers.get("Content-Length", 0) or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            return json.loads(raw or b"{}")
        except json.JSONDecodeError:
            return {}

    def log_message(self, format, *args):
        pass  # quiet — real traffic is already captured via _append_log

    def _write_chunk(self, data):
        # Plain "write bytes, leave the connection open" isn't valid framing
        # without a Content-Length — curl tolerates it, but Python's
        # http.client (what `requests` is built on) hangs waiting for
        # either a length or a chunk terminator that never comes. Real
        # HTTP/1.1 chunked encoding is what makes every client agree on
        # where one write ends and the next begins.
        self.wfile.write(f"{len(data):X}\r\n".encode("ascii"))
        self.wfile.write(data)
        self.wfile.write(b"\r\n")
        self.wfile.flush()

    def do_POST(self):
        path = urlparse(self.path).path
        try:
            if path == "/start":
                body = self._read_json()
                stub_name = body.get("stubName")
                if not stub_name:
                    return self._send_json(400, {"error": "Falta stubName"})
                # Accept either "ports" (a run can bundle several tcp_stubs
                # entries - e.g. Liverpool + Suburbia in one process/port
                # pair) or the older singular "port", for callers still on
                # the one-port-per-run shape.
                ports = body.get("ports")
                if not ports and body.get("port"):
                    ports = [body["port"]]
                if not ports:
                    return self._send_json(400, {"error": "Falta ports (o port)"})
                result = start(
                    stub_name,
                    body.get("yaml"),
                    ports,
                    body.get("outcomeName", ""),
                    body.get("pinnedCode"),
                    body.get("ttlMinutes"),
                    body.get("ownerId"),
                )
                return self._send_json(200, result)
            if path == "/stop":
                body = self._read_json()
                stub_name = body.get("stubName")
                if not stub_name:
                    return self._send_json(400, {"error": "Falta stubName"})
                return self._send_json(200, stop(stub_name, body.get("ownerId")))
            if path == "/test-request":
                body = self._read_json()
                stub_name = body.get("stubName")
                if not stub_name or not body.get("hex"):
                    return self._send_json(400, {"error": "Falta stubName o el request en hex"})
                response_hex = send_test_request(stub_name, body["hex"], port=body.get("port"))
                return self._send_json(200, {"responseHex": response_hex})
            return self._send_json(404, {"error": "not found"})
        except ValueError as e:
            return self._send_json(400, {"error": str(e)})
        except PermissionError as e:
            return self._send_json(403, {"error": str(e)})
        except RuntimeError as e:
            return self._send_json(409, {"error": str(e)})
        except Exception as e:
            return self._send_json(500, {"error": str(e)})

    def do_GET(self):
        parsed = urlparse(self.path)
        path = parsed.path
        qs = parse_qs(parsed.query)

        if path == "/status":
            return self._send_json(200, status())

        if path == "/stubs":
            # Raw text, unparsed - this agent is stdlib-only (no YAML parser); the caller parses.
            with open(os.path.join(PROGRAM_DIR, "STUBS.yaml"), encoding="utf-8") as f:
                return self._send_json(200, {"yaml": f.read()})

        if path == "/logs-stream":
            stub_name = (qs.get("stubName") or [None])[0]
            if not stub_name:
                return self._send_json(400, {"error": "Falta stubName"})
            self.protocol_version = "HTTP/1.1"
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            last_seq = 0
            try:
                while True:
                    for entry in logs_since(stub_name, last_seq):
                        last_seq = entry["seq"]
                        self._write_chunk(f"data: {json.dumps(entry)}\n\n".encode("utf-8"))
                    self._write_chunk(b": keep-alive\n\n")
                    time.sleep(1)
            except (BrokenPipeError, ConnectionResetError):
                return
            return
        return self._send_json(404, {"error": "not found"})


def main():
    global ADVERTISE_HOST
    parser = argparse.ArgumentParser(description="SPDH Sim Agent")
    parser.add_argument("--port", type=int, default=8090)
    parser.add_argument(
        "--host",
        default=None,
        help="IP/hostname to tell callers to point a real POS/B24 device at "
        "(default: auto-detect this machine's outbound IP). Set this "
        "explicitly if the machine is multi-homed or behind NAT and the "
        "auto-detected address isn't the one clients can actually reach.",
    )
    args = parser.parse_args()
    ADVERTISE_HOST = args.host or _detect_local_ip()
    server = ThreadingHTTPServer(("0.0.0.0", args.port), Handler)
    print(
        f"SPDH Sim Agent listening on :{args.port} (max {MAX_CONCURRENT_STUBS} concurrent stubs), "
        f"advertising host {ADVERTISE_HOST}"
    )
    server.serve_forever()


if __name__ == "__main__":
    main()
