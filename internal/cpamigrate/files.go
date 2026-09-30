package cpamigrate

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const maxFileSize = 1 << 20

// Keep a directory descriptor across validation and every operation. Reject
// symlinks in every path component, hardlinks, unexpected owners and modes.
type privateDir struct {
	file *os.File
	uid  int
}

func openDirectory(path string, uid int) (*privateDir, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
		return nil, errors.New("unsafe_directory")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("directory_unavailable")
	}
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd) // The read-only parent descriptor is no longer needed.
		if err != nil {
			return nil, errors.New("unsafe_directory")
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), "private-directory")
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || int(st.Uid) != uid || st.Mode&07777 != 0700 {
		_ = f.Close()
		return nil, errors.New("directory_permissions_invalid")
	}
	return &privateDir{f, uid}, nil
}

func (d *privateDir) close() { _ = d.file.Close() }
func validFilename(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsAny(name, "/\x00")
}

func (d *privateDir) read(name string) ([]byte, error) {
	if !validFilename(name) {
		return nil, errors.New("unsafe_file")
	}
	fd, err := unix.Openat(int(d.file.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, errors.New("unsafe_file")
	}
	f := os.NewFile(uintptr(fd), "private-file")
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || int(st.Uid) != d.uid || st.Nlink != 1 || st.Size > maxFileSize {
		return nil, errors.New("file_permissions_invalid")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil || len(raw) > maxFileSize {
		return nil, errors.New("file_read_failed")
	}
	return raw, nil
}

func (d *privateDir) write(name string, raw []byte) (err error) {
	if !validFilename(name) || len(raw) > maxFileSize {
		return errors.New("unsafe_file")
	}
	if _, err := d.read(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return errors.New("file_write_failed")
	}
	tmp := ".migrate-" + hex.EncodeToString(suffix[:])
	fd, err := unix.Openat(int(d.file.Fd()), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return errors.New("file_write_failed")
	}
	f := os.NewFile(uintptr(fd), "private-file")
	defer func() {
		if cleanupErr := d.finishWrite(f, tmp); cleanupErr != nil {
			err = cleanupErr
		}
	}()
	if os.Geteuid() == 0 {
		if err := unix.Fchown(fd, d.uid, d.uid); err != nil {
			return errors.New("file_owner_failed")
		}
	}
	if _, err := f.Write(raw); err != nil {
		return errors.New("file_write_failed")
	}
	if f.Sync() != nil || unix.Renameat(int(d.file.Fd()), tmp, int(d.file.Fd()), name) != nil || d.file.Sync() != nil {
		return errors.New("file_commit_failed")
	}
	return nil
}

func (d *privateDir) finishWrite(f *os.File, tmp string) error {
	closeErr := f.Close()
	if err := unix.Unlinkat(int(d.file.Fd()), tmp, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return errors.New("file_cleanup_failed")
	}
	if closeErr != nil {
		return errors.New("file_close_failed")
	}
	return nil
}

func (d *privateDir) lock() (*os.File, error) {
	const name = ".gateway-refresh.lock"
	fd, err := unix.Openat(int(d.file.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, errors.New("refresher_lock_unavailable")
	}
	f := os.NewFile(uintptr(fd), "refresh-lock")
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || st.Nlink != 1 {
		_ = f.Close()
		return nil, errors.New("refresher_lock_invalid")
	}
	if int(st.Uid) != d.uid {
		// Only a new root-owned empty lock may be assigned to the service UID.
		if os.Geteuid() != 0 || st.Uid != 0 || st.Size != 0 || unix.Fchown(fd, d.uid, d.uid) != nil {
			_ = f.Close()
			return nil, errors.New("refresher_lock_invalid")
		}
	}
	if unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) != nil {
		_ = f.Close()
		return nil, errors.New("refresher_still_running")
	}
	return f, nil
}

func uniqueJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return errors.New("json_invalid")
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		switch t {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				key, ok := k.(string)
				if err != nil || !ok || seen[key] {
					return errors.New("json_invalid")
				}
				seen[key] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("json_invalid")
			}
		case json.Delim('['):
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("json_invalid")
			}
		case json.Delim('}'), json.Delim(']'):
			return errors.New("json_invalid")
		}
		return nil
	}
	if err := value(0); err != nil {
		return errors.New("json_invalid")
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("json_invalid")
	}
	return nil
}
