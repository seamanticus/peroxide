package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// auth.json holds the sealed credentials and the usernames in clear: only
// its owner may read it, whether saveAuths creates it or rewrites one an
// older peroxide left readable by everyone, whatever the umask.
// evidence: witnesses
func TestSaveAuthsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	for _, tc := range []struct {
		name     string
		existing os.FileMode // 0: no file yet
	}{
		{"new file", 0},
		{"existing 0644", 0o644},
		{"existing 0666", 0o666},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// os.UserConfigDir reads XDG_CONFIG_HOME on Unix, and HOME on
			// macOS: point both away from the real ones.
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			p, err := authFilePath()
			if err != nil {
				t.Fatal(err)
			}
			if tc.existing != 0 {
				if err := os.WriteFile(p, []byte("{}"), tc.existing); err != nil {
					t.Fatal(err)
				}
				// WriteFile's mode is masked by the umask: set it exactly.
				if err := os.Chmod(p, tc.existing); err != nil {
					t.Fatal(err)
				}
			}

			if err := saveAuths(map[string]string{"bob": "sealed"}); err != nil {
				t.Fatalf("saveAuths() = %v", err)
			}
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != authFileMode {
				t.Errorf("%v mode = %v, want %v", filepath.Base(p), got, os.FileMode(authFileMode))
			}
			auths, err := readCachedAuths()
			if err != nil || auths["bob"] != "sealed" {
				t.Errorf("readCachedAuths() = %v, %v, want the saved record", auths, err)
			}
		})
	}
}
