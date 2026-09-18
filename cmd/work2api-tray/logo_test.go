//go:build windows

package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// ── 字形标定 ────────────────────────────────────────────────────────────
//
// logo 里那颗猴头不是「画」出来的，是 Windows 自带 emoji 字体里 🐵(U+1F435)
// 的单色字形光栅化后切出来的（见 face.go）。faceMap 把 logo 坐标映射到字形坐标，
// 用的是 emojiHeadCU/CV/RU/RV 这组**标定常量**。
//
// 这组常量一旦和真实字形对不上，整个图标就废了 —— 而且要换个 Windows 版本、
// 或者字体被换成别的 emoji 才会露馅，本地根本不会红。所以这条测试从字形本身
// 反测那组常量，把它们从「注释里的数字」变成「被盯住的不变量」。
func TestEmojiGlyphShapeUnchanged(t *testing.T) {
	const targetH = 256
	f := loadEmojiFace(targetH)
	if f == nil {
		t.Skip("拿不到 emoji 字形（字体缺失或 GDI 拒绝），跳过标定校验")
	}

	// 1) 包围盒宽高比。实测 286×232 ≈ 1.233；把 🐵 换成别的 emoji 会立刻变。
	if ratio := float64(f.w) / float64(f.h); ratio < 1.10 || ratio > 1.36 {
		t.Errorf("字形包围盒宽高比 %.3f 超出 [1.10,1.36]（实测 1.233）——"+
			"emoji 字体或字形变了，emojiHeadRU/RV 需要重新标定", ratio)
	}

	// 2) 脸盘（cls==255）区域的中心与尺寸 —— faceMap 那几个常量就是照它标的。
	var sumX, sumY, cnt int
	x0, y0, x1, y1 := f.w, f.h, -1, -1
	clsCount := [3]int{} // 0=轮廓外 128=毛 255=脸
	for y := 0; y < f.h; y++ {
		for x := 0; x < f.w; x++ {
			switch {
			case f.cls[y*f.w+x] >= 192:
				clsCount[2]++
			case f.cls[y*f.w+x] >= 64:
				clsCount[1]++
			default:
				clsCount[0]++
			}
			if f.cls[y*f.w+x] < 192 {
				continue
			}
			sumX += x
			sumY += y
			cnt++
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
	if cnt < f.w*f.h/50 {
		t.Fatalf("脸盘区域只有 %d 像素（共 %d），洪泛多半失效了", cnt, f.w*f.h)
	}
	cxF := float64(sumX) / float64(cnt) / float64(f.w-1)
	cyF := float64(sumY) / float64(cnt) / float64(f.h-1)
	if math.Abs(cxF-emojiHeadCU) > 0.05 {
		t.Errorf("脸盘横向中心 %.3f 偏离 emojiHeadCU(%.3f) 超过 0.05", cxF, emojiHeadCU)
	}
	if math.Abs(cyF-emojiHeadCV) > 0.07 {
		t.Errorf("脸盘纵向中心 %.3f 偏离 emojiHeadCV(%.3f) 超过 0.07", cyF, emojiHeadCV)
	}

	// 脸盘占包围盒的比例 —— 实测 u 0.26~0.74(宽 0.48)、v 0.17~0.81(高 0.64)。
	// 放宽到 0.35/0.45，只在字形整体走样时才会红。
	if rw := float64(x1-x0+1) / float64(f.w); rw < 0.35 {
		t.Errorf("脸盘只占包围盒宽度的 %.3f（实测 0.48），字形走样了", rw)
	}
	if rh := float64(y1-y0+1) / float64(f.h); rh < 0.45 {
		t.Errorf("脸盘只占包围盒高度的 %.3f（实测 0.64），字形走样了", rh)
	}

	// 3) 三类区域都得存在，否则着色会整片塌成单色。
	total := f.w * f.h
	if frac := float64(clsCount[1]) / float64(total); frac < 0.05 || frac > 0.75 {
		t.Errorf("「毛」区域占 %.3f，超出 [0.05,0.75] —— 分区洪泛失效", frac)
	}
	if frac := float64(clsCount[0]) / float64(total); frac < 0.02 || frac > 0.60 {
		t.Errorf("「轮廓外」区域占 %.3f，超出 [0.02,0.60] —— 分区洪泛失效", frac)
	}
}

// ── 坐标映射 ────────────────────────────────────────────────────────────

// faceMap 必须把**logo 里的头圆**精确映射到**字形里的头圆**：
// 圆心对圆心、半径对半径。映射写错的话，猴脸会整块偏移或被拉伸。
func TestFaceMapMapsHeadOntoGlyphHead(t *testing.T) {
	// 圆心
	if mu, mv := faceMap(headCU, headCV); math.Abs(mu-emojiHeadCU) > 1e-9 || math.Abs(mv-emojiHeadCV) > 1e-9 {
		t.Errorf("头心映射到 (%v,%v)，期望 (%v,%v)", mu, mv, emojiHeadCU, emojiHeadCV)
	}
	// 横向两端：u = headCU ± headR  →  mu = emojiHeadCU ± emojiHeadRU
	muL, _ := faceMap(headCU-headR, headCV)
	muR, _ := faceMap(headCU+headR, headCV)
	if math.Abs(muL-(emojiHeadCU-emojiHeadRU)) > 1e-9 || math.Abs(muR-(emojiHeadCU+emojiHeadRU)) > 1e-9 {
		t.Errorf("头的左右边界映射到 %.6f/%.6f，期望 %.6f/%.6f",
			muL, muR, emojiHeadCU-emojiHeadRU, emojiHeadCU+emojiHeadRU)
	}
	// 纵向两端
	_, mvT := faceMap(headCU, headCV-headR)
	_, mvB := faceMap(headCU, headCV+headR)
	if math.Abs(mvT-(emojiHeadCV-emojiHeadRV)) > 1e-9 || math.Abs(mvB-(emojiHeadCV+emojiHeadRV)) > 1e-9 {
		t.Errorf("头的上下边界映射到 %.6f/%.6f，期望 %.6f/%.6f",
			mvT, mvB, emojiHeadCV-emojiHeadRV, emojiHeadCV+emojiHeadRV)
	}
}

// 拿一个造出来的字形，把「字形分区 → 颜色」这条规则单独测掉。
// 用真字形测的话，断言会随字体版本漂 —— 那不是这段逻辑的错。
func TestSampleLogoFaceColorRules(t *testing.T) {
	mk := func(ink, cls uint8) *emojiFace {
		const w, h = 48, 40
		f := &emojiFace{w: w, h: h, ink: make([]uint8, w*h), cls: make([]uint8, w*h)}
		for i := range f.cls {
			f.ink[i] = ink
			f.cls[i] = cls
		}
		return f
	}
	// 取样点落在脸的上半区：既不碰帽子（v<=capCutV），也不碰帽正金珠。
	const u, v = 0.42, 0.62

	cases := []struct {
		name string
		f    *emojiFace
		want color.NRGBA
	}{
		{"墨线压在最上层", mk(255, 255), logoInk},
		{"脸盘用脸色", mk(0, 255), logoFace},
		{"头内部用毛色", mk(0, 128), logoHair},
		{"分区为 0 时退回底座色", mk(0, 0), logoPrimary},
	}
	for _, c := range cases {
		got, ok := sampleLogo(u, v, c.f)
		if !ok {
			t.Errorf("%s：点 (%v,%v) 不该透明", c.name, u, v)
			continue
		}
		if got != c.want {
			t.Errorf("%s：期望 %v，实际 %v", c.name, c.want, got)
		}
	}

	// 字形取样越界（ok=false）时不该改色 —— 否则脸的余量会糊出底座色。
	if got, _ := sampleLogo(headCU, headCV-headR-0.05, mk(255, 255)); got == logoInk {
		t.Error("越出字形包围盒的点不该被判成墨线")
	}
}

// 采样点断言：图层顺序写反、形状坐标写错，这里会直接红。
func TestLogoSampleKeyPoints(t *testing.T) {
	f := faceFor(256)
	if f == nil {
		t.Skip("拿不到 emoji 字形，脸部采样点无从断言")
	}

	// 这些都是与字形无关的点：底座、帽顶、帽正、四角。
	cases := []struct {
		name string
		u, v float64
		want color.NRGBA
	}{
		{"左侧空白处应是底座紫", 0.06, 0.50, logoPrimary},
		{"帽顶应是官帽", 0.40, 0.30, logoCap},
		{"帽正应是金珠", 0.50, 0.346, logoGold},
	}
	for _, c := range cases {
		got, ok := sampleLogo(c.u, c.v, f)
		if !ok {
			t.Errorf("%s：点 (%v,%v) 不该透明", c.name, c.u, c.v)
			continue
		}
		if got != c.want {
			t.Errorf("%s：(%v,%v) 期望 %v，实际 %v", c.name, c.u, c.v, c.want, got)
		}
	}

	// 帽檐下方必须是「脸那一类」的颜色。只断言类别、不断言具体是哪一个 ——
	// 具体分区由字形决定，硬编码到具体颜色会在字体版本变化时变成假失败。
	faceSet := map[color.NRGBA]bool{logoFace: true, logoHair: true, logoInk: true}
	for _, p := range [][2]float64{{0.50, 0.66}, {0.50, 0.56}, {0.45, 0.56}} {
		got, ok := sampleLogo(p[0], p[1], f)
		if !ok {
			t.Errorf("脸部点 (%v,%v) 不该透明", p[0], p[1])
			continue
		}
		if !faceSet[got] {
			t.Errorf("脸部点 (%v,%v) 落在 %v —— 帽子压太低，把脸盖住了", p[0], p[1], got)
		}
	}

	// 四个角必须透明（圆角底座），否则图标会是方块
	for _, p := range [][2]float64{{0.01, 0.01}, {0.99, 0.01}, {0.01, 0.99}, {0.99, 0.99}} {
		if _, ok := sampleLogo(p[0], p[1], f); ok {
			t.Errorf("角落 (%v,%v) 应该透明", p[0], p[1])
		}
	}
}

// 帽檐下沿必须**始终**与头相接，中间不能夹底座紫 —— 那看起来就是
// 「帽子浮在头上，没戴住」。
//
// 直接扫几何本体（不经过抗锯齿），沿每一列从上往下走状态机，找
// 「官帽 → 底座紫 → 头/脸」这种被夹住的三明治结构，统计它占了多宽。
func TestNoWideGapBetweenCapAndHead(t *testing.T) {
	f := faceFor(256)
	if f == nil {
		t.Skip("拿不到 emoji 字形，无法区分「头」和「背景」")
	}

	const n = 1200
	gapCols := 0
	for xi := 0; xi < n; xi++ {
		u := (float64(xi) + 0.5) / float64(n)
		state := 0 // 0=还没见到官帽  1=见到官帽  2=官帽之下又见到底座紫
		gapped := false
		for yi := 0; yi < n && !gapped; yi++ {
			v := (float64(yi) + 0.5) / float64(n)
			c, ok := sampleLogo(u, v, f)
			if !ok {
				continue // 圆角底座之外，透明
			}
			switch {
			case c == logoGold || (c == logoCap && inHatBody(u, v)):
				// 帽正金珠压在帽子上，见到它就说明帽子还在。
				// 帽翅（logoCap 但不属于 inHatBody）**故意不参与**：它是悬在
				// 头两侧上方的独立横条，下面本来就是背景色，再往下是耳朵 ——
				// 按 logoCap 判会把它误当成「官帽」，中间那截合法背景记成缝
				// （实测误报 0.164）。落到 default 就当它不存在。
				state = 1
			case c == logoPrimary:
				if state == 1 {
					state = 2
				}
			case c == logoHair || c == logoFace || c == logoInk:
				if state == 2 {
					gapped = true
				}
			}
		}
		if gapped {
			gapCols++
		}
	}

	// 阈值 0.005（u 轴单位）≈ 32px 下的 0.16px，肉眼不可见。
	// 一旦帽檐与头脱开，缝宽会跳到 0.05 以上，这条必红。
	if w := float64(gapCols) / float64(n); w > 0.005 {
		t.Errorf("官帽与头之间夹着底座紫：缝宽 %.4f（阈值 0.005），%d/%d 列命中。"+
			"调大 hatHugOverlap(%.3f)，或检查 emojiHeadRU/RV 标定是否漂了",
			w, gapCols, n, hatHugOverlap)
	}
}

// 每一层都必须真的出现在成图里 —— 某层被上层完全盖住时这条会红。
// 这是「帽子歪戴后还看得见帽翅/帽正」「脸没被帽子吞掉」这类改动的兜底。
//
// 用最近邻配色分类而不是精确等值：renderLogo 会做 4×4 超采样，
// 笔画只有一两像素宽，精确等值的像素可能一个都没有。
func TestLogoAllLayersVisible(t *testing.T) {
	const size = 128
	img := renderLogo(size)

	palette := []struct {
		name string
		c    color.NRGBA
	}{
		{"底座紫", logoPrimary},
		{"毛色", logoHair},
		{"官帽", logoCap},
		{"帽正金", logoGold},
		{"脸盘", logoFace},
		{"墨线", logoInk},
	}
	count := make([]int, len(palette))
	total := 0
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			c := img.NRGBAAt(x, y)
			if c.A < 200 {
				continue
			}
			total++
			best, bestD := 0, math.MaxInt
			for i, p := range palette {
				d := absInt(int(c.R)-int(p.c.R)) + absInt(int(c.G)-int(p.c.G)) + absInt(int(c.B)-int(p.c.B))
				if d < bestD {
					best, bestD = i, d
				}
			}
			count[best]++
		}
	}
	if total == 0 {
		t.Fatal("成图没有任何不透明像素")
	}
	for i, p := range palette {
		if count[i]*400 < total { // 少于 0.25%
			t.Errorf("%s 只占 %d/%d 像素 —— 该层可能被上层盖住了", p.name, count[i], total)
		}
	}
}

