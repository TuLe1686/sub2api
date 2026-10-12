package service

// 本文件实现 Responses 内联图片压缩（image-compress 补丁）。
//
// 背景：Codex Desktop 一代客户端不压缩对话历史、每轮全量重发，图片以 base64
// 内联在请求体里；图片是"字节便宜、token 贵"（一张 1-3MB 截图只占 1-2k token），
// 于是历史滚到 85MB+ 时被网关的内存预算守卫（gateway-request-memory-budget
// 补丁，单请求 >85.3MiB 即 503）拒收，客户端表现为"发不出去"。
// 文本受模型上下文（50 万 token ≈ 2-4MB 字节）约束，任何 >10MB 的请求体 95%+
// 都是图片——所以对"超阈值请求里的内联图片"做降采样 + JPEG 重编码，就能把
// 85MB+ 压回 10-20MB，且保留全部图片（对比"丢老图"：语义损失更小）。
//
// 关键设计：
//   - 触发：body > trigger_bytes 且 api_key 所属分组在 group_ids 白名单；否则零开销
//     （仅一次长度判断），99% 的请求完全不受影响。
//   - 定位：纯字节扫描 `"data:image/…;base64,`（不解析 JSON），对 Responses /
//     Chat / 任意嵌套 schema 均有效；扫描有界——只在"字符串以 data:image/ 开头"
//     时才认（前一位必须是引号，且引号闭合前必须出现 ;base64,）。
//   - 确定性：固定算法 + 固定参数，同一张图每轮压缩结果逐字节一致 →
//     上游 prompt 缓存前缀稳定（这是选"压缩"而不是"丢图"的核心理由之一）。
//   - 压不大就保留原图；一张都替换不了时原样返回同一 slice（零分配）。
//
// 参数与演练数据见 builder 仓库 runs/20261013-image-compress/DESIGN.md。

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"strconv"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

// inlineImageSpan 描述 body 中一个内联图片字符串的字节区间（含两侧引号）。
type inlineImageSpan struct {
	start int // 开引号位置
	end   int // 闭引号位置
}

type inlineImageCompressStats struct {
	Total      int
	Compressed int
	Skipped    int
}

type imageCompressParams struct {
	MaxDimension  int
	JPEGQuality   int
	MinImageBytes int64
	MaxPixels     int64
}

var imageDataURLStringPrefix = []byte(`"data:image/`)

const imageDataURLBase64Marker = ";base64,"

const imageDataURLJPEGPrefix = "data:image/jpeg;base64,"

// compressResponsesInlineImages 门控 + 压缩入口（挂载于 openai_gateway_forward.go，
// 缓存亲和注入之前、重试循环之外）。任何不满足门控或未发生替换的情况都原样返回，
// 绝不阻断请求本身。
func (s *OpenAIGatewayService) compressResponsesInlineImages(c *gin.Context, body []byte) ([]byte, bool) {
	if s == nil || s.cfg == nil || len(body) == 0 {
		return body, false
	}
	cfg := s.cfg.Gateway.RequestImageCompress
	if !cfg.Enabled || cfg.TriggerBytes <= 0 || int64(len(body)) <= cfg.TriggerBytes {
		return body, false
	}
	if isOpenAIResponsesCompactPath(c) {
		// compact 请求体有独立字段白名单，压缩结果下游会被重写，不在此介入。
		return body, false
	}
	groupID := getOpenAIGroupIDFromContext(c)
	if !imageCompressGroupAllowed(cfg.GroupIDs, groupID) {
		return body, false
	}
	params := imageCompressParams{
		MaxDimension:  cfg.MaxDimension,
		JPEGQuality:   cfg.JPEGQuality,
		MinImageBytes: cfg.MinImageBytes,
		MaxPixels:     cfg.MaxPixels,
	}
	originalLen := len(body)
	next, stats, changed := compressInlineImages(body, params)
	if !changed {
		return body, false
	}
	logger.LegacyPrintf("service.openai_gateway",
		"[OpenAI] 已压缩 Responses 内联图片（分组 %d：压缩 %d/%d 张，body %d → %d 字节）",
		groupID, stats.Compressed, stats.Total, originalLen, len(next))
	return next, true
}

