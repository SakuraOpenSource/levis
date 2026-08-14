// Package captcha 生成并校验图形验证码。
//
// 验证码渲染为 PNG 位图而不是 SVG：SVG 的文本节点里直接写着答案，脚本一个
// 正则就能取走，等于没有验证码。位图至少要求对方接入 OCR。
package captcha

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	mrand "math/rand/v2"
)

// 字符集类型。
const (
	// CharsetMixed 是数字与字母混排。
	CharsetMixed = "mixed"
	// CharsetDigit 是纯数字。
	CharsetDigit = "digit"
	// CharsetLetter 是纯字母。
	CharsetLetter = "letter"
)

// 位数取值范围与默认值。
const (
	MinLength     = 4
	MaxLength     = 8
	DefaultLength = 6
)

// 各字符集的取值范围。
//
// 刻意剔除易混字符，否则用户看着图也猜不准该敲哪个：
//   - 字母去掉 I、L、O（与 1、0 难分）
//   - 混排时数字再去掉 0、1，字母再去掉 S、Z（与 5、2 难分）
//
// 纯数字与纯字母模式下不存在跨类混淆，因此各自保留完整集合。
const (
	digitSet       = "0123456789"
	letterSet      = "ABCDEFGHJKMNPQRSTUVWXYZ"
	mixedDigitSet  = "23456789"
	mixedLetterSet = "ABCDEFGHJKMNPQRTUVWXY"
)

// ValidCharset 报告 name 是否为受支持的字符集类型。
func ValidCharset(name string) bool {
	switch name {
	case CharsetMixed, CharsetDigit, CharsetLetter:
		return true
	}
	return false
}

// alphabet 返回字符集对应的候选字符。未知字符集回落到混排。
func alphabet(name string) string {
	switch name {
	case CharsetDigit:
		return digitSet
	case CharsetLetter:
		return letterSet
	default:
		return mixedDigitSet + mixedLetterSet
	}
}

// ClampLength 把位数收敛到合法区间。
func ClampLength(n int) int {
	if n < MinLength {
		return MinLength
	}
	if n > MaxLength {
		return MaxLength
	}
	return n
}

// randomCode 用密码学随机数生成验证码原文。
func randomCode(charset string, length int) (string, error) {
	set := alphabet(charset)
	// 拒绝采样：直接对 256 取模会让排在前面的字符略微高频，白白削弱强度。
	limit := 256 - 256%len(set)
	out := make([]byte, 0, length)
	buf := make([]byte, 1)
	for len(out) < length {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("生成验证码失败: %w", err)
		}
		if int(buf[0]) >= limit {
			continue
		}
		out = append(out, set[int(buf[0])%len(set)])
	}
	return string(out), nil
}

// 图片尺寸与干扰强度参数。
//
// 这几个数是互相牵制的，调整时要一起看：
//   - 字符放大后的宽度约为 glyphWidth*maxScale，旋转后还要再宽一些；
//     charAdvance 必须留出余量，否则相邻字符会粘在一起，人也认不出。
//   - 字符放大后的高度约为 glyphHeight*maxScale，加上基线抖动不能超过
//     imageHeight，否则上下会被裁掉。
const (
	imageHeight = 48
	charAdvance = 28 // 每个字符占据的水平宽度
	paddingX    = 10
	minScale    = 3.2
	maxScale    = 4.0
	maxAngle    = 0.28 // 约 ±16°
	maxJitterY  = 3.0
	noiseDots   = 90
	noiseLines  = 2
)

// render 把验证码原文画成 PNG 字节流。
//
// 干扰手段：每个字符独立的颜色、缩放、旋转与基线抖动，整体叠加一条正弦
// 波形位移，再加上背景噪点与贯穿全图的干扰曲线。
func render(code string) ([]byte, error) {
	width := paddingX*2 + len(code)*charAdvance
	img := image.NewRGBA(image.Rect(0, 0, width, imageHeight))

	// 背景取一个随机浅色调，避免每张图底色完全一致。
	bg := color.RGBA{
		R: uint8(243 + mrand.IntN(13)),
		G: uint8(243 + mrand.IntN(13)),
		B: uint8(243 + mrand.IntN(13)),
		A: 255,
	}
	for y := range imageHeight {
		for x := range width {
			img.SetRGBA(x, y, bg)
		}
	}

	drawNoise(img, width)

	// 全图共用一条正弦位移，字符因此整体呈波浪状排列。
	// 幅度刻意压得比字符高度小得多：波浪是为了破坏字符的水平对齐，
	// 幅度一大就会把字符推出画面。
	amplitude := 1.5 + mrand.Float64()*1.5
	period := float64(width) / (0.8 + mrand.Float64()*0.6)
	phase := mrand.Float64() * 2 * math.Pi
	wave := func(x int) float64 {
		return amplitude * math.Sin(2*math.Pi*float64(x)/period+phase)
	}

	for i := range len(code) {
		shape, ok := glyphs[code[i]]
		if !ok {
			return nil, fmt.Errorf("captcha: 字符 %q 没有对应字模", code[i])
		}
		drawGlyph(img, shape, float64(paddingX+i*charAdvance+charAdvance/2), wave)
	}

	drawLines(img, width)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("编码验证码图片失败: %w", err)
	}
	return buf.Bytes(), nil
}