func TestRenderLogoNotBlank(t *testing.T) {
	const size = 32
	img := renderLogo(size)

	opaque := 0
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if img.NRGBAAt(x, y).A > 200 {
				opaque++
			}
		}
	}
	// 至少一半像素不透明 —— 否则说明绘制写错，托盘上会是个近乎隐形的图标
	if opaque < size*size/2 {
		t.Fatalf("不透明像素只有 %d/%d，太少了", opaque, size*size)
	}

	// 四角必须全透明
	for _, p := range [][2]int{{0, 0}, {size - 1, 0}, {0, size - 1}, {size - 1, size - 1}} {
		if a := img.NRGBAAt(p[0], p[1]).A; a != 0 {
			t.Errorf("角落 (%d,%d) 应该透明，实际 alpha=%d", p[0], p[1], a)
		}
	}
}

func TestInRoundRect(t *testing.T) {
	// 中心在内
	if !inRoundRect(0.5, 0.5, 0, 0, 1, 1, 0.22) {
		t.Error("中心点应在圆角矩形内")
	}
	// 四角在外
	for _, p := range [][2]float64{{0.01, 0.01}, {0.99, 0.01}, {0.01, 0.99}, {0.99, 0.99}} {
		if inRoundRect(p[0], p[1], 0, 0, 1, 1, 0.22) {
			t.Errorf("角落 (%v,%v) 应在圆角矩形外", p[0], p[1])
		}
	}
	// 边中在内 —— 否则说明半径算错，把整条边切掉了
	for _, p := range [][2]float64{{0.01, 0.5}, {0.99, 0.5}, {0.5, 0.01}, {0.5, 0.99}} {
		if !inRoundRect(p[0], p[1], 0, 0, 1, 1, 0.22) {
			t.Errorf("边中点 (%v,%v) 应在圆角矩形内", p[0], p[1])
		}
	}
}

// TestLogoPreview 不是断言，是把 logo 导出成 PNG 供肉眼确认造型。
// 固定写到项目 dist/ 下（dist/ 已在 .gitignore）。
func TestLogoPreview(t *testing.T) {
	dir := filepath.Join("..", "..", "dist")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建 %s 失败: %v", dir, err)
	}
	// 大图看造型细节
	writePNG(t, filepath.Join(dir, "logo-preview.png"), renderLogo(256))
	// 实际托盘尺寸（32px），最近邻放大 8 倍看小尺寸还原度
	writePNG(t, filepath.Join(dir, "logo-32-nearest.png"), upscaleNearest(renderLogo(32), 8))
}

func writePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建 %s 失败: %v", path, err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("编码 %s 失败: %v", path, err)
	}
}

func upscaleNearest(src *image.NRGBA, n int) *image.NRGBA {
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx()*n, b.Dy()*n))
	for y := 0; y < dst.Bounds().Dy(); y++ {
		for x := 0; x < dst.Bounds().Dx(); x++ {
			dst.SetNRGBA(x, y, src.NRGBAAt(b.Min.X+x/n, b.Min.Y+y/n))
		}
	}
	return dst
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
