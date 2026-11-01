package service

// Responses 内联图片压缩（image-compress 补丁）单元测试。
// 全部使用确定性输入：压缩产物必须逐字节可复现（prompt 缓存前缀稳定性依赖于此）。

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"unsafe"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// imageCompressTestPNG 生成确定性噪声 PNG（PNG 压不小、降采样后 JPEG 显著压小）。
func imageCompressTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	rng := rand.New(rand.NewSource(42))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(rng.Intn(256)),
				G: uint8(rng.Intn(256)),
				B: uint8(rng.Intn(256)),
				A: 0xFF,
			})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func imageCompressTestDataURL(t *testing.T, payload []byte, mime string) string {
	t.Helper()
	return "data:image/" + mime + ";base64," + base64.StdEncoding.EncodeToString(payload)
}

// imageCompressTestBody 构造含若干内联图片的 Responses 形状 body。
func imageCompressTestBody(t *testing.T, dataURLs []string) []byte {
	t.Helper()
	var input bytes.Buffer
	input.WriteString(`{"type":"message","role":"user","content":[`)
	for index, url := range dataURLs {
		if index > 0 {
			input.WriteString(",")
		}
		input.WriteString(`{"type":"input_image","image_url":"`)
		input.WriteString(url)
		input.WriteString(`"}`)
	}
	input.WriteString(`]}`)
	return []byte(`{"model":"gpt-6.1-sol","stream":true,"input":[` + input.String() + `]}`)
}

func imageCompressTestService(cfg config.GatewayRequestImageCompressConfig) *OpenAIGatewayService {
	return &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{RequestImageCompress: cfg}}}
}

func imageCompressTestConfig() config.GatewayRequestImageCompressConfig {
	return config.GatewayRequestImageCompressConfig{
		Enabled:       true,
		GroupIDs:      "10",
		TriggerBytes:  1024, // 测试用小阈值：语义与生产一致，只是数值不同
		MaxDimension:  128,
		JPEGQuality:   70,
		MinImageBytes: 0,
		MaxPixels:     100_000_000,
	}
}

func imageCompressTestContext(t *testing.T, path string, groupID int64) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	if groupID > 0 {
		gid := groupID
		c.Set("api_key", &APIKey{ID: 1, GroupID: &gid})
	}
	return c
}

func imageCompressTestParams(cfg config.GatewayRequestImageCompressConfig) imageCompressParams {
	return imageCompressParams{
		MaxDimension:  cfg.MaxDimension,
		JPEGQuality:   cfg.JPEGQuality,
		MinImageBytes: cfg.MinImageBytes,
		MaxPixels:     cfg.MaxPixels,
	}
}

func TestCompressResponsesInlineImages_CompressesOversizedBody(t *testing.T) {
	cfg := imageCompressTestConfig()
	service := imageCompressTestService(cfg)
	images := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		images = append(images, imageCompressTestDataURL(t, imageCompressTestPNG(t, 800, 600), "png"))
	}
	body := imageCompressTestBody(t, images)
	original := append([]byte(nil), body...)

	out, changed := service.compressResponsesInlineImages(imageCompressTestContext(t, "/v1/responses", 10), body)

	require.True(t, changed)
	require.Less(t, len(out), len(body)/2, "压缩后应显著变小")
	require.True(t, bytes.Equal(body, original), "原 body 不得被就地改写")
	// 幂等性：第二遍压缩（同输入）必须逐字节一致（缓存前缀稳定性）。
	outSecond, _, changedSecond := compressInlineImages(original, imageCompressTestParams(cfg))
	require.True(t, changedSecond)
	require.True(t, bytes.Equal(out, outSecond), "同一输入两次压缩结果必须逐字节一致")
	// JSON 结构保留 + mime 前缀改写。
	require.Contains(t, string(out), `"data:image/jpeg;base64,`)
	require.NotContains(t, string(out), `"data:image/png;base64,`)
	require.Contains(t, string(out), `"model":"gpt-6.1-sol"`)
}

func TestCompressResponsesInlineImages_GroupGateSkips(t *testing.T) {
	cfg := imageCompressTestConfig()
	service := imageCompressTestService(cfg)
	body := imageCompressTestBody(t, []string{imageCompressTestDataURL(t, imageCompressTestPNG(t, 400, 300), "png")})

	out, changed := service.compressResponsesInlineImages(imageCompressTestContext(t, "/v1/responses", 11), body)

	require.False(t, changed)
	require.Equal(t, unsafe.SliceData(body), unsafe.SliceData(out), "未命中门控必须零分配返回原 slice")
}

