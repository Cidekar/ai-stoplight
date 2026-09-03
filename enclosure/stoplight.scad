// Stoplight enclosure — parametric source.
//
// Four parts: body, lens insert (print 3), back cover, button plunger.
// Set the `part` variable below, or render from the command line:
//   openscad -D 'part="body"' -o body.stl stoplight.scad
//
// DIMENSIONS LIVE IN params.json. The block below is generated from it by
// `python3 params.py`. Edit the JSON, run that, and both this file and
// gen_stl.py stay in step. Editing the block by hand will be overwritten.

part = "all";   // "body" | "lens" | "back" | "plunger" | "all"
$fn  = 64;

// <<< generated from params.json — do not edit by hand

/* [Print tuning] */
fit          = 0.2;         // gap between mating printed parts
slack        = 1.0;         // clearance around electronic components

/* [Components - measure yours] */
board_l      = 25.0;        // ESP32-C3 SuperMini
board_w      = 20.5;
board_h      = 6.0;
batt_l       = 35.0;        // LiPo 503035
batt_w       = 30.0;
batt_h       = 5.0;
chg_l        = 26.0;        // TP4057 charging module
chg_w        = 17.0;
chg_h        = 4.0;
led_dia      = 5.0;         // 5mm LED body
led_hole     = 5.2;         // through-hole for the LED
btn_body     = 6.0;         // tactile switch footprint
btn_h        = 5.0;         // switch height incl. actuator

/* [Button] */
plunger_dia  = 4.0;         // shaft through the top face
btn_recess   = 9.0;         // dished area around the plunger
btn_recess_d = 1.2;         // how far the dish sinks

/* [Board retention] */
tab_t        = 1.6;         // rail and tab thickness
tab_w        = 4.0;         // how far each tab reaches over the board

/* [Visors] */
visor_out    = 7.0;         // projection from the front face
visor_t      = 1.6;         // wall thickness

/* [Openings] */
screen_w     = 12.0;        // window over the OLED
screen_h     = 10.0;
usb_w        = 10.0;        // USB-C cutouts
usb_h        = 6.0;

/* [Shell] */
wall         = 2.0;
top_wall     = 3.0;         // thicker: the button loads it
inner_d      = 14.0;        // front-to-back cavity
lens_dia     = 10.0;
lens_deep    = 12.0;        // bore depth in the body
lens_thick   = 5.0;         // insert thickness
lens_pitch   = 12.5;        // centre to centre
btn_sect     = 8.0;         // height of the button section
scr_sect     = 26.0;        // height of the screen section

/* [Derived] */
lamp_sect    = 3 * lens_pitch + 6.5;
inner_w      = batt_w + 2 * slack;   // battery is widest
outer_w      = inner_w + 2 * wall;
outer_d      = inner_d + 2 * wall;
outer_h      = btn_sect + lamp_sect + scr_sect;
lamp_mid     = scr_sect + lamp_sect / 2;

// >>> end generated

// Centre height of lamp i, 0 = top (red). Used by both bores and visors.
function lamp_z(i) = lamp_mid + lens_pitch - lens_pitch * i;

echo(str("Outer: ", outer_w, " x ", outer_h, " x ", outer_d, " mm"));

/* ---------------------------------------------------------------- */

module lens_bore(z) {
    // Bore for the lens insert, plus the LED hole behind it.
    translate([outer_w / 2, -1, z])
        rotate([-90, 0, 0])
            cylinder(d = lens_dia + fit, h = wall + lens_deep + 1);
    translate([outer_w / 2, wall + lens_deep - 0.1, z])
        rotate([-90, 0, 0])
            cylinder(d = led_hole, h = 6);
}

module visor(z) {
    // Hood over a lamp: projects forward, open underneath so the lamp stays
    // readable from the desk while shaded from a monitor above.
    translate([outer_w / 2, 0, z])
        difference() {
            rotate([90, 0, 0])
                cylinder(h = visor_out, r1 = lens_dia / 2 + visor_t,
                         r2 = lens_dia / 2 + visor_t + 1.5);
            rotate([90, 0, 0])
                translate([0, 0, -0.1])
                    cylinder(h = visor_out + 0.2, r1 = lens_dia / 2,
                             r2 = lens_dia / 2 + 1.5);
            translate([-15, -visor_out - 1, -15]) cube([30, visor_out + 2, 15]);
        }
}

module board_retention() {
    // Rails, ledge and front tabs. Rendered after the cavity is cut.
    bd_x0 = (outer_w - board_w) / 2 - fit;
    bd_x1 = (outer_w + board_w) / 2 + fit;
    bd_y  = wall + 0.6;
    bd_z0 = 4.0;
    bd_z1 = bd_z0 + board_l + fit;

    for (x = [bd_x0 - tab_t, bd_x1])
        translate([x, bd_y, bd_z0])
            cube([tab_t, board_h + tab_t, bd_z1 - bd_z0]);

    translate([bd_x0 - tab_t, bd_y, bd_z0 - tab_t])
        cube([bd_x1 - bd_x0 + 2 * tab_t, board_h + tab_t, tab_t]);

