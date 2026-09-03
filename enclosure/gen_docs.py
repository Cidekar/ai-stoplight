#!/usr/bin/env python3
"""Regenerate the dimension tables in readme.md from params.json.

The prose is hand-written; only the tables between the generated markers
are replaced. Run after changing params.json:

    python3 params.py && python3 gen_docs.py
"""

from pathlib import Path

import params as P

HERE = Path(__file__).parent
BEGIN = "<!-- generated from params.json — run gen_docs.py -->"
END = "<!-- end generated -->"


def components_table():
    rows = [
        ("ESP32-C3 SuperMini", f"{P.BOARD_L} × {P.BOARD_W} × {P.BOARD_H}mm",
         f"Screen on board. Lit area only {P.SCREEN_LIT_W} × {P.SCREEN_LIT_H}mm."),
        ("LiPo 500mAh, 503035", f"{P.BATT_L} × {P.BATT_W} × {P.BATT_H}mm",
         "Widest part. Sets internal width."),
        ("TP4057 module", f"{P.CHG_L} × {P.CHG_W} × {P.CHG_H}mm",
         "Mounts behind the battery"),
        ("5mm LED", f"{P.LED_DIA}mm diameter",
         "Body about 8mm long with the flange"),
    ]
    out = ["| Component | L × W × H | Notes |", "|---|---|---|"]
    out += [f"| {a} | {b} | {c} |" for a, b, c in rows]
    out.append("")
    out.append(
        f"The battery is widest at {P.BATT_W:.0f}mm, so it sets the internal "
        f"width. Nothing is longer than {P.BATT_L:.0f}mm, which is why the "
        f"device is short."
    )
    return "\n".join(out)


def key_dimensions():
    lamps = ", ".join(f"{P.lamp_z(i):.1f}" for i in range(3))
    rows = [
        ("Overall", f"{P.OUTER_W:.0f} × {P.OUTER_H:.0f} × {P.OUTER_D:.0f}mm"),
        ("Wall thickness", f"{P.WALL}mm, {P.TOP_WALL}mm on top"),
        ("Lamp bores", f"{P.LENS_DIA}mm ⌀, {P.LENS_DEEP}mm deep, "
                       f"{P.LENS_PITCH}mm apart"),
        ("Lamp centres", f"z = {lamps}mm from the base"),
        ("Lens inserts", f"{P.LENS_DIA}mm ⌀ × {P.LENS_THICK}mm"),
        ("Visors", f"{P.VISOR_OUT}mm projection, {P.VISOR_T}mm wall"),
        ("Button pocket", f"{P.BTN_BODY}mm switch, {P.BTN_RECESS_DIA}mm dish "
                          f"{P.BTN_RECESS_D}mm deep"),
        ("Screen window", f"{P.SCREEN_W} × {P.SCREEN_H}mm"),
        ("Board rails", f"{P.TAB_T}mm thick, {P.TAB_W}mm reach"),
        ("Print clearance", f"fit = {P.FIT}mm between mating parts"),
    ]
    out = ["| Dimension | Value |", "|---|---|"]
    out += [f"| {a} | {b} |" for a, b in rows]
    return "\n".join(out)


SECTIONS = {
    "components": components_table,
    "dimensions": key_dimensions,
}


def main():
    path = HERE / "readme.md"
    text = path.read_text()
    count = 0
    for name, fn in SECTIONS.items():
        begin = f"{BEGIN[:-4]} {name} -->"
        if begin not in text:
            continue
        head, rest = text.split(begin, 1)
        _, tail = rest.split(END, 1)
        text = f"{head}{begin}\n\n{fn()}\n\n{END}{tail}"
        count += 1
    path.write_text(text)
    print(f"regenerated {count} table(s) in {path.name}")
    print(f"outer: {P.OUTER_W:.0f} × {P.OUTER_H:.0f} × {P.OUTER_D:.0f} mm")


if __name__ == "__main__":
    main()
