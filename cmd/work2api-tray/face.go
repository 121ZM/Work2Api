//go:build windows

package main

import (
	"unsafe"
)

// ── 猴子脸为什么是「光栅化字形」而不是画出来的 ──────────────────────────
//
// 拿圆和椭圆拼脸，出来的是通用卡通动物 —— 32px 下读不出猴子。这里换个路子：
// 把 Windows 自带 emoji 字体里 🐵(U+1F435) 的**单色字形**用 GDI 光栅化，
// 从覆盖率里切出「头/耳/脸盘/五官」，颜色再由我们自己按区域刷。
//
// GDI 拿不到 Segoe UI Emoji 的 COLR 彩色层（实测只有 6 级灰度），这反而正好 ——
// 我们要的就是那套线稿：笔画位置精确，着色随我们便。
//
// 实测的字形结构（按包围盒归一化，286×232px）：
//
//	头（粗描边圆）  圆心 (0.500, 0.474)  半径 u=0.406 / v=0.500
//	耳朵            在头赤道高度伸到 u=0.000 / 1.000（比头宽 0.094）
//	脸盘            u 0.26–0.74  v 0.17–0.81
//	眼睛            u 0.381–0.430 / 0.570–0.619，v 0.362–0.444
//	鼻孔            u 0.441–0.462 / 0.531–0.556，v 0.509–0.530
//	嘴（实心）      u 0.378–0.619，v 0.591–0.81
//
// 这些数字是常量里 emojiHeadCU 那一组，logo_test.go 会盯着它们别漂。

var (
	procSelectObject  = gdi32.NewProc("SelectObject")
	procDeleteObject  = gdi32.NewProc("DeleteObject")
	procSetBkMode     = gdi32.NewProc("SetBkMode")
	procSetTextColor  = gdi32.NewProc("SetTextColor")
	procCreateFontW   = gdi32.NewProc("CreateFontW")
	procTextOutW      = gdi32.NewProc("TextOutW")
	procGetTextExtent = gdi32.NewProc("GetTextExtentPoint32W")
)

// 字形包围盒相对字号的实测比例（字号 320 → 包围盒 286×232）。
// 往大取一点，一次就把画布开够，不用渲染两遍试探。
const (
	glyphBoxWPerFontH = 0.94
	glyphBoxHPerFontH = 0.76

	// 覆盖率大于它就算「墨线」。0.5 是抗锯齿边缘的自然分界。
	inkThreshold = 128
)

// emojiFace 是 🐵 字形切出来的两个场，坐标按字形包围盒归一化。
//
//	ink — 笔画覆盖率 0..255
//	cls — 分区：0=轮廓之外，128=头(毛)，255=脸盘
//
// cls 的两类都是洪泛来的：0 是从画布四周往里的可达区；
// 255 是从脸心往外的可达区（会被脸盘那圈墨线挡住，所以正好框住脸盘）。
type emojiFace struct {
	w, h int
	ink  []uint8
	cls  []uint8
}

// sample 在归一化坐标 (mu,mv) 处双线性采样。
// 返回 ok=false 表示落在包围盒之外（那里一定不是脸）。
func (f *emojiFace) sample(mu, mv float64) (ink, cls float64, ok bool) {
	if f == nil || mu < 0 || mu > 1 || mv < 0 || mv > 1 {
		return 0, 0, false
	}
	x := mu * float64(f.w-1)
	y := mv * float64(f.h-1)
	x0, y0 := int(x), int(y)
	if x0 > f.w-2 {
		x0 = f.w - 2
	}
	if y0 > f.h-2 {
		y0 = f.h - 2
	}
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	fx, fy := x-float64(x0), y-float64(y0)
	at := func(x, y int) (uint8, uint8) {
		i := y*f.w + x
		return f.ink[i], f.cls[i]
	}
	i00, c00 := at(x0, y0)
	i10, c10 := at(x0+1, y0)
	i01, c01 := at(x0, y0+1)
	i11, c11 := at(x0+1, y0+1)
	blend := func(a, b, c, d uint8) float64 {
		top := float64(a)*(1-fx) + float64(b)*fx
		bot := float64(c)*(1-fx) + float64(d)*fx
		return top*(1-fy) + bot*fy
	}
	return blend(i00, i10, i01, i11), blend(c00, c10, c01, c11), true
}

