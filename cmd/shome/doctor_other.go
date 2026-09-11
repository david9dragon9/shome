//go:build !darwin && !linux

package main

func doctorPlatform(check func(name string, status rune, detail, fix string)) {
	check("platform", 'f', "unsupported operating system",
		"shome node backends exist for macOS and Linux only")
}
