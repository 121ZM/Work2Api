//go:build windows

package main

import (
	"image"
	"image/color"
	"math"
)

// 托盘图标：弼马温（孙悟空当官时）戴乌纱帽的 logo 版。
//
// 造型分三块：
//  1. 底座   —— 圆角方块，面板主色。16px 上下时没有底色，深色官帽会和暗色任务栏糊在一起
//  2. 猴头   —— **直接取 emoji 字形**（见 face.go）：头、耳、脸盘、五官都来自 🐵 的线稿，
//     颜色由我们按区域刷。用圆和椭圆拼出来的脸在小尺寸下是通用卡通动物，不像猴子
//  3. 乌纱帽 —— 帽顶拱 + 帽檐 + 帽翅 + 帽正金珠，**歪着戴**
//
// 刻意不画紧箍：紧箍是后来观音给的，弼马温时期还没有。
//
// 所有形状都用归一化坐标（0..1）描述，与输出尺寸无关 —— renderLogo 先在 4 倍画布上
// 逐点采样，再盒式降采样，边缘自然抗锯齿，换任何尺寸都不用改形状代码。
//
// ── 帽檐的下沿为什么是「贴着头轮廓走」而不是一条水平线 ────────────────────
//
// 头是个圆：前额那一段轮廓接近水平，越往两侧下降越快。帽檐下沿若是一条死的
// 水平线（v <= capCutV），头的两侧、轮廓高于这条线的那一段就会露在帽子外面 ——
// 帽子和头之间豁出一道底座紫的缝，看着就是「帽子浮在头上没戴住」。实测缝宽 0.096
// （u 轴单位，32px 下约 3px），一眼能看出来。
//
// 所以下沿取 max(直切线, 头的轮廓 + 贴合余量)：
//   - 直切线保证「前额露出多少」是固定的；
//   - 头的轮廓 + 余量保证两侧帽檐顺着头的边缘往下收，且**压进头里一点点**。
//
// 余量 hatHugOverlap 不是装饰：头的轮廓是用 emojiHeadRU/RV 那组标定值反算的，
// 字形一旦有几 % 的偏差（不同 Windows 版本的 emoji 字体），按理想轮廓切的帽檐
// 就会落在真实头顶**上方**，重新豁出背景色缝。压进去 0.018 把这个误差吃掉 ——
// 字形头部的粗描边在半径 1.0 附近还有约 ±5% 的厚度，余量小于 0.015 就会露。
//
// 还有一条同样关键：帽子横向范围 |cu-headCU| <= hatRU 用的是**旋转后**的坐标，
// 而「下沿贴头」用的是**屏幕**坐标。歪的是帽子，头是不歪的 —— 第一版把下沿也写在
// 旋转坐标里，帽子一歪裁剪线跟着抬高约 du*tan(18°)，缝就是这么来的。

var (
	logoPrimary = color.NRGBA{0x7C, 0x3B, 0xED, 0xFF} // 底座：面板主色
	logoHair    = color.NRGBA{0x6B, 0x46, 0x26, 0xFF} // 头 / 毛
	logoCap     = color.NRGBA{0x1C, 0x1F, 0x2A, 0xFF} // 乌纱帽
	logoFace    = color.NRGBA{0xE0, 0xA8, 0x5C, 0xFF} // 猴脸（emoji 字形里的脸盘）
	logoGold    = color.NRGBA{0xF5, 0xC5, 0x42, 0xFF} // 帽正
	logoInk     = color.NRGBA{0x24, 0x1A, 0x12, 0xFF} // 字形笔画：头/耳轮廓、眼睛、鼻孔、嘴
)

// 头（同时是帽子的旋转中心）。
//
// 头的大小是整张图的锚：脸、耳朵、帽子宽度全都按 headR 缩放。
// 「脸太大」只要调小 headR 就行，不用逐个改五官坐标 —— 五官是字形给的。
const (
	headCU = 0.5
	headCV = 0.598
	headR  = 0.236

	// 帽子歪戴 8°。旋转中心取**头心**：帽檐被设计成贴着头的轮廓，
	// 绕头心转就永远不会歪出一道背景色的口子。
	//
	// 别往上调太多：帽翅跟着一起转，14° 时左翅会缩成小揪、右翅甩出去，
	// 看着像两个下垂的耳朵而不是展角。8° 是「看得出歪、又不失对称」的点。
	capTilt = 8 * math.Pi / 180
)