    translate([bd_x0 - tab_t, bd_y, bd_z1])
        cube([bd_x1 - bd_x0 + 2 * tab_t, board_h + tab_t, tab_t]);

    for (z = [bd_z0 + 3.0, bd_z1 - 3.0 - tab_w]) {
        translate([bd_x0, bd_y - 1.2, z])          cube([tab_w, 1.2, tab_w]);
        translate([bd_x1 - tab_w, bd_y - 1.2, z])  cube([tab_w, 1.2, tab_w]);
    }
}

module body() {
    union() {
    difference() {
        // Outer shell.
        cube([outer_w, outer_d, outer_h]);

        // Main cavity, open at the back.
        translate([wall, wall, top_wall])
            cube([inner_w, inner_d + wall, outer_h - top_wall - wall]);

        // Three lamp bores, centred in the lamp section so the top bore
        // clears the button pocket and the bottom one clears the screen.
        for (i = [0 : 2])
            lens_bore(lamp_z(i));

        // Button: pocket for the switch, hole for the plunger, and a dish
        // around it so the cap sits below the top face.
        translate([outer_w / 2, outer_d / 2, outer_h - top_wall - btn_h])
            cube([btn_body + fit, btn_body + fit, btn_h + 1], center = true);
        translate([outer_w / 2, outer_d / 2, outer_h - top_wall - 0.5])
            cylinder(d = plunger_dia + fit, h = top_wall + 1);
        translate([outer_w / 2, outer_d / 2, outer_h - btn_recess_d])
            cylinder(d = btn_recess, h = btn_recess_d + 1);

        // Screen window in the front face.
        translate([(outer_w - screen_w) / 2, -1, scr_sect / 2 - screen_h / 2])
            cube([screen_w, wall + 2, screen_h]);

        // USB-C openings in the base: board, then charger.
        translate([(outer_w - usb_w) / 2 - 6, outer_d / 2 - 4, -1])
            cube([usb_w, usb_h, wall + 2]);
        translate([(outer_w - usb_w) / 2 + 6, outer_d - wall - 5, -1])
            cube([usb_w, usb_h, wall + 2]);

        // Back cover rebate.
        translate([wall - fit, outer_d - wall, wall])
            cube([inner_w + 2 * fit, wall + 1, outer_h - 2 * wall]);
    }

    // Rib beside the button pocket, so the top does not flex.
    translate([outer_w / 2 - btn_body, outer_d / 2 - 1.5, outer_h - top_wall - btn_h - 3])
        cube([2, 3, 3]);
    translate([outer_w / 2 + btn_body - 2, outer_d / 2 - 1.5, outer_h - top_wall - btn_h - 3])
        cube([2, 3, 3]);

    // Screw bosses for the back cover.
    for (x = [wall + 3, outer_w - wall - 3], z = [wall + 3, outer_h - wall - 3])
        translate([x, outer_d - wall - 3, z])
            difference() {
                cylinder(d = 5, h = 3, center = true);
                cylinder(d = 1.7, h = 4, center = true);   // M2 self-tap
            }

    // Board rails and tabs, added after the cavity is cut.
    board_retention();

    // Visors, one per lamp.
    for (i = [0 : 2])
        visor(lamp_z(i));
    }
}

module lens() {
    // Diffuser. Print at 15% infill, 2 perimeters, no top or bottom layers.
    difference() {
        cylinder(d = lens_dia - fit, h = lens_thick);
        translate([0, 0, -0.1])
            cylinder(d = led_hole + 0.3, h = 3);
    }
}

module back() {
    difference() {
        union() {
            cube([inner_w + 2 * wall, wall, outer_h - 2 * wall + 2 * wall]);
            // Lip that sits inside the rebate.
            translate([wall, -wall + 0.1, wall])
                cube([inner_w - 2 * fit, wall, outer_h - 2 * wall]);
        }
        // Screw holes.
        for (x = [wall + 3, inner_w + wall - 3], z = [wall + 3, outer_h - wall - 3])
            translate([x, -1, z])
                rotate([-90, 0, 0])
                    cylinder(d = 2.2, h = wall + 2);
        // Vent slot near the charger.
        translate([inner_w / 2 - 6, -1, wall + 6])
            cube([12, wall + 2, 2]);
    }
}

module plunger() {
    // Drops into the top face and rests on the switch actuator.
    cylinder(d = 4.0 - fit, h = top_wall + 2);
    translate([0, 0, top_wall + 2])
        cylinder(d = 6.0, h = 1.2);          // cap, sits proud of the case
    translate([0, 0, -1.5])
        cylinder(d = 5.5, h = 1.5);          // shoulder, stops it falling out
}

/* ---------------------------------------------------------------- */

if (part == "body")         body();
else if (part == "lens")    lens();
else if (part == "back")    back();
else if (part == "plunger") plunger();
else {
    body();
    translate([outer_w + 10, 0, 0])  for (i = [0:2]) translate([0, i * 14, 0]) lens();
    translate([0, -outer_d - 10, 0]) back();
    translate([outer_w + 30, 0, 0])  plunger();
}