// loadEmojiFace 把 🐵 字形光栅化到大约 targetH 像素高，切成 ink/cls 两个场。
// 失败（字体缺失、GDI 拒绝）返回 nil，调用方退回兜底造型。
func loadEmojiFace(targetH int) *emojiFace {
	if targetH < 64 {
		targetH = 64
	}
	fontH := int(float64(targetH)/glyphBoxHPerFontH + 0.5)
	if fontH < 32 {
		fontH = 32
	}
	// 画布开成字号的两倍。字形宽约 0.94*字号，两侧都留得下 ——
	// 第一版画布按 1.1 倍开，结果耳朵被切平了，形状读不出来。
	canvas := 2 * fontH

	cov, bw, bh, ok := rasterizeGlyph(canvas, fontH)
	if !ok || bw < 8 || bh < 8 {
		return nil
	}
	n := bw * bh

	// 覆盖率直接照搬（源已经是目标像素量级，1:1 附近，不值得再过一道重采样）
	ink := make([]uint8, n)
	copy(ink, cov)

	// 「非墨」的四连通洪泛
	open := func(i int) bool { return ink[i] <= inkThreshold }
	outside := floodBool(n, bw, bh, open, func(i int) bool {
		return i < bw || i >= n-bw || i%bw == 0 || i%bw == bw-1
	})
	// 脸心：从几何中心往外找一个非墨点再洪泛
	seed := faceSeed(ink, bw, bh)
	if seed < 0 {
		return nil
	}
	faceIn := floodBool(n, bw, bh, open, func(i int) bool { return i == seed })

	cls := make([]uint8, n)
	for i := 0; i < n; i++ {
		if !open(i) {
			continue // 墨线待会儿按邻居补
		}
		switch {
		case faceIn[i]:
			cls[i] = 255
		case !outside[i]:
			cls[i] = 128
		}
	}
	// 墨线像素取「邻居里优先级最高的分区」：脸盘那圈墨线算脸、头外沿那圈算毛。
	// 这样着色边界落在笔画两侧，抗锯齿混出来的是颜色而不是半透明的毛边。
	for y := 0; y < bh; y++ {
		for x := 0; x < bw; x++ {
			i := y*bw + x
			if open(i) {
				continue
			}
			best := uint8(0)
			for _, d := range [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
				nx, ny := x+d[0], y+d[1]
				if nx < 0 || ny < 0 || nx >= bw || ny >= bh {
					continue
				}
				j := ny*bw + nx
				if !open(j) {
					continue
				}
				if cls[j] > best {
					best = cls[j]
				}
			}
			if best == 0 {
				best = 128 // 四周全是墨、或贴着画布边：当成毛
			}
			cls[i] = best
		}
	}

	return &emojiFace{w: bw, h: bh, ink: ink, cls: cls}
}

