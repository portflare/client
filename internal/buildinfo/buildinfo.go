package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

type Module struct {
	Path    string  `json:"path"`
	Version string  `json:"version,omitempty"`
	Sum     string  `json:"sum,omitempty"`
	Replace *Module `json:"replace,omitempty"`
}

type Setting struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type ReadyInfo struct {
	OK          bool      `json:"ok"`
	Application string    `json:"application"`
	Version     string    `json:"version"`
	Commit      string    `json:"commit"`
	BuildTime   string    `json:"build_time"`
	GoVersion   string    `json:"go_version"`
	BuildInfoOK bool      `json:"build_info_ok"`
	Path        string    `json:"path,omitempty"`
	Main        Module    `json:"main"`
	Settings    []Setting `json:"settings,omitempty"`
	Deps        []Module  `json:"deps,omitempty"`
}

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

func Ready(application string) ReadyInfo {
	version, commit, buildTime := Effective()
	ready := ReadyInfo{
		OK:          true,
		Application: application,
		Version:     version,
		Commit:      commit,
		BuildTime:   buildTime,
		GoVersion:   runtime.Version(),
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		ready.BuildInfoOK = true
		ready.GoVersion = info.GoVersion
		ready.Path = info.Path
		ready.Main = moduleFromDebug(info.Main)
		ready.Settings = make([]Setting, 0, len(info.Settings))
		for _, setting := range info.Settings {
			ready.Settings = append(ready.Settings, Setting{Key: setting.Key, Value: setting.Value})
		}
		ready.Deps = make([]Module, 0, len(info.Deps))
		for _, dep := range info.Deps {
			if dep != nil {
				ready.Deps = append(ready.Deps, moduleFromDebug(*dep))
			}
		}
	}
	return ready
}

func moduleFromDebug(module debug.Module) Module {
	out := Module{Path: module.Path, Version: module.Version, Sum: module.Sum}
	if module.Replace != nil {
		replace := moduleFromDebug(*module.Replace)
		out.Replace = &replace
	}
	return out
}

func Summary(name string) string {
	version, commit, date := Effective()
	return fmt.Sprintf("%s version=%s commit=%s date=%s", name, version, commit, date)
}
