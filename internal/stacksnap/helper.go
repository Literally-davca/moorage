package stacksnap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Job is what the engine hands the helper container (a JSON file in the snapshot folder).
type Job struct {
	Op       string            `json:"op"` // create | restore
	Manifest Manifest          `json:"manifest"`
	Dirs     map[string]string `json:"dirs"` // part name -> directory inside the helper
	File     string            `json:"file"` // snapshot path inside the helper
}

// PartialSuffix marks a snapshot being written; leftovers are removed at start.
const PartialSuffix = ".partial"

// RunHelper is `moorage snapshot-helper <job.json>`: it creates or restores one snapshot.
func RunHelper(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: snapshot-helper <job.json>")
	}
	b, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var j Job
	if err := json.Unmarshal(b, &j); err != nil {
		return fmt.Errorf("job: %w", err)
	}
	switch j.Op {
	case "create":
		return create(j)
	case "restore":
		f, err := os.Open(j.File)
		if err != nil {
			return err
		}
		defer f.Close()
		return Extract(f, j.Dirs)
	}
	return fmt.Errorf("unknown op %q", j.Op)
}

func create(j Job) error {
	if !strings.HasSuffix(j.File, ".tar.zst") {
		return errors.New("snapshot file must end in .tar.zst")
	}
	tmp := j.File + PartialSuffix
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	m := j.Manifest
	m.File = filepath.Base(j.File)
	werr := Write(f, &m, j.Dirs)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp)
		return errors.Join(werr, cerr)
	}
	st, err := os.Stat(tmp)
	if err != nil {
		return err
	}
	m.Size = st.Size()
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(ManifestPath(j.File), mb, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, j.File)
}

// ManifestPath is the manifest next to a snapshot file.
func ManifestPath(file string) string { return strings.TrimSuffix(file, ".tar.zst") + ".json" }
