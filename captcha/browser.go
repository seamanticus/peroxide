package captcha

import (
	"fmt"
	"os"
	"os/exec"
)

// openURL asks the desktop environment to open a URL in the user's browser,
// through xdg-open, inside a graphical session only.
//
// It only ever receives the local http://127.0.0.1 URL, never one carrying a
// token or a credential.
func openURL(url string) error {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return fmt.Errorf("no graphical session detected; use -captcha-mode manual")
	}
	const name = "xdg-open"

	path, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("%v not found: %v", name, err)
	}

	cmd := exec.Command(path, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Don't block on a browser that stays in the foreground.
	go cmd.Wait()
	return nil
}
