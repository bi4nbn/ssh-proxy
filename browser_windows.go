//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// openDefault 用系统默认浏览器打开地址。
func openDefault(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}
