//go:build windows

package winapp

import (
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	enumMu    sync.Mutex
	enumCb    uintptr
	enumTitle string
	enumFound syscall.Handle
	enumReady sync.Once
)

// findWindowByTitle 按窗口标题查找顶层窗口。
func findWindowByTitle(title string) syscall.Handle {
	enumReady.Do(func() {
		enumCb = syscall.NewCallback(func(hwnd syscall.Handle, lparam uintptr) uintptr {
			n, _, _ := procGetWindowTextLen.Call(uintptr(hwnd))
			if n == 0 {
				return 1
			}
			buf := make([]uint16, n+1)
			procGetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), n+1)
			if syscall.UTF16ToString(buf) == enumTitle {
				enumFound = hwnd
				return 0 // 找到即停止枚举
			}
			return 1
		})
	})

	enumMu.Lock()
	defer enumMu.Unlock()
	enumTitle = title
	enumFound = 0
	procEnumWindows.Call(enumCb, 0)
	return enumFound
}

// applyIcon 把窗口图标换成我们自己的图标，解决任务栏图标模糊的问题。
// 关键点：按窗口所在显示器的 DPI 算出任务栏真正要画的像素尺寸（150% 缩放时是 36px），
// 直接喂一张同尺寸的图标，Windows 就不会再插值放大。
func applyIcon(hwnd syscall.Handle) bool {
	dpi := dpiFor(hwnd)
	big := iconFor(taskbarIconPx(dpi))
	small := iconFor(16 * dpi / 96)
	if big != 0 {
		procSendMessageW.Call(uintptr(hwnd), wmSetIcon, iconBig, uintptr(big))
	}
	if small != 0 {
		procSendMessageW.Call(uintptr(hwnd), wmSetIcon, iconSmall, uintptr(small))
	}
	got, _, _ := procSendMessageW.Call(uintptr(hwnd), wmGetIcon, iconBig, 0)
	return got == uintptr(big)
}

// watchWindow 持续跟踪目标窗口：接管图标，并在用户点最小化时把窗口藏到托盘。
func watchWindow() {
	var hwnd syscall.Handle
	wasVisible := false
	hiddenAt := time.Time{}
	iconLogged := false
	reapplied := false

	for {
		if hwnd == 0 || !isWindow(hwnd) {
			hwnd = findWindowByTitle(windowTitle)
			if hwnd != 0 {
				ok := applyIcon(hwnd)
				if trayOpts.Logf != nil && !iconLogged {
					iconLogged = true
					trayOpts.Logf("已接管窗口图标 hwnd=%d 生效=%v", hwnd, ok)
				}
				wasVisible = false
			}
			time.Sleep(300 * time.Millisecond)
			continue
		}

		// 页面加载完成后 Chrome 会用 favicon 覆盖窗口图标，
		// 这里每秒校验一次，被覆盖就立刻抢回来（只在变化时写日志）
		want := iconFor(taskbarIconPx(dpiFor(hwnd)))
		if got, _, _ := procSendMessageW.Call(uintptr(hwnd), wmGetIcon, iconBig, 0); got != uintptr(want) {
			applyIcon(hwnd)
			if trayOpts.Logf != nil && !reapplied {
				reapplied = true
				trayOpts.Logf("检测到窗口图标被覆盖，已重新接管")
			}
		}

		visible := isWindowVisible(hwnd)
		iconic := isIconic(hwnd)

		// 记录"窗口确实显示过"，避免启动瞬间被误判为最小化
		if visible && !iconic {
			wasVisible = true
		}

		// 只有在显示过、且当前处于最小化（仍 visible）时才隐藏到托盘
		if wasVisible && visible && iconic && time.Since(hiddenAt) > 2*time.Second {
			procShowWindow.Call(uintptr(hwnd), swHide)
			hiddenAt = time.Now()
			wasVisible = false
			if trayOpts.Logf != nil {
				trayOpts.Logf("窗口已最小化到托盘 hwnd=%d", hwnd)
			}
			Notify("SSH Proxy", "已最小化到托盘，代理继续在后台运行")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func isWindow(hwnd syscall.Handle) bool {
	r, _, _ := procIsWindow.Call(uintptr(hwnd))
	return r != 0
}

func isIconic(hwnd syscall.Handle) bool {
	r, _, _ := procIsIconic.Call(uintptr(hwnd))
	return r != 0
}

func isWindowVisible(hwnd syscall.Handle) bool {
	r, _, _ := procIsWindowVisible.Call(uintptr(hwnd))
	return r != 0
}

// FocusExisting 如果目标窗口已存在，就恢复并置于前台；返回是否找到。
func FocusExisting(title string) bool {
	hwnd := findWindowByTitle(title)
	if hwnd == 0 {
		return false
	}
	procShowWindow.Call(uintptr(hwnd), swRestore)
	procShowWindow.Call(uintptr(hwnd), swShowNormal)
	procSetForeground.Call(uintptr(hwnd))
	return true
}

// HideToTray 立即把窗口藏起来（用于"关闭界面但保持后台运行"）。
func HideToTray(title string) bool {
	hwnd := findWindowByTitle(title)
	if hwnd == 0 {
		return false
	}
	procShowWindow.Call(uintptr(hwnd), swHide)
	return true
}
