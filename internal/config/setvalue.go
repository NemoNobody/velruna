package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SetConfigValue writes one top-level key into an existing config file,
// leaving every other value exactly as it was. It returns the previous value
// of the key ("" when the key was absent) so the caller can say what it is
// replacing — a keyring entry under a superseded store name is not deleted,
// and the operator has to learn it remains. `creds set` uses it to record
// opensearch_cluster next to the endpoints it belongs to; a config that
// carries a plaintext password keeps its 0600 shape and the write is atomic
// (temp file in the same directory, then rename), because a half-written
// config would leave the user without any store at all. The rewrite goes
// through a Go map, so json.Marshal prints top-level keys ALPHABETICALLY:
// values survive, the file's key order is normalized.
func SetConfigValue(path, key, value string) (string, error) {
	previous := ""
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read config %s: %w", path, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		// A config we cannot parse must not be rewritten: the loader would
		// already have refused it, so this is a hand-edited file and the
		// operator gets to fix it themselves.
		return "", fmt.Errorf("parse config %s (refusing to rewrite it): %w", path, err)
	}
	if doc == nil { // a file holding "null"
		doc = map[string]json.RawMessage{}
	}
	if raw, ok := doc[key]; ok {
		var old string
		if err := json.Unmarshal(raw, &old); err == nil {
			previous = old
		}
		if string(raw) == mustJSON(value) {
			return previous, nil // already recorded: an idempotent rewrite re-stamps nothing
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return previous, fmt.Errorf("encode %s: %w", key, err)
	}
	doc[key] = encoded

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return previous, fmt.Errorf("encode config %s: %w", path, err)
	}
	out = append(out, '\n')

	mode := os.FileMode(0o600)
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp")
	if err != nil {
		return previous, fmt.Errorf("create temp next to %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return previous, fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return previous, fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return previous, fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return previous, fmt.Errorf("replace %s: %w", path, err)
	}
	tmpName = ""
	return previous, nil
}

// mustJSON renders a string as its JSON form for the "already recorded"
// comparison; json.Marshal of a string cannot fail.
func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return "\x00impossible"
	}
	return string(b)
}
