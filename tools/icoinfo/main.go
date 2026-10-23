// icoinfo 解析 ICO 文件，把每个尺寸导出为 PNG，便于肉眼校验。
// 用法：go run ./tools/icoinfo build/app.ico build/ico-check
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: icoinfo <file.ico> [输出目录]")
		os.Exit(1)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	outDir := "."
	if len(os.Args) > 2 {
		outDir = os.Args[2]
	}
	_ = os.MkdirAll(outDir, 0o755)

	if len(raw) < 6 {
		panic("文件太小")
	}
	count := int(binary.LittleEndian.Uint16(raw[4:6]))
	fmt.Printf("图标数量: %d\n", count)

	for i := 0; i < count; i++ {
		off := 6 + i*16
		if off+16 > len(raw) {
			break
		}
		w := int(raw[off])
		h := int(raw[off+1])
		if w == 0 {
			w = 256
		}
		if h == 0 {
			h = 256
		}
		size := int(binary.LittleEndian.Uint32(raw[off+8 : off+12]))
		start := int(binary.LittleEndian.Uint32(raw[off+12 : off+16]))
		if start+size > len(raw) {
			fmt.Printf("  #%d %dx%d 数据越界，跳过\n", i, w, h)
			continue
		}
		data := raw[start : start+size]
		var img image.Image
		if bytes.HasPrefix(data, []byte("\x89PNG")) {
			img, err = png.Decode(bytes.NewReader(data))
			if err != nil {
				fmt.Printf("  #%d %dx%d PNG 解码失败: %v\n", i, w, h, err)
				continue
			}
		} else {
			img, err = decodeBMPEntry(data, w, h)
			if err != nil {
				fmt.Printf("  #%d %dx%d BMP 解码失败: %v\n", i, w, h, err)
				continue
			}
		}
		name := filepath.Join(outDir, fmt.Sprintf("icon-%dx%d.png", w, h))
		f, err := os.Create(name)
		if err != nil {
			panic(err)
		}
		_ = png.Encode(f, img)
		f.Close()
		fmt.Printf("  #%d %dx%d %d 字节 -> %s\n", i, w, h, size, name)
	}
}

// decodeBMPEntry 解析 ICO 内嵌的 32 位 BMP（自下而上 BGRA）。
func decodeBMPEntry(data []byte, w, h int) (image.Image, error) {
	const headerLen = 40
	if len(data) < headerLen {
		return nil, fmt.Errorf("头部不完整")
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	pos := headerLen
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			if pos+4 > len(data) {
				return img, fmt.Errorf("像素数据不完整")
			}
			b := data[pos]
			g := data[pos+1]
			r := data[pos+2]
			a := data[pos+3]
			pos += 4
			img.Set(x, y, color.NRGBA{R: r, G: g, B: b, A: a})
		}
	}
	return img, nil
}
