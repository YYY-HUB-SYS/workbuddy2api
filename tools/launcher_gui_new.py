# -*- coding: utf-8 -*-
"""
WorkBuddy2API · 单文件 GUI 启动台
双击 exe 即用：不再依赖 bat / python / 浏览器入口。
功能：网关起停重启、端口占用检测、config.json 编辑、账号列表、日志 tail。
仅用 Python 标准库（tkinter），PyInstaller onefile 打包。
"""
import json
import os
import random
import socket
import string
import subprocess
import sys
import threading
import time
import tkinter as tk
from tkinter import messagebox, ttk

# ────────────────────────── 路径定位 ──────────────────────────

def app_dir():
    if getattr(sys, "frozen", False):
        return os.path.dirname(os.path.abspath(sys.executable))
    return os.path.dirname(os.path.abspath(__file__))


ROOT = app_dir()
CONFIG = os.path.join(ROOT, "config.json")
AUTHS = os.path.join(ROOT, "auths")
DATA = os.path.join(ROOT, "data")
LOGS = os.path.join(ROOT, "logs")
GW_LOG = os.path.join(LOGS, "gateway.log")
STATE = os.path.join(DATA, "panel_state.json")
CLIENT_MD = os.path.join(ROOT, "客户端配置.md")

CREATE_NEW_CONSOLE = 0x00000010
CREATE_NO_WINDOW = 0x08000000
DETACHED_PROCESS = 0x00000008

DEFAULT_CONFIG = {
    "listen": ":8080",
    "api_key": "",
    "auth_dir": "./auths",
    "state_file": "./data/state.json",
    "server": {"max_body_mb": 8},
    "cooldown": {"soft_rate": "600s", "soft_rate_max": "2h"},
    "schedule": {
        "checkin_hours": [9, 21], "travel_hours": [9, 21], "activity_hours": [10],
        "keepalive_hours": [22], "school_hours": [12], "cat_hours": [1],
        "checkin_enabled": True, "travel_enabled": True, "activity_enabled": True,
        "keepalive_enabled": True, "school_enabled": True, "cat_enabled": True,
    },
    "global": {"enabled": True, "chat_base": "", "billing_base": ""},
    "upstream": {
        "timeout_seconds": 120, "header_timeout_seconds": 120, "idle_timeout_seconds": 300,
        "user_agent": "", "client_version": "", "cli_version": "",
        "device_token": "", "device_token_file": "", "client_name": "WorkBuddy",
        "passthrough_ip": False,
    },
    "features": {"sanitize_blacklist_fingerprints": True},
    "prompt": {"mode": "passthrough", "file": ""},
    "upstash": {"url": "", "token": ""},
    "pool": {
        "max_in_flight": 3, "breaker_threshold": 3, "breaker_cooldown": "30m",
        "breaker_cooldown_max": "6h", "idle_weight_per_hour": 0.5,
        "idle_weight_max": 5.0, "expiring_soon": "168h",
    },
    "session_sticky": {"enabled": True, "ttl": "30m", "gc_interval": "5m"},
}


def find_exe():
    """定位 wb2api.exe：优先 exe 同目录，其次打包内置资源。"""
    for p in (os.path.join(ROOT, "wb2api.exe"),
              os.path.join(getattr(sys, "_MEIPASS", ""), "wb2api.exe")):
        if p and os.path.exists(p):
            return p
    return ""


# ────────────────────────── 系统操作 ──────────────────────────

def read_config():
    try:
        with open(CONFIG, encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return json.loads(json.dumps(DEFAULT_CONFIG))


def write_config(cfg):
    with open(CONFIG, "w", encoding="utf-8") as f:
        json.dump(cfg, f, ensure_ascii=False, indent=2)


def cfg_port(cfg=None):
    cfg = cfg or read_config()
    try:
        return int(str(cfg.get("listen", ":8080")).lstrip(":"))
    except Exception:
        return 8080


def _run(args, timeout=10):
    """bytes 模式执行，避免 text=True 的 readerthread 用 utf-8 解 GBK 输出导致 stdout 为 None。"""
    return subprocess.run(args, capture_output=True, creationflags=CREATE_NO_WINDOW,
                          timeout=timeout)


def pids_on_port(port):
    try:
        out = _run(["netstat", "-ano"]).stdout.decode("utf-8", "replace")
    except Exception:
        return []
    pids = []
    for line in out.splitlines():
        if ":%d " % port in line and "LISTENING" in line:
            p = line.split()[-1]
            if p.isdigit() and p not in pids:
                pids.append(p)
    return pids


def pid_alive(pid):
    if not pid:
        return False
    try:
        out = _run(["tasklist", "/fi", "PID eq %s" % pid, "/nh"]).stdout
        return str(pid).encode() in out
    except Exception:
        return False


def kill_pid(pid):
    try:
        return _run(["taskkill", "/f", "/pid", str(pid)]).returncode == 0
    except Exception:
        return False


def probe_health(port, timeout=2.5):
    try:
        import urllib.request
        req = urllib.request.Request("http://127.0.0.1:%d/healthz" % port)
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return True, r.read().decode("utf-8", "replace").strip()[:200]
    except Exception as e:
        return False, str(e)[:120]


def read_state():
    try:
        with open(STATE, encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return {}


def write_state(d):
    os.makedirs(DATA, exist_ok=True)
    with open(STATE, "w", encoding="utf-8") as f:
        json.dump(d, f, ensure_ascii=False)


def tail_log(n=60):
    try:
        with open(GW_LOG, "rb") as f:
            try:
                f.seek(0, 2)
                size = f.tell()
                f.seek(max(0, size - 60000))
            except Exception:
                pass
            data = f.read().decode("utf-8", "replace")
        return "\n".join(data.splitlines()[-n:])
    except Exception:
        return ""


def list_accounts():
    out = []
    try:
        names = sorted(os.listdir(AUTHS))
    except Exception:
        return out
    for fn in names:
        if not fn.lower().endswith(".json"):
            continue
        info = {"file": fn, "uid": "?", "nick": "", "realm": "", "exp": ""}
        try:
            with open(os.path.join(AUTHS, fn), encoding="utf-8") as f:
                d = json.load(f)
            info["uid"] = str(d.get("uid") or d.get("user_id") or "?")
            info["nick"] = str(d.get("nickname") or d.get("nick") or "")
            info["realm"] = str(d.get("realm") or d.get("domain") or "")
            ts = d.get("expires_at") or d.get("expired_at") or d.get("exp")
            if isinstance(ts, (int, float)) and ts > 1000000000:
                left = ts - time.time()
                info["exp"] = ("%.1f 小时" % (left / 3600)) if left > 0 else "已过期"
        except Exception:
            pass
        out.append(info)
    return out


def gen_key(n=24):
    return "".join(random.choices(string.ascii_letters + string.digits, k=n))


