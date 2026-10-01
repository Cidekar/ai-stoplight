package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// receipt is the durable record of what one Install created.
//
// The three createdFile, createdHooks and createdEvents flags on Adapter are
// the only thing that lets Uninstall tell a file, a "hooks" key or an event
// array it introduced from one the user wrote, because after our entries are
// pruned the two are byte-for-byte identical. Those flags live only in process
// memory, and `stoplight install` and `stoplight uninstall` are separate
// processes, so a command-line uninstall used to start blank and could prove
// nothing was ours. It therefore left every emptied array in place and never
// removed a file it created, which is the gap this record closes.
//
// The record is written beside the state directory rather than inside
// settings.json: Claude Code rewrites that file itself and drops any key it
// does not know, so a marker stored there would not survive to be read back.
type receipt struct {
	// CreatedFile mirrors Adapter.createdFile: Install found no settings file
	// at the path, so the one there now is ours to remove.
	CreatedFile bool `json:"createdFile"`
	// CreatedHooks mirrors Adapter.createdHooks: Install added the top-level
	// "hooks" key, so emptying it may remove it again.
	CreatedHooks bool `json:"createdHooks"`
	// CreatedEvents mirrors Adapter.createdEvents: the event keys Install
	// added. Only these may be deleted at uninstall.
	CreatedEvents []string `json:"createdEvents"`
}

// receiptPath resolves the file that records what Install created for the
// settings file this adapter edits.
//
// It is derived from the settings path, not stored once, so a test that binds
// an adapter to a temporary settings.json keeps its receipt in the same
// temporary directory and never touches the real one. The name embeds the
// settings file's own name so two adapters editing two files in one directory,
// which only tests do, do not share a record.
func (a *Adapter) receiptPath() (string, error) {
	if a.path != "" {
		return a.path + ".stoplight-install.json", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "stoplight", "claude-code.install.json"), nil
}

// saveReceipt records what this Install created so a later uninstall, in a
// fresh process, can act on it.
//
// When nothing was created there is nothing to record and no file is written,
// so an install that only refreshes existing entries leaves no record to clean
// up. A record that is absent at uninstall is read as "created nothing", which
// is the conservative state the adapter started from before this file existed.
func (a *Adapter) saveReceipt() error {
	path, err := a.receiptPath()
	if err != nil {
		return err
	}
	events := make([]string, 0, len(a.createdEvents))
	for k := range a.createdEvents {
		events = append(events, k)
	}
	// Sort so the file is stable across runs and easy to read.
	sort.Strings(events)

	r := receipt{
		CreatedFile:   a.createdFile,
		CreatedHooks:  a.createdHooks,
		CreatedEvents: events,
	}
	// Nothing was created, so there is nothing a later uninstall needs told.
	// Writing an all-false record would only be a file to clean up.
	if !r.CreatedFile && !r.CreatedHooks && len(r.CreatedEvents) == 0 {
		return nil
	}

	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode the install record: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// loadReceipt seeds this adapter's created flags from a record a previous
// Install wrote, so an uninstall in a fresh process has the same knowledge the
// installing process had.
//
// A missing record is normal: either Install created nothing, or an older
// version wrote none. In that case the flags stay at their conservative zero
// values and uninstall leaves anything it cannot prove is ours in place.
func (a *Adapter) loadReceipt() error {
	path, err := a.receiptPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	var r receipt
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	a.createdFile = r.CreatedFile
	a.createdHooks = r.CreatedHooks
	if a.createdEvents == nil {
		a.createdEvents = map[string]bool{}
	}
	for _, e := range r.CreatedEvents {
		a.createdEvents[e] = true
	}
	return nil
}

// removeReceipt deletes the install record once uninstall has acted on it, so
// a later install that creates nothing does not inherit a stale one.
func (a *Adapter) removeReceipt() error {
	path, err := a.receiptPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
