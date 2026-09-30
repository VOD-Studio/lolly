// Package handler 提供 HTTP 请求处理器，包括路由、静态文件服务和零拷贝传输。
//
// 该文件包含静态文件服务相关的核心逻辑，包括：
//   - 静态文件请求处理
//   - 目录索引文件支持
//   - 文件缓存和零拷贝传输优化
//   - 预压缩文件支持
//
// 主要用途：
//
//	用于提供静态文件服务，支持缓存和零拷贝传输优化。
//
// 注意事项：
//   - 自动处理目录遍历攻击防护
//   - 支持多索引文件（如 index.html、index.htm）
//   - 支持预压缩 .gz 文件
//   - 支持单区间 Range 请求（206/416，见 range.go）
//
// 作者：xfy
package handler

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/cache"
	"rua.plus/lolly/internal/middleware/compression"
	"rua.plus/lolly/internal/mimeutil"
	"rua.plus/lolly/internal/utils"
)

const httpTimeFormat = "Mon, 02 Jan 2006 15:04:05 GMT"

// Expires directive constants
const (
	expiresOff = "off"
	expiresMax = "max"
)

// statWithCache 从 FileInfo 缓存获取文件信息，支持负缓存。
//
// 返回值：
//   - info: 文件信息（命中负缓存或出错时为 nil）
//   - ok:   true 表示文件存在且缓存命中
//   - err:  错误信息（命中负缓存时返回 nil，表示已知文件不存在）
func (h *StaticHandler) statWithCache(filePath string) (os.FileInfo, bool, error) {
	if h.fileInfoCache == nil {
		info, err := os.Stat(filePath)
		if err != nil {
			return nil, false, err
		}
		return info, true, nil
	}

	info, hit, exists := h.fileInfoCache.GetWithNotFound(filePath)
	if hit {
		if exists {
			return info, true, nil
		}
		// 命中负缓存：已知文件不存在
		return nil, false, os.ErrNotExist
	}

	// 缓存未命中，调用 os.Stat
	info, err := os.Stat(filePath)
	if err != nil {
		h.fileInfoCache.SetWithNotFound(filePath, nil, true)
		return nil, false, err
	}
	h.fileInfoCache.SetWithNotFound(filePath, info, false)
	return info, true, nil
}

// StaticHandler 静态文件处理器。
//
// 提供静态文件服务，支持目录索引、文件缓存和零拷贝传输。
//
// 注意事项：
//   - 自动处理目录遍历攻击防护（拒绝包含 ".." 的路径）
//   - 并发安全，可在多个 goroutine 中使用
//   - 大文件（>= 8KB）自动启用零拷贝传输
//   - alias 与 root 互斥，同时配置时 alias 优先
type StaticHandler struct {
	mu sync.RWMutex // 保护以下字段的并发访问
	// 指针类型字段（按大小排列）
	fileCache     *cache.FileCache
	fileInfoCache *FileInfoCache // FileInfo 缓存，减少 os.Stat 调用
	gzipStatic    *compression.GzipStatic
	router        *Router
	// 字符串字段
	root       string
	alias      string
	pathPrefix string
	expires    string // 缓存过期时间（nginx 兼容格式）
	// 切片字段
	index    []string
	tryFiles []string
	// AutoIndex 配置
	autoIndex          bool
	autoIndexFormat    string
	autoIndexLocaltime bool
	autoIndexExactSize bool
	// 基本类型字段
	pathPrefixLen int           // 预计算的路径前缀长度，用于零分配路径剥离
	cacheTTL      time.Duration // 缓存新鲜度 TTL（默认 5s，0 表示每次验证 ModTime）
	useSendfile   bool
	tryFilesPass  bool
	symlinkCheck  bool
	internal      bool
}

// NewStaticHandler 创建静态文件处理器。
//
// 初始化并返回一个新的静态文件处理器实例。
//
// 参数：
//   - root: 静态文件根目录路径
//   - pathPrefix: 路径前缀，会被剥离后拼接 root
//   - index: 索引文件列表，当请求目录时依次查找（如 ["index.html", "index.htm"]）
//   - useSendfile: 是否启用零拷贝传输（大文件优化）
//
// 返回值：
//   - *StaticHandler: 新创建的静态文件处理器
//
// 使用示例：
//
//	handler := handler.NewStaticHandler("/var/www", "/", []string{"index.html"}, true)
func NewStaticHandler(root, pathPrefix string, index []string, useSendfile bool) *StaticHandler {
	// 规范化 root 路径，确保 TrimPrefix 能正确工作
	// filepath.Clean 会去掉 ./ 并规范化路径分隔符
	cleanRoot := filepath.Clean(root)
	if !strings.HasSuffix(cleanRoot, string(filepath.Separator)) {
		cleanRoot += string(filepath.Separator)
	}

	// 预计算前缀长度，用于零分配路径剥离
	prefixLen := len(pathPrefix)
	if pathPrefix == "/" {
		prefixLen = 0 // 根路径无需剥离
	}

	h := &StaticHandler{
		root:          cleanRoot,
		pathPrefix:    pathPrefix,
		pathPrefixLen: prefixLen,
		index:         index,
		useSendfile:   useSendfile,
		fileInfoCache: NewFileInfoCache(),
	}
	// 默认 cacheTTL=0（每次验证 ModTime），同步禁用 fileInfoCache
	h.fileInfoCache.SetTTL(0)
	return h
}

