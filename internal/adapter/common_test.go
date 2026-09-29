// Package adapter 提供 HTTP/2 和 HTTP/3 适配器共享组件的测试。
//
// 该文件测试 CommonAdapter 的各项功能，包括：
//   - NewCommonAdapter 构造函数
//   - ResetContext 上下文重置
//   - StreamRequestBody 流式请求体处理
//   - GetContext/PutContext 池操作
//   - 并发安全性
//   - DefaultBodyThreshold 常量
//
// 作者：xfy
package adapter

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// TestDefaultBodyThreshold 测试请求体大小阈值常量
func TestDefaultBodyThreshold(t *testing.T) {
	assert.Equal(t, 64*1024, DefaultBodyThreshold, "DefaultBodyThreshold 应该等于 64KB")
}

// TestNewCommonAdapter 测试构造函数
func TestNewCommonAdapter(t *testing.T) {
	a := NewCommonAdapter()

	require.NotNil(t, a, "返回值不应为 nil")
	require.NotNil(t, a.CtxPool.New, "CtxPool.New 应该被初始化")

	// 验证 pool 能创建 *fasthttp.RequestCtx
	obj := a.CtxPool.Get()
	ctx, ok := obj.(*fasthttp.RequestCtx)
	assert.True(t, ok, "CtxPool.New 应该返回 *fasthttp.RequestCtx")
	assert.NotNil(t, ctx, "从 pool 获取的 ctx 不应为 nil")
}

// TestResetContext 测试上下文重置逻辑
func TestResetContext(t *testing.T) {
	a := NewCommonAdapter()
	ctx := &fasthttp.RequestCtx{}

	// 模拟使用过的状态
	ctx.Request.SetRequestURI("/test?q=1")
	ctx.Request.Header.Set("Content-Type", "text/plain")
	ctx.Request.SetBody([]byte("hello"))
	ctx.Response.SetBody([]byte("world"))
	ctx.Response.SetStatusCode(200)
	ctx.SetUserValue("key", "value")

	// 重置
	a.ResetContext(ctx)

	// 验证请求状态已清除
	assert.Equal(t, "/", string(ctx.Request.URI().Path()), "请求路径应被重置为 /")
	assert.Equal(t, "", string(ctx.Request.Header.Peek("Content-Type")), "请求头应被清除")
	assert.Equal(t, 0, len(ctx.Request.Body()), "请求体应被清除")

	// 验证响应状态已清除
	assert.Equal(t, 0, len(ctx.Response.Body()), "响应体应被清除")
	assert.Equal(t, 200, ctx.Response.StatusCode(), "响应状态码应被重置为 200")

	// 验证用户值已清除
	assert.Nil(t, ctx.UserValue("key"), "用户值应被清除")
}

// TestResetContext_DisableNormalizing 测试重置时禁用头部规范化
func TestResetContext_DisableNormalizing(t *testing.T) {
	a := NewCommonAdapter()
	ctx := &fasthttp.RequestCtx{}

	a.ResetContext(ctx)

	// 禁用规范化后，设置混合大小写头部应保持原样
	ctx.Request.Header.Set("Content-Type", "text/plain")
	// DisableNormalizing 是通过方法调用的，验证它被调用即可
	// 这里确认 ctx 可以正常使用
	assert.Equal(t, "text/plain", string(ctx.Request.Header.Peek("Content-Type")))
}

