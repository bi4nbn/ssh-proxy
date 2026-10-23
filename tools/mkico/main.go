// mkico 把 PNG 转成多尺寸 Windows ICO（16/24/32/48/64/128 用 BMP，256 用 PNG）。
//
// 用法：
//
//	go run ./tools/mkico assets/icon-256.png build/app.ico
//	go run ./tools/mkico --dir assets build/app.ico      # 优先用目录里的 icon-N.png 原图
//	go run ./tools/mkico --png assets/icon-256.png web   # 导出网页 favicon
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
)

// sizes 覆盖所有常见 DPI 缩放下的实际像素尺寸。
// 任务栏图标是 24 逻辑像素（Win11）/32 逻辑像素（Win10），
// 在 125%/150%/175%/200% 缩放下分别是 30/36/42/48 像素；
// 只要 ICO 里存在"完全一致"的尺寸，LoadImage 就不会二次缩放，图标才够锐利。
var sizes = func() []int {
	list := make([]int, 0, 64)
	for s := 16; s <= 64; s++ {
		list = append(list, s)
	}
	return append(list, 72, 80, 96, 128, 256)
}()

// entry 是 ICO 里的一张图。
type entry struct {
	size int
	data []byte
}

func main() {
	if len(os.Args) >= 4 && os.Args[1] == "--png" {
		dumpPNGs(os.Args[2], os.Args[3])
		return
	}
	if len(os.Args) >= 4 && os.Args[1] == "--dir" {
		buildFromDir(os.Args[2], os.Args[3])
		return
	}
	if len(os.Args) < 3 {
		fmt.Println("用法: mkico <src.png> <dst.ico>")
		fmt.Println("      mkico --dir <assets目录> <dst.ico>")
		fmt.Println("      mkico --png <src.png> <输出目录>")
		os.Exit(1)
	}
	buildFromPNG(os.Args[1], os.Args[2])
}

// buildFromPNG 用一张母版图（裁掉透明边后）缩放出全部尺寸。
func buildFromPNG(src, dst string) {
	img := mustDecode(src)
	if b := img.Bounds(); b.Dx() < 256 || b.Dy() < 256 {
		fmt.Printf("提示: 源图仅 %dx%d，放大后可能发虚\n", b.Dx(), b.Dy())
	}
	content := trim(img)
	var entries []entry
	for _, s := range sizes {
		entries = append(entries, makeEntry(s, resize(content, s)))
	}
	writeICO(dst, entries)
}

// buildFromDir 用目录里最大的那张图作为母版合成 ICO。
//
// 为什么不用官方逐尺寸的小图：Microsoft 打包的 Square44x44Logo 系列四周都留了
// 约 10% 透明边（"plated" 版式），直接拿去做任务栏图标会比 Chrome/微信小一圈，
// 看起来就像糊了。这里统一裁掉透明边、让图形铺满画布，再用面积平均缩放，
// 小尺寸全部是"从大图缩小"，不会出现放大导致的发虚。
func buildFromDir(dir, dst string) {
	var master image.Image
	masterSize := 0
	found := 0
	for _, s := range sizes {
		img, err := tryDecode(filepath.Join(dir, fmt.Sprintf("icon-%d.png", s)))
		if err != nil {
			continue
		}
		found++
		if b := img.Bounds(); b.Dx() >= masterSize {
			master, masterSize = img, b.Dx()
		}
	}
	if master == nil {
		panic("目录里没有可用的 icon-N.png")
	}
	content := trim(master)
	cb := content.Bounds()
	fmt.Printf("目录内 %d 张原图，取 %dpx 作母版，裁边后 %dx%d\n", found, masterSize, cb.Dx(), cb.Dy())

	var entries []entry
	for _, s := range sizes {
		entries = append(entries, makeEntry(s, resize(content, s)))
	}
	writeICO(dst, entries)
}

// trim 裁掉四周的透明边，让图形铺满整个画布。
func trim(src image.Image) image.Image {
	b := src.Bounds()
	nrgba := toNRGBA(src)
	minX, minY, maxX, maxY := b.Max.X, b.Max.Y, b.Min.X-1, b.Min.Y-1
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if nrgba.Pix[nrgba.PixOffset(x, y)+3] <= 16 {
				continue
			}
			if x < minX {
				minX = x
			}
			if x > maxX {
				maxX = x
			}
			if y < minY {
				minY = y
			}
			if y > maxY {
				maxY = y
			}
		}
	}
	if maxX < minX || maxY < minY {
		return nrgba
	}
	// 外扩 1px，保留边缘抗锯齿
	if minX > b.Min.X {
		minX--
	}
	if minY > b.Min.Y {
		minY--
	}
	if maxX < b.Max.X-1 {
		maxX++
	}
	if maxY < b.Max.Y-1 {
		maxY++
	}
	return nrgba.SubImage(image.Rect(minX, minY, maxX+1, maxY+1))
}

// makeEntry 把一张图编码成 ICO 条目：>=256 用 PNG 压缩，其余用 32bpp BMP。
func makeEntry(size int, img image.Image) entry {
	if size >= 256 {
		var buf bytes.Buffer
		if err := png.Encode(&buf, toNRGBA(img)); err != nil {
			panic(err)
		}
		return entry{size, buf.Bytes()}
	}
	return entry{size, encodeBMP(toNRGBA(img))}
}