// SetAlias 设置路径别名。
//
// alias 与 root 互斥，设置 alias 会清空 root。
//
// 参数：
//   - alias: 路径别名
func (h *StaticHandler) SetAlias(alias string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.alias = alias
	if alias != "" {
		h.root = ""
	}
}

// stripPathPrefix 剥离路径前缀（零分配）。
// 使用切片替代 strings.TrimPrefix，避免内存分配。
func (h *StaticHandler) stripPathPrefix(reqPath string) string {
	relPath := reqPath
	if h.pathPrefixLen > 0 {
		relPath = reqPath[h.pathPrefixLen:]
		if len(relPath) > 0 && relPath[0] != '/' {
			relPath = "/" + relPath
		}
	}
	return relPath
}

// buildFilePath 构建完整文件路径。
// 支持 alias 和 root 两种模式。
func (h *StaticHandler) buildFilePath(relPath string) string {
	if h.alias != "" {
		return filepath.Join(h.alias, relPath)
	}
	return filepath.Join(h.root, relPath)
}

// SetFileCache 设置文件缓存。
//
// 为静态文件处理器启用文件缓存功能。
// 缓存可以显著提升小文件的访问性能。
//
// 参数：
//   - fc: 文件缓存实例
//
// 注意事项：
//   - 仅对小于 1MB 的文件启用缓存
//   - 缓存会自动检测文件修改并更新
func (h *StaticHandler) SetFileCache(fc *cache.FileCache) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fileCache = fc
}

// SetGzipStatic 设置预压缩文件支持。
//
// 启用后，对于匹配扩展名的请求，优先发送预压缩文件。
//
// 参数：
//   - enabled: 是否启用预压缩支持
//   - extensions: 支持预压缩的源文件扩展名列表（如 [".html", ".css", ".js"]），为空使用默认值
//   - precompressedExtensions: 预压缩文件扩展名列表（如 [".br", ".gz"]），为空使用默认值
//
// 使用示例：
//
//	handler.SetGzipStatic(true, nil, []string{".gz", ".br"})
func (h *StaticHandler) SetGzipStatic(enabled bool, extensions, precompressedExtensions []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if enabled {
		h.gzipStatic = compression.NewGzipStatic(true, h.root, extensions, precompressedExtensions)
	}
}

// SetTryFiles 设置 try_files 配置。
//
// 配置按顺序尝试查找的文件列表，支持 $uri 和 $uri/ 占位符。
// 用于 SPA 部署，当请求的文件不存在时可以回退到指定文件。
//
// 参数：
//   - tryFiles: 按顺序尝试的文件列表，如 ["$uri", "$uri/", "/index.html"]
//   - tryFilesPass: 内部重定向是否触发中间件，默认为 false
//   - router: 当 tryFilesPass 为 true 时使用的路由器
//
// 使用示例：
//
//	handler.SetTryFiles([]string{"$uri", "$uri/", "/index.html"}, false, nil)
func (h *StaticHandler) SetTryFiles(tryFiles []string, tryFilesPass bool, router *Router) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tryFiles = tryFiles
	h.tryFilesPass = tryFilesPass
	h.router = router
}

// SetSymlinkCheck 设置符号链接安全检查。
//
// 启用后，服务文件前会验证符号链接指向的文件是否在允许的根目录范围内。
// 防止通过符号链接访问敏感文件（如 /etc/passwd）。
//
// 参数：
//   - enabled: 是否启用符号链接安全检查
func (h *StaticHandler) SetSymlinkCheck(enabled bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.symlinkCheck = enabled
}

// SetInternal 设置内部访问限制。
//
// 启用后，仅允许内部重定向访问该静态位置。
// 外部直接请求将返回 404 Not Found。
//
// 参数：
//   - enabled: 是否启用内部访问限制
func (h *StaticHandler) SetInternal(enabled bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.internal = enabled
}

// SetExpires 设置缓存过期时间。
//
// 支持 nginx 兼容格式：30d, 1h, 1m, max, epoch, off
// 设置后会在响应中添加 Cache-Control 和 Expires 头。
//
// 参数：
//   - expires: 过期时间字符串
func (h *StaticHandler) SetExpires(expires string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expires = expires
}

