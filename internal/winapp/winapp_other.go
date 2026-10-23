//go:build !windows

// Package winapp 在非 Windows 平台提供空实现，保证代码可交叉编译。
package winapp

// 菜单命令 ID（与 Windows 实现保持一致）
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
	OnOpen    func()
	Logf      func(format string, args ...any)
}

func HideOwnConsole()                            {}
func AttachParentConsole() bool                  { return false }
func EnableDPIAwareness()                        {}
func MessageBox(title, text string)              {}
func Start(opt Options, watchTitle string) error { return nil }
func SetTooltip(tip string)                      {}
func Notify(title, text string)                  {}
func Quit()                                      {}
func FocusExisting(title string) bool            { return false }
func HideToTray(title string) bool               { return false }