func TestCompressResponsesInlineImages_TriggerAndSwitchGates(t *testing.T) {
	body := imageCompressTestBody(t, []string{imageCompressTestDataURL(t, imageCompressTestPNG(t, 400, 300), "png")})
	c := imageCompressTestContext(t, "/v1/responses", 10)

	// 阈值未到：返回原 slice。
	highTrigger := imageCompressTestConfig()
	highTrigger.TriggerBytes = int64(len(body)) + 1
	out, changed := imageCompressTestService(highTrigger).compressResponsesInlineImages(c, body)
	require.False(t, changed)
	require.Equal(t, unsafe.SliceData(body), unsafe.SliceData(out))

	// 总开关关闭：返回原 slice。
	disabled := imageCompressTestConfig()
	disabled.Enabled = false
	out, changed = imageCompressTestService(disabled).compressResponsesInlineImages(c, body)
	require.False(t, changed)
	require.Equal(t, unsafe.SliceData(body), unsafe.SliceData(out))

	// nil cfg 的 service 不得 panic。
	out, changed = (&OpenAIGatewayService{}).compressResponsesInlineImages(c, body)
	require.False(t, changed)
	require.Equal(t, unsafe.SliceData(body), unsafe.SliceData(out))
}

func TestCompressResponsesInlineImages_CompactPathSkips(t *testing.T) {
	service := imageCompressTestService(imageCompressTestConfig())
	body := imageCompressTestBody(t, []string{imageCompressTestDataURL(t, imageCompressTestPNG(t, 400, 300), "png")})

	out, changed := service.compressResponsesInlineImages(imageCompressTestContext(t, "/v1/responses/compact", 10), body)

	require.False(t, changed)
	require.Equal(t, unsafe.SliceData(body), unsafe.SliceData(out))
}

func TestCompressInlineImages_SkipsSmallForeignAndCorrupt(t *testing.T) {
	cfg := imageCompressTestConfig()
	cfg.MinImageBytes = 64 << 10 // 小图跳过
	small := imageCompressTestDataURL(t, imageCompressTestPNG(t, 24, 24), "png")
	foreign := "data:image/webp;base64," + base64.StdEncoding.EncodeToString(imageCompressTestPNG(t, 400, 300))
	corrupt := "data:image/png;base64,!!!not-base64!!!"
	// 文本字段里恰好出现 data URL 字面量：不是图片、尺寸解析失败 → 必须原样保留。
	textLike := "data:image/png;base64,AAAA"
	body := imageCompressTestBody(t, []string{small, foreign, corrupt, textLike})

	out, stats, changed := compressInlineImages(body, imageCompressTestParams(cfg))

	require.False(t, changed, "全部跳过时不得改写")
	// 候选=3（webp 不是候选，不计数）：小图 / 坏 base64 / 非图片字面量全部跳过。
	require.Equal(t, 3, stats.Total)
	require.Equal(t, 0, stats.Compressed)
	require.Equal(t, unsafe.SliceData(body), unsafe.SliceData(out))
}

func TestCompressInlineImages_MixedKeepsUnchangedBytesIntact(t *testing.T) {
	cfg := imageCompressTestConfig()
	compressible := imageCompressTestDataURL(t, imageCompressTestPNG(t, 800, 600), "png")
	foreign := "data:image/webp;base64," + base64.StdEncoding.EncodeToString(imageCompressTestPNG(t, 64, 64))
	body := imageCompressTestBody(t, []string{foreign, compressible})

	out, stats, changed := compressInlineImages(body, imageCompressTestParams(cfg))

	require.True(t, changed)
	// 候选=1（webp 不入候选），压缩成功 1 张；不可压缩的 webp 字符串逐字节保留。
	require.Equal(t, 1, stats.Total)
	require.Equal(t, 1, stats.Compressed)
	require.Contains(t, string(out), foreign, "不可压缩的图片必须逐字节原样保留")
}

func TestCompressImageDataURL_DownscaleKeepsAspect(t *testing.T) {
	cfg := imageCompressTestConfig()
	payload := imageCompressTestPNG(t, 2000, 1000)
	raw := []byte("data:image/png;base64," + base64.StdEncoding.EncodeToString(payload))

	out, ok := compressImageDataURL(raw, imageCompressTestParams(cfg))

	require.True(t, ok)
	require.True(t, bytes.HasPrefix(out, []byte("data:image/jpeg;base64,")))
	decoded, err := base64.StdEncoding.DecodeString(string(out[len("data:image/jpeg;base64,"):]))
	require.NoError(t, err)
	img, err := jpeg.Decode(bytes.NewReader(decoded))
	require.NoError(t, err)
	bounds := img.Bounds()
	require.Equal(t, 128, bounds.Dx(), "长边必须压到 max_dimension")
	require.Equal(t, 64, bounds.Dy(), "纵横比必须保持")
}

