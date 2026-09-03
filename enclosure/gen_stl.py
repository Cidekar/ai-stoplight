#!/usr/bin/env python3
"""Write the Stoplight enclosure STLs.

No dependencies beyond the standard library. Run it:

    python3 gen_stl.py

Geometry is built from axis-aligned boxes and cylinders, combined by a small
CSG evaluator that meshes the result on a voxel grid. That is cruder than a
real kernel, but it needs nothing installed and the parts are simple.

Dimensions come from params.json via params.py, the same source stoplight.scad
is generated from. Edit the JSON, never this file, then run `python3 params.py`
followed by this script.
"""

import struct
import sys
from pathlib import Path

from params import (  # noqa: F401  (re-exported for tests and probes)
    FIT, SLACK, RES,
    BOARD_L, BOARD_W, BOARD_H,
    BATT_L, BATT_W, BATT_H,
    CHG_L, CHG_W, CHG_H,
    LED_DIA, LED_HOLE,
    BTN_BODY, BTN_H, PLUNGER_DIA, BTN_RECESS_DIA, BTN_RECESS_D,
    TAB_T, TAB_W,
    VISOR_OUT, VISOR_T,
    SCREEN_W, SCREEN_H, USB_W, USB_H,
    WALL, TOP_WALL, INNER_D,
    LENS_DIA, LENS_DEEP, LENS_THICK, LENS_PITCH,
    BTN_SECT, SCR_SECT, LAMP_SECT,
    INNER_W, OUTER_W, OUTER_D, OUTER_H, LAMP_MID,
    lamp_z,
)

# ---------------------------------------------------------------- primitives


class Box:
    def __init__(self, x0, y0, z0, x1, y1, z1):
        self.b = (x0, y0, z0, x1, y1, z1)

    def inside(self, x, y, z):
        x0, y0, z0, x1, y1, z1 = self.b
        return x0 <= x <= x1 and y0 <= y <= y1 and z0 <= z <= z1

    def bounds(self):
        return self.b


class Cyl:
    """Cylinder along one axis: 'x', 'y' or 'z'."""

    def __init__(self, axis, ca, cb, lo, hi, dia):
        self.axis, self.ca, self.cb = axis, ca, cb
        self.lo, self.hi, self.r = lo, hi, dia / 2.0

    def inside(self, x, y, z):
        if self.axis == "z":
            a, b, t = x, y, z
        elif self.axis == "y":
            a, b, t = x, z, y
        else:
            a, b, t = y, z, x
        if not (self.lo <= t <= self.hi):
            return False
        return (a - self.ca) ** 2 + (b - self.cb) ** 2 <= self.r * self.r

    def bounds(self):
        r = self.r
        if self.axis == "z":
            return (self.ca - r, self.cb - r, self.lo, self.ca + r, self.cb + r, self.hi)
        if self.axis == "y":
            return (self.ca - r, self.lo, self.cb - r, self.ca + r, self.hi, self.cb + r)
        return (self.lo, self.ca - r, self.cb - r, self.hi, self.ca + r, self.cb + r)


class Wedge:
    """Half-cone visor: a hood over a lamp, open at the front and underneath.

    Sits on the front face, projecting forward. Radius tapers from `r0` at
    the wall to `r1` at the tip, and everything below the lamp centre is
    removed so light escapes downward toward the desk.
    """

    def __init__(self, cx, cz, y0, y1, r0, r1):
        self.cx, self.cz = cx, cz
        self.y0, self.y1 = y0, y1
        self.r0, self.r1 = r0, r1

    def inside(self, x, y, z):
        if not (self.y0 <= y <= self.y1):
            return False
        if z < self.cz:                      # open underneath
            return False
        t = (y - self.y0) / (self.y1 - self.y0)
        r = self.r0 + (self.r1 - self.r0) * t
        d2 = (x - self.cx) ** 2 + (z - self.cz) ** 2
        return (r - 1.6) ** 2 <= d2 <= r * r  # shell, not solid

    def bounds(self):
        r = max(self.r0, self.r1)
        return (self.cx - r, self.y0, self.cz - r,
                self.cx + r, self.y1, self.cz + r)


class Solid:
    """(union(add) - union(sub)) + union(post).

    `post` is added after the subtraction, which is how features that live
    inside a hollowed cavity survive: board rails, ledges and stops would
    otherwise be erased by the cavity that contains them.
    """

    def __init__(self, add, sub=(), post=()):
        self.add, self.sub, self.post = list(add), list(sub), list(post)

    def inside(self, x, y, z):
        if any(s.inside(x, y, z) for s in self.post):
            return True
        if not any(s.inside(x, y, z) for s in self.add):
            return False
        return not any(s.inside(x, y, z) for s in self.sub)

    def bounds(self):
        # Union of every additive part, so geometry projecting outside the
        # main shell (visors, tabs) is not clipped away when meshing.
        bs = [s.bounds() for s in self.add + self.post]
        return (
            min(b[0] for b in bs), min(b[1] for b in bs), min(b[2] for b in bs),
            max(b[3] for b in bs), max(b[4] for b in bs), max(b[5] for b in bs),
        )

    def probe(self, x, y, z):
        return self.inside(x, y, z)


