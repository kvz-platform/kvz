#!/usr/bin/env python3
"""Render results/report.html: small-multiple bar panels (one per metric),
M4 mini median vs Studio, every platform; plus the full table view.
Reads results/summary.json (written by bench/report.py)."""
import json, os, statistics, html

ROOT = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "results")
S = json.load(open(os.path.join(ROOT, "summary.json")))
MINIS = ["amelias-mini", "dereks-mac-mini", "lexies-mac-mini"]
PLATS = [("kvz", "kvz (new)"), ("lima-k3s", "k3s in Lima"), ("apple-cluster", "Apple container cluster"), ("kvz-v1", "kvz v1 (Apple daemon)")]
SLOT = {"kvz": 1, "lima-k3s": 2, "apple-cluster": 3, "kvz-v1": 4}

# (summary key, title, unit, lower_is_better, scale)
METRICS = [
    ("Control-plane startup:re_api", "Restart → API ready", "s", True, 1),
    ("Idle cost (empty cluster):fp", "Idle memory (phys footprint)", "MB", True, 1),
    ("Idle cost (empty cluster):cpu", "Idle CPU", "% core", True, 1),
    ("Single pod: create → Ready (nginx:alpine, image cached):p50", "1 nginx pod → Ready", "ms", True, 1),
    ("Burst: 20 nginx pods at once:wall", "20 nginx pods → all Ready", "s", True, 1),
    ("Burst: 50 nginx pods at once:wall", "50 nginx pods → all Ready", "s", True, 1),
    ("Burst: 20 nginx pods at once:fp", "Host memory with 20 pods running", "MB", True, 1),
    ("Burst: 20 nginx pods at once:td", "Delete 20 pods → gone", "s", True, 1),
    ("API server (2000 ConfigMaps, 32 clients):create_ops", "API creates / s", "ops/s", False, 1),
    ("API server (2000 ConfigMaps, 32 clients):get_ops", "API gets / s", "ops/s", False, 1),
    ("API server (2000 ConfigMaps, 32 clients):watch_p50", "Create → watch event p50", "ms", True, 1),
    ("API server (2000 ConfigMaps, 32 clients):create_p99", "API create p99", "ms", True, 1),
]


def val(host, plat, key):
    return (S.get(host, {}).get(plat, {}) or {}).get(key)


def mini_median(plat, key):
    xs = [val(h, plat, key) for h in MINIS]
    xs = [x for x in xs if isinstance(x, (int, float))]
    return round(statistics.median(xs), 2) if xs else None


def fmt(v, unit):
    if v is None:
        return "–"
    if unit in ("ops/s", "MB") or v >= 100:
        return f"{v:,.0f}"
    return f"{v:g}"


def panel(key, title, unit, lower, view):
    rows = []
    for p, name in PLATS:
        v = mini_median(p, key) if view == "m4" else val("studio", p, key)
        if v is not None:
            rows.append((p, name, v))
    if not rows:
        return ""
    vmax = max(r[2] for r in rows) or 1
    best = (min if lower else max)(r[2] for r in rows)
    W, BH, GAP, LW = 520, 22, 8, 170
    H = len(rows) * (BH + GAP)
    bars = []
    for i, (p, name, v) in enumerate(rows):
        y = i * (BH + GAP)
        w = max(2, (W - LW - 70) * v / vmax)
        tip = html.escape(f"{name}: {fmt(v, unit)} {unit}")
        star = " ★" if v == best else ""
        bars.append(f'''<g class="bar" tabindex="0"><title>{tip}</title>
<rect class="hit" x="0" y="{y-3}" width="{W}" height="{BH+6}" />
<text class="lbl" x="{LW-8}" y="{y+BH/2+4}" text-anchor="end">{html.escape(name)}</text>
<path class="s{SLOT[p]}" d="M{LW},{y} h{w-4} a4,4 0 0 1 4,4 v{BH-8} a4,4 0 0 1 -4,4 h-{w-4} z"/>
<text class="val" x="{LW+w+6}" y="{y+BH/2+4}">{fmt(v, unit)} {unit}{star}</text></g>''')
    dirn = "lower is better" if lower else "higher is better"
    return f'''<figure class="panel"><figcaption><b>{html.escape(title)}</b><span>{dirn}</span></figcaption>
<svg viewBox="0 0 {W} {H}" role="img" aria-label="{html.escape(title)}">{''.join(bars)}</svg></figure>'''