// SetAutoIndex 设置目录列表功能。
//
// 启用后，当请求目录且没有索引文件时，生成目录列表页面。
//
// 参数：
//   - enabled: 是否启用
//   - format: 输出格式（html/json/xml）
//   - localtime: 使用本地时间
//   - exactSize: 显示精确大小
func (h *StaticHandler) SetAutoIndex(enabled bool, format string, localtime, exactSize bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.autoIndex = enabled
	h.autoIndexFormat = format
	h.autoIndexLocaltime = localtime
	h.autoIndexExactSize = exactSize
}

// SetCacheTTL 设置缓存新鲜度 TTL。
//
// TTL 控制缓存条目的新鲜度验证间隔。
// 在 TTL 窗口内，缓存命中时跳过 ModTime 验证以减少 os.Stat 调用。
//
// 参数：
//   - ttl: TTL 时间间隔
//
// TTL 值说明：
//   - ttl > 0: TTL 内跳过 ModTime 验证，过期后验证
//   - ttl = 0: 每次请求验证 ModTime（向后兼容）
//
// 默认 TTL 为 5 秒。
func (h *StaticHandler) SetCacheTTL(ttl time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cacheTTL = ttl
	if h.fileInfoCache != nil {
		h.fileInfoCache.SetTTL(ttl)
	}
}

// Handle 处理静态文件请求。
//
// 根据请求路径查找并返回对应的静态文件。
// 支持目录索引文件、try_files、缓存查找和零拷贝传输。
//
// 参数：
//   - ctx: fasthttp 请求上下文
//
// 处理流程：
//  1. 安全检查：防止目录遍历攻击
//  2. 如果配置了 try_files，按顺序尝试查找文件
//  3. 检查文件/目录是否存在
//  4. 如果是目录，尝试查找索引文件
//  5. 尝试发送预压缩文件
//  6. 尝试从缓存获取
//  7. 大文件使用零拷贝传输
//  8. 读取文件并存入缓存
func (h *StaticHandler) Handle(ctx *fasthttp.RequestCtx) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	reqPath := string(ctx.Path())

	// 检查 internal 限制
	if h.internal && !utils.IsInternalRedirect(ctx) {
		utils.SendError(ctx, utils.ErrNotFound)
		return
	}

	// 安全检查：解析后的绝对路径必须位于 root/alias 目录下
	requestedPath := string(ctx.Path())
	rawPath := string(ctx.Request.URI().PathOriginal())
	if !h.isPathSafe(requestedPath, rawPath) {
		utils.SendError(ctx, utils.ErrForbidden)
		return
	}

	// 如果配置了 try_files，按顺序尝试
	if len(h.tryFiles) > 0 {
		h.handleTryFiles(ctx, reqPath)
		return
	}

	// 标准处理流程
	h.handleStandard(ctx, reqPath)
}

// isPathSafe 检查请求路径解析后是否始终位于 root/alias 目录范围内。
// 它同时检查规范化后的路径和原始编码路径，防止通过 URL 编码的 ..
// 或经过 fasthttp 规范化后超出目录边界。
func (h *StaticHandler) isPathSafe(reqPath, rawPath string) bool {
	// 快速拒绝原始路径中的遍历片段（在 URL 解码/规范化之前）。
	// 特别拦截编码的 traversal，如 /..%2fsecret.txt。
	if strings.Contains(rawPath, "..") && strings.Contains(rawPath, "%") {
		return false
	}

	// 拒绝规范化路径中仍包含 .. 的字符串（如 file..txt）。
	if strings.Contains(reqPath, "..") {
		return false
	}

	base := h.root
	if h.alias != "" {
		base = h.alias
	}
	if base == "" {
		return false
	}

	absRoot, err := filepath.Abs(base)
	if err != nil {
		return false
	}

	// 使用与后续文件服务一致的相对路径计算方式，并单独清理相对部分，
	// 避免 filepath.Clean 删除对前缀剥离有意义的尾部斜杠。
	relPath := h.stripPathPrefix(reqPath)
	if relPath == "" {
		relPath = "."
	}
	target := filepath.Join(absRoot, filepath.Clean(relPath))
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return false
	}

	sep := string(filepath.Separator)
	if !strings.HasPrefix(absTarget, absRoot+sep) && absTarget != absRoot {
		return false
	}

	return true
}

