//go:build !windows

package winapp

// AcquireSingleInstance 在非 Windows 平台不做限制（主用途是 Windows 桌面常驻）。
func AcquireSingleInstance(name string) bool { return true }
