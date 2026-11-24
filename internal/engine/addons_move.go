package engine

import (
	"fmt"
	"os"
	"path/filepath"
)

func isStackConfigFile(name string) bool {
	if name == ".env" {
		return true
	}
	for _, cf := range ComposeFileNames {
		if name == cf {
			return true
		}
	}
	return false
}

// RelocateBindMounts moves any non-compose / non-.env files and subdirectories inside
// <stacksDir>/<fromStack>/ into <stacksDir>/add-ons/<addonID>/ so relative bind mounts
// rewritten as ./<addonID>/... keep pointing at the existing data without copying.
func (e *Engine) RelocateBindMounts(fromStack, addonID string) error {
	if e.cfg.ReadOnly {
		return ErrReadOnly
	}
	if e.IsSelf(fromStack) || fromStack == "add-ons" {
		return ErrSelfStack
	}
	if !nameRe.MatchString(fromStack) || !nameRe.MatchString(addonID) {
		return ErrInvalidName
	}
	srcDir, err := e.stackDir(fromStack)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var toMove []string
	for _, ent := range entries {
		if isStackConfigFile(ent.Name()) {
			continue
		}
		toMove = append(toMove, ent.Name())
	}
	if len(toMove) == 0 {
		return nil
	}
	dstBase := filepath.Join(e.cfg.StacksDir, "add-ons", addonID)
	if !within(e.cfg.StacksDir, dstBase) {
		return ErrInvalidName
	}
	if err := os.MkdirAll(dstBase, 0o750); err != nil {
		return fmt.Errorf("prepare add-on data dir: %w", err)
	}
	var moved []string
	for _, item := range toMove {
		oldPath := filepath.Join(srcDir, item)
		newPath := filepath.Join(dstBase, item)
		if err := os.Rename(oldPath, newPath); err != nil {
			for i := len(moved) - 1; i >= 0; i-- {
				_ = os.Rename(filepath.Join(dstBase, moved[i]), filepath.Join(srcDir, moved[i]))
			}
			return fmt.Errorf("relocate %s: %w", item, err)
		}
		moved = append(moved, item)
	}
	return nil
}

// RollbackBindMounts moves relocated bind-mount entries from <stacksDir>/add-ons/<addonID>/
// back to <stacksDir>/<fromStack>/ when a Move to add-ons deploy fails.
func (e *Engine) RollbackBindMounts(fromStack, addonID string) error {
	if !nameRe.MatchString(fromStack) || !nameRe.MatchString(addonID) {
		return ErrInvalidName
	}
	srcDir, err := e.stackDir(fromStack)
	if err != nil {
		return err
	}
	dstBase := filepath.Join(e.cfg.StacksDir, "add-ons", addonID)
	entries, err := os.ReadDir(dstBase)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	_ = os.MkdirAll(srcDir, 0o750)
	for _, ent := range entries {
		_ = os.Rename(filepath.Join(dstBase, ent.Name()), filepath.Join(srcDir, ent.Name()))
	}
	_ = os.Remove(dstBase)
	return nil
}
