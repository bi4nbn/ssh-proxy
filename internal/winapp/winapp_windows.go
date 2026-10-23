//go:build windows

// Package winapp 提供 Windows 原生外壳能力：系统托盘图标、窗口图标接管、
// 最小化到托盘、隐藏控制台。全部通过 syscall 调用 Win32，不依赖 CGO。
package winapp

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassExW  = user32.NewProc("RegisterClassExW")
	procCreateWindowExW   = user32.NewProc("CreateWindowExW")
	procDefWindowProcW    = user32.NewProc("DefWindowProcW")
	procGetMessageW       = user32.NewProc("GetMessageW")
	procTranslateMessage  = user32.NewProc("TranslateMessage")
	procDispatchMessageW  = user32.NewProc("DispatchMessageW")
	procPostQuitMessage   = user32.NewProc("PostQuitMessage")
	procPostMessageW      = user32.NewProc("PostMessageW")
	procCreatePopupMenu   = user32.NewProc("CreatePopupMenu")
	procAppendMenuW       = user32.NewProc("AppendMenuW")
	procTrackPopupMenu    = user32.NewProc("TrackPopupMenu")
	procDestroyMenu       = user32.NewProc("DestroyMenu")
	procGetCursorPos      = user32.NewProc("GetCursorPos")
	procSetForeground     = user32.NewProc("SetForegroundWindow")
	procRegisterWindowMsg = user32.NewProc("RegisterWindowMessageW")
	procLoadImageW        = user32.NewProc("LoadImageW")
	procDestroyIcon       = user32.NewProc("DestroyIcon")
	procSendMessageW      = user32.NewProc("SendMessageW")
	procShowWindow        = user32.NewProc("ShowWindow")
	procIsIconic          = user32.NewProc("IsIconic")
	procIsWindow          = user32.NewProc("IsWindow")
	procIsWindowVisible   = user32.NewProc("IsWindowVisible")
	procEnumWindows       = user32.NewProc("EnumWindows")
	procGetWindowTextW    = user32.NewProc("GetWindowTextW")
	procGetWindowTextLen  = user32.NewProc("GetWindowTextLengthW")
	procMessageBoxW       = user32.NewProc("MessageBoxW")
	procGetSystemMetrics  = user32.NewProc("GetSystemMetrics")

	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procGetConsolePids   = kernel32.NewProc("GetConsoleProcessList")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")

	procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")
)

// Win32 常量
const (
	wmDestroy     = 0x0002
	wmNull        = 0x0000
	wmApp         = 0x8000
	wmSetIcon     = 0x0080
	wmGetIcon     = 0x007F
	wmContextMenu = 0x007B

	iconSmall = 0
	iconBig   = 1

	swHide       = 0
	swShowNormal = 1
	swRestore    = 9

	imageIcon = 1

	lrLoadFromFile = 0x0010

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfChecked   = 0x0008
	mfSeparator = 0x0800
	mfDefault   = 0x1000

	tpmLeftAlign   = 0x0000
	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	nimAdd    = 0x0000
	nimModify = 0x0001
	nimDelete = 0x0002

	nifMessage = 0x0001
	nifIcon    = 0x0002
	nifTip     = 0x0004
	nifInfo    = 0x0010

	niifInfo = 0x00000001

	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205

	smCxSmIcon = 49
	smCySmIcon = 50
)

// 托盘菜单命令 ID
const (
	CmdOpen   = 1
	CmdToggle = 2
	CmdQuit   = 3
)

// MenuItem 是一个托盘菜单项。
type MenuItem struct {
	ID        int
	Label     string
	Separator bool
	Checked   bool
	Disabled  bool
	Default   bool
}

// Options 配置托盘行为。
type Options struct {
	IconPath  string
	Tooltip   func() string
	Menu      func() []MenuItem
	OnCommand func(id int)
	OnOpen    func() // 单击/双击托盘图标
	Logf      func(format string, args ...any)
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     syscall.Handle
	HIcon         syscall.Handle
	HCursor       syscall.Handle
	HbrBackground syscall.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       syscall.Handle
}