// imageCompressGroupAllowed 解析逗号分隔的分组白名单；空清单视为不生效。
func imageCompressGroupAllowed(list string, groupID int64) bool {
	if groupID <= 0 || strings.TrimSpace(list) == "" {
		return false
	}
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if value, err := strconv.ParseInt(part, 10, 64); err == nil && value == groupID {
			return true
		}
	}
	return false
}

// compressInlineImages 扫描并压缩 body 中的 base64 内联图片。
// 返回 (新 body, 统计, 是否有替换)；无替换时返回原 slice。
func compressInlineImages(body []byte, params imageCompressParams) ([]byte, inlineImageCompressStats, bool) {
	stats := inlineImageCompressStats{}
	spans := findInlineImageSpans(body)
	if len(spans) == 0 {
		return body, stats, false
	}
	type replacement struct {
		span inlineImageSpan
		text []byte // 新的 data URL 字符串内容（不含引号）
	}
	replacements := make([]replacement, 0, len(spans))
	for _, span := range spans {
		stats.Total++
		raw := body[span.start+1 : span.end]
		next, ok := compressImageDataURL(raw, params)
		if !ok {
			stats.Skipped++
			continue
		}
		stats.Compressed++
		replacements = append(replacements, replacement{span: span, text: next})
	}
	if len(replacements) == 0 {
		return body, stats, false
	}
	// 一次重建：按区间 span 拷贝（与 responses-bootstrap-memory 补丁同一模式）。
	saved := 0
	for _, item := range replacements {
		saved += (item.span.end - item.span.start - 1) - len(item.text)
	}
	out := make([]byte, 0, len(body)-saved)
	cursor := 0
	for _, item := range replacements {
		out = append(out, body[cursor:item.span.start+1]...) // 含开引号
		out = append(out, item.text...)
		out = append(out, '"')
		cursor = item.span.end + 1
	}
	out = append(out, body[cursor:]...)
	return out, stats, true
}

// findInlineImageSpans 扫描 `"data:image/<mime>;base64,<payload>"` 字符串区间。
// 只在确认形态时收录：引号开头、mime 在 {png,jpeg,jpg}、收尾引号未转义。
// 非 base64 的 data URL（如 URL 编码形式）不识别，原样保留。
func findInlineImageSpans(body []byte) []inlineImageSpan {
	var spans []inlineImageSpan
	searchFrom := 0
	for searchFrom < len(body) {
		idx := bytes.Index(body[searchFrom:], imageDataURLStringPrefix)
		if idx < 0 {
			break
		}
		start := searchFrom + idx // 指向开引号
		end, ok := findJSONStringEnd(body, start+1)
		if !ok {
			break
		}
		raw := body[start+1 : end]
		if isCompressibleImageDataURL(raw) {
			spans = append(spans, inlineImageSpan{start: start, end: end})
		}
		searchFrom = end + 1
	}
	return spans
}

// findJSONStringEnd 从 from（字符串内容起点）找到未转义的闭引号位置。
// 内联图片字符串是预期内唯一会出现超大字符串的地方，扫描必须是线性的。
func findJSONStringEnd(body []byte, from int) (int, bool) {
	for i := from; i < len(body); i++ {
		if body[i] != '"' {
			continue
		}
		// 统计反斜杠奇偶：偶数个（含 0）才是真闭引号。
		backslashes := 0
		for j := i - 1; j >= from && body[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return i, true
		}
	}
	return 0, false
}

