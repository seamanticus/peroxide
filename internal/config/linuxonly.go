//go:build !linux

package config

// peroxide is Linux only: it relies on flock(2), XDG directories and
// xdg-open, and is tested on Linux alone. This file is compiled only for
// other systems, where the undefined name below stops the build with the
// reason in the error. It lives in config, which every command imports and
// which is compiled first, so it is the first error on every system.
const _ = peroxide_builds_for_linux_only
