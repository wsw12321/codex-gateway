package gatewayplugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

var errStrictJSON = errors.New("invalid control document")

// decodeObject parses one JSON object and rejects duplicate keys instead of
// letting encoding/json silently keep the last value of an ambiguous control
// document. With fields, exactly those keys must be present.
func decodeObject(data []byte, fields ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errStrictJSON
	}
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err = decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, errStrictJSON
		}
		if _, exists := object[key]; exists {
			return nil, errStrictJSON
		}
		if len(fields) > 0 {
			allowed := false
			for _, field := range fields {
				allowed = allowed || key == field
			}
			if !allowed {
				return nil, errStrictJSON
			}
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return nil, errStrictJSON
		}
		object[key] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errStrictJSON
	}
	if _, err = decoder.Token(); err != io.EOF || len(fields) > 0 && len(object) != len(fields) {
		return nil, errStrictJSON
	}
	return object, nil
}

// readControlFile reads a regular control document of bounded size. A missing
// file is reported with exists=false; any other problem is an error, because an
// unreadable existing document must never be treated as an empty one.
func readControlFile(path string, requireMode os.FileMode) (data []byte, exists bool, err error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil, false, errStrictJSON
	}
	if requireMode != 0 && info.Mode().Perm() != requireMode {
		return nil, false, errStrictJSON
	}
	data, err = os.ReadFile(path) // #nosec G304 -- fixed file name inside the configured credential directory
	if err != nil {
		return nil, false, errStrictJSON
	}
	return data, true, nil
}

// writeControlFile atomically replaces path: mode-0600 temporary file, file
// sync, rename, then directory sync. The change is durable before it is
// acknowledged.
func writeControlFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, filepath.Base(path)+".")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if errClose := file.Close(); err == nil {
		err = errClose
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(dir) // #nosec G304 -- the configured credential directory
	if err != nil {
		return err
	}
	err = directory.Sync()
	if errClose := directory.Close(); err == nil {
		err = errClose
	}
	return err
}