// 乌纱帽的形状，全部定义在「正着戴」的坐标系里（判断前把采样点反向转回去）。
//
//	hatRU/hatRV  帽顶拱的横向/纵向半径
//	hatCV        拱的腰线高度 —— 拱的上轮廓 = hatCV - hatRV*sqrt(1-(x/hatRU)^2)
//	capCutV      帽檐下沿的直切线：前额露出到这个高度
//	wingU0/U1    帽翅距头心的水平距离（内侧塞在帽子下面，外侧伸出去）
//	wingV0/V1    帽翅在腰线高度上的上下沿
//	hatHalfW     帽子左右伸到多宽（**屏幕坐标**，理由见 inHatBody 注释）
const (
	// hatRV 别超过 hatRU 太多 —— 1.4 倍时拱变成尖顶蛋，读起来是毡帽或头巾。
	// 两者接近时才是半球形的「帽顶」，高度约等于半宽。
	hatRU = 0.205
	hatRV = 0.245
	hatCV = 0.4755

	// 比 headR(0.236) 明显小 —— 帽子比头窄才像「戴着一顶帽子」。
	// 放到和头一样宽（0.230）时，帽檐会盖到耳朵上方、连成一条横杠，
	// 整顶帽子读起来是头盔或宽檐礼帽。
	hatHalfW = 0.195

	capCutV = 0.442

	// 帽檐下沿贴着头的轮廓往下收时，额外压进头里的深度。见文件头注释。
	hatHugOverlap = 0.018

	// 帽翅：细杆，故意和帽檐下沿（capCutV）拉开一截再往上放。
	// 贴着帽檐放时两条水平线会连成一片，整个帽子被读成「宽帽檐」；
	// 抬到拱肩高度才有「帽 + 展角」的层次。
	wingU0 = 0.115
	wingU1 = 0.330
	wingV0 = 0.320
	wingV1 = 0.362
)

// 🐵 字形里「头」的圆心与半径，按字形包围盒归一化（实测标定值，见 face.go 注释）。
// test 里 TestEmojiGlyphShapeUnchanged 盯着它们。
const (
	emojiHeadCU = 0.500
	emojiHeadCV = 0.474
	emojiHeadRU = 0.406
	emojiHeadRV = 0.500
)

// faceMap 把 logo 坐标映射到字形包围盒坐标，让字形里的「头」正好落在
// (headCU,headCV,headR) 上。u/v 两个方向的比例不同 —— 字形里的头是正圆，
// 但包围盒是 286×232 的扁矩形 —— 所以必须分开算。
func faceMap(u, v float64) (mu, mv float64) {
	mu = emojiHeadCU + (u-headCU)*(emojiHeadRU/headR)
	mv = emojiHeadCV + (v-headCV)*(emojiHeadRV/headR)
	return mu, mv
}

