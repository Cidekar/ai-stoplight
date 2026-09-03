# Enclosure

The case for [Stoplight](../readme.md). Four printed parts, about **36 × 78 × 18mm**, roughly a matchbox standing upright.

```
          FRONT                       SIDE (cut away)
                                    visor    body
         ╭───────╮                  ├──┤  ├───────┤
     ┌───┤  ( )  ├───┐          ┌───────────────────┐   0mm
     │   ╰───────╯   │          │ ╲     │  button   │
     │    recessed   │          │       ├───────────┤   8mm
     ├───────────────┤          │  ╲    │           │
     │               │          │   ╲___│▓  ┌─────┐ │
     │     ╭─────╮   │          │   ╱▒▒▒│▓  │ ███ │ │
     │     │  ●  │ R │          │  ╱────┤▓  │ ███ │ │  battery
     │     ╰─────╯   │          │       │   │ ███ │ │  stands
     │     ╭─────╮   │          │   ╲___│▓  │ ███ │ │  upright
     │     │  ●  │ Y │          │   ╱▒▒▒│▓  │ ███ │ │  behind
     │     ╰─────╯   │          │  ╱────┤▓  │ ███ │ │  the lamps
     │     ╭─────╮   │          │       │   └─────┘ │
     │     │  ●  │ G │          │   ╲___│▓          │
     │     ╰─────╯   │          │   ╱▒▒▒│▓          │
     │               │          │  ╱────┤           │  52mm
     ├───────────────┤          ├───────────────────┤
     │  ┌─────────┐  │          │       │┌──┐ ┌───┐ │
     │  │ ▪ 2/4   │  │          │       ││▉▉│ │TP4│ │  board and
     │  │ auth-api│  │          │       ││▉▉│ └───┘ │  charger
     │  │ ● needs │  │          │       │└──┘       │
     │  └─────────┘  │          │        ╧          │
     └───────┬───────┘          └───────────────────┘  78mm
             ╧                    ├─── 25mm total ──┤
           USB-C
     ├──── 36mm ────┤            ▒ lens   ▓ LED   ╲╱ visor
```

Three things earn their place in that shape.

The **battery stands upright behind the lamp column**, in space the lamps do not use. Lay it flat underneath instead and the device grows to 184mm, or sprawls to 70mm deep. This is what keeps it the size of a matchbox.

The **visors** hood each lamp and are open underneath, so a lamp is shaded from the ceiling and your monitor but still bright from where you sit. This is the detail that makes the thing read as a traffic light rather than three LEDs in a box.

The **button is recessed** into a dish on the top face. Your finger finds it without looking, and nothing knocks it by accident.

## Files

| File | What it is |
|---|---|
| **`params.json`** | **Every dimension. The only file you edit.** |
| `params.py` | Loads the JSON, computes derived values, writes the `.scad` block |
| `stoplight.scad` | OpenSCAD geometry. Its parameter block is generated. |
| `gen_stl.py` | Writes the STLs. Standard library only. |
| `gen_docs.py` | Regenerates the tables in this file |
| `stl/*.stl` | Ready to slice. |

One dimension lives in one place. Change the button size in `params.json` and the OpenSCAD source, the STL generator and the tables below all follow:

```bash
python3 params.py     # rewrites the parameter block in stoplight.scad
python3 gen_stl.py    # rewrites stl/*.stl
python3 gen_docs.py   # rewrites the tables in this readme
```

Two paths to a printable file, because they suit different moments.

Run `python3 gen_stl.py` and slice what lands in `stl/`. Nothing to install, and it takes under a second.