// TestStreamRequestBody 测试流式请求体处理的表驱动测试
func TestStreamRequestBody(t *testing.T) {
	a := NewCommonAdapter()

	tests := []struct {
		name          string
		body          io.Reader
		contentLength int64
		wantBody      string
		wantBodySet   bool
	}{
		{
			name:          "nil body 应该跳过处理",
			body:          nil,
			contentLength: 0,
			wantBody:      "",
			wantBodySet:   false,
		},
		{
			name:          "NoBody 应该跳过处理",
			body:          http.NoBody,
			contentLength: 0,
			wantBody:      "",
			wantBodySet:   false,
		},
		{
			name:          "空请求体",
			body:          strings.NewReader(""),
			contentLength: 0,
			wantBody:      "",
			wantBodySet:   false,
		},
		{
			name:          "小请求体直接读取",
			body:          strings.NewReader("hello world"),
			contentLength: 11,
			wantBody:      "hello world",
			wantBodySet:   true,
		},
		{
			name:          "恰好等于阈值",
			body:          bytes.NewReader(make([]byte, DefaultBodyThreshold)),
			contentLength: DefaultBodyThreshold,
			wantBody:      string(make([]byte, DefaultBodyThreshold)),
			wantBodySet:   true,
		},
		{
			name:          "超过阈值走流式路径",
			body:          bytes.NewReader(make([]byte, DefaultBodyThreshold+1)),
			contentLength: DefaultBodyThreshold + 1,
			wantBody:      string(make([]byte, DefaultBodyThreshold+1)),
			wantBodySet:   true,
		},
		{
			name:          "未知 ContentLength 走流式路径",
			body:          strings.NewReader("unknown length body"),
			contentLength: -1,
			wantBody:      "unknown length body",
			wantBodySet:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body io.ReadCloser
			if tt.body != nil {
				body = io.NopCloser(tt.body)
			}

			r := &http.Request{
				Body:          body,
				ContentLength: tt.contentLength,
			}

			ctx := &fasthttp.RequestCtx{}
			err := a.StreamRequestBody(r, ctx)
			require.NoError(t, err, "StreamRequestBody 不应返回错误")

			if tt.wantBodySet {
				assert.Equal(t, tt.wantBody, string(ctx.Request.Body()), "请求体内容应匹配")
			} else {
				assert.Equal(t, 0, len(ctx.Request.Body()), "请求体应为空")
			}
		})
	}
}

// TestStreamRequestBody_ReadError 测试读取请求体时的错误处理
func TestStreamRequestBody_ReadError(t *testing.T) {
	a := NewCommonAdapter()

	// 模拟读取错误
	errReader := &errorReader{err: errors.New("read error")}
	r := &http.Request{
		Body:          io.NopCloser(errReader),
		ContentLength: -1, // 走流式路径
	}

	ctx := &fasthttp.RequestCtx{}
	// 读取错误应被返回
	err := a.StreamRequestBody(r, ctx)
	require.Error(t, err, "读取错误应返回错误")
}

// errorReader 是一个始终返回错误的 io.Reader
type errorReader struct {
	err error
}

func (r *errorReader) Read(_ []byte) (int, error) {
	return 0, r.err
}

// TestStreamRequestBody_PartialReadError 测试部分读取后出错
func TestStreamRequestBody_PartialReadError(t *testing.T) {
	a := NewCommonAdapter()

	// 先读一些数据，然后出错
	pr := &partialErrorReader{data: []byte("partial"), err: errors.New("broken")}
	r := &http.Request{
		Body:          io.NopCloser(pr),
		ContentLength: -1,
	}

	ctx := &fasthttp.RequestCtx{}
	err := a.StreamRequestBody(r, ctx)
	require.Error(t, err, "部分读取后出错应返回错误")

	// 出错后不再保留已读取的部分数据
	assert.Equal(t, 0, len(ctx.Request.Body()), "出错后请求体应为空")
}

// partialErrorReader 先返回数据，再返回错误
type partialErrorReader struct {
	data []byte
	err  error
	read bool
}

func (r *partialErrorReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		n := copy(p, r.data)
		return n, r.err
	}
	return 0, r.err
}

// TestGetContext 测试从 pool 获取上下文
func TestGetContext(t *testing.T) {
	a := NewCommonAdapter()

	ctx, ok := a.GetContext()
	assert.True(t, ok, "首次获取应该成功")
	assert.NotNil(t, ctx, "返回的 ctx 不应为 nil")

	// 获取的类型正确
	_, ok2 := interface{}(ctx).(*fasthttp.RequestCtx)
	assert.True(t, ok2, "返回值类型应该是 *fasthttp.RequestCtx")
}

// TestGetContext_TypeAssertion 测试 pool 类型断言失败的降级
func TestGetContext_TypeAssertion(t *testing.T) {
	a := NewCommonAdapter()

	// 手动往 pool 放入错误类型
	a.CtxPool.Put("wrong type")

	ctx, ok := a.GetContext()
	assert.False(t, ok, "类型断言应该失败")
	assert.NotNil(t, ctx, "即使断言失败，也应该返回新创建的 ctx")
}

// TestPutContext 测试将上下文放回 pool
func TestPutContext(t *testing.T) {
	a := NewCommonAdapter()

	ctx, _ := a.GetContext()
	ctx.Request.SetRequestURI("/test")

	// 放回
	a.PutContext(ctx)

	// 再次获取，应该能拿到（可能是同一个）
	ctx2, _ := a.GetContext()
	assert.NotNil(t, ctx2, "放回后应能重新获取")
}