// sampleLogo 返回归一化坐标 (u,v) 处的颜色；ok=false 表示该点透明。
// 后面的判断覆盖前面的，所以顺序就是图层顺序。
// f 为 nil（拿不到 emoji 字形）时猴头退回一个纯圆脸兜底。
func sampleLogo(u, v float64, f *emojiFace) (color.NRGBA, bool) {
	// 底座：圆角方块，四角之外透明
	if !inRoundRect(u, v, 0, 0, 1, 1, 0.22) {
		return color.NRGBA{}, false
	}
	c := logoPrimary

	// 猴头：整颗头（含耳、脸盘、五官）都来自 emoji 字形。
	// 笔画压在最上面，其次是脸盘，剩下头内部算毛。
	if f != nil {
		if ink, cls, ok := f.sample(faceMap(u, v)); ok {
			switch {
			case ink > inkThreshold:
				c = logoInk
			case cls >= 192:
				c = logoFace
			case cls >= 64:
				c = logoHair
			}
		}
	} else if inCircle(u, v, headCU, headCV, headR) {
		// 兜底造型：比例照 emoji 那套（脸偏下、眼在中上），全部按 headR 缩放，
		// 换 headR 不用重算这些数字。
		c = logoHair
		if inCircle(u, v, headCU, headCV+0.17*headR, 0.68*headR) {
			c = logoFace
		}
		eyeDx, eyeDv := 0.254*headR, 0.058*headR
		if inCircle(u, v, headCU-eyeDx, headCV-eyeDv, 0.12*headR) ||
			inCircle(u, v, headCU+eyeDx, headCV-eyeDv, 0.12*headR) {
			c = logoInk
		}
	}

	// 帽子：帽顶拱的形状定义在「正着戴」的坐标系里，inHatBody 内部会把
	// 采样点绕头心反向转回去；帽翅留在屏幕坐标里保持对称（理由见 inHatWing）。
	if inHatBody(u, v) || inHatWing(u, v) {
		c = logoCap
	}

	// 帽正：帽檐正中一颗小金珠。正圆，旋转不变，所以直接写在屏幕坐标里。
	if inCircle(u, v, 0.5, 0.346, 0.032) {
		c = logoGold
	}

	return c, true
}

// inHatBody 判断是否落在帽顶拱或帽檐上（**不含帽翅**）。
//
// 三件事分属两套坐标，这是整个造型最容易写错的地方：
//
//	左右宽度 |u-headCU| <= hatHalfW   —— **屏幕**坐标。帽子最终是坐在头上，
//	                                    宽度是屏幕概念，必须和头的轮廓同一套。
//	下沿     v <= max(capCutV, 头轮廓) —— **屏幕**坐标，理由同文件头。
//	拱的上轮廓 cv >= hatArcV(cu-headCU) —— **旋转**坐标。这是帽子的形状，得跟着歪。
//
// 宽度写成旋转坐标（第一版）会让倾斜的横向带扫出头轮廓之外：右下角那一小块
// 帽子悬在头外面，下面就是背景色。实测命中 u=0.804~0.809，而 headR 只到 0.795。
//
// 拱的横向偏移是**夹紧**而不是「超出就判否」：倾斜的拱在屏幕坐标里同样会扫到
// 头的轮廓上方，判否会把帽檐切在头顶以上、豁出背景色缝。夹紧之后拱外按腰线算，
// 帽檐正好接到头的轮廓上。
//
// 测试扫「帽子和头之间有没有漏背景色」时用它，而不是用 logoCap 那个颜色 ——
// 帽翅是天生长在帽子外面的横条，它下面当然是背景色，算进来会误报。
func inHatBody(u, v float64) bool {
	if math.Abs(u-headCU) > hatHalfW {
		return false
	}
	if v > math.Max(capCutV, headTopV(u-headCU)+hatHugOverlap) {
		return false
	}
	cu, cv := rotateAbout(u, v, headCU, headCV, -capTilt)
	x := cu - headCU
	if x > hatRU {
		x = hatRU
	} else if x < -hatRU {
		x = -hatRU
	}
	return cv >= hatArcV(x)
}

// inHatWing 判断是否落在帽翅上。x 取绝对值 → 左右各一枚；
// 竖着用 inRoundRect 收两道圆角，出来是胶囊而不是方棍。
//
// 帽翅**故意不参与歪戴旋转**，留在屏幕坐标里保持左右对称 —— 跟着帽子转的话，
// 8° 就能让左翅比右翅高 0.09、一长一短，读起来像两只下垂的耳朵而不是展角。
// 「歪」这件事交给帽顶拱去表达，那里歪得再明显也不会有歧义。
func inHatWing(u, v float64) bool {
	halfH := (wingV1 - wingV0) / 2
	return inRoundRect(math.Abs(u-headCU), v,
		wingU0-halfH, wingV0, wingU1+halfH, wingV1, halfH)
}