# ---------------------------------------------------------------- meshing

FACES = (
    ((-1, 0, 0), ((0, 0, 0), (0, 0, 1), (0, 1, 1), (0, 1, 0))),
    ((1, 0, 0), ((1, 0, 0), (1, 1, 0), (1, 1, 1), (1, 0, 1))),
    ((0, -1, 0), ((0, 0, 0), (1, 0, 0), (1, 0, 1), (0, 0, 1))),
    ((0, 1, 0), ((0, 1, 0), (0, 1, 1), (1, 1, 1), (1, 1, 0))),
    ((0, 0, -1), ((0, 0, 0), (0, 1, 0), (1, 1, 0), (1, 0, 0))),
    ((0, 0, 1), ((0, 0, 1), (1, 0, 1), (1, 1, 1), (0, 1, 1))),
)


def mesh(solid, res=RES):
    """Surface-mesh a solid by emitting the faces of boundary voxels."""
    x0, y0, z0, x1, y1, z1 = solid.bounds()
    pad = res
    x0, y0, z0 = x0 - pad, y0 - pad, z0 - pad
    nx = int((x1 - x0 + pad) / res) + 1
    ny = int((y1 - y0 + pad) / res) + 1
    nz = int((z1 - z0 + pad) / res) + 1

    def occupied(i, j, k):
        return solid.inside(x0 + (i + 0.5) * res,
                            y0 + (j + 0.5) * res,
                            z0 + (k + 0.5) * res)

    grid = [[[occupied(i, j, k) for k in range(nz)] for j in range(ny)]
            for i in range(nx)]

    tris = []
    for i in range(nx):
        for j in range(ny):
            for k in range(nz):
                if not grid[i][j][k]:
                    continue
                for (dx, dy, dz), corners in FACES:
                    ni, nj, nk = i + dx, j + dy, k + dz
                    if (0 <= ni < nx and 0 <= nj < ny and 0 <= nk < nz
                            and grid[ni][nj][nk]):
                        continue
                    pts = [(x0 + (i + cx) * res,
                            y0 + (j + cy) * res,
                            z0 + (k + cz) * res) for cx, cy, cz in corners]
                    n = (float(dx), float(dy), float(dz))
                    tris.append((n, pts[0], pts[1], pts[2]))
                    tris.append((n, pts[0], pts[2], pts[3]))
    return tris


def write_stl(path, tris):
    with open(path, "wb") as f:
        f.write(b"stoplight".ljust(80, b"\0"))
        f.write(struct.pack("<I", len(tris)))
        for n, a, b, c in tris:
            f.write(struct.pack("<12fH", *n, *a, *b, *c, 0))


# ---------------------------------------------------------------- parts


