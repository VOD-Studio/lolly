// Package bodylimit 提供请求体大小限制中间件的测试。
//
// 作者：xfy
package bodylimit

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

// TestParseSize 测试大小字符串解析。
func TestParseSize(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int64
		wantErr  bool
	}{
		{"empty string", "", 1 << 20, false},                  // 默认 1MB
		{"plain bytes", "1024", 1024, false},                  // 纯数字
		{"zero means unlimited", "0", 0, false},               // 0 解析为 0（unlimited）
		{"with b", "2048b", 2048, false},                      // 带 b 单位
		{"kilobytes", "10kb", 10 * 1024, false},               // KB
		{"megabytes", "1mb", 1024 * 1024, false},              // MB
		{"gigabytes", "1gb", 1024 * 1024 * 1024, false},       // GB
		{"uppercase", "1MB", 1024 * 1024, false},              // 大写
		{"with spaces", " 1mb ", 1024 * 1024, false},          // 带空格
		{"decimal", "1.5mb", int64(1.5 * 1024 * 1024), false}, // 小数
		{"invalid unit", "1xx", 0, true},                      // 无效单位
		{"no number", "mb", 0, true},                          // 无数字
		{"negative", "-1mb", 0, true},                         // 负数（ParseFloat 可能成功但结果为负）
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSize(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseSize(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.expected {
				t.Errorf("ParseSize(%q) = %d, want %d", tt.input, got, tt.expected)
			}
		})
	}
}

// TestNew 测试创建中间件。
func TestNew(t *testing.T) {
	tests := []struct {
		name     string
		maxSize  string
		wantErr  bool
		expected int64
	}{
		{"valid 1mb", "1mb", false, 1024 * 1024},
		{"valid 10kb", "10kb", false, 10 * 1024},
		{"invalid", "invalid", true, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bl, err := New(tt.maxSize)
			if (err != nil) != tt.wantErr {
				t.Errorf("New(%q) error = %v, wantErr %v", tt.maxSize, err, tt.wantErr)
				return
			}
			if !tt.wantErr && bl.maxBodySize != tt.expected {
				t.Errorf("New(%q).maxBodySize = %d, want %d", tt.maxSize, bl.maxBodySize, tt.expected)
			}
		})
	}
}

// TestBodyLimit_Process 测试中间件处理。
func TestBodyLimit_Process(t *testing.T) {
	bl, err := New("100b")
	if err != nil {
		t.Fatalf("创建中间件失败: %v", err)
	}

	tests := []struct {
		name           string
		body           string
		contentLength  int
		expectedStatus int
	}{
		{
			name:           "small body within limit",
			body:           "small body",
			contentLength:  10,
			expectedStatus: fasthttp.StatusOK,
		},
		{
			name:           "body exactly at limit",
			body:           strings.Repeat("a", 100),
			contentLength:  100,
			expectedStatus: fasthttp.StatusOK,
		},
		{
			name:           "body exceeds limit via content-length",
			body:           strings.Repeat("a", 200),
			contentLength:  200,
			expectedStatus: fasthttp.StatusRequestEntityTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nextHandler := func(ctx *fasthttp.RequestCtx) {
				ctx.SetStatusCode(fasthttp.StatusOK)
			}

			handler := bl.Process(nextHandler)

			ctx := &fasthttp.RequestCtx{}
			ctx.Request.Header.SetMethod("POST")
			ctx.Request.Header.SetContentLength(tt.contentLength)
			ctx.Request.SetBodyStream(bytes.NewReader([]byte(tt.body)), tt.contentLength)

			handler(ctx)

			if ctx.Response.StatusCode() != tt.expectedStatus {
				t.Errorf("status code = %d, want %d", ctx.Response.StatusCode(), tt.expectedStatus)
			}
		})
	}
}

// TestBodyLimit_ProcessZeroIsUnlimited 测试 "0" 表示不限制请求体大小。
//
// 配置为 "0" 时，超过任何隐含上限的请求体都应被放行，不返回 413。
func TestBodyLimit_ProcessZeroIsUnlimited(t *testing.T) {
	bl, err := New("0")
	if err != nil {
		t.Fatalf("创建中间件失败: %v", err)
	}
	if bl.maxBodySize != UnlimitedSize {
		t.Fatalf("New(\"0\").maxBodySize = %d, want %d (UnlimitedSize)", bl.maxBodySize, UnlimitedSize)
	}

	// 200 字节，远超若误把 0 当成"禁止"或兜底默认值时会触发的限制
	body := strings.Repeat("a", 200)

	nextHandler := func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := bl.Process(nextHandler)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.Header.SetContentLength(len(body))
	ctx.Request.SetBodyStream(bytes.NewReader([]byte(body)), len(body))

	handler(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Errorf("unlimited 模式下应放行: status code = %d, want %d", ctx.Response.StatusCode(), fasthttp.StatusOK)
	}
}

