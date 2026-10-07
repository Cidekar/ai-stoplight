//go:build !race

package stoplight

// raceEnabled is false in a build without -race. See race_on_test.go.
const raceEnabled = false