Or open `stoplight.scad` in [OpenSCAD](https://openscad.org) and export from there. The geometry is exact rather than voxelised, which matters for the curved surfaces. Use this once you have measured your own parts.

Both read the same numbers, so they cannot drift.

## The four parts

```
              ◓             1 × PLUNGER
              │               drops through the top face
              ▼               into the dish

        ╭───────────╮
        │     ◌     │       1 × BODY
   ◍ ───┤   ╭───╮   │         everything else is
        │   ╰───╯   │         a hole in this
   ◍ ───┤   ╭───╮   │
        │   ╰───╯   │       3 × LENS
   ◍ ───┤   ╭───╮   │         translucent, sparse
        │   ╰───╯   │         press straight in
        │ ┌───────┐ │
        │ │screen │ │
        │ └───────┘ │
        ╰───────────╯
              ▲
        ┌─────┴─────┐       1 × BACK COVER
        │ ▪▪▪   ▪▪▪ │         4 screws, one vent
        │  ═══════  │         closes the whole thing
        └───────────┘
```

| Part | Qty | Filament | Settings | Time |
|---|---|---|---|---|
| `body` | 1 | Matte black | 0.2mm layers, 5 perimeters, 20% infill | A few hours |
| `lens` | 3 | Translucent or natural | 15% infill, 2 perimeters, **no top or bottom layers** | Two minutes each |
| `back` | 1 | Matte black | 0.2mm layers, 20% infill | Under an hour |
| `plunger` | 1 | Matte black | 0.15mm layers for a smoother slide | Two minutes |

The lens setting is the one that matters. Sparse infill with open ends is what diffuses the LED into an even glow. Print a lens solid and you get a bright dot with a dark ring around it.

Print the body with the open back face down on the plate. No supports needed: the lamp bores are horizontal cylinders under 10mm, which bridge cleanly, and the screen window is small enough to span.

Print a lens and the plunger first. They cost you four minutes and they tell you whether your printer's tolerances match the model, which is worth knowing before you commit hours to the body.

## Component sizes

Every dimension derives from these. Measure your own parts, because module sizes vary between suppliers.

<!-- generated from params.json — run gen_docs.py components -->

| Component | L × W × H | Notes |
|---|---|---|
| ESP32-C3 SuperMini | 25.0 × 20.5 × 6.0mm | Screen on board. Lit area only 9.2 × 5.2mm. |
| LiPo 500mAh, 503035 | 35.0 × 30.0 × 5.0mm | Widest part. Sets internal width. |
| TP4057 module | 26.0 × 17.0 × 4.0mm | Mounts behind the battery |
| 5mm LED | 5.0mm diameter | Body about 8mm long with the flange |

The battery is widest at 30mm, so it sets the internal width. Nothing is longer than 35mm, which is why the device is short.

<!-- end generated -->

## Key dimensions

<!-- generated from params.json — run gen_docs.py dimensions -->

| Dimension | Value |
|---|---|
| Overall | 36 × 78 × 18mm |
| Wall thickness | 2.0mm, 3.0mm on top |
| Lamp bores | 10.0mm ⌀, 12.0mm deep, 12.5mm apart |
| Lamp centres | z = 60.5, 48.0, 35.5mm from the base |
| Lens inserts | 10.0mm ⌀ × 5.0mm |
| Visors | 7.0mm projection, 1.6mm wall |
| Button pocket | 6.0mm switch, 9.0mm dish 1.2mm deep |
| Screen window | 12.0 × 10.0mm |
| Board rails | 1.6mm thick, 4.0mm reach |
| Print clearance | fit = 0.2mm between mating parts |

<!-- end generated -->

These tables are generated. To change any of them, edit `params.json` and run `python3 params.py && python3 gen_docs.py`.

## Body

Matte black PLA, 2.0mm walls, which is five perimeters at a 0.4mm nozzle. Described from the top down, which is the order it prints in.

The **button** occupies the top 8mm. A 6 × 6mm tactile switch sits in a 7 × 7mm pocket below the top face, with a 4mm hole through the face for the plunger and a 9mm dish around it so the cap finishes below the surface. Keep the top face at 3mm rather than 2mm and add a short internal rib beside the pocket, because pressing down puts a load through the whole body and a 2mm top flexes enough to make the press feel soft.

The top is the right face for the button. You press straight down, which the desk resists, so the case does not rock or slide the way a side press would. It is also the face you can find without looking, which matters for something you operate while reading something else.

The **lamp column** is three bores, 10mm diameter, 12mm deep, spaced 12.5mm centre to centre. Behind each, a 5.2mm hole passes through for the LED. Keep the wall between adjacent bores solid, because that wall is what stops red bleeding into yellow. Each bore carries a visor: a hood projecting 7mm from the front face, open underneath.

The **battery bay** sits behind the lamp column, 32 × 37 × 6mm, open to the back. The charging module sits below it against the back wall, USB-C port facing down through a 10 × 6mm opening in the base.

The **board** goes in the lower front section, screen facing forward, with a 12 × 10mm window over the screen and an opening in the base for its USB-C port so you can reflash without opening the case.

It is held by printed rails rather than screws. The board slides down between two 1.6mm side rails onto a ledge, four tabs catch its front face, and a stop above it means the board cannot lift once the cover is on. Nothing to fasten, nothing to lose, and it comes out again if you need it.

## Lens inserts

Three 10mm discs, 5mm thick, with a 5.2mm blind socket in the back for the LED.

Print at 15% infill, two perimeters, no top or bottom solid layers if your slicer allows it. The sparse infill is doing the diffusing. A solid lens looks like a bright dot; a sparse one glows evenly across the whole face, and that difference is the single biggest visual upgrade in the build.

Press fit is intended. Size the discs 0.2mm under the bore and they stay put without glue.

## Back cover

Flat, and small enough that four M2 screws into printed bosses are enough. Heat-set inserts are cleaner if you have them, but at this size the cover is under no load.

It carries a 2mm vent slot near the charging module. A LiPo charging in a sealed box is a bad habit even at these currents.

## Fit

`fit = 0.20` in both files is the gap between mating parts. Printers vary, so the first set is a test.

If the lens inserts fall out, lower it to 0.15. If they will not seat, raise it to 0.25. The same variable governs the plunger, which should drop in and move freely without rattling.

If red bleeds into yellow, thicken the divider walls to 3mm.

## Before you print

Measure your own components and edit `params.json`. The defaults are nominal figures from supplier listings, and two are most likely to differ.

**The button.** `components.btn_body` assumes a 6 × 6mm tactile switch. A panel-mount button is often 12mm across and needs 10mm behind the face, which changes the pocket, the top thickness and the overall height.

**The LEDs.** `components.led_hole` fits a bare 5mm LED. Many have a flange at the base about 5.8mm across, which needs a wider hole or a shallow counterbore.

Also check the battery. `503035` cells vary by about a millimetre between suppliers, and the bay has 1mm of slack on each side, set by `print.slack`.

After any edit:

```bash
python3 params.py && python3 gen_stl.py && python3 gen_docs.py
```

Print the enclosure last. It should follow parts you are holding rather than numbers from a product page.

## Known limits

`gen_stl.py` meshes on a 0.5mm voxel grid, so curved surfaces are faceted at that scale and the files are larger than they need to be. It is fine for printing at 0.2mm layers, where the slicer smooths most of it away, but the OpenSCAD output is cleaner if you care about the finish on the lens bores.

The STL geometry has been verified by point probes rather than by a slicer. Open the files in your slicer before committing to a long print, and check that it does not report non-manifold edges.

Neither file models the wiring. Leave room for three LED leads, two button wires and the battery cable when you assemble, and route them down the back of the cavity where the cover closes over them.

## License

[Apache License 2.0](../LICENSE), the same as the rest of Stoplight. Copyright 2026 Cidekar, LLC.

Print them, remix them, sell the prints. Keep the notice and say what you changed.

If you improve the models, [`CONTRIBUTING.md`](../CONTRIBUTING.md) explains how to send them back. Fit tolerances from other printers are especially welcome.