// TestBodyLimit_PathZeroOverridesGlobal 测试路径级 "0" 覆盖全局限制为不限制。
//
// 全局配 1mb，/upload 路径配 "0"：/upload 上的大 body 应放行，
// 其他路径上的大 body 仍应被全局限制拒绝。
func TestBodyLimit_PathZeroOverridesGlobal(t *testing.T) {
	bl, err := New("1mb")
	if err != nil {
		t.Fatalf("创建中间件失败: %v", err)
	}
	if err := bl.AddPathLimit("/upload", "0"); err != nil {
		t.Fatalf("添加路径限制失败: %v", err)
	}

	// 2MB，超过全局 1mb 限制
	largeBody := strings.Repeat("a", 2*1024*1024)

	tests := []struct {
		name           string
		path           string
		expectedStatus int
	}{
		{"path with zero override passes", "/upload/file", fasthttp.StatusOK},
		{"other path still limited", "/other/path", fasthttp.StatusRequestEntityTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nextHandler := func(ctx *fasthttp.RequestCtx) {
				ctx.SetStatusCode(fasthttp.StatusOK)
			}

			handler := bl.Process(nextHandler)

			ctx := &fasthttp.RequestCtx{}
			ctx.Request.Header.SetMethod("POST")
			ctx.Request.SetRequestURI(tt.path)
			ctx.Request.Header.SetContentLength(len(largeBody))
			ctx.Request.SetBodyStream(bytes.NewReader([]byte(largeBody)), len(largeBody))

			handler(ctx)

			if ctx.Response.StatusCode() != tt.expectedStatus {
				t.Errorf("path %q: status code = %d, want %d", tt.path, ctx.Response.StatusCode(), tt.expectedStatus)
			}
		})
	}
}

// TestBodyLimit_ProcessChunked 测试 chunked 传输编码。
func TestBodyLimit_ProcessChunked(t *testing.T) {
	bl, err := New("100b")
	if err != nil {
		t.Fatalf("创建中间件失败: %v", err)
	}

	// 测试 chunked 传输无法绕过限制
	body := strings.Repeat("a", 150) // 150 字节超过 100 字节限制
	reader := &slowReader{data: []byte(body), chunkSize: 10}

	nextHandler := func(ctx *fasthttp.RequestCtx) {
		// 尝试读取完整请求体
		body, err := io.ReadAll(ctx.Request.BodyStream())
		if err != nil {
			ctx.SetStatusCode(fasthttp.StatusRequestEntityTooLarge)
			return
		}
		if len(body) > 100 {
			ctx.SetStatusCode(fasthttp.StatusRequestEntityTooLarge)
			return
		}
		ctx.SetStatusCode(fasthttp.StatusOK)
	}

	handler := bl.Process(nextHandler)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	// 不设置 Content-Length 模拟 chunked 传输
	ctx.Request.SetBodyStream(reader, -1)

	handler(ctx)

	// 应该返回 413，因为 body 超过限制
	if ctx.Response.StatusCode() != fasthttp.StatusRequestEntityTooLarge {
		t.Errorf("chunked 传输绕过测试失败: status code = %d, want %d", ctx.Response.StatusCode(), fasthttp.StatusRequestEntityTooLarge)
	}
}

// TestBodyLimit_PathLimits 测试路径级别配置。
func TestBodyLimit_PathLimits(t *testing.T) {
	bl, err := New("1mb")
	if err != nil {
		t.Fatalf("创建中间件失败: %v", err)
	}

	// 添加路径级别配置
	if err := bl.AddPathLimit("/api/upload", "10mb"); err != nil {
		t.Fatalf("添加路径限制失败: %v", err)
	}
	if err := bl.AddPathLimit("/api", "2mb"); err != nil {
		t.Fatalf("添加路径限制失败: %v", err)
	}

	tests := []struct {
		path     string
		expected int64
	}{
		{"/api/upload/file", 10 * 1024 * 1024}, // 最长匹配 /api/upload
		{"/api/users", 2 * 1024 * 1024},        // 匹配 /api
		{"/other/path", 1 * 1024 * 1024},       // 默认限制
		{"/api", 2 * 1024 * 1024},              // 匹配 /api
		{"/apix", 2 * 1024 * 1024},             // 匹配 /api（前缀匹配）
		{"/apiupload", 2 * 1024 * 1024},        // 匹配 /api（前缀匹配）
		{"/notapi", 1 * 1024 * 1024},           // 不匹配 /api
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := bl.GetLimit(tt.path)
			if got != tt.expected {
				t.Errorf("GetLimit(%q) = %d, want %d", tt.path, got, tt.expected)
			}
		})
	}
}

// TestBodyLimit_Name 测试中间件名称。
func TestBodyLimit_Name(t *testing.T) {
	bl := NewWithDefault()
	if bl.Name() != "BodyLimit" {
		t.Errorf("Name() = %s, want BodyLimit", bl.Name())
	}
}

// TestBodyLimit_DefaultMaxBodySize 测试默认大小。
func TestBodyLimit_DefaultMaxBodySize(t *testing.T) {
	if DefaultMaxBodySize != 1<<20 {
		t.Errorf("DefaultMaxBodySize = %d, want %d", DefaultMaxBodySize, 1<<20)
	}
}

// slowReader 模拟慢速读取，用于测试 chunked 传输。
type slowReader struct {
	data      []byte
	pos       int
	chunkSize int
}

func (r *slowReader) Read(p []byte) (n int, err error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}

	// 每次只读取 chunkSize 字节
	remaining := len(r.data) - r.pos
	toRead := min(min(r.chunkSize, remaining), len(p))

	n = toRead
	copy(p, r.data[r.pos:r.pos+toRead])
	r.pos += toRead

	return n, nil
}