// isCompressibleImageDataURL 判断 data URL 内容是否是支持压缩的 base64 图片。
func isCompressibleImageDataURL(raw []byte) bool {
	if !bytes.HasPrefix(raw, []byte("data:image/")) {
		return false
	}
	rest := raw[len("data:image/"):]
	semi := bytes.Index(rest, []byte(imageDataURLBase64Marker))
	if semi <= 0 {
		return false
	}
	subtype := rest[:semi]
	return strings.EqualFold(string(subtype), "png") ||
		strings.EqualFold(string(subtype), "jpeg") ||
		strings.EqualFold(string(subtype), "jpg")
}

// compressImageDataURL 压缩单个 data URL；不可压缩或压不大时返回 (nil, false)。
func compressImageDataURL(raw []byte, params imageCompressParams) ([]byte, bool) {
	if !isCompressibleImageDataURL(raw) {
		return nil, false
	}
	rest := raw[len("data:image/"):]
	semi := bytes.Index(rest, []byte(imageDataURLBase64Marker))
	subtype := string(rest[:semi])
	payload := rest[semi+len(imageDataURLBase64Marker):]
	// 小图不折腾（比较 base64 长度，换算为原始字节数）。
	if int64(len(payload)) < params.MinImageBytes*4/3 {
		return nil, false
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(payload)))
	n, err := base64.StdEncoding.Decode(decoded, payload)
	if err != nil {
		return nil, false
	}
	encoded := decoded[:n]
	width, height, ok := imageDimensions(encoded)
	if !ok || width <= 0 || height <= 0 {
		return nil, false
	}
	if int64(width)*int64(height) > params.MaxPixels {
		// 超大像素图解码工作集会失控（max_pixels 约束见设计文档），跳过。
		return nil, false
	}
	isJPEGInput := strings.EqualFold(subtype, "jpeg") || strings.EqualFold(subtype, "jpg")
	if isJPEGInput && width <= params.MaxDimension && height <= params.MaxDimension {
		// 已是 JPEG 且尺寸达标：不再二压（质量损失换不来多少体积）。
		return nil, false
	}
	img, _, err := image.Decode(bytes.NewReader(encoded))
	if err != nil {
		return nil, false
	}
	rendered := renderOnWhite(img, params.MaxDimension)
	var buf bytes.Buffer
	buf.Grow(len(encoded) / 3)
	if err := jpeg.Encode(&buf, rendered, &jpeg.Options{Quality: params.JPEGQuality}); err != nil || buf.Len() == 0 {
		return nil, false
	}
	newB64Len := base64.StdEncoding.EncodedLen(buf.Len())
	if newB64Len+len(imageDataURLJPEGPrefix) >= len(raw) {
		// 压不大：保留原图（也避免了 JPEG 质量损失白付）。
		return nil, false
	}
	out := make([]byte, 0, len(imageDataURLJPEGPrefix)+newB64Len)
	out = append(out, imageDataURLJPEGPrefix...)
	encodedB64 := make([]byte, newB64Len)
	base64.StdEncoding.Encode(encodedB64, buf.Bytes())
	out = append(out, encodedB64...)
	return out, true
}

