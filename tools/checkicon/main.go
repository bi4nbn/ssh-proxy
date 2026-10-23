// checkicon 通过 Windows API 检查 exe 内是否真的嵌入了图标与版本资源。
// 用法：go run ./tools/checkicon ssh-proxy.exe
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLoadLibEx    = kernel32.NewProc("LoadLibraryExW")
	procFindResource = kernel32.NewProc("FindResourceW")
	procSizeofRes    = kernel32.NewProc("SizeofResource")
	procLoadRes      = kernel32.NewProc("LoadResource")
	procLockRes      = kernel32.NewProc("LockResource")
)

const (
	rtIcon      = 3
	rtGroupIcon = 14
	rtVersion   = 16
	loadAsData  = 0x00000002 // LOAD_LIBRARY_AS_DATAFILE
)

func findRes(h uintptr, name, typ uintptr) (uintptr, uint32) {
	r, _, _ := procFindResource.Call(h, name, typ)
	if r == 0 {
		return 0, 0
	}
	sz, _, _ := procSizeofRes.Call(h, r, 0)
	lr, _, _ := procLoadRes.Call(h, r)
	ptr, _, _ := procLockRes.Call(lr)
	return ptr, uint32(sz)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: checkicon <exe>")
		os.Exit(1)
	}
	path, err := syscall.UTF16PtrFromString(os.Args[1])
	if err != nil {
		panic(err)
	}
	h, _, e := procLoadLibEx.Call(uintptr(unsafe.Pointer(path)), 0, loadAsData)
	if h == 0 {
		panic("无法加载文件: " + e.Error())
	}

	// RT_GROUP_ICON：图标组
	grp, grpSize := findRes(h, 1, rtGroupIcon)
	if grp == 0 {
		fmt.Println("✗ 未找到图标资源")
		os.Exit(2)
	}
	fmt.Printf("✓ 找到图标组资源，%d 字节\n", grpSize)

	// GRPICONDIR: reserved(2) type(2) count(2)，之后每项 14 字节
	data := unsafe.Slice((*byte)(unsafe.Pointer(grp)), grpSize)
	count := binary.LittleEndian.Uint16(data[4:6])
	fmt.Printf("  包含 %d 个尺寸：\n", count)
	for i := 0; i < int(count); i++ {
		off := 6 + i*14
		if off+14 > len(data) {
			break
		}
		w := data[off]
		hh := data[off+1]
		bits := binary.LittleEndian.Uint16(data[off+6 : off+8])
		bytes := binary.LittleEndian.Uint32(data[off+8 : off+12])
		id := binary.LittleEndian.Uint16(data[off+12 : off+14])
		wid, hid := int(w), int(hh)
		if wid == 0 {
			wid = 256
		}
		if hid == 0 {
			hid = 256
		}
		// 校验对应的 RT_ICON 数据确实存在
		ptr, sz := findRes(h, uintptr(id), rtIcon)
		ok := "✓"
		if ptr == 0 {
			ok = "✗"
		}
		fmt.Printf("   %s %dx%d  %d bpp  资源ID=%d  实际数据=%d 字节(声明 %d)\n", ok, wid, hid, bits, id, sz, bytes)
	}

	if ptr, sz := findRes(h, 1, rtVersion); ptr != 0 {
		fmt.Printf("✓ 版本信息资源存在，%d 字节\n", sz)
	} else {
		fmt.Println("· 无版本信息资源")
	}
}
