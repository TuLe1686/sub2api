package service

// 内联图片压缩结果的内容寻址缓存（image-compress 补丁，rev 2）。
//
// 动机：同一个会话每轮全量重发历史，图片内容逐轮不变——没有缓存的实现会把
// 同一批图片反复解码/缩放/重编码（服务器实测 ~3.8s / 53.8MB·6 图），浪费 CPU。
// 本缓存按"原始 data URL 字符串 + 参数指纹"的 SHA256 寻址：
//   - 命中正结果：直接复用上次压好的替换字节（确定性函数 ⇒ 字节一致，不影响缓存前缀语义）；
//   - 命中负结果（压缩无收益/不可压）：跳过整个解码路径；
//   - 有界：字节预算 + 条目数上限的 LRU（默认 96MiB / 8192 条），超限淘汰最久未用。
// 进程内单例，随容器重启清空（重建成本仅首轮一次）。

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/MACOS-DO/sub4api/internal/config"
)

const (
	imageCompressCacheNegativeEntryBytes = int64(256) // 负结果按小条目计费
	imageCompressCacheMaxEntriesDefault  = 8192
)

type imageCompressCacheEntry struct {
	key   string
	value []byte // nil = 负结果（保留原图）
	size  int64
}

type imageCompressCache struct {
	mu          sync.Mutex
	budget      int64
	used        int64
	maxEntries  int
	fingerprint string
	entries     map[string]*list.Element
	order       *list.List // front = 最近使用
}

func imageCompressFingerprint(cfg config.GatewayRequestImageCompressConfig) string {
	return fmt.Sprintf("ic1|%d|%d|%d|%d", cfg.MaxDimension, cfg.JPEGQuality, cfg.MinImageBytes, cfg.MaxPixels)
}

func newImageCompressCache(budgetBytes int64, fingerprint string) *imageCompressCache {
	maxEntries := imageCompressCacheMaxEntriesDefault
	if budgetBytes <= 0 {
		return nil
	}
	return &imageCompressCache{
		budget:      budgetBytes,
		maxEntries:  maxEntries,
		fingerprint: fingerprint,
		entries:     make(map[string]*list.Element),
		order:       list.New(),
	}
}

func (c *imageCompressCache) keyFor(raw []byte) string {
	hasher := sha256.New()
	hasher.Write([]byte(c.fingerprint))
	hasher.Write(raw)
	return hex.EncodeToString(hasher.Sum(nil))
}

// get 返回 (替换字节或 nil, 是否命中)。value==nil 且 hit 表示负结果命中。
func (c *imageCompressCache) get(key string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(elem)
	return elem.Value.(*imageCompressCacheEntry).value, true
}

func (c *imageCompressCache) put(key string, value []byte) {
	if c == nil {
		return
	}
	size := int64(len(value))
	if size == 0 {
		size = imageCompressCacheNegativeEntryBytes
	}
	if size > c.budget {
		return // 单条就超预算：不缓存（如异常大的替换结果）
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[key]; ok {
		c.order.MoveToFront(existing)
		return
	}
	for c.used+size > c.budget || c.order.Len() >= c.maxEntries {
		back := c.order.Back()
		if back == nil {
			break
		}
		entry := back.Value.(*imageCompressCacheEntry)
		c.order.Remove(back)
		delete(c.entries, entry.key)
		c.used -= entry.size
	}
	elem := c.order.PushFront(&imageCompressCacheEntry{key: key, value: value, size: size})
	c.entries[key] = elem
	c.used += size
}

// stats 供运行观测（条目数/占用）。
func (c *imageCompressCache) stats() (entries int, usedBytes int64) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len(), c.used
}

// 进程内单例：首次使用时按配置（Budget/Fingerprint）创建；容器重启清空。
var imageCompressCacheSingletonState struct {
	sync.Mutex
	cache *imageCompressCache
}

func imageCompressCacheFor(cfg config.GatewayRequestImageCompressConfig) *imageCompressCache {
	if cfg.CacheBytes <= 0 {
		return nil
	}
	imageCompressCacheSingletonState.Lock()
	defer imageCompressCacheSingletonState.Unlock()
	if imageCompressCacheSingletonState.cache == nil {
		imageCompressCacheSingletonState.cache = newImageCompressCache(cfg.CacheBytes, imageCompressFingerprint(cfg))
	}
	return imageCompressCacheSingletonState.cache
}
