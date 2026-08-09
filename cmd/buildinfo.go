package cmd

import "runtime/debug"

// resolveVersion returns the CLI's version, commit, and build date.
//
// goreleaser injects Version/Commit/Date via ldflags for released binaries
// (GitHub Release + Homebrew tap). For a `go install <module>@<tag>` build the
// ldflags are absent (Version stays "dev"), so fall back to the module version
// and VCS metadata the Go toolchain embeds in every binary -- giving go-install
// users a real "vX.Y.Z" instead of "dev".
func resolveVersion() (version, commit, date string) {
	version, commit, date = Version, Commit, Date
	if version != "dev" {
		return // released binary: the ldflag values are authoritative
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	// `go install module@vX.Y.Z` records the tag here; a local `go build`
	// records "(devel)", which we leave as "dev".
	if v := info.Main.Version; v != "" && v != "(devel)" {
		version = v
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) > 7 {
				commit = s.Value[:7]
			} else if s.Value != "" {
				commit = s.Value
			}
		case "vcs.time":
			date = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				commit += "-dirty"
			}
		}
	}
	return
}
