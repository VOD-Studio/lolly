// Package bodylimit 提供 HTTP 请求体大小限制的中间件。
//
// 该文件包含请求体大小限制相关的核心功能，包括：
//   - BodyLimit 中间件：限制请求体大小
//   - 解析大小字符串：支持 b, kb, mb, gb 等单位
//   - 路径级别的覆盖配置
//
// 主要用途：
//
//	防止客户端通过发送超大请求体或 chunked 传输绕过限制导致服务器资源耗尽。
//
// 注意事项：
//   - 使用 io.LimitReader 强制限制实际读取的字节数
//   - 支持路径级别配置覆盖全局配置
//   - 超限返回 413 Request Entity Too Large
//   - 大小为 0（UnlimitedSize）表示不限制，与 nginx client_max_body_size 0 语义一致
//
// 作者：xfy
package bodylimit

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/valyala/fasthttp"
)

// DefaultMaxBodySize 默认请求体大小限制为 1MB。
const DefaultMaxBodySize = 1 << 20 // 1MB

// UnlimitedSize 表示不限制请求体大小。
//
// 取值 0，与 nginx 的 client_max_body_size 0 语义一致：禁用请求体大小检查。
// ParseSize("0") 返回 0，执行层（Process / adapter）据此跳过限制逻辑。
const UnlimitedSize int64 = 0

// BodyLimit 请求体大小限制中间件。
//
// 限制请求体的最大字节数，超过限制的请求将被拒绝并返回 413 错误。
// 支持全局配置和路径级别的覆盖配置。
type BodyLimit struct {
	pathLimits   map[string]int64
	maxBodySize  int64
	pathLimitsMu sync.RWMutex
}

// New 创建请求体大小限制中间件。
//
// 参数：
//   - maxBodySize: 最大请求体大小字符串，如 "1mb", "10kb" 等；"0" 表示不限制
//
// 返回值：
//   - *BodyLimit: 创建的中间件实例
//   - error: 解析大小字符串失败时的错误
func New(maxBodySize string) (*BodyLimit, error) {
	size, err := ParseSize(maxBodySize)
	if err != nil {
		return nil, fmt.Errorf("解析 client_max_body_size 失败: %w", err)
	}

	return &BodyLimit{
		maxBodySize: size,
		pathLimits:  make(map[string]int64),
	}, nil
}

// NewWithDefault 使用默认限制（1MB）创建中间件。
//
// 返回值：
//   - *BodyLimit: 创建的中间件实例
func NewWithDefault() *BodyLimit {
	return &BodyLimit{
		maxBodySize: DefaultMaxBodySize,
		pathLimits:  make(map[string]int64),
	}
}

// Name 返回中间件名称。
//
// 返回值：
//   - string: 中间件名称
func (bl *BodyLimit) Name() string {
	return "BodyLimit"
}

// AddPathLimit 添加路径级别的限制配置。
//
// 参数：
//   - path: 路径前缀
//   - sizeStr: 大小字符串，如 "1mb", "10kb" 等；"0" 表示该路径不限制
//
// 返回值：
//   - error: 解析大小字符串失败时的错误
func (bl *BodyLimit) AddPathLimit(path, sizeStr string) error {
	size, err := ParseSize(sizeStr)
	if err != nil {
		return fmt.Errorf("解析路径 %s 的 client_max_body_size 失败: %w", path, err)
	}

	bl.pathLimitsMu.Lock()
	bl.pathLimits[path] = size
	bl.pathLimitsMu.Unlock()

	return nil
}

// GetLimit 获取指定路径的请求体限制。
//
// 优先使用路径级别配置，如无则使用全局配置。
//
// 参数：
//   - path: 请求路径
//
// 返回值：
//   - int64: 该路径的最大请求体大小（字节）
func (bl *BodyLimit) GetLimit(path string) int64 {
	bl.pathLimitsMu.RLock()
	defer bl.pathLimitsMu.RUnlock()

	// 查找匹配的路径配置（最长匹配优先）
	var matchedLimit int64
	var matchedPath string
	var matched bool

	for prefix, limit := range bl.pathLimits {
		if strings.HasPrefix(path, prefix) {
			// 选择最长的匹配路径
			if !matched || len(prefix) > len(matchedPath) {
				matchedLimit = limit
				matchedPath = prefix
				matched = true
			}
		}
	}

	if matched {
		return matchedLimit
	}

	return bl.maxBodySize
}