def table():
    hosts = MINIS + ["studio"]
    head = "<tr><th>metric</th><th>platform</th>" + "".join(f"<th>{h}</th>" for h in hosts) + "</tr>"
    body = []
    for key, title, unit, lower, _ in METRICS:
        for p, name in PLATS:
            cells = [fmt(val(h, p, key), unit) for h in hosts]
            if all(c == "–" for c in cells):
                continue
            body.append(f"<tr><td>{html.escape(title)} ({unit})</td><td>{html.escape(name)}</td>" + "".join(f"<td>{c}</td>" for c in cells) + "</tr>")
    return f"<table>{head}{''.join(body)}</table>"


def page():
    m4 = "".join(panel(k, t, u, lo, "m4") for k, t, u, lo, _ in METRICS)
    st = "".join(panel(k, t, u, lo, "studio") for k, t, u, lo, _ in METRICS)
    legend = "".join(f'<span class="key"><i class="s{SLOT[p]}"></i>{html.escape(n)}</span>' for p, n in PLATS)
    return f'''<!doctype html><html><head><meta charset="utf-8"><title>kvz benchmarks</title>
<style>
.viz-root {{ color-scheme: light; --surface-1:#fcfcfb; --text-primary:#0b0b0b; --text-secondary:#52514e; --grid:#e6e5e1;
  --series-1:#2a78d6; --series-2:#eb6834; --series-3:#1baf7a; --series-4:#eda100; }}
@media (prefers-color-scheme: dark) {{ :root:where(:not([data-theme="light"])) .viz-root {{ color-scheme: dark;
  --surface-1:#1a1a19; --text-primary:#f0efec; --text-secondary:#c3c2b7; --grid:#33322f;
  --series-1:#3987e5; --series-2:#d95926; --series-3:#199e70; --series-4:#c98500; }} }}
:root[data-theme="dark"] .viz-root {{ color-scheme: dark; --surface-1:#1a1a19; --text-primary:#f0efec; --text-secondary:#c3c2b7; --grid:#33322f;
  --series-1:#3987e5; --series-2:#d95926; --series-3:#199e70; --series-4:#c98500; }}
body {{ margin:0; }}
.viz-root {{ background:var(--surface-1); color:var(--text-primary); font:14px/1.4 -apple-system, system-ui, sans-serif; padding:24px 32px; }}
h1 {{ font-size:22px; margin:0 0 4px; }} h2 {{ font-size:16px; margin:28px 0 8px; }}
p.sub {{ color:var(--text-secondary); margin:0 0 12px; max-width:900px; }}
.legend {{ display:flex; gap:18px; flex-wrap:wrap; margin:8px 0 16px; color:var(--text-secondary); }}
.key i {{ display:inline-block; width:12px; height:12px; border-radius:3px; margin-right:6px; vertical-align:-1px; }}
.grid {{ display:grid; grid-template-columns:repeat(auto-fill, minmax(460px, 1fr)); gap:18px 28px; }}
.panel {{ margin:0; }} figcaption {{ display:flex; justify-content:space-between; margin-bottom:6px; }}
figcaption span {{ color:var(--text-secondary); font-size:12px; }}
svg {{ width:100%; height:auto; overflow:visible; }}
.lbl, .val {{ fill:var(--text-secondary); font-size:12px; }} .val {{ fill:var(--text-primary); }}
.hit {{ fill:transparent; }} .bar:hover .hit, .bar:focus .hit {{ fill:var(--grid); }}
.s1 {{ fill:var(--series-1); background:var(--series-1); }} .s2 {{ fill:var(--series-2); background:var(--series-2); }}
.s3 {{ fill:var(--series-3); background:var(--series-3); }} .s4 {{ fill:var(--series-4); background:var(--series-4); }}
table {{ border-collapse:collapse; font-size:12px; margin-top:8px; }} th, td {{ padding:4px 10px; border-bottom:1px solid var(--grid); text-align:right; }}
th:first-child, td:first-child, th:nth-child(2), td:nth-child(2) {{ text-align:left; }}
details {{ margin-top:24px; }}
</style></head><body><div class="viz-root">
<h1>kvz: native-macOS Kubernetes, benchmarked</h1>
<p class="sub">Same harness, same hosts, one platform at a time. <b>M4</b> = median of three Mac minis (M4, 16 GB, macOS 26.6.2);
<b>Studio</b> = M5 Max, 36 GB, macOS 27 (busy host: ~26 unrelated containers running). ★ marks the best value per panel.
Every kvz Linux pod is its own cold-booted VM; no pools, snapshots or pre-started pods.</p>
<div class="legend">{legend}</div>
<h2>M4 Mac mini (median of 3)</h2><div class="grid">{m4}</div>
<h2>Mac Studio (M5 Max)</h2><div class="grid">{st}</div>
<details><summary>Table view (all hosts)</summary>{table()}</details>
</div></body></html>'''


open(os.path.join(ROOT, "report.html"), "w").write(page())
print(os.path.join(ROOT, "report.html"))
