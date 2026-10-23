// mkico 把一张 PNG 转成多尺寸 Windows ICO（16/24/32/48/64/128 用 BMP，256 用 PNG）。
// 用法：go run ./tools/mkico assets/icon-256.png build/app.ico
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"os"
)

var sizes = []int{16, 24, 32, 48, 64, 128, 256}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("用法: mkico <src.png> <dst.ico>")
		os.Exit(1)
	}
	src := os.Args[1]
	dst := os.Args[2]

	f, err := os.Open(src)
	if err != nil {
		panic(err)
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		panic(err)
	}
	if b := img.Bounds(); b.Dx() < 256 || b.Dy() < 256 {
		fmt.Printf("提示: 源图仅 %dx%d，放大后可能发虚\n", b.Dx(), b.Dy())
	}

	type entry struct {
		size int
		data []byte
	}
	var entries []entry
	for _, s := range sizes {
		small := resize(img, s)
		if s >= 256 {
			var buf bytes.Buffer
			if err := png.Encode(&buf, small); err != nil {
				panic(err)
			}
			entries = append(entries, entry{s, buf.Bytes()})
			continue
		}
		entries = append(entries, entry{s, encodeBMP(small)})
	}

	// ICO 头部
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
		out.WriteByte(w)          // width
		out.WriteByte(w)          // height
		out.WriteByte(0)          // palette
		out.WriteByte(0)          // reserved
		_ = binary.Write(&out, binary.LittleEndian, uint16(1))    // planes
		_ = binary.Write(&out, binary.LittleEndian, uint16(32))   // bpp
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

// resize 使用面积平均（box filter）缩放，缩小时不易出现锯齿。
func resize(src image.Image, size int) *image.NRGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
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