// tryServeFromFileCache 尝试从文件缓存直接响应。
// 命中缓存且文件未修改时直接写入响应并返回 true。
func (h *StaticHandler) tryServeFromFileCache(ctx *fasthttp.RequestCtx, filePath string, info os.FileInfo) bool {
	if h.fileCache == nil {
		return false
	}
	entry, ok := h.fileCache.Get(filePath)
	if !ok {
		return false
	}

	// 缓存命中路径不会发送预压缩文件，响应内容始终是 identity 字节，
	// 仅可能被后续压缩中间件动态压缩；Range 生效时则不会被压缩。
	spec := evalRange(ctx, int64(len(entry.Data)), entry.ETag, info.ModTime())
	respTag, mayFallBack := h.representationETag(ctx, entry.ETag, filePath, entry.ContentType, int64(len(entry.Data)), spec, false)

	// respond 写入缓存内容，按 Range 请求截取（206/416）。
	respond := func() {
		ctx.Response.Header.SetContentType(entry.ContentType)
		ctx.Response.Header.Set("ETag", entry.ETag)
		ctx.Response.Header.Set("Last-Modified", info.ModTime().UTC().Format(httpTimeFormat))
		spec.apply(ctx)
		if spec.state != rangeUnsatisfiable {
			ctx.Response.SetBody(spec.slice(entry.Data))
		}
	}

	// TTL 验证（cacheTTL > 0 时启用）
	if h.cacheTTL > 0 && time.Since(entry.CachedAt) < h.cacheTTL {
		if notModifiedForRepresentation(ctx, entry.ETag, respTag, mayFallBack, info.ModTime()) {
			ctx.Response.SetStatusCode(fasthttp.StatusNotModified)
			ctx.Response.Header.Set("ETag", respTag)
			ctx.Response.Header.Set("Last-Modified", info.ModTime().UTC().Format(httpTimeFormat))
			ctx.Response.Header.Set("Accept-Ranges", "bytes")
			ctx.Response.SkipBody = true
			return true
		}
		respond()
		return true
	}

	// TTL 过期或未启用 TTL，验证文件新鲜度
	if entry.ModTime.Equal(info.ModTime()) {
		if notModifiedForRepresentation(ctx, entry.ETag, respTag, mayFallBack, info.ModTime()) {
			ctx.Response.SetStatusCode(fasthttp.StatusNotModified)
			ctx.Response.Header.Set("ETag", respTag)
			ctx.Response.Header.Set("Last-Modified", info.ModTime().UTC().Format(httpTimeFormat))
			ctx.Response.Header.Set("Accept-Ranges", "bytes")
			ctx.Response.SkipBody = true
			return true
		}
		if h.cacheTTL > 0 {
			h.fileCache.RefreshCachedAt(filePath)
		}
		respond()
		return true
	}

	// 文件已修改，删除缓存
	h.fileCache.Delete(filePath)
	return false
}

// handleTryFiles 处理 try_files 逻辑。
//
// 按顺序尝试查找文件，支持 $uri 和 $uri/ 占位符。
//
// 占位符说明：
//   - $uri: 请求路径对应的文件
//   - $uri/: 请求路径对应的目录下的索引文件
//
// 参数：
//   - ctx: fasthttp 请求上下文
//   - reqPath: 原始请求路径
func (h *StaticHandler) handleTryFiles(ctx *fasthttp.RequestCtx, reqPath string) {
	// 零分配路径剥离
	relPath := h.stripPathPrefix(reqPath)

	for _, tryFile := range h.tryFiles {
		// 解析占位符
		targetPath := h.resolveTryFilePath(tryFile, relPath)

		// 构建完整文件路径
		filePath := h.buildFilePath(targetPath)

		// 检查文件/目录是否存在（带负缓存）
		info, exists, _ := h.statWithCache(filePath)
		if !exists {
			continue // 不存在，尝试下一个
		}

		if info.IsDir() {
			// 如果是目录，尝试查找索引文件
			for _, idx := range h.index {
				idxPath := filepath.Join(filePath, idx)
				idxInfo, idxExists, _ := h.statWithCache(idxPath)
				if !idxExists {
					continue
				}
				if !idxInfo.IsDir() {
					if h.tryServeFromFileCache(ctx, idxPath, idxInfo) {
						return
					}
					h.serveFile(ctx, idxPath, idxInfo, false)
					return
				}
			}
			continue // 目录中没有索引文件，尝试下一个
		}

		// 找到文件，检查是否是内部重定向
		if tryFile != "$uri" && !strings.HasPrefix(tryFile, "$uri") {
			// 这是内部重定向（fallback 文件）
			h.handleInternalRedirect(ctx, targetPath)
			return
		}

		// 直接服务文件
		h.serveFile(ctx, filePath, info, false)
		return
	}

	// 所有 try_files 都未找到
	utils.SendError(ctx, utils.ErrNotFound)
}

