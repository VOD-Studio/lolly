// Package compression 提供 HTTP 响应压缩中间件，支持 gzip 和 brotli 算法。
//
// 该文件包含压缩相关的核心逻辑，包括：
//   - gzip 压缩（兼容性好，所有浏览器支持）
//   - brotli 压缩（压缩率更高，适合现代浏览器）
//   - MIME 类型过滤
//   - 最小压缩大小控制
//
// 主要用途：
//
//	用于压缩 HTTP 响应内容，减少传输数据量，提升页面加载速度。
//
// 注意事项：
//   - 使用缓冲池复用压缩对象，减少内存分配
//   - 小于 MinSize 的响应不压缩
//
// 作者：xfy
package compression

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/config"
	"rua.plus/lolly/internal/utils"
)

// resettableWriteCloser 接口用于统一 gzip.Writer 和 brotli.Writer 的操作。
// 这两个 writer 都实现了 Reset(io.Writer), Write([]byte) (int, error), Close() error
type resettableWriteCloser interface {
	Reset(w io.Writer)
	Write(data []byte) (int, error)
	Close() error
}

// compressorPool 是一个通用的压缩 writer 池。
type compressorPool struct {
	pool    sync.Pool
	level   int
	factory func(level int) resettableWriteCloser
}

// newGzipPool 创建 gzip writer 池。
func newGzipPool(level int) *compressorPool {
	p := &compressorPool{
		level: level,
		factory: func(level int) resettableWriteCloser {
			w, err := gzip.NewWriterLevel(nil, level)
			if err != nil {
				w, _ = gzip.NewWriterLevel(nil, gzip.DefaultCompression)
			}
			return w
		},
	}
	p.pool.New = func() any { return p.factory(p.level) }
	return p
}

// newBrotliPool 创建 brotli writer 池。
func newBrotliPool(level int) *compressorPool {
	p := &compressorPool{
		level: level,
		factory: func(level int) resettableWriteCloser {
			return brotli.NewWriterOptions(nil, brotli.WriterOptions{
				Quality: level,
			})
		},
	}
	p.pool.New = func() any { return p.factory(p.level) }
	return p
}

// Get 从池中获取 writer。
func (p *compressorPool) Get() (resettableWriteCloser, bool) {
	v, ok := p.pool.Get().(resettableWriteCloser)
	return v, ok
}

// Put 将 writer 放回池中。
func (p *compressorPool) Put(w resettableWriteCloser) {
	p.pool.Put(w)
}

// streamingThreshold 流式压缩阈值。
// 响应体超过此大小时使用 SetBodyStreamWriter 流式压缩，
// 消除 compressed buffer 分配，降低内存峰值。
const streamingThreshold = 64 * 1024 // 64KB

// Algorithm 压缩算法类型。
type Algorithm int

const (
	// AlgorithmGzip 使用 gzip 压缩算法。
	AlgorithmGzip Algorithm = iota
	// AlgorithmBrotli 使用 brotli 压缩算法。
	AlgorithmBrotli

	compressionGZIP = "gzip"
)

// Middleware 响应压缩中间件。
type Middleware struct {
	// gzipPool gzip.Writer 缓冲池
	gzipPool *compressorPool
	// brotliPool brotli.Writer 缓冲池
	brotliPool *compressorPool
	// types 可压缩的 MIME 类型列表
	types []string
	// typesBytes 预计算的小写 MIME 类型字节切片
	typesBytes [][]byte
	// typesWildcardPrefix 预计算的通配符前缀字节切片
	typesWildcardPrefix [][]byte

	// level 压缩级别（1-9）
	level int
	// minSize 最小压缩大小（字节）
	minSize int
	// algorithm 压缩算法
	algorithm Algorithm
}

