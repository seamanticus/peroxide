package paths

import (
	"os"
	"path/filepath"
)

func Path(filename string) (string, error) {
	configHome, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}

	p := filepath.Join(configHome, "peroxide", filename)

	dirname, _ := filepath.Split(p)
	if err := os.MkdirAll(dirname, 0700); err != nil {
		return "", err
	}

	return p, nil
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
	return filepath.Join(dir, name), nil
}