// writeICO 按 ICO 规范写出文件。
func writeICO(dst string, entries []entry) {
	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, uint16(0)) // reserved
	_ = binary.Write(&out, binary.LittleEndian, uint16(1)) // type=icon
	_ = binary.Write(&out, binary.LittleEndian, uint16(len(entries)))

	offset := 6 + 16*len(entries)
	for _, e := range entries {
		w := byte(e.size)
		if e.size >= 256 {
			w = 0
		}
		out.WriteByte(w)                                        // width
		out.WriteByte(w)                                        // height
		out.WriteByte(0)                                        // 调色板数
		out.WriteByte(0)                                        // 保留
		_ = binary.Write(&out, binary.LittleEndian, uint16(1))  // planes
		_ = binary.Write(&out, binary.LittleEndian, uint16(32)) // bpp
		_ = binary.Write(&out, binary.LittleEndian, uint32(len(e.data)))
		_ = binary.Write(&out, binary.LittleEndian, uint32(offset))
		offset += len(e.data)
	}
	for _, e := range entries {
		out.Write(e.data)
	}

	if err := os.WriteFile(dst, out.Bytes(), 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("已生成 %s（%d 个尺寸，共 %d 字节）\n", dst, len(entries), out.Len())
}

func mustDecode(path string) image.Image {
	img, err := tryDecode(path)
	if err != nil {
		panic(fmt.Sprintf("读取 %s 失败: %v", path, err))
	}
	return img
}

func tryDecode(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

// toNRGBA 把任意 image 统一成 NRGBA。
func toNRGBA(src image.Image) *image.NRGBA {
	if n, ok := src.(*image.NRGBA); ok {
		return n
	}
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			dst.Set(x, y, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

// dumpPNGs 把源图按 favicon 常用尺寸导出为 PNG。
func dumpPNGs(src, outDir string) {
	img := trim(mustDecode(src))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		panic(err)
	}
	for _, s := range []int{16, 32, 48, 64, 128} {
		small := resize(img, s)
		name := filepath.Join(outDir, fmt.Sprintf("favicon-%d.png", s))
		out, err := os.Create(name)
		if err != nil {
			panic(err)
		}
		if err := png.Encode(out, small); err != nil {
			out.Close()
			panic(err)
		}
		out.Close()
		fmt.Println("已生成", name)
	}
}

// resize 使用面积平均（box filter）缩放，缩小时不易出现锯齿。
func resize(src image.Image, size int) *image.NRGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	if sw == size && sh == size {
		return toNRGBA(src)
	}
	scaleX := float64(sw) / float64(size)
	scaleY := float64(sh) / float64(size)

	for y := 0; y < size; y++ {
		y0 := float64(y) * scaleY
		y1 := y0 + scaleY
		for x := 0; x < size; x++ {
			x0 := float64(x) * scaleX
			x1 := x0 + scaleX
			var r, g, b, a, wsum float64
			for sy := int(y0); sy < int(y1+0.9999) && sy < sh; sy++ {
				wy := overlap(float64(sy), float64(sy+1), y0, y1)
				if wy <= 0 {
					continue
				}
				for sx := int(x0); sx < int(x1+0.9999) && sx < sw; sx++ {
					wx := overlap(float64(sx), float64(sx+1), x0, x1)
					w := wx * wy
					if w <= 0 {
						continue
					}
					cr, cg, cb, ca := src.At(sb.Min.X+sx, sb.Min.Y+sy).RGBA()
					// 预乘 alpha 参与加权，避免透明边缘发黑
					r += float64(cr) * w
					g += float64(cg) * w
					b += float64(cb) * w
					a += float64(ca) * w
					wsum += w
				}
			}
			if wsum == 0 {
				continue
			}
			alpha := a / wsum // 0..65535
			idx := dst.PixOffset(x, y)
			if alpha > 0 {
				// 反预乘回 straight alpha，并把 16 位分量压到 8 位
				k := 65535 / alpha / 257
				dst.Pix[idx+0] = clamp8(r / wsum * k)
				dst.Pix[idx+1] = clamp8(g / wsum * k)
				dst.Pix[idx+2] = clamp8(b / wsum * k)
			}
			dst.Pix[idx+3] = clamp8(alpha / 257)
		}
	}
	return dst
}

func overlap(a0, a1, b0, b1 float64) float64 {
	lo := a0
	if b0 > lo {
		lo = b0
	}
	hi := a1
	if b1 < hi {
		hi = b1
	}
	if hi > lo {
		return hi - lo
	}
	return 0
}

func clamp8(v float64) byte {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return byte(v + 0.5)
}

// encodeBMP 生成 ICO 内部的 32 位 BMP（含 AND 掩码）。
func encodeBMP(img *image.NRGBA) []byte {
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()
	var buf bytes.Buffer

	// BITMAPINFOHEADER
	_ = binary.Write(&buf, binary.LittleEndian, uint32(40))
	_ = binary.Write(&buf, binary.LittleEndian, int32(w))
	_ = binary.Write(&buf, binary.LittleEndian, int32(h*2)) // XOR + AND
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0)) // BI_RGB
	_ = binary.Write(&buf, binary.LittleEndian, uint32(w*h*4))
	_ = binary.Write(&buf, binary.LittleEndian, int32(2835))
	_ = binary.Write(&buf, binary.LittleEndian, int32(2835))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))

	// 像素数据：自下而上，BGRA
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			i := img.PixOffset(x, y)
			buf.WriteByte(img.Pix[i+2])
			buf.WriteByte(img.Pix[i+1])
			buf.WriteByte(img.Pix[i+0])
			buf.WriteByte(img.Pix[i+3])
		}
	}

	// AND 掩码：每行按 4 字节对齐，全 0（完全由 alpha 决定）
	rowBytes := ((w + 31) / 32) * 4
	mask := make([]byte, rowBytes*h)
	buf.Write(mask)

	return buf.Bytes()
}
