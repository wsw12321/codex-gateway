package cpamigrate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFinishWriteCleansCredentialsAndReportsFailures(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"temporary", ""},
		{"renamed", ""},
		{"unlink-failure", "file_cleanup_failed"},
		{"close-failure", "file_close_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			d, err := openDirectory(dir, os.Geteuid())
			if err != nil {
				t.Fatal(err)
			}
			defer d.close()
			const name = ".migrate-private-fixture"
			path := filepath.Join(dir, name)
			f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if tc.name == "renamed" || tc.name == "unlink-failure" {
				if err := os.Rename(path, filepath.Join(dir, "credential.json")); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "unlink-failure" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "close-failure" {
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			err = d.finishWrite(f, name)
			if (err != nil) != (tc.want != "") || (err != nil && err.Error() != tc.want) {
				t.Fatalf("finishWrite error = %v, want %q", err, tc.want)
			}
			if tc.name != "unlink-failure" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("temporary credential remains: %v", err)
				}
			}
		})
	}
}

func TestRemoveStagingDeletesCredentialsAndRedactsFailures(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "credential"), []byte("private-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeStaging(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("staging directory remains: %v", err)
	}
	if err := removeStaging(home); err != nil {
		t.Fatalf("absent staging directory: %v", err)
	}
	if err := removeStaging(home + "\x00private-fixture"); err == nil || err.Error() != "staging_cleanup_failed" {
		t.Fatalf("cleanup failure was not redacted: %v", err)
	}
}