// resolveTryFilePath 解析 try_files 中的占位符。
//
// 支持的占位符：
//   - $uri: 请求路径
//   - $uri/: 请求路径加斜杠
//   - $uri.<ext>: 请求路径加扩展名（如 $uri.html）
//
// nginx 兼容性说明：
//   - $uri 变量语义与 nginx try_files 一致
//   - 附加安全验证在 validateStatics 时执行
//
// 参数：
//   - tryFile: try_files 配置项（已在 validateStatics 时验证）
//   - relPath: 相对请求路径
//
// 返回值：
//   - string: 解析后的文件路径，根路径边界返回空字符串触发回退
func (h *StaticHandler) resolveTryFilePath(tryFile, relPath string) string {
	switch {
	// ====== 保留：现有逻辑 ======
	case tryFile == "$uri":
		return relPath
	case tryFile == "$uri/":
		return relPath + "/"

	// ====== 新增：动态后缀支持 ======
	case strings.HasPrefix(tryFile, "$uri."):
		// 提取后缀部分（包含点，如 ".html"）
		suffix := tryFile[4:] // "$uri" 是4个字符，后面是 ".html" 等后缀
		// 根路径边界处理：返回空字符串让 try_files 继续下一个条目
		// 避免 "/.html" 这样的隐藏文件名
		if relPath == "/" {
			return "" // 触发回退到下一个 try_files 条目
		}
		return relPath + suffix

	// ====== 保留：现有逻辑 ======
	case strings.HasPrefix(tryFile, "/"):
		// 绝对路径，直接返回（去掉开头的 /）
		return tryFile[1:]
	default:
		// 其他情况直接返回
		return tryFile
	}
}

// handleInternalRedirect 处理内部重定向。
//
// 当 try_files 的回退文件与原始请求不同时触发。
// 根据 tryFilesPass 配置决定是否重新进入中间件链。
//
// 参数：
//   - ctx: fasthttp 请求上下文
//   - targetPath: 重定向目标路径（相对于 root 或 alias）
func (h *StaticHandler) handleInternalRedirect(ctx *fasthttp.RequestCtx, targetPath string) {
	if h.tryFilesPass && h.router != nil {
		// tryFilesPass 为 true，重新进入中间件链
		// 修改请求路径后重新路由
		newPath := h.pathPrefix + targetPath
		if !strings.HasPrefix(newPath, "/") {
			newPath = "/" + newPath
		}
		ctx.Request.SetRequestURI(newPath)
		h.router.Handler()(ctx)
		return
	}

	// tryFilesPass 为 false（默认），直接服务文件，不触发中间件
	filePath := h.buildFilePath(targetPath)

	info, exists, err := h.statWithCache(filePath)
	if !exists {
		if err != nil {
			utils.SendError(ctx, utils.ErrNotFound)
		} else {
			utils.SendError(ctx, utils.ErrForbidden)
		}
		return
	}

	if info.IsDir() {
		utils.SendError(ctx, utils.ErrForbidden)
		return
	}
	h.serveFile(ctx, filePath, info, false)
}

// handleStandard 标准静态文件处理流程。
//
// 参数：
//   - ctx: fasthttp 请求上下文
//   - reqPath: 请求路径
func (h *StaticHandler) handleStandard(ctx *fasthttp.RequestCtx, reqPath string) {
	// 零分配路径剥离
	relPath := h.stripPathPrefix(reqPath)

	// 构建完整文件路径
	filePath := h.buildFilePath(relPath)

	// 检查文件/目录是否存在（带负缓存）
	info, exists, _ := h.statWithCache(filePath)
	if !exists {
		utils.SendError(ctx, utils.ErrNotFound)
		return
	}

	// 符号链接安全检查
	if h.symlinkCheck {
		if err := h.validateSymlink(filePath); err != nil {
			utils.SendError(ctx, utils.ErrForbidden)
			return
		}
	}

	// 如果是目录，尝试索引文件（带负缓存）
	if info.IsDir() {
		for _, idx := range h.index {
			idxPath := filepath.Join(filePath, idx)
			idxInfo, idxExists, _ := h.statWithCache(idxPath)
			if idxExists && !idxInfo.IsDir() {
				if h.tryServeFromFileCache(ctx, idxPath, idxInfo) {
					return
				}
				h.serveFile(ctx, idxPath, idxInfo, true)
				return
			}
		}
		// 尝试 autoindex
		if h.autoIndex {
			config := AutoIndexConfig{
				Format:    h.autoIndexFormat,
				Localtime: h.autoIndexLocaltime,
				ExactSize: h.autoIndexExactSize,
			}
			if GenerateAutoIndex(ctx, filePath, reqPath, config) {
				return
			}
		}
		utils.SendError(ctx, utils.ErrForbidden)
		return
	}

	// Phase 2: 缓存查找 + TTL 验证，减少 os.ReadFile 调用
	if h.tryServeFromFileCache(ctx, filePath, info) {
		return
	}

	// Phase 3: 缓存未命中，调用 serveFile 处理
	h.serveFile(ctx, filePath, info, true)
}

