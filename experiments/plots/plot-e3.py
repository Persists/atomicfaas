#!/usr/bin/env python3
"""Usage: plot-e3.py <results-dir>..."""

import json
import sys
from datetime import datetime, timedelta
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import plot
from plot import COLUMN, pct
import matplotlib.pyplot as plt
from matplotlib.ticker import MaxNLocator
import seaborn as sns

LABEL = dict(plot.NAME)
COLOR = dict(plot.COLOR)
DIRS = [Path(a) for a in sys.argv[1:]]

def cancel_delay_ms():
    for d in DIRS:
        for f in d.glob("e3-*.jsonl"):
            meta = json.loads(f.read_text().splitlines()[0])
            if meta.get("meta"):
                return meta.get("cancel_delay_ms", meta.get("delay_ms", 0))
    return 0

CANCEL_DELAY_MS = cancel_delay_ms()

def records(name):
    """First directory holding this file. Warm-up rounds and runs that never
    met the cell's condition are dropped here, with a printed count."""
    for d in DIRS:
        if not (d / name).exists():
            continue
        rows = [json.loads(l) for l in (d / name).read_text().splitlines() if l.strip()]
        rows = [r for r in rows if not r.get("meta") and not r.get("warmup")]
        if not rows:
            continue
        if "-none-" in name:
            keep = [r for r in rows if r["status"] == 200]
        else:
            keep = [r for r in rows
                    if any(e["kind"] == "rejected" for e in r.get("events", []))]
        if len(keep) != len(rows):
            print(f"  note: {name}: dropped {len(rows) - len(keep)}/{len(rows)} "
                  f"run(s) that never met the cell's condition")
        if keep:
            return keep
    sys.exit(f"{name}: no usable records in {[str(d) for d in DIRS]}")

ZORDER = {"saga": 2, "atomic": 3, "vanilla": 4}
LW = 2.0
DASHED = (0, (2, 1.5))

def style(v):
    return DASHED if v == "vanilla" else "-"

def ecdf(ax, xs, color, label, ls="-", z=2):
    sns.ecdfplot(x=xs, ax=ax, color=color, lw=LW, ls=ls, label=label, zorder=z)

def _ts(s):
    """RFC3339 with nanoseconds, which datetime cannot parse before 3.11."""
    s = s.rstrip("Z")
    if "." in s:
        head, frac = s.split(".")
        s = f"{head}.{frac[:6].ljust(6, '0')}"
    return datetime.fromisoformat(s)

def undo_span(r):
    """The refusal -> the last inverse completed, both stamped by the mock."""
    return r["derived"].get("undo_ms")

def comp_duration(r):
    """The rollback itself, excluding the wait to be picked up: from the first
    inverse request arriving at its provider to the last one completing. Older
    campaigns stamped only completions; there the first arrival is derived as
    its completion minus the cancel delay, which the mock sleeps before it acts."""
    undos = [u for u in r["undos"] if u.get("statusAt")]
    if len(undos) < 2:
        return None
    last = max(_ts(u["statusAt"]) for u in undos)
    if all(u.get("undoAt") for u in undos):
        first = min(_ts(u["undoAt"]) for u in undos)
    else:
        first = min(_ts(u["statusAt"]) for u in undos) - timedelta(milliseconds=CANCEL_DELAY_MS)
    return (last - first).total_seconds() * 1000

def undo_start(r):
    """The refusal -> the first inverse completed. Derived here as well as in
    the load generator, so campaigns predating that field still render."""
    if (v := r["derived"].get("undo_start_ms")) is not None:
        return v
    fail = next((e for e in r["events"] if e["kind"] in ("rejected", "hung")), None)
    ends = [u["statusAt"] for u in r["undos"] if u.get("statusAt")]
    if not fail or not ends:
        return None
    t0 = _ts(fail["at"])
    return min((_ts(e) - t0).total_seconds() * 1000 for e in ends)

def fig_latency():
    fig, axes = plt.subplots(1, 3, figsize=(COLUMN, 1.0), sharey=True)

    for ax, cell, title in (
        (axes[0], "none", "No failures"),
        (axes[1], "error", "Last API down"),
    ):
        for v in ("vanilla", "saga", "atomic"):
            xs = [r["latency_ms"] for r in records(f"e3-{cell}-{v}.jsonl")]
            ecdf(ax, xs, COLOR[v], LABEL[v], style(v), ZORDER[v])
            print(f"{cell:9}{v:8} client p50={pct(xs,.5):.0f} p95={pct(xs,.95):.0f}")
        ax.set_xlabel("")
        ax.set_title(title, pad=6)
    base = pct([r["latency_ms"] for r in records("e3-none-vanilla.jsonl")], .5)
    over = pct([r["latency_ms"] for r in records("e3-none-atomic.jsonl")], .5)
    print(f"overhead: +{over - base:.0f} ms ({100 * (over - base) / base:.1f}% of {base:.0f} ms)")

    ax = axes[2]
    for v in ("saga", "atomic"):
        rows = records(f"e3-error-{v}.jsonl")
        comp = [x for x in map(comp_duration, rows) if x is not None]
        disp = [x for x in map(undo_start, rows) if x is not None]
        tot = [x for x in map(undo_span, rows) if x is not None]
        ecdf(ax, comp, COLOR[v], LABEL[v], style(v), ZORDER[v])
        print(f"undo     {v:8} n={len(comp):4} compensate_p50={pct(comp,.5):.0f} "
              f"dispatch_p50={pct(disp,.5):.0f} total_p50={pct(tot,.5):.0f}")
    ax.set_xlabel("")
    ax.set_title("Cost of rollback", pad=6)

    axes[1].set_xlabel("Latency (ms)")
    for ax in axes:
        ax.set_ylim(0, 1.05)
        ax.set_yticks([0, .5, 1])
        ax.xaxis.set_major_locator(MaxNLocator(3))
        ax.grid(axis="y")
        ax.set_xlim(left=0)
        ax.set_ylabel("ECDF" if ax is axes[0] else "")
    right = max(ax.get_xlim()[1] for ax in axes)
    for ax in axes:
        ax.set_xlim(0, right)
    handles = [plt.Line2D([], [], color=COLOR[v], lw=LW, ls=style(v), label=LABEL[v])
               for v in ("vanilla", "saga", "atomic")]
    fig.legend(handles=handles, loc="upper center", bbox_to_anchor=(0.5, 1.38),
               ncols=3, columnspacing=0.7, handlelength=1.0, handletextpad=0.35)
    fig.subplots_adjust(wspace=0.14)
    plot.save(fig, "e3-latency")

if __name__ == "__main__":
    fig_latency()
