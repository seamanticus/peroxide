package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

func Path(filename string) (string, error) {
	configHome, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}

	p := filepath.Join(configHome, "peroxide", filename)

	dirname := filepath.Dir(p)
	if err := os.MkdirAll(dirname, 0700); err != nil {
		return "", err
	}
	if err := ownerOnly(dirname); err != nil {
		return "", err
	}
	return p, nil
}

// ownerOnly makes dir, which exists, unreadable to all but its owner.
// MkdirAll leaves a directory that exists as it is: one made by hand or by
// an older peroxide may let others list the account names and read whatever
// file isn't 0600. A directory closed to others already is left alone, so
// that one peroxide may not change (another owner's, a read-only mount)
// still works; one open to others that can't be closed is an error. A
// symbolic link is followed: the directory it names is checked.
func ownerOnly(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("%v is open to other users (mode %o) and can't be restricted to its owner: %w", dir, fi.Mode().Perm(), err)
	}
	return nil
}

// DataPath returns the path of name in peroxide's data directory, where what
// it keeps of the user's mail lives: $XDG_DATA_HOME/peroxide if
// XDG_DATA_HOME is set to an absolute path (the XDG Base Directory
// specification ignores a relative one), else ~/.local/share/peroxide. Mail
// is data, not cache: it belongs where backups look. The directory is
// created, readable by its owner only; name itself is not.
func DataPath(name string) (string, error) {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(dataHome) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataHome = filepath.Join(home, ".local", "share")
	}

	dir := filepath.Join(dataHome, "peroxide")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	// As in Path: the sealed messages' names are Proton's message IDs.
	if err := ownerOnly(dir); err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}