// drawGlyph 以 cx 为水平中心画一个字符。
//
// 采用逆向映射：遍历目标区域的每个像素，反算回字模坐标再取样。正向映射
// （遍历字模像素往外画）在旋转放大后会留下摩尔纹似的空洞。
func drawGlyph(img *image.RGBA, shape glyph, cx float64, wave func(int) float64) {
	cy := float64(imageHeight)/2 + (mrand.Float64()*2-1)*maxJitterY
	scale := minScale + mrand.Float64()*(maxScale-minScale)
	angle := (mrand.Float64()*2 - 1) * maxAngle
	sin, cos := math.Sin(angle), math.Cos(angle)

	// 字符颜色必须明显深于背景，否则叠上噪点就分不清哪个是字。
	col := color.RGBA{
		R: uint8(20 + mrand.IntN(70)),
		G: uint8(20 + mrand.IntN(70)),
		B: uint8(20 + mrand.IntN(70)),
		A: 255,
	}

	// 旋转后字符的最大半径，据此圈定需要扫描的目标区域。
	radius := scale * math.Hypot(glyphWidth, glyphHeight) / 2
	bounds := img.Bounds()
	minX := max(int(cx-radius), bounds.Min.X)
	maxX := min(int(cx+radius)+1, bounds.Max.X)

	for x := minX; x < maxX; x++ {
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			dx := float64(x) - cx
			dy := float64(y) - cy - wave(x)
			u := dx*cos + dy*sin
			v := -dx*sin + dy*cos
			gx := int(math.Floor(u/scale + glyphWidth/2))
			gy := int(math.Floor(v/scale + glyphHeight/2))
			if gx < 0 || gx >= glyphWidth || gy < 0 || gy >= glyphHeight {
				continue
			}
			if shape[gy][gx] == '#' {
				img.SetRGBA(x, y, col)
			}
		}
	}
}

// drawNoise 撒背景噪点。噪点画在字符之下，且颜色接近背景，
// 只是为了打乱平整底色，不去和字符抢辨识度。
func drawNoise(img *image.RGBA, width int) {
	for range noiseDots {
		img.SetRGBA(mrand.IntN(width), mrand.IntN(imageHeight), color.RGBA{
			R: uint8(170 + mrand.IntN(60)),
			G: uint8(170 + mrand.IntN(60)),
			B: uint8(170 + mrand.IntN(60)),
			A: 255,
		})
	}
}

// drawLines 画贯穿全图的干扰曲线。
//
// 曲线压在字符之上，专门对付按连通域切字的 OCR。只画细线且颜色偏浅：
// 人眼能顺着笔画补全被划过的字符，太粗太深就变成人也认不出了。
func drawLines(img *image.RGBA, width int) {
	for range noiseLines {
		col := color.RGBA{
			R: uint8(130 + mrand.IntN(80)),
			G: uint8(130 + mrand.IntN(80)),
			B: uint8(130 + mrand.IntN(80)),
			A: 255,
		}
		amplitude := 4.0 + mrand.Float64()*8
		period := float64(width) / (0.6 + mrand.Float64()*1.4)
		phase := mrand.Float64() * 2 * math.Pi
		offset := imageHeight/2 + (mrand.Float64()*2-1)*imageHeight/3
		for x := range width {
			y := int(offset + amplitude*math.Sin(2*math.Pi*float64(x)/period+phase))
			setPixel(img, x, y, col)
		}
	}
}

// setPixel 越界时静默丢弃，省去调用点到处判边界。
func setPixel(img *image.RGBA, x, y int, col color.RGBA) {
	if !(image.Point{X: x, Y: y}).In(img.Bounds()) {
		return
	}
	img.SetRGBA(x, y, col)
}

// dataURL 把 PNG 字节流包装成可直接塞进 <img src> 的 data URL。
func dataURL(png []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}
