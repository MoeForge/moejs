"""chartkit.py holds what plugin_chart.py and v8_chart.py share: the palette,
the type and a canvas laid out in CSS pixels of an 830 px README column,
saved at twice that size.

Both charts draw engines as rows of horizontal bars from a zero baseline,
moejs first. moejs is the only colour; the other engines a Go program can
embed are grey; the C references are outlined and hatched under their own
header. Every bar ends in its value, so no axis is needed.
"""

# The palette: one accent for moejs, a lighter step of it for its second
# configuration, one grey for everything else.
SURFACE = "#fcfcfb"
INK = "#0b0b0b"
SECONDARY = "#52514e"
MUTED = "#898781"
RULE = "#e1e0d9"
BASELINE = "#c3c2b7"
MOEJS = "#2a78d6"
MOEJS_LIGHT = "#86b6ef"
OTHER = "#898781"
REF = "#898781"
FILL = {"moejs": MOEJS, "moejs-light": MOEJS_LIGHT, "other": OTHER}

# Latin text in Nimbus Sans, Chinese in Noto Sans CJK SC.
FONT = ["Nimbus Sans", "Noto Sans CJK SC"]

WIDTH = 830
PAD = 28
SCALE = 2


class Canvas:
    """Canvas is a figure in CSS pixels, y growing downwards."""

    def __init__(self, height, width=WIDTH):
        import matplotlib

        matplotlib.use("Agg")
        import matplotlib.pyplot as plt

        plt.rcParams.update({
            "font.family": FONT,
            "axes.unicode_minus": False,
            "hatch.linewidth": 0.8 * 0.72,
        })
        self.plt = plt
        self.w, self.h = width, height
        self.fig = plt.figure(figsize=(width / 100, height / 100), dpi=100 * SCALE)
        self.fig.patch.set_facecolor(SURFACE)
        ax = self.fig.add_axes([0, 0, 1, 1])
        ax.set_xlim(0, width)
        ax.set_ylim(height, 0)
        ax.axis("off")
        self.ax = ax
        self.renderer = self.fig.canvas.get_renderer()

    def text(self, x, y, s, size=12, color=INK, weight="normal", ha="left", va="baseline", fit=None):
        """text draws s with its baseline at y. With fit, it fails if s is
        wider than fit pixels, so no line runs off the chart."""
        t = self.ax.text(x, y, s, fontsize=size * 0.72, color=color, fontweight=weight, ha=ha, va=va)
        if fit is not None:
            w = t.get_window_extent(self.renderer).width / SCALE
            if w > fit:
                raise SystemExit(f"text is {w:.0f} px, wider than {fit:.0f} px: {s}")
        return t

    def width(self, s, size=12, weight="normal"):
        t = self.text(0, 0, s, size=size, weight=weight)
        w = t.get_window_extent(self.renderer).width / SCALE
        t.remove()
        return w

    def rule(self, x0, y0, x1, y1, color=RULE):
        self.ax.plot([x0, x1], [y0, y1], color=color, linewidth=0.72, solid_capstyle="butt")

    def bar(self, x, y, length, thick, style):
        """bar draws a bar from x along y, square at x and rounded at its
        end. style is moejs, moejs-light, other or ref (outlined, hatched)."""
        from matplotlib.patches import PathPatch
        from matplotlib.path import Path

        if length <= 0:
            return
        x1, y0, y1 = x + length, y - thick / 2, y + thick / 2
        r = min(4, thick / 2, length)
        verts = [(x, y0), (x1 - r, y0), (x1, y0), (x1, y0 + r), (x1, y1 - r), (x1, y1), (x1 - r, y1), (x, y1), (x, y0)]
        codes = [Path.MOVETO, Path.LINETO, Path.CURVE3, Path.CURVE3, Path.LINETO, Path.CURVE3, Path.CURVE3,
                 Path.LINETO, Path.CLOSEPOLY]
        if style == "ref":
            patch = PathPatch(Path(verts, codes), facecolor=SURFACE, edgecolor=REF, hatch="//////",
                              linewidth=0.72, joinstyle="miter")
        else:
            patch = PathPatch(Path(verts, codes), facecolor=FILL[style], edgecolor="none", linewidth=0)
        self.ax.add_patch(patch)

    def save(self, path):
        self.fig.savefig(path, dpi=100 * SCALE, facecolor=SURFACE)
        self.plt.close(self.fig)
