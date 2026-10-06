package clients

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/oauth/jsoncedit"
)

func exists(path string) bool { _, err := os.Stat(path); return err == nil }
func readText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(b), err
}
func objectText(text string) (map[string]any, error) {
	if strings.TrimSpace(text) == "" {
		return map[string]any{}, nil
	}
	var v map[string]any
	if err := config.UnmarshalJSONC([]byte(text), &v); err != nil {
		return nil, err
	}
	if v == nil {
		return nil, fmt.Errorf("client config must be a JSON object")
	}
	return v, nil
}
func readObject(path string) map[string]any {
	text, _ := readText(path)
	v, _ := objectText(text)
	if v == nil {
		return map[string]any{}
	}
	return v
}

// stripJSONC decodes comments and trailing commas from the edited output; a
// JSONC write is valid only when this cleaned parse succeeds.
func stripObject(text string) (map[string]any, error) {
	var v map[string]any
	clean, err := config.StripJSONC([]byte(text))
	if err == nil {
		err = json.Unmarshal(clean, &v)
	}
	if err == nil && v == nil {
		err = fmt.Errorf("client config must be a JSON object")
	}
	return v, err
}
func jsonText(value any) (string, error) {
	b, err := json.MarshalIndent(value, "", "  ")
	return string(b) + "\n", err
}
func writeText(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Atomic replacement prevents a partially written client config. Do not follow
	// an existing symlink when publishing the result.
	f, err := os.CreateTemp(filepath.Dir(path), ".jevonian-client-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err = f.Chmod(mode); err == nil {
		_, err = f.WriteString(text)
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
func writeJSON(path string, value any) error {
	text, err := jsonText(value)
	if err != nil {
		return err
	}
	return writeText(path, text)
}
func (m *Manager) backup(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	stamp := strings.NewReplacer(":", "-", ".", "-").Replace(m.opts.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	// Keep the source in place until a successful atomic write, and never overwrite
	// a previous backup when the clock has millisecond resolution.
	for i := 0; ; i++ {
		suffix := ""
		if i > 0 {
			suffix = fmt.Sprintf("-%d", i)
		}
		name := path + ".jevonian-backup-" + stamp + suffix
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		ce := f.Close()
		if err == nil {
			err = ce
		}
		return err
	}
}
func (m *Manager) replace(path, text string) error {
	current, err := readText(path)
	if err != nil {
		return err
	}
	if current == text && exists(path) {
		return nil
	}
	if err = m.backup(path); err != nil {
		return err
	}
	return writeText(path, text)
}
func (m *Manager) removeBackedUp(path string) error {
	if !exists(path) {
		return nil
	}
	if err := m.backup(path); err != nil {
		return err
	}
	return os.Remove(path)
}

type restoreState struct {
	Client  string            `json:"client"`
	SavedAt string            `json:"savedAt"`
	Files   map[string]string `json:"files"`
	// Applied allows exact byte restoration only while the file is unchanged.
	// Edits made after Connect are restored surgically, not clobbered.
	Applied map[string]string `json:"applied,omitempty"`
	Existed map[string]bool   `json:"existed,omitempty"`
}

func (m *Manager) statePath(client string) string {
	return filepath.Join(m.StateDir(), client+"-restore.json")
}
func (m *Manager) loadState(client string) (*restoreState, error) {
	b, err := os.ReadFile(m.statePath(client))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s restoreState
	if err = json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Files == nil {
		return nil, fmt.Errorf("invalid %s restore state", client)
	}
	return &s, nil
}
func (m *Manager) saveState(client string, files []string) error {
	if exists(m.statePath(client)) {
		_, err := m.loadState(client)
		return err
	}
	s := restoreState{Client: client, SavedAt: m.opts.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Files: map[string]string{}, Existed: map[string]bool{}}
	for _, p := range files {
		text, err := readText(p)
		if err != nil {
			return err
		}
		s.Files[p] = text
		s.Existed[p] = exists(p)
	}
	return writeJSON(m.statePath(client), s)
}
func (m *Manager) recordApplied(client string, files []string) error {
	s, err := m.loadState(client)
	if err != nil || s == nil {
		return err
	}
	if s.Applied == nil {
		s.Applied = map[string]string{}
	}
	for _, p := range files {
		text, err := readText(p)
		if err != nil {
			return err
		}
		s.Applied[p] = text
	}
	return writeJSON(m.statePath(client), s)
}
func (m *Manager) clearState(client string) error { return m.removeBackedUp(m.statePath(client)) }
func originalFile(s *restoreState, path string) (text string, known, existed bool) {
	if s == nil {
		return "", false, false
	}
	text, known = s.Files[path]
	existed = text != ""
	if e, ok := s.Existed[path]; ok {
		existed = e
	}
	return
}

// exactRestore preserves the TS byte-for-byte restore contract while refusing to
// discard unrelated settings added since Connect.
func (m *Manager) exactRestore(s *restoreState, path string) (bool, error) {
	original, known, existed := originalFile(s, path)
	if !known {
		return false, nil
	}
	current, err := readText(path)
	if err != nil {
		return false, err
	}
	applied, recorded := s.Applied[path]
	if !recorded || current != applied {
		return false, nil
	}
	if !existed {
		return true, m.removeBackedUp(path)
	}
	return true, m.replace(path, original)
}
func jsonEdits(values map[string]any) []jsoncedit.Edit {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	edits := make([]jsoncedit.Edit, 0, len(keys))
	for _, k := range keys {
		edits = append(edits, jsoncedit.Edit{Path: []string{k}, Value: values[k]})
	}
	return edits
}
func jsonEqual(a, b any) bool {
	rawA, _ := json.Marshal(a)
	rawB, _ := json.Marshal(b)
	return string(rawA) == string(rawB)
}
func child(v any, key string) (any, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	value, ok := obj[key]
	return value, ok
}
func editedObject(original string, edits []jsoncedit.Edit) (string, error) {
	if _, err := objectText(original); err != nil {
		return "", err
	}
	if original == "" {
		original = "{}\n"
	}
	next := jsoncedit.ApplyEdits(original, edits)
	// Verify on strict-cleaned JSON what the editor wrote: malformed originals
	// that made edits unresolvable stay untouched, and generated JSONC must be
	// decodable before touching the user's file.
	obj, err := stripObject(next)
	if err != nil {
		return "", err
	}
	for i, e := range edits {
		// A later descendant edit deliberately changes this parent's final value.
		parent := false
		for _, later := range edits[i+1:] {
			if len(later.Path) <= len(e.Path) {
				continue
			}
			prefix := true
			for j, key := range e.Path {
				if later.Path[j] != key {
					prefix = false
					break
				}
			}
			if prefix {
				parent = true
				break
			}
		}
		if parent {
			continue
		}
		v := any(obj)
		found := true
		for _, k := range e.Path {
			v, found = child(v, k)
			if !found {
				break
			}
		}
		if e.Remove {
			if found {
				return "", fmt.Errorf("unable to remove client config path %v", e.Path)
			}
			continue
		}
		if !found || !jsonEqual(v, e.Value) {
			return "", fmt.Errorf("unable to edit client config path %v", e.Path)
		}
	}
	return next, nil
}
func (m *Manager) restoreJSON(s *restoreState, path string, paths [][]string, cleanEnv bool) error {
	if done, err := m.exactRestore(s, path); done || err != nil {
		return err
	}
	if !exists(path) {
		return nil
	}
	current, err := readText(path)
	if err != nil {
		return err
	}
	original, known, _ := originalFile(s, path)
	before := map[string]any{}
	if known {
		before, err = objectText(original)
		if err != nil {
			return err
		}
	}
	edits := []jsoncedit.Edit{}
	for _, p := range paths {
		v := any(before)
		found := known
		for _, k := range p {
			o, ok := v.(map[string]any)
			if !ok {
				found = false
				break
			}
			v, ok = o[k]
			if !ok {
				found = false
				break
			}
		}
		edits = append(edits, jsoncedit.Edit{Path: p, Value: v, Remove: !found})
	}
	next, err := editedObject(current, edits)
	if err != nil {
		return err
	}
	if cleanEnv {
		if keys := jsoncedit.ObjectKeys(next, []string{"env"}); keys != nil && len(keys) == 0 {
			next = jsoncedit.ApplyEdits(next, []jsoncedit.Edit{{Path: []string{"env"}, Remove: true}})
		}
	}
	return m.replace(path, next)
}