// serveFile 提供文件服务，支持缓存和零拷贝传输。
//
// 内部方法，负责实际的文件发送逻辑。
//
// 参数：
//   - ctx: fasthttp 请求上下文
//   - filePath: 文件绝对路径
//   - info: 文件信息（用于判断文件大小和修改时间）
func (h *StaticHandler) serveFile(ctx *fasthttp.RequestCtx, filePath string, info os.FileInfo, skipCacheLookup bool) {
	etag := utils.GenerateETag(info.ModTime(), info.Size())

	// 评估 Range 请求。Range 始终作用于未压缩（identity）内容，且 If-Range 只与 identity
	// 强 ETag 比较；Range 生效时不使用预压缩文件（见下），压缩中间件也不会压缩 206。
	// Range 的评估不依赖 If-None-Match，先算出它才能确定本次响应实际选用的表示。
	spec := evalRange(ctx, info.Size(), etag, info.ModTime())

	// 确定本次响应实际选用的表示（identity / gzip / br），条件请求只与该表示的 ETag 比较。
	// 例如响应将被 gzip 时，客户端持有的 identity ETag 不匹配，须返回 200 而非 304（RFC 9110 13.1.2）。
	contentType := mimeutil.DetectContentType(filePath)
	respTag, mayFallBack := h.representationETag(ctx, etag, filePath, contentType, info.Size(), spec, true)
	if notModifiedForRepresentation(ctx, etag, respTag, mayFallBack, info.ModTime()) {
		ctx.Response.SetStatusCode(fasthttp.StatusNotModified)
		ctx.Response.Header.Set("ETag", respTag)
		ctx.Response.Header.Set("Last-Modified", info.ModTime().UTC().Format(httpTimeFormat))
		ctx.Response.Header.Set("Accept-Ranges", "bytes")
		h.setCacheHeaders(ctx)
		ctx.Response.SkipBody = true
		return
	}

	// 区间不可满足时直接 416
	if spec.state == rangeUnsatisfiable {
		ctx.Response.Header.Set("ETag", etag)
		ctx.Response.Header.Set("Last-Modified", info.ModTime().UTC().Format(httpTimeFormat))
		spec.apply(ctx)
		return
	}

	// 尝试发送预压缩文件。
	// Range 生效（206/416）时跳过：预压缩文件是另一种编码表示，其字节偏移与原文件不同，
	// 直接对其应用 Range 会得到与 ETag/Content-Length 不一致的数据。
	// Range 被忽略（HEAD、If-Range 不匹配、非法/多区间）时正常发送预压缩文件。
	if h.gzipStatic != nil && spec.state == rangeNone {
		relPath := strings.TrimPrefix(filePath, h.root)
		// Range 已被忽略，但 fasthttp.ServeFile 会自行处理 Range（且不看 If-Range），
		// 会对预压缩字节返回 206；此处移除 Range 头，保证返回完整的压缩表示。
		ctx.Request.Header.Del("Range")
		if h.gzipStatic.ServeFile(ctx, relPath) {
			// 预压缩文件已发送，补充验证头。
			// 压缩表示与 identity 字节不同，使用带编码后缀的独立强 ETag。
			ctx.Response.Header.Set("ETag", utils.ETagForEncoding(etag, string(ctx.Response.Header.Peek("Content-Encoding"))))
			ctx.Response.Header.Set("Last-Modified", info.ModTime().UTC().Format(httpTimeFormat))
			h.setCacheHeaders(ctx)
			return
		}
	}

	// 尝试从缓存获取
	if !skipCacheLookup && h.fileCache != nil {
		if entry, ok := h.fileCache.Get(filePath); ok {
			// 检查文件是否被修改
			if entry.ModTime.Equal(info.ModTime()) {
				// 缓存命中且文件未修改
				// 使用缓存的 ETag 和 ContentType，避免重新生成
				ctx.Response.Header.SetContentType(entry.ContentType)
				ctx.Response.Header.Set("ETag", entry.ETag)
				ctx.Response.Header.Set("Last-Modified", info.ModTime().UTC().Format(httpTimeFormat))
				h.setCacheHeaders(ctx)
				spec.apply(ctx)
				ctx.Response.SetBody(spec.slice(entry.Data))
				return
			}
			// 文件已修改，删除旧缓存
			h.fileCache.Delete(filePath)
		}
	}

	// 大文件使用零拷贝传输
	// 使用 fasthttp 的 SetBodyStream，它会：
	// 1. 先写 HTTP 头到 bufio.Writer
	// 2. Flush HTTP 头到 socket（关键步骤）
	// 3. copyZeroAlloc → ReadFrom → sendfile
	// 这样保证 HTTP 头先发送，避免顺序错乱导致的 "200 0" malformed response
	// Range 请求同样走该路径，由 rangeFileReader 限定偏移与长度（仍可 sendfile）。
	if h.useSendfile && info.Size() >= MinSendfileSize {
		file, err := os.Open(filePath)
		if err == nil {
			var body io.Reader = file
			bodySize := info.Size()
			if spec.state == rangePartial {
				rr, rerr := newRangeFileReader(file, spec.start, spec.length)
				if rerr != nil {
					_ = file.Close()
					file = nil
				} else {
					body, bodySize = rr, spec.length
				}
			}
			if file != nil {
				ctx.Response.Header.SetContentType(mimeutil.DetectContentType(filePath))
				ctx.Response.Header.Set("ETag", etag)
				ctx.Response.Header.Set("Last-Modified", info.ModTime().UTC().Format(httpTimeFormat))
				h.setCacheHeaders(ctx)
				spec.apply(ctx)
				// SetBodyStream 会在 handler 返回后由 fasthttp 统一处理
				// HTTP 头写入、Flush 和 sendfile 的顺序
				ctx.Response.SetBodyStream(body, int(bodySize))
				return
			}
		}
	}

	// 读取文件内容
	data, err := os.ReadFile(filePath)
	if err != nil {
		utils.SendError(ctx, utils.ErrInternalError)
		return
	}

	// 存入缓存（仅对小文件缓存，缓存的始终是完整内容）
	if h.fileCache != nil && info.Size() < 1024*1024 { // < 1MB
		_ = h.fileCache.Set(filePath, data, info.Size(), info.ModTime(), contentType)
	}

	// 文件在 stat 之后被截断/替换时，按实际读取长度重新评估 Range
	if int64(len(data)) != info.Size() {
		spec = evalRange(ctx, int64(len(data)), etag, info.ModTime())
		if spec.state == rangeUnsatisfiable {
			spec.apply(ctx)
			return
		}
	}

	ctx.Response.Header.SetContentType(contentType)
	ctx.Response.Header.Set("ETag", etag)
	ctx.Response.Header.Set("Last-Modified", info.ModTime().UTC().Format(httpTimeFormat))
	h.setCacheHeaders(ctx)
	spec.apply(ctx)
	ctx.Response.SetBody(spec.slice(data))
}