def body():
    add = [Box(0, 0, 0, OUTER_W, OUTER_D, OUTER_H)]
    sub = []

    # Main cavity, open at the back.
    sub.append(Box(WALL, WALL, TOP_WALL,
                   OUTER_W - WALL, OUTER_D + 1, OUTER_H - WALL))

    # Lamp bores and LED holes, centred within the lamp section so the top
    # bore clears the button pocket and the bottom clears the screen.
    for i in range(3):
        z = lamp_z(i)
        sub.append(Cyl("y", OUTER_W / 2, z, -1, WALL + LENS_DEEP,
                       LENS_DIA + FIT))
        sub.append(Cyl("y", OUTER_W / 2, z, WALL + LENS_DEEP - 0.1,
                       WALL + LENS_DEEP + 6, LED_HOLE))

    # Button pocket and plunger hole.
    bx, by = OUTER_W / 2, OUTER_D / 2
    sub.append(Box(bx - (BTN_BODY + FIT) / 2, by - (BTN_BODY + FIT) / 2,
                   OUTER_H - TOP_WALL - BTN_H,
                   bx + (BTN_BODY + FIT) / 2, by + (BTN_BODY + FIT) / 2,
                   OUTER_H - TOP_WALL + 0.1))
    sub.append(Cyl("z", bx, by, OUTER_H - TOP_WALL - 0.5, OUTER_H + 1,
                   PLUNGER_DIA + FIT))
    # Recess around the plunger, so the cap sits below the top face and you
    # can feel for it without looking. Also stops a knock pressing the button.
    sub.append(Cyl("z", bx, by, OUTER_H - BTN_RECESS_D, OUTER_H + 1,
                   BTN_RECESS_DIA))

    # Screen window.
    sub.append(Box((OUTER_W - SCREEN_W) / 2, -1, SCR_SECT / 2 - SCREEN_H / 2,
                   (OUTER_W + SCREEN_W) / 2, WALL + 1,
                   SCR_SECT / 2 + SCREEN_H / 2))

    # USB-C openings in the base.
    sub.append(Box((OUTER_W - USB_W) / 2 - 6, OUTER_D / 2 - 4, -1,
                   (OUTER_W + USB_W) / 2 - 6, OUTER_D / 2 - 4 + USB_H, WALL + 1))
    sub.append(Box((OUTER_W - USB_W) / 2 + 6, OUTER_D - WALL - 5, -1,
                   (OUTER_W + USB_W) / 2 + 6, OUTER_D - WALL - 5 + USB_H,
                   WALL + 1))

    # --- board retention -------------------------------------------------
    # Added after the cavity is subtracted, or the cavity would erase them.
    # The board slides down between two rails onto a ledge, and two tabs hold
    # its front face so it cannot rock forward against the screen window.
    post = []
    bd_x0 = (OUTER_W - BOARD_W) / 2 - FIT
    bd_x1 = (OUTER_W + BOARD_W) / 2 + FIT
    bd_y = WALL + 0.6                     # board face sits just behind the wall
    bd_z0 = 4.0                           # ledge height above the base
    bd_z1 = bd_z0 + BOARD_L + FIT

    for x in (bd_x0 - TAB_T, bd_x1):      # side rails
        post.append(Box(x, bd_y, bd_z0, x + TAB_T,
                        bd_y + BOARD_H + TAB_T, bd_z1))
    post.append(Box(bd_x0 - TAB_T, bd_y, bd_z0 - TAB_T,      # bottom ledge
                    bd_x1 + TAB_T, bd_y + BOARD_H + TAB_T, bd_z0))
    post.append(Box(bd_x0 - TAB_T, bd_y, bd_z1,              # top stop
                    bd_x1 + TAB_T, bd_y + BOARD_H + TAB_T, bd_z1 + TAB_T))
    for z in (bd_z0 + 3.0, bd_z1 - 3.0 - TAB_W):             # front tabs
        post.append(Box(bd_x0, bd_y - 1.2, z, bd_x0 + TAB_W, bd_y, z + TAB_W))
        post.append(Box(bd_x1 - TAB_W, bd_y - 1.2, z, bd_x1, bd_y, z + TAB_W))

    # --- visors ----------------------------------------------------------
    # A hood over each lamp, projecting forward and open underneath, so the
    # lamp is shaded from above but still readable from the desk.
    for i in range(3):
        z = lamp_z(i)
        post.append(Wedge(OUTER_W / 2, z, -VISOR_OUT, 0.0,
                          LENS_DIA / 2 + VISOR_T + 1.5,
                          LENS_DIA / 2 + VISOR_T))

    return Solid(add, sub, post)


def lens():
    return Solid(
        [Cyl("z", 0, 0, 0, LENS_THICK, LENS_DIA - FIT)],
        [Cyl("z", 0, 0, -0.1, 3, LED_HOLE + 0.3)],
    )


def back():
    add = [Box(0, 0, 0, INNER_W + 2 * WALL, WALL, OUTER_H)]
    add.append(Box(WALL, WALL - 0.1, WALL,
                   INNER_W + WALL - 2 * FIT, 2 * WALL - 0.1, OUTER_H - WALL))
    sub = []
    for x in (WALL + 3, INNER_W + WALL - 3):
        for z in (WALL + 3, OUTER_H - WALL - 3):
            sub.append(Cyl("y", x, z, -1, WALL + 1, 2.2))
    sub.append(Box(INNER_W / 2 - 6 + WALL, -1, WALL + 6,
                   INNER_W / 2 + 6 + WALL, WALL + 1, WALL + 8))
    return Solid(add, sub)


def plunger():
    # Shaft passes through the top face; the cap sits down inside the recess
    # so it finishes flush with, or just below, the surrounding surface.
    shaft = TOP_WALL + BTN_RECESS_D + 1.0
    return Solid([
        Cyl("z", 0, 0, 0, 1.5, 5.5),                       # shoulder, under the face
        Cyl("z", 0, 0, 1.5, 1.5 + shaft, PLUNGER_DIA - FIT),
        Cyl("z", 0, 0, 1.5 + shaft, 1.5 + shaft + 1.2,
            BTN_RECESS_DIA - 1.5),                          # cap, inside the dish
    ])


PARTS = {"body": body, "lens": lens, "back": back, "plunger": plunger}


def main():
    out = Path(__file__).parent / "stl"
    out.mkdir(exist_ok=True)
    want = sys.argv[1:] or list(PARTS)

    print(f"outer: {OUTER_W:.1f} x {OUTER_H:.1f} x {OUTER_D:.1f} mm  "
          f"(resolution {RES}mm)\n")

    for name in want:
        if name not in PARTS:
            print(f"unknown part: {name}", file=sys.stderr)
            continue
        print(f"  {name:8s} meshing...", end="", flush=True)
        tris = mesh(PARTS[name]())
        path = out / f"{name}.stl"
        write_stl(path, tris)
        kb = path.stat().st_size / 1024
        print(f"\r  {name:8s} {len(tris):7d} triangles  {kb:8.1f} KB  "
              f"-> stl/{name}.stl")

    print("\nPrint: body, back and plunger in matte black; lens 3x in "
          "translucent\nat 15% infill, 2 perimeters, no top or bottom layers.")


if __name__ == "__main__":
    main()