type msgStruct struct {
	HWnd    syscall.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

type point struct{ X, Y int32 }

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type notifyIconData struct {
	CbSize           uint32
	HWnd             syscall.Handle
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            syscall.Handle
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         guid
	HBalloonIcon     syscall.Handle
}

var (
	trayHWnd    syscall.Handle
	trayIconBig syscall.Handle
	trayIconSm  syscall.Handle
	trayOpts    Options
	taskbarMsg  uint32
	trayAdded   bool
	trayMu      sync.Mutex
	quitOnce    sync.Once
	windowTitle string
	windowWatch bool
)

// HideOwnConsole 如果控制台窗口是独占的（双击启动），就把它藏起来，
// 实现"后台静默运行"；从终端启动时保持可见，方便看日志。
func HideOwnConsole() {
	var pids [4]uint32
	n, _, _ := procGetConsolePids.Call(uintptr(unsafe.Pointer(&pids[0])), 4)
	if n != 1 {
		return
	}
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd != 0 {
		procShowWindow.Call(hwnd, swHide)
	}
}

// MessageBox 弹出系统提示框（无控制台时用于报错）。
func MessageBox(title, text string) {
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString(title)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), 0x10)
}

func loadIcon(path string, cx, cy int32) syscall.Handle {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	h, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(p)), imageIcon,
		uintptr(cx), uintptr(cy), lrLoadFromFile)
	return syscall.Handle(h)
}

// Start 初始化托盘图标并在独立线程上运行消息循环（非阻塞）。
// watchTitle 非空时，会接管该标题窗口的图标并把最小化动作转为隐藏到托盘。
func Start(opt Options, watchTitle string) error {
	windowTitle = watchTitle
	trayOpts = opt
	if trayOpts.Logf == nil {
		trayOpts.Logf = func(string, ...any) {}
	}

	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := initTray(); err != nil {
			ready <- err
			return
		}
		ready <- nil
		messageLoop()
	}()
	return <-ready
}

// initTray 注册窗口类、创建消息窗口、加载图标并挂上托盘图标。
func initTray() error {
	hInst, _, _ := procGetModuleHandleW.Call(0)

	className, _ := syscall.UTF16PtrFromString("SSHProxyTrayWindow")
	wc := wndClassEx{
		CbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		LpfnWndProc:   syscall.NewCallback(wndProc),
		HInstance:     syscall.Handle(hInst),
		LpszClassName: className,
	}
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("注册窗口类失败: %v", err)
	}

	const hwndMessage = ^uintptr(2) // HWND_MESSAGE
	hwnd, _, err := procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(className)), 0, 0, 0, 0, 0, 0,
		hwndMessage, 0, hInst, 0)
	if hwnd == 0 {
		return fmt.Errorf("创建消息窗口失败: %v", err)
	}
	trayHWnd = syscall.Handle(hwnd)

	if r, _, _ := procRegisterWindowMsg.Call(uintptr(unsafe.Pointer(mustPtr("TaskbarCreated")))); r != 0 {
		taskbarMsg = uint32(r)
	}

	cx, _, _ := procGetSystemMetrics.Call(smCxSmIcon)
	cy, _, _ := procGetSystemMetrics.Call(smCySmIcon)
	trayIconSm = loadIcon(trayOpts.IconPath, int32(cx), int32(cy))
	trayIconBig = loadIcon(trayOpts.IconPath, 32, 32)
	if trayIconSm == 0 {
		return fmt.Errorf("加载托盘图标失败: %s", trayOpts.IconPath)
	}

	if err := addTrayIcon(); err != nil {
		return err
	}

	// 图标接管 + 最小化到托盘
	if windowTitle != "" {
		windowWatch = true
		go watchWindow()
	}
	// 定期刷新提示文字（显示当前连接状态）
	go refreshLoop()
	return nil
}

