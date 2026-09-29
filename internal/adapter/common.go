// Package adapter 提供 HTTP/2 和 HTTP/3 适配器的共享组件。
//
// 该包提取了两个适配器中通用的功能，避免代码重复：
//
//   - 共享的 bufferPool singleton（零拷贝优化）
//   - 统一的请求体处理阈值
//   - 通用的上下文重置逻辑
//   - 流式请求体读取
//
// 关键设计决策：
//
//  1. bufferPool 使用 singleton 模式，ctxPool 保持独立
//  2. CommonAdapter 不包含 ConvertResponse（HTTP/2/HTTP/3 行为不同）
//  3. 阈值常量统一，避免 HTTP/2 inline 和 HTTP/3 constant 不一致
//
// 作者：xfy
package adapter

import (
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/valyala/fasthttp"
)

// DefaultBodyThreshold 是请求体大小阈值，超过此值使用流式处理。
//
// 64KB 是经过测试的平衡点：
//   - 小于此值：直接读取到内存，避免 pool 开销
//   - 大于此值：使用流式缓冲区，避免大内存分配
const DefaultBodyThreshold = 64 * 1024 // 64KB

// bufferPoolInstance 是全局共享的缓冲区池 singleton。
//
// 使用 singleton 模式避免多个适配器实例创建多个 pool，
// 提高内存复用效率。该 pool 被 HTTP/2 和 HTTP/3 适配器共享。
var bufferPoolInstance = &sync.Pool{
	New: func() any {
		buf := make([]byte, 4096) // 4KB 初始缓冲区
		return &buf
	},
}

// CommonAdapter 提供 HTTP/2 和 HTTP/3 适配器的共享基础结构。
//
// 该结构体提取了两个适配器共用的字段和方法，
// 但不包含 ConvertResponse（HTTP/2 和 HTTP/3 的响应转换逻辑不同）。
type CommonAdapter struct {
	// CtxPool 用于复用 fasthttp.RequestCtx 对象
	// 每个协议适配器实例独立维护自己的 ctxPool
	CtxPool sync.Pool

	// MaxBodySize 限制允许读取的请求体最大字节数（0 表示不限制，与 nginx 语义一致）
	MaxBodySize int64

	// StreamEnabled 控制请求体是否以流式方式注入 fasthttp.Request。
	// 为 true 时，请求体通过 SetBodyStream 交给 handler（及下游代理），
	// 不在适配器层物化，支持请求体流式转发到上游。
	// 为 false 时（默认），按既有阈值物化到 ctx.Request.body 缓冲区。
	StreamEnabled bool
}

// NewCommonAdapter 创建新的共享适配器实例。
//
// 初始化 CommonAdapter，设置 ctxPool 的 New 函数。
// bufferPool 使用全局 singleton，不需要在实例中存储。
//
// 返回值：
//   - *CommonAdapter: 初始化的共享适配器实例
func NewCommonAdapter() *CommonAdapter {
	return &CommonAdapter{
		CtxPool: sync.Pool{
			New: func() any {
				return &fasthttp.RequestCtx{}
			},
		},
	}
}

// ResetContext 重置 fasthttp.RequestCtx 状态。
//
// 从 pool 获取的 ctx 可能带有之前请求的残留状态，
// 必须在每次使用前调用此方法进行清理。
//
// 参数：
//   - ctx: 需要重置的 fasthttp 请求上下文
func (a *CommonAdapter) ResetContext(ctx *fasthttp.RequestCtx) {
	// 禁用头部规范化以保持原始大小写
	ctx.Request.Header.DisableNormalizing()
	// 重置请求和响应状态
	ctx.Request.Reset()
	ctx.Response.Reset()
	// 清除用户自定义值
	ctx.SetUserValueBytes(nil, nil)
}

