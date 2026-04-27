package buildinfo

import (
	"fmt"
	"runtime/debug"
)

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func Effective() (version, commit, date string) {
	version, commit, date = Version, Commit, Date

	if info, ok := debug.ReadBuildInfo(); ok {
		if version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if commit == "unknown" && setting.Value != "" {
					commit = setting.Value
				}
			case "vcs.time":
				if date == "unknown" && setting.Value != "" {
					date = setting.Value
				}
			case "vcs.modified":
				if setting.Value == "true" {
					version += "+dirty"
				}
			}
		}
	}

	return version, commit, date
}

func Summary(name string) string {
	version, commit, date := Effective()
	return fmt.Sprintf("%s version=%s commit=%s date=%s", name, version, commit, date)
}
