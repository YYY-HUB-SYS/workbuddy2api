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


# ────────────────────────── GUI 常量 ──────────────────────────

BG = "#0e1014"
BG2 = "#151820"
BG3 = "#1e232e"
BG4 = "#0a0c10"
LINE = "#262b36"
TX = "#e8eaf0"
TX2 = "#98a0b3"
TX3 = "#5d6577"
BLUE = "#3d8bfd"
TEAL = "#2bd48c"
AMBER = "#f0a832"
RED = "#f0524f"
SELT = "#28486e"
FONT = ("Microsoft YaHei UI", 10)
FONTB = ("Microsoft YaHei UI", 11)
SMALL = ("Microsoft YaHei UI", 9)
MONO = ("Consolas", 10)
BEAT = 3.0


def card(parent, title=None, pad=16, grid_args=None):
    """自绘卡片：1px 外框 + 深色内衬 + 小标题。返回内容层（.outer 为外框）。"""
    outer = tk.Frame(parent, bg=LINE, highlightthickness=1,
                     highlightbackground=LINE, highlightcolor=LINE)
    if grid_args:
        outer.grid(**grid_args)
    inner = tk.Frame(outer, bg=BG2)
    inner.pack(fill="both", expand=True, padx=1, pady=1)
    if title:
        tk.Label(inner, text=title, bg=BG2, fg=TX2, font=SMALL).pack(
            anchor="w", padx=pad, pady=(11, 2))
    content = tk.Frame(inner, bg=BG2)
    content.pack(fill="both", expand=True, padx=pad, pady=(4, 12))
    content._pad = pad
    content.outer = outer
    return content


class FlatBtn(tk.Label):
    """平面按钮：无边框、hover 变色、支持禁用态。"""

    KIND = {"n": (BG3, TX, "#2a3140"),
            "p": (BLUE, "#ffffff", "#5a9dfe"),
            "d": (RED, "#ffffff", "#ff6b68")}

    def __init__(self, master, text, cmd, kind="n", padx=14):
        self.bg, self.fg, self.hv = self.KIND[kind]
        super().__init__(master, text=text, bg=self.bg, fg=self.fg, font=SMALL,
                         padx=padx, pady=6, cursor="hand2")
        self.cmd = cmd
        self.on = True
        self.bind("<Enter>", lambda e: self._hover())
        self.bind("<Leave>", lambda e: self.configure(bg=self.bg))
        self.bind("<Button-1>", lambda e: self._click())

    def set_kind(self, kind):
        self.bg, self.fg, self.hv = self.KIND[kind]
        self.configure(bg=self.bg, fg=self.fg)

    def _hover(self):
        if self.on:
            self.configure(bg=self.hv)

    def _click(self):
        if self.on and self.cmd:
            self.cmd()

    def set_enabled(self, on):
        self.on = on
        self.configure(bg=self.bg if on else BG2,
                       fg=self.fg if on else TX3,
                       cursor="hand2" if on else "arrow")


def flat_btn(parent, text, cmd, kind="n", padx=14):
    return FlatBtn(parent, text, cmd, kind, padx)


# ── 移植自 addaccount.py / list_models.py（不依赖外部脚本，便于单文件打包） ──

REALMS = {
    "cn": {"base": "https://copilot.tencent.com", "origin": "https://www.codebuddy.cn"},
    "global": {"base": "https://www.workbuddy.ai", "origin": "https://www.workbuddy.ai"},
}
_UA = "CLI/2.63.2 CodeBuddy/2.63.2"


def http_json(url, method="GET", body=None, headers=None, timeout=30):
    data = None
    if body is not None:
        data = json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method=method)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        raw = r.read().decode("utf-8", "replace")
    try:
        env = json.loads(raw)
    except Exception:
        raise RuntimeError("响应不是合法 JSON: %s" % raw[:200])
    if env.get("code") != 0:
        raise RuntimeError("code=%s msg=%s" % (env.get("code"), env.get("msg")))
    return env.get("data") or {}


def base_headers(origin, token=None):
    h = {
        "Content-Type": "application/json",
        "Accept": "application/json, text/plain, */*",
        "X-Requested-With": "XMLHttpRequest",
        "Origin": origin,
        "Referer": origin + "/",
        "User-Agent": _UA,
    }
    if token:
        h["Authorization"] = "Bearer " + token
    return h


def daily_checkin(token, uid, ent_id, domain):
    """国内版每日签到（幂等，失败不影响登录）。"""
    req = urllib.request.Request(
        "https://www.codebuddy.cn/v2/billing/meter/daily-checkin",
        data=b"{}", method="POST",
        headers={
            "Authorization": "Bearer " + token,
            "Accept": "application/json",
            "Content-Type": "application/json",
            "X-User-Id": uid,
            **({"X-Enterprise-Id": ent_id, "X-Tenant-Id": ent_id} if ent_id else {}),
            **({"X-Domain": domain} if domain else {}),
        })
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            b = json.loads(r.read().decode() or "{}")
        return b.get("msg") or ("成功 %s" % json.dumps(b.get("data") or {}, ensure_ascii=False)[:100])
    except urllib.error.HTTPError as e:
        try:
            return json.loads(e.read().decode() or "{}").get("msg", "http %d" % e.code)
        except Exception:
            return "http %d" % e.code
    except Exception as e:
        return str(e)


