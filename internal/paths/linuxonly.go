//go:build !linux

package paths

// peroxide is Linux only: it relies on flock(2), XDG directories and
// xdg-open, and is tested on Linux alone. This file is compiled only for
// other systems, where the undefined name below stops the build with the
// reason in the error. It lives in paths, which every command imports and
// which imports no other peroxide package, so it is among the first errors.
const _ = peroxide_builds_for_linux_only