func TestCompressImageDataURL_AlreadySmallJPEGIsKept(t *testing.T) {
	cfg := imageCompressTestConfig()
	source := image.NewRGBA(image.Rect(0, 0, 100, 50))
	var jpegBuf bytes.Buffer
	require.NoError(t, jpeg.Encode(&jpegBuf, source, &jpeg.Options{Quality: 80}))
	raw := []byte("data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegBuf.Bytes()))

	out, ok := compressImageDataURL(raw, imageCompressTestParams(cfg))

	require.False(t, ok, "已在尺寸内的 JPEG 不应二次压缩")
	require.Nil(t, out)
}

func TestFindInlineImageSpans_IgnoresLiteralTextAndEscapedQuotes(t *testing.T) {
	// 文本里出现 data:image/ 字面量但没有 ;base64,；以及转义引号场景。
	body := []byte(`{"input":[{"content":"look at \"data:image/png\" and data:image/png;base64,AAAA here"}]}`)
	spans := findInlineImageSpans(body)
	require.Empty(t, spans)
}

func TestImageDimensions_PNGAndJPEG(t *testing.T) {
	width, height, ok := imageDimensions(imageCompressTestPNG(t, 300, 200))
	require.True(t, ok)
	require.Equal(t, 300, width)
	require.Equal(t, 200, height)

	var jpegBuf bytes.Buffer
	require.NoError(t, jpeg.Encode(&jpegBuf, image.NewRGBA(image.Rect(0, 0, 123, 45)), &jpeg.Options{Quality: 80}))
	width, height, ok = imageDimensions(jpegBuf.Bytes())
	require.True(t, ok)
	require.Equal(t, 123, width)
	require.Equal(t, 45, height)

	_, _, ok = imageDimensions([]byte("not an image"))
	require.False(t, ok)
}

func TestImageCompressGroupAllowed(t *testing.T) {
	require.True(t, imageCompressGroupAllowed("10", 10))
	require.True(t, imageCompressGroupAllowed("10, 12", 12))
	require.True(t, imageCompressGroupAllowed(" 8 ,10,", 10))
	require.False(t, imageCompressGroupAllowed("10", 11))
	require.False(t, imageCompressGroupAllowed("", 10))
	require.False(t, imageCompressGroupAllowed("10", 0))
}

func imageCompressTestCache(cfg config.GatewayRequestImageCompressConfig) *imageCompressCache {
	return newImageCompressCache(64<<20, imageCompressFingerprint(cfg))
}

func imageCompressTestConfigView(cfg config.GatewayRequestImageCompressConfig) config.GatewayRequestImageCompressConfig {
	return config.GatewayRequestImageCompressConfig{
		MaxDimension: cfg.MaxDimension, JPEGQuality: cfg.JPEGQuality,
		MinImageBytes: cfg.MinImageBytes, MaxPixels: cfg.MaxPixels,
	}
}

func TestCompressInlineImages_CacheHitsSkipRecompression(t *testing.T) {
	cfg := imageCompressTestConfig()
	params := imageCompressTestParams(cfg)
	params.Cache = imageCompressTestCache(imageCompressTestConfigView(cfg))
	body := imageCompressTestBody(t, []string{
		imageCompressTestDataURL(t, imageCompressTestPNG(t, 300, 200), "png"),
		imageCompressTestDataURL(t, imageCompressTestPNG(t, 320, 240), "png"),
	})

	out1, stats1, changed1 := compressInlineImages(body, params)
	require.True(t, changed1)
	require.Equal(t, 2, stats1.Compressed)
	require.Equal(t, 0, stats1.CacheHits)

	out2, stats2, changed2 := compressInlineImages(body, params)
	require.True(t, changed2)
	// 命中路径同样计入 Compressed（语义=被替换张数）；零重压的信号是 CacheHits 全覆盖。
	require.Equal(t, 2, stats2.CacheHits, "第二轮应全部走缓存")
	require.Equal(t, stats1.Compressed, stats2.Compressed)
	require.True(t, bytes.Equal(out1, out2), "缓存命中输出必须与首轮逐字节一致")
}

func TestCompressInlineImages_NegativeCacheForKeptImages(t *testing.T) {
	cfg := imageCompressTestConfig()
	cfg.MinImageBytes = 1 << 20 // 小图必被保留 → 负结果
	params := imageCompressTestParams(cfg)
	params.Cache = imageCompressTestCache(imageCompressTestConfigView(cfg))
	body := imageCompressTestBody(t, []string{
		imageCompressTestDataURL(t, imageCompressTestPNG(t, 64, 64), "png"),
	})

	_, stats1, changed1 := compressInlineImages(body, params)
	require.False(t, changed1)
	require.Equal(t, 1, stats1.Skipped)
	require.Equal(t, 0, stats1.CacheHits)

	_, stats2, _ := compressInlineImages(body, params)
	require.Equal(t, 1, stats2.CacheHits, "保留决策应命中负结果缓存")
	require.Equal(t, 1, stats2.Skipped)
}

func TestImageCompressCache_LRUAndBudget(t *testing.T) {
	cache := newImageCompressCache(1000, "fp")
	cache.put("a", bytes.Repeat([]byte{1}, 600))
	cache.put("b", bytes.Repeat([]byte{2}, 600))
	if _, ok := cache.get("a"); ok {
		t.Fatal("超预算应淘汰最久未用条目")
	}
	if value, ok := cache.get("b"); !ok || len(value) != 600 {
		t.Fatalf("最近条目应保留: ok=%v len=%d", ok, len(value))
	}
	cache.put("neg", nil)
	if value, ok := cache.get("neg"); !ok || value != nil {
		t.Fatal("负结果命中语义错误")
	}
	cache.put("oversize", bytes.Repeat([]byte{3}, 2000))
	if _, ok := cache.get("oversize"); ok {
		t.Fatal("单条超预算不应入缓存")
	}
}
