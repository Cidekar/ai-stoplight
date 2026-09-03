#!/usr/bin/env python3
"""Load enclosure dimensions from params.json.

One source of truth. `stoplight.scad`, `gen_stl.py` and the generated
tables in `readme.md` all derive from this file, so a dimension is
edited in exactly one place.

Run this module directly to regenerate the derived files:

    python3 params.py
"""

import json
from pathlib import Path

HERE = Path(__file__).parent
SRC = HERE / "params.json"

_raw = json.loads(SRC.read_text())

# Flatten the sections into module-level names. The JSON is grouped for
# readability; the geometry code wants flat constants.
FIT = _raw["print"]["fit"]
SLACK = _raw["print"]["slack"]
RES = _raw["print"]["resolution"]

_c = _raw["components"]
BOARD_L, BOARD_W, BOARD_H = _c["board_l"], _c["board_w"], _c["board_h"]
BATT_L, BATT_W, BATT_H = _c["batt_l"], _c["batt_w"], _c["batt_h"]
CHG_L, CHG_W, CHG_H = _c["chg_l"], _c["chg_w"], _c["chg_h"]
LED_DIA, LED_HOLE = _c["led_dia"], _c["led_hole"]
BTN_BODY, BTN_H = _c["btn_body"], _c["btn_h"]
SCREEN_LIT_W, SCREEN_LIT_H = _c["screen_lit_w"], _c["screen_lit_h"]

_s = _raw["shell"]
WALL, TOP_WALL = _s["wall"], _s["top_wall"]
INNER_D = _s["inner_d"]
BTN_SECT, SCR_SECT = _s["btn_sect"], _s["scr_sect"]

_l = _raw["lens"]
LENS_DIA, LENS_DEEP = _l["dia"], _l["deep"]
LENS_THICK, LENS_PITCH = _l["thick"], _l["pitch"]

_b = _raw["button"]
PLUNGER_DIA = _b["plunger_dia"]
BTN_RECESS_DIA = _b["recess_dia"]
BTN_RECESS_D = _b["recess_depth"]

_r = _raw["board_retention"]
TAB_T, TAB_W = _r["tab_t"], _r["tab_w"]

_v = _raw["visor"]
VISOR_OUT, VISOR_T = _v["out"], _v["thickness"]

_o = _raw["openings"]
SCREEN_W, SCREEN_H = _o["screen_w"], _o["screen_h"]
USB_W, USB_H = _o["usb_w"], _o["usb_h"]

# --- derived ---------------------------------------------------------
# The battery is the widest component, so it sets the internal width.
LAMP_SECT = 3 * LENS_PITCH + 6.5
INNER_W = BATT_W + 2 * SLACK
OUTER_W = INNER_W + 2 * WALL
OUTER_D = INNER_D + 2 * WALL
OUTER_H = BTN_SECT + LAMP_SECT + SCR_SECT

# Lamp bores are centred in the lamp section so the top one clears the
# button pocket and the bottom one clears the screen.
LAMP_MID = SCR_SECT + LAMP_SECT / 2


def lamp_z(i):
    """Centre height of lamp i, counting 0 = top (red)."""
    return LAMP_MID + LENS_PITCH - LENS_PITCH * i


# --- SCAD emission ---------------------------------------------------

_SCAD_MAP = [
    ("Print tuning", [
        ("fit", FIT, "gap between mating printed parts"),
        ("slack", SLACK, "clearance around electronic components"),
    ]),
    ("Components - measure yours", [
        ("board_l", BOARD_L, "ESP32-C3 SuperMini"),
        ("board_w", BOARD_W, None),
        ("board_h", BOARD_H, None),
        ("batt_l", BATT_L, "LiPo 503035"),
        ("batt_w", BATT_W, None),
        ("batt_h", BATT_H, None),
        ("chg_l", CHG_L, "TP4057 charging module"),
        ("chg_w", CHG_W, None),
        ("chg_h", CHG_H, None),
        ("led_dia", LED_DIA, "5mm LED body"),
        ("led_hole", LED_HOLE, "through-hole for the LED"),
        ("btn_body", BTN_BODY, "tactile switch footprint"),
        ("btn_h", BTN_H, "switch height incl. actuator"),
    ]),
    ("Button", [
        ("plunger_dia", PLUNGER_DIA, "shaft through the top face"),
        ("btn_recess", BTN_RECESS_DIA, "dished area around the plunger"),
        ("btn_recess_d", BTN_RECESS_D, "how far the dish sinks"),
    ]),
    ("Board retention", [
        ("tab_t", TAB_T, "rail and tab thickness"),
        ("tab_w", TAB_W, "how far each tab reaches over the board"),
    ]),
    ("Visors", [
        ("visor_out", VISOR_OUT, "projection from the front face"),
        ("visor_t", VISOR_T, "wall thickness"),
    ]),
    ("Openings", [
        ("screen_w", SCREEN_W, "window over the OLED"),
        ("screen_h", SCREEN_H, None),
        ("usb_w", USB_W, "USB-C cutouts"),
        ("usb_h", USB_H, None),
    ]),
    ("Shell", [
        ("wall", WALL, None),
        ("top_wall", TOP_WALL, "thicker: the button loads it"),
        ("inner_d", INNER_D, "front-to-back cavity"),
        ("lens_dia", LENS_DIA, None),
        ("lens_deep", LENS_DEEP, "bore depth in the body"),
        ("lens_thick", LENS_THICK, "insert thickness"),
        ("lens_pitch", LENS_PITCH, "centre to centre"),
        ("btn_sect", BTN_SECT, "height of the button section"),
        ("scr_sect", SCR_SECT, "height of the screen section"),
    ]),
]

_BEGIN = "// <<< generated from params.json — do not edit by hand"
_END = "// >>> end generated"


def scad_block():
    """The parameter block for stoplight.scad, generated from params.json."""
    out = [_BEGIN]
    for section, entries in _SCAD_MAP:
        out.append("")
        out.append(f"/* [{section}] */")
        for name, val, comment in entries:
            line = f"{name:<13}= {val};"
            if comment:
                line = f"{line:<28}// {comment}"
            out.append(line)
    out += [
        "",
        "/* [Derived] */",
        "lamp_sect    = 3 * lens_pitch + 6.5;",
        "inner_w      = batt_w + 2 * slack;   // battery is widest",
        "outer_w      = inner_w + 2 * wall;",
        "outer_d      = inner_d + 2 * wall;",
        "outer_h      = btn_sect + lamp_sect + scr_sect;",
        "lamp_mid     = scr_sect + lamp_sect / 2;",
        "",
        _END,
    ]
    return "\n".join(out)


def write_scad():
    """Replace the generated block in stoplight.scad."""
    path = HERE / "stoplight.scad"
    text = path.read_text()
    if _BEGIN not in text or _END not in text:
        raise SystemExit(f"{path.name}: generated markers not found")
    head = text.split(_BEGIN)[0]
    tail = text.split(_END, 1)[1]
    path.write_text(head + scad_block() + tail)
    return path


if __name__ == "__main__":
    p = write_scad()
    print(f"outer: {OUTER_W:.1f} x {OUTER_H:.1f} x {OUTER_D:.1f} mm")
    print(f"lamps at z = {', '.join(f'{lamp_z(i):.1f}' for i in range(3))}")
    print(f"wrote {p.name}")