// New 创建压缩中间件。
//
// 参数：
//   - cfg: 压缩配置，包含算法类型、压缩级别、最小压缩大小等
//
// 返回值：
//   - *Middleware: 压缩中间件实例
//   - error: 配置无效时返回错误
func New(cfg *config.CompressionConfig) (*Middleware, error) {
	if cfg == nil {
		cfg = &config.CompressionConfig{
			Type:    "gzip",
			Level:   6,
			MinSize: 1024,
			Types:   defaultCompressibleTypes(),
		}
	}

	// 设置默认值
	if cfg.Level == 0 {
		cfg.Level = 6
	}
	if cfg.MinSize == 0 {
		cfg.MinSize = 1024
	}
	if len(cfg.Types) == 0 {
		cfg.Types = defaultCompressibleTypes()
	}

	// 解析算法类型
	var algo Algorithm
	switch strings.ToLower(cfg.Type) {
	case "brotli":
		algo = AlgorithmBrotli
	case compressionGZIP:
		algo = AlgorithmGzip
	case "both":
		// both 模式优先使用 brotli（如果客户端支持）
		algo = AlgorithmBrotli
	default:
		algo = AlgorithmGzip
	}

	m := &Middleware{
		types:     cfg.Types,
		level:     cfg.Level,
		minSize:   cfg.MinSize,
		algorithm: algo,
	}

	for _, t := range m.types {
		lower := strings.ToLower(t)
		if base, found := strings.CutSuffix(lower, "/*"); found {
			m.typesWildcardPrefix = append(m.typesWildcardPrefix, []byte(base))
		} else {
			m.typesBytes = append(m.typesBytes, []byte(lower))
		}
	}

	// 初始化缓冲池
	m.gzipPool = newGzipPool(cfg.Level)
	m.brotliPool = newBrotliPool(cfg.Level)

	return m, nil
}

// defaultCompressibleTypes 返回默认可压缩的 MIME 类型。
func defaultCompressibleTypes() []string {
	return []string{
		"text/html",
		"text/css",
		"text/javascript",
		"text/plain",
		"text/xml",
		"application/json",
		"application/javascript",
		"application/xml",
		"application/xhtml+xml",
	}
}

// Name 返回中间件名称。
func (m *Middleware) Name() string {
	return "compression"
}

// Process 应用压缩中间件。
//
// 参数：
//   - next: 下一个请求处理器
//
// 返回值：
//   - fasthttp.RequestHandler: 包装后的请求处理器
func (m *Middleware) Process(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		// 让下游处理器（如静态文件）能够预测本中间件对响应的压缩决定，
		// 以便在条件请求（If-None-Match）中比较正确表示的 ETag。
		ctx.SetUserValue(ContextKey, m)

		// 检查客户端是否支持压缩（零拷贝使用 []byte）并选择压缩方式
		encoding := m.negotiate(ctx.Request.Header.Peek("Accept-Encoding"))

		// 如果不需要压缩，直接执行
		if encoding == "" {
			next(ctx)
			return
		}
		useBrotli := encoding == "br"
		useGzip := encoding == compressionGZIP

		// 执行处理器
		next(ctx)

		// 检查是否已有 Content-Encoding（由 gzip_static 或上游处理器设置）
		// 已编码的响应不应再次压缩，避免双重编码导致数据损坏
		if len(ctx.Response.Header.Peek("Content-Encoding")) > 0 {
			return
		}

		// 206 Partial Content 的 Content-Range 描述的是未压缩内容的字节区间，
		// 压缩后区间语义将失效，因此不压缩部分内容响应（同时避免读取整个 body 流）。
		if ctx.Response.StatusCode() == fasthttp.StatusPartialContent ||
			len(ctx.Response.Header.Peek("Content-Range")) > 0 {
			return
		}

		// 获取响应体
		body := ctx.Response.Body()
		bodyLen := len(body)

		// 检查是否满足压缩条件（最小长度、MIME 类型；零拷贝使用 []byte）
		if !m.shouldCompress(ctx.Response.Header.ContentType(), bodyLen) {
			return // 不压缩
		}

		if bodyLen > streamingThreshold {
			// 大响应：流式压缩，消除 compressed buffer 分配
			// 压缩后的表示无法按原始字节区间寻址，撤销 Accept-Ranges 声明
			ctx.Response.Header.Del("Accept-Ranges")
			if useBrotli {
				m.streamWithPool(ctx, encoding, m.brotliPool)
			} else if useGzip {
				m.streamWithPool(ctx, encoding, m.gzipPool)
			}
		} else {
			// 小响应：缓冲压缩
			var compressed []byte

			if useBrotli {
				compressed = m.compressWithPool(body, m.brotliPool)
			} else if useGzip {
				compressed = m.compressWithPool(body, m.gzipPool)
			}

			if len(compressed) > 0 && len(compressed) < bodyLen {
				ctx.Response.SetBody(compressed)
				ctx.Response.Header.Set("Content-Encoding", encoding)
				setEncodedETag(ctx, encoding)
				ctx.Response.Header.Del("Content-Length")
				ctx.Response.Header.Del("Accept-Ranges")
				// 与表示无关的条件（If-None-Match: * / 仅 If-Modified-Since）命中：
				// 按压缩后的表示（变体 ETag）返回 304。
				completeDeferredNotModified(ctx)
			} else {
				// 压缩后体积没有变小，放弃压缩，最终表示是 identity。
				// 下游若因此推迟了条件请求的判定，在这里按最终表示补上。
				completeDeferredRevalidation(ctx)
				completeDeferredNotModified(ctx)
			}
		}
	}
}