def fetch_models(base, key="", timeout=20):
    """GET /v1/models，返回模型列表（dict）。失败抛异常。"""
    req = urllib.request.Request(
        base.rstrip("/") + "/v1/models",
        headers={"Authorization": "Bearer " + key, "Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode("utf-8", "replace")).get("data") or []


# ── 后台采集：全部阻塞 IO 都在这里，主线程永远不碰 ──

def collect():
    port = cfg_port()
    st = read_state()
    pids = pids_on_port(port)
    ok, text = probe_health(port, timeout=1.5)
    pid = pids[0] if pids else (st.get("pid") if pid_alive(st.get("pid")) else None)
    return {"pids": pids, "pid": pid, "port": port, "running": bool(pids),
            "health": ok, "health_text": text, "started_at": st.get("started_at", ""),
            "accounts": list_accounts(), "log": tail_log()}


class App:
    def __init__(self, root):
        self.root = root
        root.title("WorkBuddy2API · 启动台")
        root.geometry("960x680")
        root.minsize(860, 580)
        root.configure(bg=BG)
        root.protocol("WM_DELETE_WINDOW", self.on_close)
        self.proc = None
        self.busy = False
        self.alive = True
        self.s = {"pids": [], "pid": None, "port": cfg_port(), "running": False,
                  "health": False, "health_text": "", "started_at": "",
                  "accounts": [], "log": ""}
        self.lhash = 0
        self._build_style()
        self._build()
        self.refresh_async(first=True)
        self._beat()

    # ── 后台心跳 + 异步任务 ──

    def _beat(self):
        def loop():
            while self.alive:
                try:
                    s = collect()
                    if self.alive:
                        self.root.after(0, lambda s=s: self._apply(s))
                except Exception:
                    pass
                time.sleep(BEAT)
        threading.Thread(target=loop, daemon=True).start()

    def refresh_async(self, first=False):
        """状态刷新：只提交后台任务，主线程立即返回。"""
        def job():
            s = collect()
            if self.alive:
                self.root.after(0, lambda: self._apply(s))
        threading.Thread(target=job, daemon=True).start()
        if first:
            self.stat.configure(text="检测中…", fg=TX2)

    def _main_btn(self, text, kind):
        if self.b_main.cget("text") != text:
            self.b_main.set_kind(kind)
            self.b_main.configure(text=text)

    def _apply(self, s):
        """主线程渲染（毫秒级，无 IO）。"""
        try:
            self.s = s
            if self.busy:
                self.dot.configure(fg=AMBER)
                self.b_main.configure(text="处理中…")
            elif s["running"]:
                self.dot.configure(fg=TEAL)
                self.stat.configure(
                    text="API 服务运行中 · PID %s · 端口 %d" % (s["pid"] or "?", s["port"]), fg=TX)
                self.health.configure(
                    text="healthz 正常" if s["health"] else "healthz 异常",
                    fg=TEAL if s["health"] else AMBER)
                self._main_btn("■ 停止服务", "d")
            else:
                self.dot.configure(fg=RED)
                self.stat.configure(text="API 服务未运行 · 端口 %d" % s["port"], fg=TX2)
                self.health.configure(text="")
                self._main_btn("▶ 一键启动 API 服务", "p")

            if not find_exe():
                self.warn.configure(text="⚠ 未找到 wb2api.exe，请把本程序放到项目根目录")
            else:
                self.warn.configure(text="")

            for i, a in enumerate(s["accounts"]):
                pass
            self._render_accounts(s["accounts"])
            h = hash(s["log"])
            if h != self.lhash:
                self.lhash = h
                self.log.configure(state="normal")
                self.log.delete("1.0", "end")
                self.log.insert("end", s["log"] or "（暂无网关日志）")
                self.log.configure(state="disabled")
                self.log.see("end")
        except tk.TclError:
            pass

    def _render_accounts(self, accounts):
        sel = self.tree.selection()
        keep = self.tree.item(sel[0], "values")[0] if sel else None
        self.tree.delete(*self.tree.get_children())
        for i, a in enumerate(accounts):
            iid = self.tree.insert("", "end",
                                   values=(a["file"], a["uid"], a["realm"], a["exp"]),
                                   tags=("even" if i % 2 else "odd",))
            if keep and a["file"] == keep:
                self.tree.selection_set(iid)

    # ── 布局 ──

    def _build_style(self):
        st = ttk.Style()
        try:
            st.theme_use("clam")
        except Exception:
            pass
        st.configure(".", background=BG, foreground=TX, font=FONT, borderwidth=0)
        st.configure("TFrame", background=BG)
        st.configure("C.TFrame", background=BG2)
        st.configure("TLabel", background=BG, foreground=TX)
        st.configure("C.TLabel", background=BG2, foreground=TX)
        st.configure("Dim.TLabel", background=BG2, foreground=TX2)
        st.configure("TEntry", fieldbackground=BG3, foreground=TX, insertcolor=TX,
                     borderwidth=0, lightcolor=BG3, darkcolor=BG3, padding=(9, 6))
        st.map("TEntry", lightcolor=[("focus", BLUE)], darkcolor=[("focus", BLUE)])
        st.configure("TNotebook", background=BG, borderwidth=0, tabmargins=(0, 0, 0, 0))
        st.configure("TNotebook.Tab", background=BG, foreground=TX3,
                     padding=(2, 6), borderwidth=0, font=SMALL)
        st.map("TNotebook.Tab",
               background=[("selected", BG)], foreground=[("selected", TX)])
        st.configure("Treeview", background=BG2, fieldbackground=BG2,
                     foreground=TX, borderwidth=0, rowheight=30, font=FONT)
        st.map("Treeview",
               background=[("selected", SELT)],
               foreground=[("selected", TX)])
        st.configure("Treeview.Heading", background=BG2, foreground=TX3,
                     borderwidth=0, relief="flat", padding=(4, 6), font=SMALL)
        st.map("Treeview.Heading", background=[("active", BG2)])
        st.configure("Vertical.TScrollbar", background=BG2, troughcolor=BG2,
                     borderwidth=0, arrowcolor=TX3, relief="flat")
        st.configure("TCheckbutton", background=BG2, foreground=TX, focuscolor=BG2)
        st.map("TCheckbutton",
               indicatorcolor=[("selected", BLUE), ("!selected", BG3)],
               background=[("active", BG2)])

    def _build(self):
        head = tk.Frame(self.root, bg=BG, padx=20, pady=14)
        head.pack(fill="x")
        self.dot = tk.Label(head, text="●", bg=BG, fg=TX3, font=(FONT[0], 12))
        self.dot.pack(side="left")
        tk.Label(head, text="WorkBuddy2API", bg=BG, fg=TX, font=FONTB).pack(
            side="left", padx=(10, 0))
        self.stat = tk.Label(head, text="检测中…", bg=BG, fg=TX2, font=FONT)
        self.stat.pack(side="left", padx=(12, 0))
        self.health = tk.Label(head, text="", bg=BG, fg=TX3, font=SMALL)
        self.health.pack(side="left", padx=(12, 0))
        btns = tk.Frame(head, bg=BG)
        btns.pack(side="right")
        flat_btn(btns, "复制接入信息", self.copy_client).pack(side="left", padx=4)
        flat_btn(btns, "日志目录", self.open_logs).pack(side="left", padx=4)

        bar = tk.Frame(self.root, bg=BG, padx=20)
        bar.pack(fill="x", pady=(0, 14))
        self.b_main = flat_btn(bar, "▶ 一键启动 API 服务", self.toggle, "p", padx=24)
        self.b_main.configure(font=FONTB, pady=9)
        self.b_main.pack(side="left")
        self.b_restart = flat_btn(bar, "重启", self.restart)
        self.b_restart.pack(side="left", padx=8)
        self.b_refresh = flat_btn(bar, "刷新状态", self.refresh_async)
        self.b_refresh.pack(side="left")
        self.warn = tk.Label(bar, text="", bg=BG, fg=AMBER, font=SMALL)
        self.warn.pack(side="left", padx=16)

        body = tk.Frame(self.root, bg=BG, padx=20)
        body.pack(fill="both", expand=True)
        body.columnconfigure(0, weight=1, uniform="a")
        body.columnconfigure(1, weight=1, uniform="a")
        body.rowconfigure(0, weight=1)

        self._build_config(card(body, "网关配置",
                                grid_args=dict(row=0, column=0, sticky="nsew", padx=(0, 8))))
        self._build_accounts(card(body, "账号池（auths/）",
                                  grid_args=dict(row=0, column=1, sticky="nsew", padx=(8, 0))))

        nb = ttk.Notebook(self.root)
        nb.pack(fill="both", expand=True, padx=20, pady=(12, 0))
        tab_add = tk.Frame(nb, bg=BG)
        nb.add(tab_add, text="  添加账号  ")
        self._build_add_account(tab_add)
        tab_models = tk.Frame(nb, bg=BG)
        nb.add(tab_models, text="  模型列表  ")
        self._build_models(tab_models)
        tab_log = tk.Frame(nb, bg=BG)
        nb.add(tab_log, text="  网关日志  ")
        logw = tk.Frame(tab_log, bg=LINE, highlightthickness=1, highlightbackground=LINE)
        logw.pack(fill="both", expand=True, padx=12, pady=12)
        self.log = tk.Text(logw, height=8, bg=BG4, fg="#b6bfd2", font=MONO,
                           relief="flat", wrap="none", state="disabled",
                           padx=12, pady=10, insertbackground=TX,
                           selectbackground=SELT)
        sb = ttk.Scrollbar(logw, command=self.log.yview, style="Vertical.TScrollbar")
        sb.pack(side="right", fill="y")
        self.log.pack(fill="both", expand=True, padx=1, pady=1)
        self.log.configure(yscrollcommand=sb.set)

    def _row(self, parent, r):
        f = tk.Frame(parent, bg=BG2)
        f.grid(row=r, column=0, columnspan=3, sticky="ew", padx=16, pady=5)
        f.columnconfigure(1, weight=1)
        return f

    def _build_config(self, c):
        r0 = self._row(c, 0)
        tk.Label(r0, text="监听端口", bg=BG2, fg=TX2, font=SMALL).grid(row=0, column=0, sticky="w")
        self.port = ttk.Entry(r0, width=10, font=MONO)
        self.port.insert(0, str(cfg_port()))
        self.port.grid(row=0, column=1, sticky="w", padx=10)
        flat_btn(r0, "检测占用", self.check_port).grid(row=0, column=2, sticky="e")

        r1 = self._row(c, 1)
        tk.Label(r1, text="API 密钥", bg=BG2, fg=TX2, font=SMALL).grid(row=0, column=0, sticky="w")
        self.key = ttk.Entry(r1, width=10, font=MONO)
        self.key.insert(0, str(read_config().get("api_key") or ""))
        self.key.grid(row=0, column=1, sticky="we", padx=10)
        flat_btn(r1, "随机生成", self.rand_key).grid(row=0, column=2, sticky="e")

        r2 = self._row(c, 2)
        tk.Label(r2, text="局域网地址", bg=BG2, fg=TX2, font=SMALL).grid(row=0, column=0, sticky="w")
        self.lan = tk.Label(r2, text=self._lan(), bg=BG2, fg=BLUE, font=MONO)
        self.lan.grid(row=0, column=1, sticky="w", padx=10)
        flat_btn(r2, "复制地址", self.copy_base).grid(row=0, column=2, sticky="e")

        r3 = tk.Frame(c, bg=BG2)
        r3.grid(row=3, column=0, columnspan=3, sticky="ew", padx=16, pady=(8, 4))
        self.autostart = tk.BooleanVar(value=self._startup_on())
        ttk.Checkbutton(r3, text="开机自启（写当前用户启动文件夹）", variable=self.autostart,
                        style="TCheckbutton", command=self.toggle_startup).pack(anchor="w")

        r4 = tk.Frame(c, bg=BG2)
        r4.grid(row=4, column=0, columnspan=3, sticky="ew", padx=16, pady=(10, 12))
        self.b_save = flat_btn(r4, "保存并重启网关", self.save_config, "p")
        self.b_save.pack(side="left")
        flat_btn(r4, "复制客户端配置", self.copy_client).pack(side="left", padx=8)
        self.cfgmsg = tk.Label(r4, text="", bg=BG2, fg=TEAL, font=SMALL)
        self.cfgmsg.pack(side="left", padx=10)

    def _build_accounts(self, c):
        wrap = tk.Frame(c, bg=BG2)
        wrap.pack(fill="both", expand=True, padx=14, pady=(6, 4))
        cols = ("file", "uid", "realm", "exp")
        self.tree = ttk.Treeview(wrap, columns=cols, show="headings", height=5)
        for col, t, w, a in (("file", "凭证文件", 150, "w"), ("uid", "UID", 118, "w"),
                             ("realm", "域", 55, "w"), ("exp", "Token 剩余", 90, "e")):
            self.tree.heading(col, text=t)
            self.tree.column(col, width=w, anchor=a, stretch=(col == "file"))
        vsb = ttk.Scrollbar(wrap, orient="vertical", command=self.tree.yview,
                            style="Vertical.TScrollbar")
        self.tree.configure(yscrollcommand=vsb.set)
        self.tree.pack(side="left", fill="both", expand=True)
        vsb.pack(side="right", fill="y")
        self.tree.tag_configure("odd", background=BG2)
        self.tree.tag_configure("even", background="#191d26")
        rest = 118 + 55 + 90 + 6
        wrap.bind("<Configure>", lambda e: self.tree.column(
            "file", width=max(120, e.width - rest)))

        bf = tk.Frame(c, bg=BG2)
        bf.pack(fill="x", padx=14, pady=(8, 2))
        flat_btn(bf, "刷新", self.refresh_async).pack(side="left")
        flat_btn(bf, "打开目录", lambda: self._open(AUTHS)).pack(side="left", padx=8)
        flat_btn(bf, "删除选中", self.del_account, "d").pack(side="left")
        tk.Label(c, text="删除或新增凭证后必须重启网关才生效（网关只在启动时读 auths/）",
                 bg=BG2, fg=TX3, font=SMALL).pack(anchor="w", padx=14, pady=(8, 12))

    # ── 工具（无阻塞，主线程安全） ──

    def _lan(self):
        try:
            s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            s.connect(("8.8.8.8", 80))
            ip = s.getsockname()[0]
            s.close()
            return ip
        except Exception:
            return "127.0.0.1"

    def _startup_on(self):
        return os.path.exists(os.path.join(
            os.environ.get("APPDATA", ""),
            "Microsoft", "Windows", "Start Menu", "Programs", "Startup",
            "WorkBuddy2API-Autostart.vbs"))

    def _open(self, path):
        try:
            os.makedirs(path, exist_ok=True)
        except Exception:
            pass
        subprocess.Popen(["explorer", os.path.normpath(path)])

    def open_logs(self):
        self._open(LOGS)

    def toast(self, msg, ok=True):
        self.cfgmsg.configure(text=msg, fg=TEAL if ok else AMBER)
        self.root.after(4000, lambda: self.cfgmsg.configure(text=""))

    def copy_base(self):
        url = "http://%s:%d/v1" % (self.lan.cget("text"), cfg_port())
        self.root.clipboard_clear()
        self.root.clipboard_append(url)
        self.toast("已复制 " + url)

    def copy_client(self):
        cfg = read_config()
        port = cfg_port(cfg)
        lines = [
            "# 客户端接入", "",
            "- 网关地址: http://127.0.0.1:%d/v1" % port,
            "- 局域网地址: http://%s:%d/v1" % (self.lan.cget("text"), port),
            "- API 密钥: %s" % (cfg.get("api_key") or "(未设置，不鉴权)"),
            "- 探活地址: http://127.0.0.1:%d/healthz" % port,
            "- 日志文件: %s" % GW_LOG, "",
            "Trae / Cline / Continue 请填局域网地址，勿填 127.0.0.1。",
        ]
        self.root.clipboard_clear()
        self.root.clipboard_append("\n".join(lines))
        self.toast("客户端接入信息已复制到剪贴板")

    def rand_key(self):
        self.key.delete(0, "end")
        self.key.insert(0, gen_key())

    def on_close(self):
        self.alive = False
        try:
            self.root.destroy()
        except Exception:
            pass

    # ── 异步动作：主线程只做确认与置灰，实际活儿全在线程 ──

    def _lock(self, on, text=None):
        self.busy = on
        for b in (self.b_main, self.b_restart, self.b_refresh, self.b_save):
            b.set_enabled(not on)
        if text:
            self.stat.configure(text=text, fg=AMBER)
            self.dot.configure(fg=AMBER)

    def check_port(self):
        try:
            port = int(self.port.get())
        except Exception:
            self.toast("端口不合法", False)
            return

        def job():
            pids = pids_on_port(port)
            self.root.after(0, lambda: self._check_done(port, pids))
        threading.Thread(target=job, daemon=True).start()
        self.toast("检测中…")

    def _check_done(self, port, pids):
        if not pids:
            self.toast("端口 %d 空闲" % port)
            return
        if messagebox.askyesno("端口占用", "端口 %d 被进程 %s 占用。\n强制结束这些进程？"
                               % (port, ", ".join(pids))):
            def job():
                for p in pids:
                    kill_pid(p)
                self.root.after(0, lambda: self._after_kill(pids))
            threading.Thread(target=job, daemon=True).start()

    def _after_kill(self, pids):
        self.toast("已结束 %s" % ", ".join(pids))
        self.refresh_async()

    def save_config(self):
        cfg = read_config()
        try:
            port = int(self.port.get())
            if not (1 <= port <= 65535):
                raise ValueError
        except Exception:
            self.toast("端口不合法", False)
            return
        key = self.key.get().strip()
        if key and len(key) < 8:
            self.toast("密钥太短（至少 8 位）", False)
            return
        cfg["listen"] = ":%d" % port
        cfg["api_key"] = key
        write_config(cfg)
        self.toast("已写入 config.json，正在重启网关…")
        self.restart()

    def toggle_startup(self):
        vbs = os.path.join(os.environ.get("APPDATA", ""), "Microsoft", "Windows",
                           "Start Menu", "Programs", "Startup",
                           "WorkBuddy2API-Autostart.vbs")
        if self.autostart.get():
            exe = find_exe()
            if not exe:
                self.autostart.set(False)
                messagebox.showerror("错误", "找不到 wb2api.exe，无法设置开机自启")
                return
            body = ('Set ws = CreateObject("WScript.Shell")\r\n'
                    'ws.CurrentDirectory = "%s"\r\n'
                    'ws.Run """%s"" -config ""%s""", 0, False\r\n'
                    % (ROOT, exe, CONFIG))
            try:
                with open(vbs, "w", encoding="mbcs") as f:
                    f.write(body)
                self.toast("开机自启已开启")
            except Exception as e:
                self.autostart.set(False)
                self.toast("写入失败: %s" % e, False)
        else:
            try:
                if os.path.exists(vbs):
                    os.remove(vbs)
                self.toast("开机自启已关闭")
            except Exception as e:
                self.autostart.set(True)
                self.toast("删除失败: %s" % e, False)

    def toggle(self):
        """一键：运行中就停，未运行就起。"""
        if self.busy:
            return
        if self.s.get("running"):
            self.stop()
        else:
            self.start()

    def start(self):
        if self.busy:
            return
        if not find_exe():
            messagebox.showerror("错误", "找不到 wb2api.exe。\n请把本 exe 放在项目根目录（与 wb2api.exe 同级）。")
            return
        occ = self.s.get("pids") or []
        if occ:
            if not messagebox.askyesno("端口占用", "端口 %d 被进程 %s 占用，将强制结束后再启动。继续？"
                                       % (self.s["port"], ", ".join(occ))):
                return
        self._lock(True, "启动中…")
        threading.Thread(target=self._start_job, daemon=True).start()

    def _start_job(self):
        cfg = read_config()
        port = cfg_port(cfg)
        try:
            st = read_state()
            old = st.get("pid")
            if old and pid_alive(old) and not pids_on_port(port):
                kill_pid(old)
            for p in pids_on_port(port):
                kill_pid(p)
            time.sleep(0.6)
            for d in (AUTHS, DATA, LOGS):
                os.makedirs(d, exist_ok=True)
            if not os.path.exists(CONFIG):
                write_config(cfg)
            flags = CREATE_NO_WINDOW | DETACHED_PROCESS
            f = open(GW_LOG, "ab")
            try:
                self.proc = subprocess.Popen([find_exe(), "-config", CONFIG], cwd=ROOT,
                                             stdout=f, stderr=subprocess.STDOUT,
                                             stdin=subprocess.DEVNULL, creationflags=flags)
            finally:
                f.close()
            write_state({"pid": self.proc.pid, "mode": "hidden",
                         "started_at": time.strftime("%Y-%m-%d %H:%M:%S"), "port": port})
            ok = False
            for _ in range(10):
                time.sleep(0.5)
                ok, text = probe_health(port, timeout=1.2)
                if ok:
                    break
            pid, msg, good = self.proc.pid, "启动成功", ok
        except Exception as e:
            pid, msg, good = None, "启动失败: %s" % e, False
        if self.alive:
            self.root.after(0, lambda: self._start_done(pid, msg, good))

    def _start_done(self, pid, msg, good):
        self._lock(False)
        self.toast(msg, good)
        if not good:
            messagebox.showwarning("启动", msg)
        self.refresh_async()
        self.model_refresh(silent=True)

    def stop(self):
        if self.busy:
            return
        self._lock(True, "停止中…")
        threading.Thread(target=self._stop_job, daemon=True).start()

    def _stop_job(self):
        port = cfg_port()
        st = read_state()
        killed = []
        try:
            for p in pids_on_port(port):
                if kill_pid(p):
                    killed.append(p)
            if st.get("pid") and str(st["pid"]) not in killed and pid_alive(st.get("pid")):
                if kill_pid(st["pid"]):
                    killed.append(str(st["pid"]))
            write_state({})
        except Exception:
            pass
        if self.alive:
            self.root.after(0, lambda: self._stop_done(killed))

    def _stop_done(self, killed):
        self._lock(False)
        self.toast("已停止（%s）" % (", ".join(killed) or "无运行中进程"))
        self.refresh_async()

    def restart(self):
        if self.busy:
            return
        self._lock(True, "重启中…")
        threading.Thread(target=self._restart_job, daemon=True).start()

    def _restart_job(self):
        self._stop_job()
        time.sleep(0.8)
        self._start_job()

    def del_account(self):
        sel = self.tree.selection()
        if not sel:
            return
        fn = self.tree.item(sel[0], "values")[0]
        if not messagebox.askyesno("删除凭证", "删除 auths/%s ？\n删除后需重启网关生效。" % fn):
            return
        try:
            os.remove(os.path.join(AUTHS, fn))
            self.refresh_async()
            self.toast("已删除 %s，请重启网关" % fn)
        except Exception as e:
            self.toast("删除失败: %s" % e, False)

    # ── 添加账号页 ──

    def _build_add_account(self, tab):
        f = tk.Frame(tab, bg=BG, padx=20, pady=14)
        f.pack(fill="both", expand=True)

        r = tk.Frame(f, bg=BG)
        r.pack(fill="x", pady=(0, 8))
        tk.Label(r, text="选择版本", bg=BG, fg=TX2, font=SMALL).pack(side="left")
        self.realm_var = tk.StringVar(value="cn")
        for txt, val in (("国内版 cn", "cn"), ("国际版 global", "global")):
            tk.Radiobutton(r, text=txt, variable=self.realm_var, value=val,
                           bg=BG, fg=TX, selectcolor=BG3, activebackground=BG,
                           activeforeground=TX, font=SMALL, borderwidth=0).pack(side="left", padx=(14, 0))

        b = tk.Frame(f, bg=BG)
        b.pack(fill="x", pady=(0, 8))
        self.b_acct_begin = flat_btn(b, "🔗 获取登录链接", self.acct_begin, "p", padx=16)
        self.b_acct_begin.pack(side="left")
        self.b_acct_open = flat_btn(b, "浏览器打开", self.acct_open)
        self.b_acct_open.pack(side="left", padx=8)
        self.b_acct_done = flat_btn(b, "✅ 我已完成登录", self.acct_done, "p", padx=16)
        self.b_acct_done.pack(side="left", padx=8)
        self.b_acct_done.set_enabled(False)

        lf = tk.Frame(f, bg=BG)
        lf.pack(fill="x", pady=(0, 8))
        self.acct_url = tk.StringVar()
        tk.Entry(lf, textvariable=self.acct_url, bg=BG3, fg=TX, insertbackground=TX,
                 relief="flat", font=MONO, state="readonly").pack(fill="x")

        lw = tk.Frame(f, bg=LINE, highlightthickness=1, highlightbackground=LINE)
        lw.pack(fill="both", expand=True, pady=(4, 0))
        self.acct_log = tk.Text(lw, bg=BG4, fg="#9fd0a0", font=MONO, relief="flat",
                                wrap="word", state="disabled", padx=10, pady=8)
        self.acct_log.pack(fill="both", expand=True, padx=1, pady=1)

        self._acct_state = None
        self.acct_busy = False

    def _acct_log(self, msg, color="#9fd0a0"):
        try:
            self.acct_log.configure(state="normal")
            self.acct_log.insert("end", msg + "\n")
            self.acct_log.configure(state="disabled")
            self.acct_log.see("end")
        except tk.TclError:
            pass

    def acct_begin(self):
        if self.acct_busy:
            return
        self.acct_busy = True
        self.b_acct_begin.set_enabled(False)
        self._acct_log("申请授权码…")
        realm = self.realm_var.get()

        def job():
            try:
                st = http_json(REALMS[realm]["base"] + "/v2/plugin/auth/state?platform=CLI",
                               method="POST", body={},
                               headers=base_headers(REALMS[realm]["origin"]))
                state, url = st.get("state"), st.get("authUrl")
            except Exception as e:
                self.root.after(0, lambda: self._acct_err("获取失败: %s" % e))
                return
            self.root.after(0, lambda: self._acct_got(realm, state, url))
        threading.Thread(target=job, daemon=True).start()

    def _acct_got(self, realm, state, url):
        self._acct_state = state
        self.acct_url.set(url)
        self.b_acct_open.set_enabled(True)
        self.b_acct_done.set_enabled(True)
        self.acct_busy = False
        self._acct_log("已拿到授权链接，请在浏览器完成登录。", "#9fd0a0")

    def _acct_err(self, msg):
        self._acct_log(msg, "#f0524f")
        self.acct_busy = False
        self.b_acct_begin.set_enabled(True)
        self.b_acct_done.set_enabled(False)

    def acct_open(self):
        u = self.acct_url.get()
        if not u:
            return
        try:
            os.startfile(u)
        except Exception:
            subprocess.Popen(["cmd", "/c", "start", "", u], creationflags=CREATE_NO_WINDOW)

    def acct_done(self):
        if not self._acct_state:
            return
        self.acct_busy = True
        self.b_acct_done.set_enabled(False)
        self._acct_log("用 state 换取 token…")
        realm = self.realm_var.get()
        state = self._acct_state

        def job():
            try:
                tok = http_json(REALMS[realm]["base"] + "/v2/plugin/auth/token?state=" + state,
                                headers=base_headers(REALMS[realm]["origin"]))
            except Exception as e:
                self.root.after(0, lambda: self._acct_err(
                    "换取 token 失败：%s（请先在浏览器完成登录再点完成）" % e))
                return
            access = tok.get("accessToken") or ""
            if not access:
                self.root.after(0, lambda: self._acct_err("未拿到 accessToken"))
                return
            try:
                acct = http_json(REALMS[realm]["base"] + "/v2/plugin/login/account?state=" + state,
                                 headers=base_headers(REALMS[realm]["origin"], token=access))
            except Exception:
                acct = {}
            uid = acct.get("uid") or ""
            if not uid:
                self.root.after(0, lambda: self._acct_err("无法获取 uid"))
                return
            os.makedirs(AUTHS, exist_ok=True)
            payload = {
                "account": {"uid": uid, "enterpriseId": acct.get("enterpriseId", ""),
                            "nickname": acct.get("nickname", "")},
                "auth": {"accessToken": access, "refreshToken": tok.get("refreshToken", ""),
                         "expiresAt": int(time.time()) + int(tok.get("expiresIn") or 0),
                         "domain": tok.get("domain", ""), "realm": realm},
            }
            fn = "workbuddy-%s.json" % uid
            with open(os.path.join(AUTHS, fn), "w", encoding="utf-8") as fp:
                json.dump(payload, fp, ensure_ascii=False, indent=1)
            checkin = ""
            if realm == "cn":
                try:
                    checkin = daily_checkin(access, uid, acct.get("enterpriseId", ""),
                                            tok.get("domain", ""))
                except Exception as e:
                    checkin = str(e)
            self.root.after(0, lambda: self._acct_saved(fn, uid,
                                                        acct.get("nickname", ""), checkin, realm))
        threading.Thread(target=job, daemon=True).start()

    def _acct_saved(self, fn, uid, nick, checkin, realm):
        self._acct_log("已保存 %s （UID %s，昵称 %s）" % (fn, uid, nick or "-"), "#9fd0a0")
        if realm == "cn" and checkin:
            self._acct_log("每日签到: %s" % checkin)
        self._acct_log("正在自动重启网关使账号生效…", "#9fd0a0")
        self.acct_busy = False
        self.b_acct_begin.set_enabled(True)
        self.b_acct_done.set_enabled(False)
        self.refresh_async()
        if not self.busy:
            self.restart()

    # ── 模型列表页 ──

    def _build_models(self, tab):
        f = tk.Frame(tab, bg=BG, padx=20, pady=14)
        f.pack(fill="both", expand=True)
        b = tk.Frame(f, bg=BG)
        b.pack(fill="x", pady=(0, 8))
        flat_btn(b, "刷新模型清单", self.model_refresh, "p", padx=16).pack(side="left")
        flat_btn(b, "复制选中 ID", self.model_copy_id).pack(side="left", padx=8)
        tk.Label(b, text="需网关运行中且已加号", bg=BG, fg=TX3, font=SMALL).pack(side="left", padx=12)

        wrap = tk.Frame(f, bg=BG2)
        wrap.pack(fill="both", expand=True)
        cols = ("id", "name", "credits", "desc")
        self.mtree = ttk.Treeview(wrap, columns=cols, show="headings", height=12)
        for c, t, w, a in (("id", "模型 ID", 260, "w"), ("name", "展示名", 170, "w"),
                           ("credits", "倍率", 70, "e"), ("desc", "说明", 300, "w")):
            self.mtree.heading(c, text=t)
            self.mtree.column(c, width=w, anchor=a)
        vsb = ttk.Scrollbar(wrap, orient="vertical", command=self.mtree.yview,
                            style="Vertical.TScrollbar")
        self.mtree.configure(yscrollcommand=vsb.set)
        self.mtree.pack(side="left", fill="both", expand=True)
        vsb.pack(side="right", fill="y")
        self.mtree.tag_configure("odd", background=BG2)
        self.mtree.tag_configure("even", background="#191d26")
        self.mtree.bind("<Double-1>", lambda e: self.model_copy_id())
        rest = 170 + 70 + 300 + 6
        wrap.bind("<Configure>", lambda e: self.mtree.column(
            "id", width=max(160, e.width - rest)))

    def model_refresh(self, silent=False):
        def job():
            port = cfg_port()
            cfg = read_config()
            key = cfg.get("api_key") or ""
            try:
                models = fetch_models("http://127.0.0.1:%d" % port, key, timeout=20)
            except Exception as e:
                if not silent:
                    self.root.after(0, lambda: self.toast("获取模型失败: %s" % e, False))
                return
            self.root.after(0, lambda: self._model_fill(models, silent))
        threading.Thread(target=job, daemon=True).start()

    def _model_fill(self, models, silent):
        try:
            self.mtree.delete(*self.mtree.get_children())
            for i, m in enumerate(models):
                self.mtree.insert("", "end",
                                  values=(m.get("id", ""), m.get("name", ""),
                                          m.get("credits") or "-",
                                          (m.get("description") or "").replace("|", "/")),
                                  tags=("even" if i % 2 else "odd",))
            if not silent:
                self.toast("已更新 %d 个模型" % len(models))
        except tk.TclError:
            pass

    def model_copy_id(self):
        sel = self.mtree.selection()
        if not sel:
            self.toast("先选中一行", False)
            return
        mid = self.mtree.item(sel[0], "values")[0]
        if mid:
            self.root.clipboard_clear()
            self.root.clipboard_append(mid)
            self.toast("已复制模型 ID: %s" % mid)


def main():
    root = tk.Tk()
    App(root)
    root.mainloop()


if __name__ == "__main__":
    main()
