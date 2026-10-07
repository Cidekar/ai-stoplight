//go:build race

package stoplight

// raceEnabled reports whether the test binary was built with -race. The race
// detector adds large, variable overhead, so a wall-clock budget that is fair
// without it is not fair under it. Tests that measure elapsed time widen their
// budget when this is true.
const raceEnabled = true