// ContextKey 是压缩中间件在 RequestCtx.UserValue 中登记自身的键。
//
// 中间件在调用下游处理器之前写入 *Middleware，处理器可通过 PredictEncoding
// 得知本次响应是否会被压缩，从而在压缩发生之前就用正确表示的 ETag 评估条件请求。
const ContextKey = "lolly.compression"

// RevalidateKey 是下游处理器在 RequestCtx.UserValue 中登记“推迟条件请求判定”的键，值为 identity ETag（string）。
//
// 背景：处理器先于本中间件运行，只能预测“会尝试压缩”，无法预知压缩后体积是否反而变大而被放弃。
// 当客户端携带的 If-None-Match 命中 identity ETag、而预测的压缩表示 ETag 不匹配，
// 且压缩可能被放弃（见 CanFallBack）时，处理器不直接判定，而是调用 DeferRevalidation
// 正常产生 200 响应，由本中间件在确定最终表示后决定：
//   - 压缩确实发生：响应为压缩表示（ETag 为变体），保持 200，identity 标签不应匹配压缩表示；
//   - 压缩被放弃：最终表示是 identity，标签命中，本中间件把响应改写为 304。
const RevalidateKey = "lolly.compression.revalidate"

// DeferRevalidation 让下游处理器把 If-None-Match 命中 identity ETag 的判定推迟给压缩中间件。
// etag 是 identity 表示的 ETag，中间件只在最终响应仍为 200、无 Content-Encoding 且 ETag 相同时改写为 304。
func DeferRevalidation(ctx *fasthttp.RequestCtx, etag string) {
	ctx.SetUserValue(RevalidateKey, etag)
}

// completeDeferredRevalidation 在压缩被放弃（最终为 identity 表示）时兑现被推迟的条件请求：
// 把 200 响应改写为 304（保留 ETag、Last-Modified、缓存头，丢弃正文）。
func completeDeferredRevalidation(ctx *fasthttp.RequestCtx) {
	etag, ok := ctx.UserValue(RevalidateKey).(string)
	if !ok || etag == "" {
		return
	}
	if ctx.Response.StatusCode() != fasthttp.StatusOK ||
		len(ctx.Response.Header.Peek("Content-Encoding")) > 0 ||
		string(ctx.Response.Header.Peek("ETag")) != etag {
		return
	}
	rewriteNotModified(ctx)
}

// NotModifiedKey 是下游处理器登记“条件已满足但与具体表示无关”的键（值为 true）。
const NotModifiedKey = "lolly.compression.notmodified"