// Process 实现中间件接口。
//
// 检查请求体大小是否超过限制，超限返回 413 错误。
//
// 参数：
//   - next: 下一个请求处理器
//
// 返回值：
//   - fasthttp.RequestHandler: 包装后的请求处理器
func (bl *BodyLimit) Process(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		path := string(ctx.Path())
		limit := bl.GetLimit(path)

		// 0 表示不限制请求体大小（与 nginx client_max_body_size 0 语义一致），
		// 跳过 Content-Length 预检和流式包装，直接放行。
		if limit == UnlimitedSize {
			next(ctx)
			return
		}

		// 检查 Content-Length 头
		contentLength := ctx.Request.Header.ContentLength()
		if contentLength > 0 && int64(contentLength) > limit {
			ctx.Error("Request Entity Too Large", fasthttp.StatusRequestEntityTooLarge)
			return
		}

		// 对于 chunked 传输或没有 Content-Length 的请求
		// 只有当 BodyStream 存在时才设置限制
		bodyStream := ctx.Request.BodyStream()
		if bodyStream != nil {
			ctx.Request.SetBodyStream(bodyStream, int(limit))

			// 包装请求体读取以检测超限
			limitedReader := &limitedBodyReader{
				ctx:      ctx,
				limit:    limit,
				original: bodyStream,
			}
			ctx.Request.SetBodyStream(limitedReader, -1)
		}

		next(ctx)
	}
}

// limitedBodyReader 包装请求体读取器以限制最大读取字节数。
//
// 当读取的字节数超过限制时，返回错误并在上下文中设置 413 状态码。
type limitedBodyReader struct {
	// original 原始读取器
	original interface {
		Read(p []byte) (n int, err error)
	}
	// ctx 请求上下文，用于设置错误响应
	ctx *fasthttp.RequestCtx
	// limit 最大允许读取的字节数
	limit int64
	// read 已读取的字节数
	read int64
	// done 是否已达到限制
	done bool
}

// Read 实现读取接口，在超过限制时返回错误。
func (l *limitedBodyReader) Read(p []byte) (n int, err error) {
	if l.done {
		return 0, fmt.Errorf("request body too large")
	}

	// 计算还能读取多少字节
	remaining := l.limit - l.read
	if remaining <= 0 {
		l.done = true
		// 返回 413 错误
		l.ctx.Error("Request Entity Too Large", fasthttp.StatusRequestEntityTooLarge)
		return 0, fmt.Errorf("request body exceeds limit of %d bytes", l.limit)
	}

	// 限制读取长度
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}

	n, err = l.original.Read(p)
	l.read += int64(n)

	return n, err
}

// ParseSize 解析大小字符串为字节数。
//
// 支持的单位：b, kb, mb, gb（不区分大小写）
// 无单位时默认为字节。
//
// 参数：
//   - sizeStr: 大小字符串，如 "1mb", "10kb", "1024" 等
//
// 返回值：
//   - int64: 字节数
//   - error: 解析失败时的错误
func ParseSize(sizeStr string) (int64, error) {
	if sizeStr == "" {
		return DefaultMaxBodySize, nil
	}

	sizeStr = strings.TrimSpace(strings.ToLower(sizeStr))

	// 解析数值和单位
	var numStr string
	var unit string

	for i, c := range sizeStr {
		if c >= '0' && c <= '9' || c == '.' {
			numStr = sizeStr[:i+1]
		} else {
			unit = sizeStr[i:]
			break
		}
	}

	if numStr == "" {
		return 0, fmt.Errorf("无效的大小格式: %s", sizeStr)
	}

	value, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("解析数值失败: %w", err)
	}

	var multiplier float64
	switch unit {
	case "", "b":
		multiplier = 1
	case "kb":
		multiplier = 1024
	case "mb":
		multiplier = 1024 * 1024
	case "gb":
		multiplier = 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("不支持的大小单位: %s", unit)
	}

	return int64(value * multiplier), nil
}
