#!/usr/bin/env python3

from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt
import seaborn as sns

OUT = Path("experiments/results/figures")

COLUMN = 3.33
GRID, AXIS = "#e1e0d9", "#c3c2b7"
COLOR = {"vanilla": "#E15554", "atomic": "#083D77", "saga": "#E3B505"}
NAME = {"vanilla": "Plain Knative", "atomic": "AtomicFaaS", "saga": "Manual saga"}

from matplotlib import font_manager
import glob
for f in glob.glob("/usr/local/texlive/*/texmf-dist/fonts/opentype/public/libertine/LinLibertine_R*.otf"):
    font_manager.fontManager.addfont(f)
FONT = ["Linux Libertine O", "Helvetica", "Arial", "DejaVu Sans"]

sns.set_theme(style="ticks", palette="tab10")
plt.rcParams.update({
    "font.family": FONT,
    "axes.unicode_minus": False,
    "font.size": 10,
    "axes.labelsize": 10,
    "axes.titlesize": 10,
    "text.color": "black",
    "axes.edgecolor": AXIS,
    "axes.labelcolor": "black",
    "axes.linewidth": 0.6,
    "axes.spines.top": False,
    "axes.spines.right": False,
    "xtick.color": AXIS,
    "ytick.color": AXIS,
    "xtick.major.width": 0.6,
    "ytick.major.width": 0.6,
    "xtick.labelcolor": "black",
    "ytick.labelcolor": "black",
    "xtick.labelsize": 10,
    "ytick.labelsize": 10,
    "grid.color": GRID,
    "grid.linewidth": 0.5,
    "legend.frameon": False,
    "legend.fontsize": 10,
    "figure.dpi": 300,
    "savefig.bbox": "tight",
    "savefig.pad_inches": 0.02,
})

def save(fig, name, width=COLUMN):
    """Widens the figure until its tight bounding box is `width` inches."""
    for _ in range(4):
        fig.canvas.draw()
        bbox = fig.get_tightbbox(fig.canvas.get_renderer())
        w, h = fig.get_size_inches()
        fig.set_size_inches(w * (width - 2 * plt.rcParams["savefig.pad_inches"]) / bbox.width, h)
    OUT.mkdir(parents=True, exist_ok=True)
    fig.savefig(OUT / f"{name}.pdf")
    plt.close(fig)
    print(f"wrote {OUT}/{name}.pdf")

def pct(xs, q):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(q * len(xs)))]

def rows(path):
    import gzip
    import json
    opener = gzip.open if str(path).endswith(".gz") else open
    with opener(path, "rt") as f:
        return [json.loads(l) for l in f if l.strip()]
