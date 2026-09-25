#!/usr/bin/env python3

import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import plot
from plot import COLUMN
import matplotlib.pyplot as plt
import numpy as np

scc = json.loads(Path("experiments/results/campaigns/e1-scc.json").read_text())
flows = [("simple", "Base flow"), ("extended", "Extended flow")]
series = [("vanilla", "Forward-only", plot.COLOR["vanilla"]), ("saga", plot.NAME["saga"], plot.COLOR["saga"])]

plt.rcParams["hatch.linewidth"] = 4
fig, ax = plt.subplots(figsize=(COLUMN, 0.9))
x = np.arange(len(flows))
w = 0.36
for i, (key, label, color) in enumerate(series):
    ys = [scc[f"{key}-{flow}"]["code"] for flow, _ in flows]
    extra = dict(hatch="//", edgecolor=plot.COLOR["atomic"], linewidth=0) if key == "vanilla" else {}
    bars = ax.bar(x + (i - 0.5) * w, ys, w, color=color, label=label, **extra)
    ax.bar_label(bars, padding=2)
ax.set_xticks(x)
ax.set_xticklabels([name for _, name in flows])
ax.set_ylabel("Lines of code")
ax.set_ylim(0, max(v["code"] for v in scc.values()) * 1.35)
ax.grid(axis="y", alpha=0.5)
ax.set_axisbelow(True)
fig.legend(loc="upper center", bbox_to_anchor=(0.5, 1.2), ncols=2, handlelength=1.2, handletextpad=0.4)
plot.save(fig, "e1-loc")