// StreamRequestBody 流式读取 HTTP 请求体到 fasthttp。
//
// 当 a.StreamRequestBody 为 true 时走流式路径（setRequestBodyStream）：
// 请求体以 reader 形式注入 ctx.Request.BodyStream()，不在适配器层物化，
// 支持下游代理直接把流转发到上游；r.Body 的关闭交由 net/http 框架负责。
//
// 为 false 时（默认）走物化路径：按阈值把请求体读入 ctx.Request.body，
// 小于等于 DefaultBodyThreshold（64KB）直接读取，大于则用共享 buffer 流式拼装，
// 超过 MaxBodySize 拒绝并返回 413；MaxBodySize 为 0 时不限制。
//
// 参数：
//   - r: 标准库的 HTTP 请求
//   - ctx: fasthttp 请求上下文，用于存储读取的请求体
//
// 返回值：
//   - error: 读取或限制失败时返回错误
func (a *CommonAdapter) StreamRequestBody(r *http.Request, ctx *fasthttp.RequestCtx) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}

	if a.StreamEnabled {
		return a.setRequestBodyStream(r, ctx)
	}

	defer func() {
		_ = r.Body.Close()
	}()

	limit := a.MaxBodySize
	// 0 表示不限制请求体大小，跳过 Content-Length 预检和硬上限。
	unlimited := limit == 0

	// Reject early via Content-Length when possible.
	if !unlimited && r.ContentLength > 0 && r.ContentLength > limit {
		ctx.Error("Request Entity Too Large", fasthttp.StatusRequestEntityTooLarge)
		return fmt.Errorf("request body %d exceeds limit %d", r.ContentLength, limit)
	}

	// 限制模式下用 LimitReader 设硬上限，防止 chunked/未知长度请求体 OOM；
	// 不限制模式直接读原始流。
	bodyReader := io.Reader(r.Body)
	if !unlimited {
		bodyReader = io.LimitReader(r.Body, limit+1)
	}

	if r.ContentLength > 0 && r.ContentLength <= DefaultBodyThreshold {
		body, err := io.ReadAll(bodyReader)
		if err != nil {
			return err
		}
		if !unlimited && int64(len(body)) > limit {
			ctx.Error("Request Entity Too Large", fasthttp.StatusRequestEntityTooLarge)
			return fmt.Errorf("request body exceeds limit %d", limit)
		}
		ctx.Request.SetBody(body)
		return nil
	}

	bufPtr, ok := bufferPoolInstance.Get().(*[]byte)
	if !ok {
		buf := make([]byte, 4096)
		bufPtr = &buf
	}
	defer bufferPoolInstance.Put(bufPtr)

	buf := *bufPtr
	var body []byte
	if r.ContentLength > 0 {
		body = make([]byte, 0, r.ContentLength)
	}

	var total int64
	for {
		n, err := bodyReader.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
			total += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if !unlimited && total > limit {
			ctx.Error("Request Entity Too Large", fasthttp.StatusRequestEntityTooLarge)
			return fmt.Errorf("request body exceeds limit %d", limit)
		}
	}

	if len(body) > 0 {
		ctx.Request.SetBody(body)
	}
	return nil
}

// setRequestBodyStream 以流式方式把请求体注入 fasthttp.Request。
//
// 不在适配器层物化请求体：直接将 reader 经 SetBodyStream 交给 ctx.Request，
// 供下游代理（buffering.request_mode: off）把流转发到上游。
// r.Body 的关闭由 net/http 框架负责（server 端请求契约），此处不关闭。
//
// 超过 MaxBodySize 时拒绝并返回 413；MaxBodySize 为 0 时不限制。
// chunked 请求（ContentLength <= 0）以 contentLength=-1 传入，fasthttp 将使用
// Transfer-Encoding: chunked 发往上游。
//
// 参数：
//   - r: 标准库的 HTTP 请求
//   - ctx: fasthttp 请求上下文
//
// 返回值：
//   - error: Content-Length 预检超限时返回错误
func (a *CommonAdapter) setRequestBodyStream(r *http.Request, ctx *fasthttp.RequestCtx) error {
	limit := a.MaxBodySize
	unlimited := limit == 0

	if !unlimited && r.ContentLength > 0 && r.ContentLength > limit {
		ctx.Error("Request Entity Too Large", fasthttp.StatusRequestEntityTooLarge)
		return fmt.Errorf("request body %d exceeds limit %d", r.ContentLength, limit)
	}

	bodyReader := io.Reader(r.Body)
	if !unlimited {
		// 限制模式下设硬上限，防止 chunked/未知长度请求体 OOM。
		bodyReader = io.LimitReader(r.Body, limit+1)
	}

	contentLength := -1
	if r.ContentLength > 0 {
		contentLength = int(r.ContentLength)
	}
	ctx.Request.SetBodyStream(bodyReader, contentLength)
	return nil
}

// GetContext 从 pool 获取一个 fasthttp.RequestCtx。
//
// 使用 pool 复用 RequestCtx 对象，减少 GC 压力。
// 获取的 ctx 必须通过 ResetContext 重置后才能使用。
//
// 返回值：
//   - *fasthttp.RequestCtx: fasthttp 请求上下文
//   - bool: 如果为 false，表示类型断言失败，ctx 是新创建的
func (a *CommonAdapter) GetContext() (*fasthttp.RequestCtx, bool) {
	ctx, ok := a.CtxPool.Get().(*fasthttp.RequestCtx)
	if !ok {
		ctx = &fasthttp.RequestCtx{}
	}
	return ctx, ok
}

// PutContext 将 fasthttp.RequestCtx 放回 pool。
//
// 在放回 pool 前应该调用 ResetContext 清理状态。
//
// 参数：
//   - ctx: 要放回 pool 的 fasthttp 请求上下文
func (a *CommonAdapter) PutContext(ctx *fasthttp.RequestCtx) {
	a.CtxPool.Put(ctx)
}
