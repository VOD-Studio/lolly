// Package compression 提供预压缩静态文件支持。
//
// 该文件实现预压缩文件的检测和发送，避免实时压缩开销。
//
// 主要功能：
//   - 检测 .gz 和 .br 预压缩文件
//   - 按优先级选择编码（brotli > gzip）
//   - 客户端 Accept-Encoding 协商
//   - 扩展名过滤，仅对文本类资源启用预压缩
//
// 主要用途：
//
//	用于发送预压缩的静态资源文件，减少服务器 CPU 开销，提升响应速度。
//
// 使用场景：
//   - 静态资源预先压缩（如 CSS、JS、HTML 文件）
//   - 构建时生成 .gz 或 .br 文件
//   - 运行时直接发送预压缩文件，避免实时压缩
//
// 注意事项：
//   - 需要客户端支持对应的 Content-Encoding
//   - 预压缩文件需要与源文件放在同一目录
//
// 作者：xfy
package compression

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/mimeutil"
	"rua.plus/lolly/internal/utils"
)

// existResult 预压缩文件存在性缓存结果。
//
// 采用永不过期策略，与 nginx gzip_static 行为一致：
// 预压缩文件通常在构建时生成，运行时不会频繁增删。
// 如需更新缓存，重启服务即可。
type existResult struct {
	// exists 预压缩文件是否存在
	exists bool
}

// GzipStatic 预压缩文件支持中间件。
//
// 检查是否存在预压缩的 .gz 或 .br 文件，如果存在且客户端支持对应编码，
// 则直接发送，避免实时压缩的 CPU 开销。
type GzipStatic struct {
	root                    string
	precompressedExtensions []string
	extensions              []string
	extSet                  map[string]bool
	enabled                 bool
	existCache              sync.Map
}

// NewGzipStatic 创建预压缩文件处理器。
//
// 根据配置创建预压缩文件处理器实例，设置默认扩展名列表。
//
// 参数：
//   - enabled: 是否启用预压缩支持
//   - root: 静态文件根目录路径
//   - extensions: 支持预压缩的源文件扩展名列表（如 .html, .css），为空则使用默认值
//   - precompressedExtensions: 预压缩文件扩展名列表（如 .br, .gz），为空则使用默认值
//
// 返回值：
//   - *GzipStatic: 创建的预压缩文件处理器
func NewGzipStatic(enabled bool, root string, extensions, precompressedExtensions []string) *GzipStatic {
	if len(extensions) == 0 {
		extensions = []string{".html", ".css", ".js", ".json", ".xml", ".svg", ".txt"}
	}
	if len(precompressedExtensions) == 0 {
		precompressedExtensions = []string{".br", ".gz"}
	}
	g := &GzipStatic{
		enabled:                 enabled,
		root:                    root,
		extensions:              extensions,
		precompressedExtensions: precompressedExtensions,
	}
	g.extSet = make(map[string]bool, len(g.extensions))
	for _, e := range g.extensions {
		g.extSet[strings.ToLower(e)] = true
	}
	return g
}

// ServeFile 发送预压缩文件（如果存在）。
//
// 检查是否存在对应的 .br 或 .gz 文件，按优先级（br > gzip）选择：
// 1. 如果客户端支持 br 且 .br 文件存在，返回 .br 文件
// 2. 否则如果客户端支持 gzip 且 .gz 文件存在，返回 .gz 文件
//
// 参数：
//   - ctx: FastHTTP 请求上下文
//   - filePath: 请求的文件路径
//
// 返回值：
//   - bool: true 表示已发送预压缩文件，false 表示未发送
func (g *GzipStatic) ServeFile(ctx *fasthttp.RequestCtx, filePath string) bool {
	ext, fullPath, ok := g.selectPrecompressed(ctx.Request.Header.Peek("Accept-Encoding"), filePath)
	if !ok {
		return false
	}

	// 设置 Content-Encoding 头
	ctx.Response.Header.Set("Content-Encoding", precompressedEncoding(ext))
	ctx.Response.Header.Set("Vary", "Accept-Encoding")
	// 设置原始文件的 Content-Type
	// filePath 是原始文件路径 (如 "test.js")，直接使用即可
	ctx.Response.Header.SetContentType(mimeutil.DetectContentType(filePath))

	fasthttp.ServeFile(ctx, fullPath)
	return true
}

