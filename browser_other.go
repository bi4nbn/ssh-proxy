//go:build !windows

package main

import (
	"os/exec"
	"runtime"
)

// openDefault 用系统默认浏览器打开地址。
func openDefault(url string) {
	bin := "xdg-open"
	if runtime.GOOS == "darwin" {
		bin = "open"
	}
	_ = exec.Command(bin, url).Start()
}