// TestGetContext_PutAndGet 测试完整的获取-放回-再获取流程
func TestGetContext_PutAndGet(t *testing.T) {
	a := NewCommonAdapter()

	// 获取一个 ctx
	ctx1, ok1 := a.GetContext()
	require.True(t, ok1, "首次获取应该成功")

	// 放回
	a.PutContext(ctx1)

	// 再次获取，可能拿到同一个
	ctx2, ok2 := a.GetContext()
	assert.True(t, ok2, "再次获取应该成功")
	assert.NotNil(t, ctx2)
}

// TestConcurrentPoolAccess 测试并发访问 pool 的安全性
func TestConcurrentPoolAccess(t *testing.T) {
	a := NewCommonAdapter()
	const goroutines = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()

			ctx, ok := a.GetContext()
			if !ok {
				ctx = &fasthttp.RequestCtx{}
			}

			// 模拟使用
			ctx.Request.SetRequestURI("/concurrent")
			a.ResetContext(ctx)

			a.PutContext(ctx)
		}()
	}

	wg.Wait()
}

// TestConcurrentStreamRequestBody 测试并发流式请求体处理
func TestConcurrentStreamRequestBody(t *testing.T) {
	a := NewCommonAdapter()
	const goroutines = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()

			body := strings.NewReader("concurrent body data")
			r := &http.Request{
				Body:          io.NopCloser(body),
				ContentLength: 19,
			}

			ctx := &fasthttp.RequestCtx{}
			err := a.StreamRequestBody(r, ctx)
			require.NoError(t, err)
			assert.Equal(t, "concurrent body data", string(ctx.Request.Body()))
		}()
	}

	wg.Wait()
}

// TestStreamRequestBody_ClosesBody 测试请求体是否被正确关闭
func TestStreamRequestBody_ClosesBody(t *testing.T) {
	a := NewCommonAdapter()

	closeTracker := &trackableReader{data: strings.NewReader("data")}
	r := &http.Request{
		Body:          closeTracker,
		ContentLength: 4,
	}

	ctx := &fasthttp.RequestCtx{}
	err := a.StreamRequestBody(r, ctx)
	require.NoError(t, err)

	assert.True(t, closeTracker.closed, "请求体应该被关闭")
}

// trackableReader 追踪关闭状态的 io.ReadCloser
type trackableReader struct {
	data   io.Reader
	closed bool
}

func (r *trackableReader) Read(p []byte) (int, error) {
	return r.data.Read(p)
}

func (r *trackableReader) Close() error {
	r.closed = true
	return nil
}

// TestStreamRequestBodyEnforcesMaxBodySize 测试请求体超过最大限制时被拒绝。
func TestStreamRequestBodyEnforcesMaxBodySize(t *testing.T) {
	a := NewCommonAdapter()
	a.MaxBodySize = 10

	ctx := &fasthttp.RequestCtx{}
	body := strings.NewReader("this body is way too large")
	r := &http.Request{
		Body:          io.NopCloser(body),
		ContentLength: int64(body.Len()),
	}

	err := a.StreamRequestBody(r, ctx)
	if err == nil {
		t.Fatal("expected error for oversized body")
	}
	if ctx.Response.StatusCode() != fasthttp.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", ctx.Response.StatusCode())
	}
}

// TestStreamRequestBodyZeroIsUnlimited 测试 MaxBodySize 为 0 时不限制请求体大小。
//
// 配置 0 时，超过任何隐含上限的请求体都应被完整读入，不返回 413。
func TestStreamRequestBodyZeroIsUnlimited(t *testing.T) {
	a := NewCommonAdapter()
	a.MaxBodySize = 0 // 0 表示不限制

	// 2MB，远超此前 <=0 兜底的 1MB 默认值
	bodyBytes := bytes.Repeat([]byte("a"), 2*1024*1024)
	ctx := &fasthttp.RequestCtx{}
	body := bytes.NewReader(bodyBytes)
	r := &http.Request{
		Body:          io.NopCloser(body),
		ContentLength: int64(body.Len()),
	}

	if err := a.StreamRequestBody(r, ctx); err != nil {
		t.Fatalf("unlimited 模式不应报错: %v", err)
	}
	if ctx.Response.StatusCode() == fasthttp.StatusRequestEntityTooLarge {
		t.Fatalf("unlimited 模式不应返回 413")
	}
	if !bytes.Equal(ctx.Request.Body(), bodyBytes) {
		t.Fatalf("请求体未被完整读入: got %d bytes, want %d", len(ctx.Request.Body()), len(bodyBytes))
	}
}