// SelectEncoding 返回 ServeFile 会为该请求选用的预压缩内容编码（"br" 或 "gzip"），
// 不会发送预压缩文件时返回空字符串。与 ServeFile 共用同一选择逻辑，且不修改响应。
//
// 用于在发送前评估条件请求：预压缩表示的 ETag 与 identity 表示不同。
func (g *GzipStatic) SelectEncoding(ctx *fasthttp.RequestCtx, filePath string) string {
	ext, _, ok := g.selectPrecompressed(ctx.Request.Header.Peek("Accept-Encoding"), filePath)
	if !ok {
		return ""
	}
	return precompressedEncoding(ext)
}

// precompressedEncoding 将预压缩文件扩展名映射为 Content-Encoding 值。
func precompressedEncoding(ext string) string {
	switch ext {
	case ".br":
		return "br"
	case ".gz":
		return "gzip"
	}
	return ""
}

// selectPrecompressed 按优先级选择要发送的预压缩文件。
// 返回所选扩展名、预压缩文件完整路径；不适用时 ok 为 false。
func (g *GzipStatic) selectPrecompressed(acceptEncoding []byte, filePath string) (ext, fullPath string, ok bool) {
	if !g.enabled {
		return "", "", false
	}

	// 检查文件扩展名
	if !g.matchExtension(filePath) {
		return "", "", false
	}

	// 安全检查：防止目录遍历
	if strings.Contains(filePath, "..") {
		return "", "", false
	}

	// 按优先级检查预压缩文件
	for _, ext := range g.precompressedExtensions {
		// 检查客户端是否支持该编码
		if !supportsEncoding(acceptEncoding, ext) {
			continue
		}

		// 构建预压缩文件路径
		compressedPath := filePath + ext
		fullPath := filepath.Join(g.root, compressedPath)

		// 检查文件是否存在（优先查缓存）
		var exists bool
		if cached, ok := g.existCache.Load(fullPath); ok {
			if er, ok := cached.(existResult); ok {
				exists = er.exists
			} else {
				// 意外类型，回退到 os.Stat
				_, err := os.Stat(fullPath)
				exists = err == nil
				g.existCache.Store(fullPath, existResult{exists: exists})
			}
		} else {
			_, err := os.Stat(fullPath)
			exists = err == nil
			g.existCache.Store(fullPath, existResult{exists: exists})
		}
		if !exists {
			continue
		}
		if precompressedEncoding(ext) == "" {
			continue
		}
		return ext, fullPath, true
	}

	return "", "", false
}

// matchExtension 检查文件扩展名是否在支持的预压缩扩展名列表中。
//
// 参数：
//   - filePath: 文件路径
//
// 返回值：
//   - bool: true 表示文件扩展名匹配支持列表
func (g *GzipStatic) matchExtension(filePath string) bool {
	ext := strings.ToLower(filepath.Ext(filePath))
	return g.extSet[ext]
}

// supportsEncoding 检查客户端是否支持指定编码。
//
// 通过检查 Accept-Encoding 请求头中是否包含指定编码名称。
// 忽略 q-value（质量因子），固定优先级由遍历顺序决定。
//
// 参数：
//   - acceptEncoding: Accept-Encoding 请求头值
//   - ext: 预压缩文件扩展名（如 ".br"、".gz"）
//
// 返回值：
//   - bool: true 表示客户端支持该编码
func supportsEncoding(acceptEncoding []byte, ext string) bool {
	if len(acceptEncoding) == 0 {
		return false
	}
	switch ext {
	case ".br":
		return utils.BytesContainsFold(acceptEncoding, []byte("br"))
	case ".gz":
		return utils.BytesContainsFold(acceptEncoding, []byte("gzip"))
	default:
		return false
	}
}
