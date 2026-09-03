#!/usr/bin/env python3
"""Check that the generated files match params.json.

Run before committing:

    python3 test_params.py

Catches the failure this layout exists to prevent: someone edits
stoplight.scad or a readme table by hand, and the sources drift apart.
"""

import re
import sys
from pathlib import Path

import params as P

HERE = Path(__file__).parent
fails = []


def check(name, cond, detail=""):
    if cond:
        print(f"  ok    {name}")
    else:
        print(f"  FAIL  {name}  {detail}")
        fails.append(name)


def scad_value(text, var):
    m = re.search(rf"^{var}\s*=\s*([0-9.]+);", text, re.M)
    return float(m.group(1)) if m else None


print("params.json -> stoplight.scad")
scad = (HERE / "stoplight.scad").read_text()
for var, want in [
    ("fit", P.FIT), ("slack", P.SLACK),
    ("board_l", P.BOARD_L), ("board_w", P.BOARD_W), ("board_h", P.BOARD_H),
    ("batt_l", P.BATT_L), ("batt_w", P.BATT_W), ("batt_h", P.BATT_H),
    ("led_hole", P.LED_HOLE), ("btn_body", P.BTN_BODY), ("btn_h", P.BTN_H),
    ("plunger_dia", P.PLUNGER_DIA), ("btn_recess", P.BTN_RECESS_DIA),
    ("btn_recess_d", P.BTN_RECESS_D),
    ("tab_t", P.TAB_T), ("tab_w", P.TAB_W),
    ("visor_out", P.VISOR_OUT), ("visor_t", P.VISOR_T),
    ("screen_w", P.SCREEN_W), ("screen_h", P.SCREEN_H),
    ("usb_w", P.USB_W), ("usb_h", P.USB_H),
    ("wall", P.WALL), ("top_wall", P.TOP_WALL), ("inner_d", P.INNER_D),
    ("lens_dia", P.LENS_DIA), ("lens_deep", P.LENS_DEEP),
    ("lens_thick", P.LENS_THICK), ("lens_pitch", P.LENS_PITCH),
    ("btn_sect", P.BTN_SECT), ("scr_sect", P.SCR_SECT),
]:
    got = scad_value(scad, var)
    check(f"{var} = {want}", got == want, f"scad has {got}")

print("\nno hardcoded geometry left in the .scad body")
body = scad.split("// >>> end generated", 1)[1]
# Bare decimals are fine in local offsets; flag only the known constants.
for lit, var in [("10.0mm", "lens_dia"), ("12.5", "lens_pitch")]:
    check(f"{lit} not repeated", lit not in body, "found a literal")

print("\nparams.json -> readme tables")
readme = (HERE / "readme.md").read_text()
check(f"overall {P.OUTER_W:.0f} x {P.OUTER_H:.0f} x {P.OUTER_D:.0f}mm",
      f"{P.OUTER_W:.0f} × {P.OUTER_H:.0f} × {P.OUTER_D:.0f}mm" in readme)
lamps = ", ".join(f"{P.lamp_z(i):.1f}" for i in range(3))
check(f"lamp centres {lamps}", lamps in readme)
check("battery width stated", f"widest at {P.BATT_W:.0f}mm" in readme)

print("\ngeometry sanity")
top_bore = P.lamp_z(0) + P.LENS_DIA / 2
btn_floor = P.OUTER_H - P.TOP_WALL - P.BTN_H
check("top bore clears the button pocket", top_bore < btn_floor - 2,
      f"{top_bore:.1f} vs {btn_floor:.1f}")
bot_bore = P.lamp_z(2) - P.LENS_DIA / 2
check("bottom bore clears the screen section", bot_bore > P.SCR_SECT + 2,
      f"{bot_bore:.1f} vs {P.SCR_SECT}")
check("lamp bores do not overlap", P.LENS_PITCH > P.LENS_DIA)
check("battery fits the width", P.BATT_W + 2 * P.SLACK <= P.INNER_W)
check("sections sum to the height",
      abs(P.BTN_SECT + P.LAMP_SECT + P.SCR_SECT - P.OUTER_H) < 1e-9)

if fails:
    print(f"\n{len(fails)} failure(s). Run: python3 params.py && "
          f"python3 gen_docs.py")
    sys.exit(1)
print("\nall checks passed")