// hatArcV 帽顶拱的上轮廓（帽子自己的坐标系）：水平偏移 x 处帽子的最高点。
func hatArcV(x float64) float64 {
	t := 1 - (x/hatRU)*(x/hatRU)
	if t < 0 {
		t = 0
	}
	return hatCV - hatRV*math.Sqrt(t)
}

// headTopV 头轮廓在水平偏移 du 处的高度（屏幕坐标，v 轴向下）。
// |du| > headR（头最宽处之外）返回头心高度 —— 那个位置本来就没有头。
func headTopV(du float64) float64 {
	t := headR*headR - du*du
	if t <= 0 {
		return headCV
	}
	return headCV - math.Sqrt(t)
}

// faceFor 按渲染尺寸准备 emoji 字形掩膜。
// 分辨率跟超采样对齐：内部按 4*size 的网格采样，字形只占其中约 0.9 宽，
// 所以 4*size 就够；上限 512 是为了 256px 预览图别一次开几百 MB。
func faceFor(renderSize int) *emojiFace {
	h := renderSize * 4
	if h < 128 {
		h = 128
	}
	if h > 512 {
		h = 512
	}
	return loadEmojiFace(h)
}

// renderLogo 渲染 size×size 的 logo。
// 返回 NRGBA（**非预乘** alpha）—— Windows 图标的 32bpp 位图要的就是非预乘，
// 直接逐字节搬到 DIB 即可，不用再做 alpha 还原。
func renderLogo(size int) *image.NRGBA {
	const ss = 4 // 超采样倍数
	n := size * ss
	f := faceFor(size)

	out := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var sumR, sumG, sumB, sumA float64
			for dy := 0; dy < ss; dy++ {
				for dx := 0; dx < ss; dx++ {
					u := (float64(x*ss+dx) + 0.5) / float64(n)
					v := (float64(y*ss+dy) + 0.5) / float64(n)
					c, ok := sampleLogo(u, v, f)
					if !ok {
						continue
					}
					// 按 alpha 加权累加，这样边缘混出来的是「颜色」而不是「颜色×透明度」
					a := float64(c.A) / 255
					sumR += float64(c.R) * a
					sumG += float64(c.G) * a
					sumB += float64(c.B) * a
					sumA += a
				}
			}
			if sumA == 0 {
				continue // 保持全透明
			}
			out.SetNRGBA(x, y, color.NRGBA{
				R: clamp8(sumR / sumA),
				G: clamp8(sumG / sumA),
				B: clamp8(sumB / sumA),
				A: clamp8(sumA / float64(ss*ss) * 255),
			})
		}
	}
	return out
}

// ---------------- 几何判定 ----------------

func inCircle(u, v, cu, cv, r float64) bool {
	du, dv := u-cu, v-cv
	return du*du+dv*dv <= r*r
}

// inRoundRect 判断点是否落在 (x0,y0)-(x1,y1)、圆角半径 r 的圆角矩形内。
func inRoundRect(u, v, x0, y0, x1, y1, r float64) bool {
	if u < x0 || u > x1 || v < y0 || v > y1 {
		return false
	}
	cx, cy := u, v
	if u < x0+r {
		cx = x0 + r
	} else if u > x1-r {
		cx = x1 - r
	}
	if v < y0+r {
		cy = y0 + r
	} else if v > y1-r {
		cy = y1 - r
	}
	du, dv := u-cx, v-cy
	return du*du+dv*dv <= r*r
}

// rotateAbout 把 (u,v) 绕 (cu,cv) 旋转 rad 弧度。
// 图像坐标 y 轴向下，所以正角度在视觉上是**顺时针**。
func rotateAbout(u, v, cu, cv, rad float64) (float64, float64) {
	du, dv := u-cu, v-cv
	cos, sin := math.Cos(rad), math.Sin(rad)
	return cu + du*cos - dv*sin, cv + du*sin + dv*cos
}

func clamp8(f float64) uint8 {
	if f < 0 {
		return 0
	}
	if f > 255 {
		return 255
	}
	return uint8(f + 0.5)
}