// setCacheHeaders 设置缓存控制响应头。
func (h *StaticHandler) setCacheHeaders(ctx *fasthttp.RequestCtx) {
	if h.expires == "" || h.expires == expiresOff {
		return
	}

	if h.expires == "epoch" {
		ctx.Response.Header.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		ctx.Response.Header.Set("Expires", "Thu, 01 Jan 1970 00:00:00 GMT")
		return
	}

	if h.expires == expiresMax {
		ctx.Response.Header.Set("Cache-Control", "public, max-age=315360000, immutable")
		ctx.Response.Header.Set("Expires", time.Now().Add(315360000*time.Second).UTC().Format(httpTimeFormat))
		return
	}

	maxAge := parseExpires(h.expires)
	if maxAge > 0 {
		ctx.Response.Header.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
		ctx.Response.Header.Set("Expires", time.Now().Add(time.Duration(maxAge)*time.Second).UTC().Format(httpTimeFormat))
	}
}

// parseExpires 解析 nginx 兼容的过期时间格式。
// 支持格式：30d, 1h, 1m, 1s, 30d1h 等
// 返回秒数。
func parseExpires(expires string) int64 {
	if expires == "" || expires == expiresOff {
		return 0
	}
	if expires == expiresMax {
		return 315360000
	}
	if expires == "epoch" {
		return -1
	}

	var total int64
	var num int64
	for _, ch := range expires {
		switch {
		case ch >= '0' && ch <= '9':
			num = num*10 + int64(ch-'0')
		case ch == 'd':
			total += num * 86400
			num = 0
		case ch == 'h':
			total += num * 3600
			num = 0
		case ch == 'm':
			total += num * 60
			num = 0
		case ch == 's':
			total += num
			num = 0
		}
	}
	return total
}

// validateSymlink 验证符号链接是否安全。
//
// 检查文件是否是符号链接，如果是则验证链接指向的文件
// 是否在允许的根目录（root 或 alias）范围内。
// 防止通过符号链接访问敏感文件（如 /etc/passwd）。
//
// 参数：
//   - filePath: 要验证的文件路径
//
// 返回值：
//   - error: 如果符号链接不安全或解析失败，返回错误
func (h *StaticHandler) validateSymlink(filePath string) error {
	// 获取文件信息（不跟随符号链接）
	info, err := os.Lstat(filePath)
	if err != nil {
		return err
	}

	// 如果不是符号链接，直接返回成功
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}

	// 获取符号链接指向的实际路径
	realPath, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		return err
	}

	// 获取允许的基础路径
	basePath := h.root
	if h.alias != "" {
		basePath = h.alias
	}

	// 如果没有配置根目录，拒绝符号链接
	if basePath == "" {
		return os.ErrPermission
	}

	// 解析基础路径为绝对路径
	absBase, err := filepath.Abs(basePath)
	if err != nil {
		return err
	}

	// 解析目标路径为绝对路径
	absTarget, err := filepath.Abs(realPath)
	if err != nil {
		return err
	}

	// 确保目标路径在基础路径范围内
	if !strings.HasPrefix(absTarget, absBase+string(filepath.Separator)) && absTarget != absBase {
		return os.ErrPermission
	}

	return nil
}