// rasterizeGlyph 把 🐵 画到 canvas×canvas 的 32bpp DIB（纯白底、黑字），
// 返回覆盖率与实心包围盒尺寸。
func rasterizeGlyph(canvas, fontH int) (cov []uint8, bw, bh int, ok bool) {
	const (
		fwNormal    = 400
		outTT       = 0
		clipDefault = 0
		qualityAA   = 4 // ANTIALIASED_QUALITY：要灰度抗锯齿，不要 ClearType 的彩边
		pitchFF     = 0
		transparent = 1
		black       = 0x000000
	)

	hdc, _, _ := procCreateCompatibleDC.Call(0)
	if hdc == 0 {
		return nil, 0, 0, false
	}
	defer procDeleteDC.Call(hdc)

	bi := bitmapInfoHeader{
		biSize:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		biWidth:       int32(canvas),
		biHeight:      -int32(canvas), // 负值 = 自上而下
		biPlanes:      1,
		biBitCount:    32,
		biCompression: biRGB,
	}
	var bits unsafe.Pointer
	hbm, _, _ := procCreateDIBSection.Call(hdc, uintptr(unsafe.Pointer(&bi)), dibRGBColors,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbm == 0 || bits == nil {
		return nil, 0, 0, false
	}
	defer procDeleteObject.Call(hbm)
	procSelectObject.Call(hdc, hbm)

	px := unsafe.Slice((*byte)(bits), canvas*canvas*4)
	for i := range px {
		px[i] = 0xFF // 纯白底：BGRA 全 FF
	}

	hfont, _, _ := procCreateFontW.Call(
		^uintptr(fontH-1), 0, 0, 0, fwNormal, 0, 0, 0,
		0, outTT, clipDefault, qualityAA, pitchFF,
		uintptr(unsafe.Pointer(utf16p("Segoe UI Emoji"))),
	)
	if hfont == 0 {
		return nil, 0, 0, false
	}
	defer procDeleteObject.Call(hfont)
	procSelectObject.Call(hdc, hfont)

	procSetBkMode.Call(hdc, transparent)
	procSetTextColor.Call(hdc, black)

	emoji := []uint16{0xD83D, 0xDC35} // U+1F435 🐵 的 UTF-16 代理对
	var sz struct{ cx, cy int32 }
	procGetTextExtent.Call(hdc, uintptr(unsafe.Pointer(&emoji[0])), 2,
		uintptr(unsafe.Pointer(&sz)))
	if sz.cx <= 0 || sz.cy <= 0 || int(sz.cx) > canvas || int(sz.cy) > canvas {
		return nil, 0, 0, false
	}
	procTextOutW.Call(hdc,
		uintptr((canvas-int(sz.cx))/2), uintptr((canvas-int(sz.cy))/2),
		uintptr(unsafe.Pointer(&emoji[0])), 2)

	// 覆盖率 = 1 - R/255（白底黑字，三通道相等）
	all := make([]uint8, canvas*canvas)
	for i := 0; i < canvas*canvas; i++ {
		all[i] = clamp8(255 - float64(px[i*4+2]))
	}

	// 实心包围盒（覆盖率 > 50% 才认）
	x0, y0, x1, y1 := canvas, canvas, -1, -1
	for y := 0; y < canvas; y++ {
		for x := 0; x < canvas; x++ {
			if all[y*canvas+x] <= inkThreshold {
				continue
			}
			if x < x0 {
				x0 = x
			}
			if y < y0 {
				y0 = y
			}
			if x > x1 {
				x1 = x
			}
			if y > y1 {
				y1 = y
			}
		}
	}
	if x1 < 0 {
		return nil, 0, 0, false
	}
	bw, bh = x1-x0+1, y1-y0+1
	out := make([]uint8, bw*bh)
	for y := 0; y < bh; y++ {
		copy(out[y*bw:(y+1)*bw], all[(y0+y)*canvas+x0:(y0+y)*canvas+x0+bw])
	}
	return out, bw, bh, true
}

// faceSeed 从字形几何中心往外螺旋找一个非墨像素，作为脸盘洪泛的起点。
func faceSeed(ink []uint8, w, h int) int {
	cx, cy := w/2, h/2
	for r := 0; r < w; r++ {
		for dy := -r; dy <= r; dy++ {
			for dx := -r; dx <= r; dx++ {
				if dx > -r && dx < r && dy > -r && dy < r {
					continue // 只看这一圈的边，省得重复扫
				}
				x, y := cx+dx, cy+dy
				if x < 0 || y < 0 || x >= w || y >= h {
					continue
				}
				i := y*w + x
				if ink[i] <= inkThreshold {
					return i
				}
			}
		}
	}
	return -1
}

// floodBool 四连通洪泛：从所有满足 seed 的像素出发，在满足 open 的像素上扩散。
func floodBool(n, w, h int, open, seed func(int) bool) []bool {
	seen := make([]bool, n)
	stack := make([]int, 0, n/4)
	for i := 0; i < n; i++ {
		if seed(i) && open(i) {
			seen[i] = true
			stack = append(stack, i)
		}
	}
	for len(stack) > 0 {
		i := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		x, y := i%w, i/w
		for _, d := range [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
			nx, ny := x+d[0], y+d[1]
			if nx < 0 || ny < 0 || nx >= w || ny >= h {
				continue
			}
			j := ny*w + nx
			if seen[j] || !open(j) {
				continue
			}
			seen[j] = true
			stack = append(stack, j)
		}
	}
	return seen
}