// DeferNotModified 让下游处理器把“与表示无关的条件”（If-None-Match: * 或仅 If-Modified-Since 命中）
// 的 304 响应推迟给压缩中间件：处理器无法预知压缩会发生还是被放弃，而 304 必须带上
// 200 本会携带的 ETag。中间件在确定最终表示（压缩变体或 identity）后再改写为 304，ETag 因此准确。
func DeferNotModified(ctx *fasthttp.RequestCtx) {
	ctx.SetUserValue(NotModifiedKey, true)
}

// completeDeferredNotModified 兑现 DeferNotModified：最终响应仍为 200 时改写为 304。
func completeDeferredNotModified(ctx *fasthttp.RequestCtx) {
	if v, _ := ctx.UserValue(NotModifiedKey).(bool); !v || ctx.Response.StatusCode() != fasthttp.StatusOK {
		return
	}
	rewriteNotModified(ctx)
}

// rewriteNotModified 把 200 响应改写为 304：保留 ETag、Last-Modified、Cache-Control 等验证/缓存头，
// 丢弃正文以及仅对正文有意义的 Content-Encoding/Content-Length/Content-Type。
func rewriteNotModified(ctx *fasthttp.RequestCtx) {
	ctx.Response.SetStatusCode(fasthttp.StatusNotModified)
	ctx.Response.ResetBody()
	ctx.Response.SkipBody = true
	ctx.Response.Header.Del("Content-Encoding")
	ctx.Response.Header.Del("Content-Length")
	ctx.Response.Header.Del("Content-Type")
}

// CanFallBack 报告对长度为 bodyLen 的响应体，Process 尝试压缩后是否可能放弃压缩回退 identity。
//
// 仅缓冲压缩路径（bodyLen <= streamingThreshold）会比较压缩前后体积并可能放弃；
// 流式压缩路径（bodyLen > streamingThreshold）一旦决定压缩就不回退，预测是精确的。
func (m *Middleware) CanFallBack(bodyLen int) bool {
	return bodyLen <= streamingThreshold
}

// negotiate 根据配置的算法与客户端 Accept-Encoding 选择编码，
// 返回 "gzip"、"br"，客户端不支持时返回空字符串。
func (m *Middleware) negotiate(acceptEncoding []byte) string {
	switch m.algorithm {
	case AlgorithmGzip:
		if bytes.Contains(acceptEncoding, []byte("gzip")) {
			return compressionGZIP
		}
	case AlgorithmBrotli:
		// brotli 或 both 模式
		if bytes.Contains(acceptEncoding, []byte("br")) {
			return "br"
		}
		if bytes.Contains(acceptEncoding, []byte("gzip")) {
			return compressionGZIP
		}
	}
	return ""
}

// shouldCompress 判断给定长度与 MIME 类型的响应体是否满足压缩条件。
func (m *Middleware) shouldCompress(contentType []byte, bodyLen int) bool {
	return bodyLen >= m.minSize && m.isCompressible(contentType)
}

// PredictEncoding 预测 Process 对一个未编码、非部分内容（非 206）的 200 响应
// 会选择的内容编码，返回 "gzip"、"br"，预计不压缩时返回空字符串。
//
// 它与 Process 使用同一套决策（Accept-Encoding 协商、最小长度、MIME 类型），
// 但无法预知压缩后体积是否反而变大（此时 Process 会放弃压缩，仅缓冲路径，见 CanFallBack），
// 因此预测的是“尝试压缩”的编码；需要精确结果的条件请求应配合 DeferRevalidation。
// 调用方需自行排除已带 Content-Encoding、或会产生 206/416（Range）的响应。
func (m *Middleware) PredictEncoding(acceptEncoding, contentType []byte, bodyLen int) string {
	encoding := m.negotiate(acceptEncoding)
	if encoding == "" || !m.shouldCompress(contentType, bodyLen) {
		return ""
	}
	return encoding
}