// representationETag 返回本次响应预测选用的表示的 ETag，条件请求（If-None-Match）主要与它比较。
//
// 静态处理器先于压缩中间件运行，所以需要在发送前预测最终的内容编码，规则与实际发送一致：
//  1. Range 生效（206/416）：始终是 identity 字节，既不用预压缩文件，压缩中间件也不压缩 206；
//  2. 存在客户端可接受的预压缩文件（allowPrecompressed 时）：使用其编码（br > gzip）；
//  3. 否则若链上有压缩中间件：按其协商结果、min_size、MIME 类型预测动态压缩的编码；
//  4. 其余为 identity。
//
// 编码变体 ETag 由 utils.ETagForEncoding 生成，与压缩中间件改写后的 ETag 一致。
//
// 第二个返回值 mayFallBack 为 true 表示预测的是动态压缩且该压缩可能被中间件放弃
// （缓冲路径下压缩后体积没有变小，此时最终表示是 identity，ETag 为 identity 标签）。
// 处理器无法预知这一点，条件请求需经 notModifiedForRepresentation 交由中间件兜底。
func (h *StaticHandler) representationETag(ctx *fasthttp.RequestCtx, etag, filePath, contentType string,
	size int64, spec rangeSpec, allowPrecompressed bool,
) (tag string, mayFallBack bool) {
	if spec.state != rangeNone {
		return etag, false
	}
	if allowPrecompressed && h.gzipStatic != nil {
		if enc := h.gzipStatic.SelectEncoding(ctx, strings.TrimPrefix(filePath, h.root)); enc != "" {
			return utils.ETagForEncoding(etag, enc), false
		}
	}
	if mw, ok := ctx.UserValue(compression.ContextKey).(*compression.Middleware); ok && mw != nil {
		enc := mw.PredictEncoding(ctx.Request.Header.Peek("Accept-Encoding"), []byte(contentType), int(size))
		if enc != "" {
			return utils.ETagForEncoding(etag, enc), mw.CanFallBack(int(size))
		}
	}
	return etag, false
}

// notModifiedForRepresentation 评估条件请求，返回 true 表示处理器应直接返回 304。
//
// respTag 是预测表示的 ETag（见 representationETag）。预测表示为动态压缩且可能被放弃（mayFallBack）时，
// 若 If-None-Match 未命中 respTag 却命中 identity 标签 idTag，处理器无法确定最终表示：
//   - 压缩发生：最终表示是压缩变体，identity 标签不应匹配，须 200；
//   - 压缩被放弃：最终表示是 identity，应 304。
//
// 此时返回 false 让响应照常生成，并通过 compression.DeferRevalidation 交由压缩中间件在决定最终表示后改写为 304。
// 其余情形（预压缩、Range、无压缩中间件、流式压缩路径）预测是精确的，不涉及推迟。
func notModifiedForRepresentation(ctx *fasthttp.RequestCtx, idTag, respTag string, mayFallBack bool, modTime time.Time) bool {
	if isNotModified(ctx, respTag, modTime) {
		return true
	}
	if mayFallBack && respTag != idTag && ctx.Request.Header.Peek("If-None-Match") != nil &&
		isNotModified(ctx, idTag, modTime) {
		compression.DeferRevalidation(ctx, idTag)
	}
	return false
}

// weakETagMatch 按弱比较（RFC 9110 8.8.3.2）判断两个 entity-tag 是否相同：仅忽略 W/ 前缀。
func weakETagMatch(a, b string) bool {
	return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
}

// isNotModified 检查条件请求是否匹配（返回 true 表示应返回 304）。
//
// etag 必须是本次响应实际选用的表示的 ETag（见 representationETag）。
// If-None-Match 使用弱比较（RFC 9110 13.1.2），不同编码表示的 ETag 互不匹配；
// 存在 If-None-Match 时忽略 If-Modified-Since（RFC 9110 13.1.3）。
func isNotModified(ctx *fasthttp.RequestCtx, etag string, modTime time.Time) bool {
	if match := ctx.Request.Header.Peek("If-None-Match"); len(match) > 0 {
		// RFC 9110: If-None-Match = #entity-tag，逗号分隔
		for tag := range strings.SplitSeq(string(match), ",") {
			if weakETagMatch(strings.TrimSpace(tag), etag) {
				return true
			}
		}
		return false
	}
	if since := ctx.Request.Header.Peek("If-Modified-Since"); len(since) > 0 {
		if t, err := fasthttp.ParseHTTPDate(since); err == nil {
			return !modTime.After(t)
		}
	}
	return false
}