func messageLoop() {
	var msg msgStruct
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func mustPtr(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func wndProc(hwnd syscall.Handle, msg uint32, wparam, lparam uintptr) uintptr {
	switch {
	case msg == wmApp+1:
		switch lparam {
		case wmRButtonUp, wmContextMenu:
			showMenu(hwnd)
		case wmLButtonUp, wmLButtonDblClk:
			if trayOpts.OnOpen != nil {
				go trayOpts.OnOpen()
			}
		}
		return 0
	case taskbarMsg != 0 && msg == taskbarMsg:
		trayAdded = false
		_ = addTrayIcon()
		return 0
	case msg == wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wparam, lparam)
	return r
}

func addTrayIcon() error {
	trayMu.Lock()
	defer trayMu.Unlock()

	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = trayHWnd
	nid.UID = 1
	nid.UFlags = nifMessage | nifIcon | nifTip
	nid.UCallbackMessage = wmApp + 1
	nid.HIcon = trayIconSm
	tip := "SSH Proxy"
	if trayOpts.Tooltip != nil {
		tip = trayOpts.Tooltip()
	}
	copy(nid.SzTip[:], syscall.StringToUTF16(tip))

	op := uintptr(nimAdd)
	if trayAdded {
		op = nimModify
	}
	r, _, err := procShellNotifyIcon.Call(op, uintptr(unsafe.Pointer(&nid)))
	if r == 0 {
		return fmt.Errorf("添加托盘图标失败: %v", err)
	}
	trayAdded = true
	return nil
}

// SetTooltip 更新托盘提示文字。
func SetTooltip(tip string) {
	trayMu.Lock()
	defer trayMu.Unlock()
	if !trayAdded {
		return
	}
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = trayHWnd
	nid.UID = 1
	nid.UFlags = nifTip
	copy(nid.SzTip[:], syscall.StringToUTF16(tip))
	procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

// Notify 弹出托盘气泡提示。
func Notify(title, text string) {
	trayMu.Lock()
	defer trayMu.Unlock()
	if !trayAdded {
		return
	}
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = trayHWnd
	nid.UID = 1
	nid.UFlags = nifInfo
	nid.DwInfoFlags = niifInfo
	copy(nid.SzInfoTitle[:], syscall.StringToUTF16(title))
	copy(nid.SzInfo[:], syscall.StringToUTF16(text))
	procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

func showMenu(hwnd syscall.Handle) {
	items := []MenuItem{}
	if trayOpts.Menu != nil {
		items = trayOpts.Menu()
	}
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	// 保持字符串指针存活，直到菜单销毁
	keep := make([]*uint16, 0, len(items))
	for _, it := range items {
		if it.Separator {
			procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
			continue
		}
		flags := uintptr(mfString)
		if it.Checked {
			flags |= mfChecked
		}
		if it.Disabled {
			flags |= mfGrayed
		}
		if it.Default {
			flags |= mfDefault
		}
		p := mustPtr(it.Label)
		keep = append(keep, p)
		procAppendMenuW.Call(hMenu, flags, uintptr(it.ID), uintptr(unsafe.Pointer(p)))
	}

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForeground.Call(uintptr(hwnd))
	cmd, _, _ := procTrackPopupMenu.Call(hMenu,
		tpmReturnCmd|tpmRightButton|tpmLeftAlign,
		uintptr(pt.X), uintptr(pt.Y), 0, uintptr(hwnd), 0)
	procPostMessageW.Call(uintptr(hwnd), wmNull, 0, 0)

	if cmd != 0 && trayOpts.OnCommand != nil {
		go trayOpts.OnCommand(int(cmd))
	}
}

func refreshLoop() {
	for i := 0; ; i++ {
		time.Sleep(2 * time.Second)
		if trayOpts.Tooltip != nil {
			SetTooltip(trayOpts.Tooltip())
		}
	}
}

// Quit 移除托盘图标并退出进程。
func Quit() {
	quitOnce.Do(func() {
		trayMu.Lock()
		if trayAdded {
			var nid notifyIconData
			nid.CbSize = uint32(unsafe.Sizeof(nid))
			nid.HWnd = trayHWnd
			nid.UID = 1
			procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
			trayAdded = false
		}
		trayMu.Unlock()
		if trayHWnd != 0 {
			procPostMessageW.Call(uintptr(trayHWnd), wmDestroy, 0, 0)
		}
		if trayIconSm != 0 {
			procDestroyIcon.Call(uintptr(trayIconSm))
		}
		if trayIconBig != 0 {
			procDestroyIcon.Call(uintptr(trayIconBig))
		}
	})
}