// setEncodedETag 为压缩后的表示改写强 ETag，使其区别于未压缩表示。
//
// 压缩表示与 identity 表示字节不同，共用同一强 ETag 会让 If-Range 等验证
// 误判（如 Range 作用于未压缩内容却用压缩响应的 ETag 通过验证）。
// 无 ETag 或弱 ETag 时不改动。
func setEncodedETag(ctx *fasthttp.RequestCtx, encoding string) {
	etag := string(ctx.Response.Header.Peek("ETag"))
	if etag == "" {
		return
	}
	if enc := utils.ETagForEncoding(etag, encoding); enc != etag {
		ctx.Response.Header.Set("ETag", enc)
	}
}

// isCompressible 检查 MIME 类型是否可压缩。
//
// 参数：
//   - contentType: 内容类型（MIME 类型）[]
//
// 返回值：
//   - bool: 是否可压缩
func (m *Middleware) isCompressible(contentType []byte) bool {
	ct := contentType
	if idx := bytes.IndexByte(ct, ';'); idx >= 0 {
		ct = ct[:idx]
	}
	ct = bytes.TrimSpace(ct)

	for _, t := range m.typesBytes {
		if bytes.Equal(t, ct) {
			return true
		}
	}
	for _, prefix := range m.typesWildcardPrefix {
		if bytes.HasPrefix(ct, prefix) {
			return true
		}
	}
	return false
}

// compressWithPool 使用缓冲池压缩数据。
//
// 参数：
//   - data: 待压缩的原始数据
//   - pool: 压缩 writer 缓冲池
//
// 返回值：
//   - []byte: 压缩后的数据
func (m *Middleware) compressWithPool(data []byte, pool *compressorPool) []byte {
	w, ok := pool.Get()
	if !ok {
		return data // fallback to uncompressed
	}
	defer pool.Put(w)

	var buf bytes.Buffer
	w.Reset(&buf)
	if _, err := w.Write(data); err != nil { //nolint:staticcheck // intentionally empty branch
		// 忽略写入错误，缓冲到 bytes.Buffer 时不太可能失败
	}
	_ = w.Close()

	return buf.Bytes()
}

// streamWithPool 使用流式压缩。
//
// 通过 SetBodyStreamWriter 将压缩数据直接写入响应流，
// 消除 compressed buffer 分配，降低内存峰值。
//
// 参数：
//   - ctx: fasthttp 请求上下文
//   - encoding: Content-Encoding 值
//   - pool: 压缩 writer 缓冲池
func (m *Middleware) streamWithPool(ctx *fasthttp.RequestCtx, encoding string, pool *compressorPool) {
	ctx.Response.Header.Set("Content-Encoding", encoding)
	setEncodedETag(ctx, encoding)
	ctx.Response.Header.Del("Content-Length") // 使用 chunked encoding

	// SetBodyStreamWriter 内部会 ResetBody：未启用 keepBodyBuffer（如 reduce_memory_usage: true）时，
	// 原 body 缓冲区被归还 fasthttp 的池，而流式 goroutine 稍后才读取它，会与其他请求复用该缓冲区产生数据竞争。
	// 因此在重置前复制一份，由闭包独占。
	body := bytes.Clone(ctx.Response.Body())
	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		writer, ok := pool.Get()
		if !ok {
			// pool 获取失败，直接写原始 body
			_, _ = w.Write(body)
			_ = w.Flush()
			return
		}
		defer pool.Put(writer)

		writer.Reset(w)
		_, _ = writer.Write(body)
		_ = writer.Close()
		_ = w.Flush()
	})
}

// Types 返回可压缩的 MIME 类型列表。
//
// 返回值：
//   - []string: 可压缩的 MIME 类型列表
func (m *Middleware) Types() []string {
	return m.types
}

// Level 返回压缩级别。
//
// 返回值：
//   - int: 压缩级别（1-9）
func (m *Middleware) Level() int {
	return m.level
}

// MinSize 返回最小压缩大小。
//
// 返回值：
//   - int: 最小压缩大小（字节）
func (m *Middleware) MinSize() int {
	return m.minSize
}
