//go:build windows

package winapp

import (
	"syscall"
	"unsafe"
)

var procCreateMutexW = kernel32.NewProc("CreateMutexW")

const errorAlreadyExists = 183

var instanceHandle syscall.Handle

// AcquireSingleInstance 用内核命名互斥体保证全局只有一个实例。
//
// 相比"探测端口是否被占用"，互斥体是原子的：两个进程同时启动也只会有一个成功；
// 进程被强杀时系统会自动释放，不会留下"僵尸锁"。
// 优先用 Global\ 命名空间（跨登录会话，含远程桌面），没有权限时退回 Local\（当前会话）。
//
// 返回 true 表示本进程是唯一实例；false 表示已经有实例在跑，本进程应立即退出。
func AcquireSingleInstance(name string) bool {
	for _, full := range []string{`Global\` + name, `Local\` + name, name} {
		p, err := syscall.UTF16PtrFromString(full)
		if err != nil {
			continue
		}
		h, _, lastErr := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(p)))
		if h == 0 {
			// 这个命名空间建不了（通常是权限不足），换下一个
			continue
		}
		// LazyProc.Call 的第三个返回值是 error（errno 为 0 时为 nil），需要断言回 Errno
		if errno, ok := lastErr.(syscall.Errno); ok && errno == errorAlreadyExists {
			// 别人已经持有了；关掉自己这份句柄即可
			syscall.CloseHandle(syscall.Handle(h))
			return false
		}
		instanceHandle = syscall.Handle(h)
		return true
	}
	// 三种命名空间都建不出来（极端情况），放行以免程序打不开
	return true
}
