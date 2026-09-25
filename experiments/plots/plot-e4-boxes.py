#!/usr/bin/env python3
"""Usage: plot-e4-boxes.py <results-dir>..."""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import plot
from plot import COLUMN
import matplotlib.pyplot as plt
from matplotlib.patches import ConnectionPatch
import numpy as np
import pandas as pd
import seaborn as sns

COLOR = dict(plot.COLOR)
HEIGHT = {5: 1.7, 50: 1.2}
INSET_POS = [0.02, 0.52, 0.31, 0.47]
NAME = dict(plot.NAME)

frames = []
for d in [Path(a) for a in sys.argv[1:]]:
    for f in sorted(d.glob("e4-r*-*.jsonl*")):
        if "-mock.jsonl" in f.name:
            continue
        rows = plot.rows(f)
        recs = [r for r in rows[1:] if "status" in r and not r.get("warmup") and not r.get("cooldown")]
        if not recs:
            continue
        meta, n = rows[0], rows[0].get("fail_every_n", 0)
        variant, offered = recs[0]["variant"], recs[0]["rate"]
        ok = np.array([r["latency_ms"] for r in recs if r["status"] == 200])
        cool = [r["start_ms"] for r in rows[1:] if r.get("cooldown")]
        end = min(cool) / 1000 if cool else meta["duration_s"] - meta.get("cut_tail_s", meta["cut_s"])
        expected = offered * (1 - 1 / n) if n else offered
        if len(ok) / (end - meta["cut_s"]) < 0.95 * expected:
            print(f"{variant} {offered} req/s: under 95% of offered, dropped")
            continue
        frames.append(pd.DataFrame({"delay": meta.get("delay_ms", 0), "offered": offered,
                                    "variant": NAME[variant], "latency_ms": ok}))
df = pd.concat(frames, ignore_index=True)

for delay in sorted(df["delay"].unique()):
    sub = df[df["delay"] == delay]
    order = [NAME[v] for v in ("vanilla", "saga", "atomic") if NAME[v] in set(sub["variant"])]
    fig, ax = plt.subplots(figsize=(COLUMN, HEIGHT.get(delay, 2.2)))
    box = dict(width=0.8, gap=0.08, whis=1.5, showfliers=False, linewidth=0.8, linecolor="black", medianprops=dict(color="white", linewidth=1.2),
               palette={NAME[v]: c for v, c in COLOR.items()})
    sns.boxplot(data=sub, x="offered", y="latency_ms", hue="variant", hue_order=order, ax=ax, **box)
    ax.set_ylim(bottom=0)
    ax.set_xlabel("Throughput (req/s)")
    ax.set_ylabel("Latency (ms)")
    for i in range(sub["offered"].nunique() - 1):
        ax.axvline(i + 0.5, color="0.45", lw=0.8, ls=(0, (4, 3)), zorder=0)
    if delay == 5:
        low = sub[sub["offered"] <= 150]
        ins = ax.inset_axes(INSET_POS)
        sns.boxplot(data=low, x="offered", y="latency_ms", hue="variant", hue_order=order, ax=ins, legend=False, **box)
        for i in range(low["offered"].nunique() - 1):
            ins.axvline(i + 0.5, color="0.45", lw=0.8, ls=(0, (4, 3)), zorder=0)
        ins.set_xlabel("")
        ins.set_ylabel("")
        ins.set_xlim(-0.5, low["offered"].nunique() - 0.5)
        ins.set_xticks([])
        ins.set_ylim(40, 84)
        ins.set_yticks([50, 60, 70])
        ins.yaxis.tick_right()
        ins.tick_params(length=2, pad=2)
        for t in ins.get_yticklabels():
            t.set_bbox(dict(facecolor="white", edgecolor="none", pad=1))
        ins.grid(axis="y", alpha=0.5)
        ins.set_axisbelow(True)
        for sp in ins.spines.values():
            sp.set_visible(True)
            sp.set_edgecolor("0.45")
        y0, y1 = ins.get_ylim()
        n = low["offered"].nunique()
        _, conns = ax.indicate_inset([-0.5, y0, n, y1 - y0], inset_ax=ins, edgecolor="0.45", alpha=1, lw=0.8)
        for c in conns:
            c.set_visible(False)
        for xy, corner in (((-0.5, y1), (0, 0)), ((n - 0.5, y1), (1, 0))):
            ax.add_artist(ConnectionPatch(xyA=xy, coordsA="data", axesA=ax, xyB=corner, coordsB="axes fraction",
                                          axesB=ins, color="0.45", lw=0.8))
    ax.grid(axis="y", alpha=0.5)
    ax.set_axisbelow(True)
    leg = ax.get_legend()
    handles, labels = leg.legend_handles, [t.get_text() for t in leg.get_texts()]
    leg.remove()
    fig.legend(handles, labels, loc="upper center", bbox_to_anchor=(0.5, 1.10), ncols=2,
               columnspacing=1.5, handlelength=1.4, handletextpad=0.5)
    plot.save(fig, f"e4-boxes-{delay}ms")