// renderOnWhite 把任意解码图渲染为白底 RGBA：长边超过 maxDimension 时做区域平均
// 降采样（面积平均，抗锯齿优于最近邻）；透明像素合成到白底（JPEG 无 alpha 通道）。
// 无降采样需求时即逐像素合成，产物直接可编码。
func renderOnWhite(src image.Image, maxDimension int) *image.RGBA {
	bounds := src.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	dstW, dstH := srcW, srcH
	if maxDimension > 0 && (srcW > maxDimension || srcH > maxDimension) {
		scale := float64(maxDimension) / float64(max(srcW, srcH))
		dstW = max(1, int(float64(srcW)*scale+0.5))
		dstH = max(1, int(float64(srcH)*scale+0.5))
	}
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	getPremul := premultipliedGetter(src)
	for dy := 0; dy < dstH; dy++ {
		srcY0 := bounds.Min.Y + dy*srcH/dstH
		srcY1 := bounds.Min.Y + (dy+1)*srcH/dstH
		if srcY1 <= srcY0 {
			srcY1 = srcY0 + 1
		}
		for dx := 0; dx < dstW; dx++ {
			srcX0 := bounds.Min.X + dx*srcW/dstW
			srcX1 := bounds.Min.X + (dx+1)*srcW/dstW
			if srcX1 <= srcX0 {
				srcX1 = srcX0 + 1
			}
			var rSum, gSum, bSum, aSum uint64
			var count uint64
			for y := srcY0; y < srcY1; y++ {
				for x := srcX0; x < srcX1; x++ {
					r, g, b, a := getPremul(x, y)
					rSum += uint64(r)
					gSum += uint64(g)
					bSum += uint64(b)
					aSum += uint64(a)
					count++
				}
			}
			r := uint32(rSum / count)
			g := uint32(gSum / count)
			b := uint32(bSum / count)
			a := uint32(aSum / count)
			// 合成到白底：out = premul + (1-alpha)*white（16 位空间）。
			white := 0xFFFF - a
			dst.SetRGBA(dx, dy, color.RGBA{
				R: uint8(min(0xFFFF, r+white) >> 8),
				G: uint8(min(0xFFFF, g+white) >> 8),
				B: uint8(min(0xFFFF, b+white) >> 8),
				A: 0xFF,
			})
		}
	}
	return dst
}

// premultipliedGetter 返回 16 位预乘像素读取函数；RGBA/NRGBA 走直接内存访问快路径
// （截图解码结果以这两种类型为主），其余走通用 At()。
func premultipliedGetter(img image.Image) func(x, y int) (uint32, uint32, uint32, uint32) {
	switch typed := img.(type) {
	case *image.RGBA:
		return func(x, y int) (uint32, uint32, uint32, uint32) {
			i := typed.PixOffset(x, y)
			return uint32(typed.Pix[i]) * 0x101, uint32(typed.Pix[i+1]) * 0x101,
				uint32(typed.Pix[i+2]) * 0x101, uint32(typed.Pix[i+3]) * 0x101
		}
	case *image.NRGBA:
		return func(x, y int) (uint32, uint32, uint32, uint32) {
			i := typed.PixOffset(x, y)
			a := uint32(typed.Pix[i+3])
			if a == 0 {
				return 0, 0, 0, 0
			}
			scale := a * 0x101
			return uint32(typed.Pix[i]) * 0x101 * scale / 0xFFFF,
				uint32(typed.Pix[i+1]) * 0x101 * scale / 0xFFFF,
				uint32(typed.Pix[i+2]) * 0x101 * scale / 0xFFFF,
				scale
		}
	default:
		return func(x, y int) (uint32, uint32, uint32, uint32) {
			return img.At(x, y).RGBA()
		}
	}
}

// imageDimensions 从 PNG/JPEG 头部解析宽高（避免为尺寸判断做整图解码）。
func imageDimensions(data []byte) (int, int, bool) {
	if len(data) >= 24 && bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")) {
		width := int(binary.BigEndian.Uint32(data[16:20]))
		height := int(binary.BigEndian.Uint32(data[20:24]))
		return width, height, width > 0 && height > 0
	}
	if len(data) > 4 && data[0] == 0xFF && data[1] == 0xD8 {
		i := 2
		for i+9 <= len(data) {
			if data[i] != 0xFF {
				i++
				continue
			}
			marker := data[i+1]
			if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
				i += 2
				continue
			}
			segmentLen := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
			if segmentLen < 2 {
				return 0, 0, false
			}
			// SOF0..SOF15（除 DHT=C4 / JPG=C8 / DAC=CC）携带尺寸。
			if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
				height := int(binary.BigEndian.Uint16(data[i+5 : i+7]))
				width := int(binary.BigEndian.Uint16(data[i+7 : i+9]))
				return width, height, width > 0 && height > 0
			}
			i += 2 + segmentLen
		}
	}
	return 0, 0, false
}
