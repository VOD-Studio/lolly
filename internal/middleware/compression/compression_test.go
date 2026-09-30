// Package compression 提供压缩功能的测试。
//
// 该文件测试压缩中间件模块的各项功能，包括：
//   - 压缩中间件创建
//   - gzip 和 brotli 压缩
//   - 可压缩类型检查
//   - 压缩级别配置
//   - 响应处理
//
// 作者：xfy
package compression

import (
	"bytes"
	"io"
	"slices"
	"sync"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/valyala/fasthttp"
	"rua.plus/lolly/internal/config"
)

func TestNew(t *testing.T) {
	tests := []struct {
		cfg  *config.CompressionConfig
		name string
	}{
		{
			name: "nil config uses defaults",
			cfg:  nil,
		},
		{
			name: "gzip config",
			cfg: &config.CompressionConfig{
				Type:  "gzip",
				Level: 6,
			},
		},
		{
			name: "brotli config",
			cfg: &config.CompressionConfig{
				Type:  "brotli",
				Level: 4,
			},
		},
		{
			name: "both config",
			cfg: &config.CompressionConfig{
				Type:  "both",
				Level: 6,
			},
		},
		{
			name: "custom types",
			cfg: &config.CompressionConfig{
				Type:  "gzip",
				Types: []string{"text/html", "application/json"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := New(tt.cfg)
			if err != nil {
				t.Errorf("New() error: %v", err)
			}
			if m == nil {
				t.Error("Expected non-nil middleware")
			}
		})
	}
}

func TestDefaultCompressibleTypes(t *testing.T) {
	types := defaultCompressibleTypes()
	if len(types) == 0 {
		t.Error("Expected non-empty default types")
	}

	// 检查关键类型
	expected := []string{"text/html", "text/css", "application/json"}
	for _, e := range expected {
		found := slices.Contains(types, e)
		if !found {
			t.Errorf("Expected type %s in default list", e)
		}
	}
}

func TestIsCompressible(t *testing.T) {
	m, _ := New(&config.CompressionConfig{
		Types: []string{"text/html", "text/*", "application/json"},
	})

	tests := []struct {
		contentType []byte
		expected    bool
	}{
		{[]byte("text/html"), true},
		{[]byte("text/html; charset=utf-8"), true},
		{[]byte("text/css"), true},
		{[]byte("text/plain"), true},
		{[]byte("application/json"), true},
		{[]byte("application/json; charset=utf-8"), true},
		{[]byte("image/png"), false},
		{[]byte("application/octet-stream"), false},
		{[]byte(""), false},
	}

	for _, tt := range tests {
		t.Run(string(tt.contentType), func(t *testing.T) {
			result := m.isCompressible(tt.contentType)
			if result != tt.expected {
				t.Errorf("isCompressible(%s) = %v, expected %v", tt.contentType, result, tt.expected)
			}
		})
	}
}

func TestCompressGzip(t *testing.T) {
	m, _ := New(&config.CompressionConfig{
		Type:  "gzip",
		Level: 6,
	})

	// 测试数据
	data := []byte("Hello, World! This is a test string that should be compressed.")

	compressed := m.compressWithPool(data, m.gzipPool)
	if len(compressed) == 0 {
		t.Error("Expected compressed data")
	}

	// 压缩后应该更小（对于重复文本）
	if len(compressed) >= len(data) {
		t.Logf("Warning: compressed size %d >= original %d", len(compressed), len(data))
	}
}

func TestCompressBrotli(t *testing.T) {
	m, _ := New(&config.CompressionConfig{
		Type:  "brotli",
		Level: 4,
	})

	data := []byte("Hello, World! This is a test string that should be compressed with brotli.")

	compressed := m.compressWithPool(data, m.brotliPool)
	if len(compressed) == 0 {
		t.Error("Expected compressed data")
	}
}

func TestProcessNoCompression(t *testing.T) {
	m, _ := New(&config.CompressionConfig{
		Type:    "gzip",
		MinSize: 1000, // 大阈值
	})

	nextHandler := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/html")
		_, _ = ctx.WriteString("Short response")
	}

	handler := m.Process(nextHandler)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("Accept-Encoding", "gzip")

	handler(ctx)

	// 响应太短，不应压缩
	encoding := ctx.Response.Header.Peek("Content-Encoding")
	if string(encoding) != "" {
		t.Errorf("Expected no Content-Encoding, got %s", encoding)
	}

	body := string(ctx.Response.Body())
	if body != "Short response" {
		t.Errorf("Expected 'Short response', got %s", body)
	}
}

func TestProcessWithGzip(t *testing.T) {
	m, _ := New(&config.CompressionConfig{
		Type:    "gzip",
		Level:   6,
		MinSize: 10, // 小阈值
		Types:   []string{"text/html"},
	})

	// 创建足够长的响应
	longResponse := bytes.Repeat([]byte("Hello World! "), 100) // 1300+ bytes

	nextHandler := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/html")
		_, _ = ctx.Write(longResponse)
	}

	handler := m.Process(nextHandler)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("Accept-Encoding", "gzip")

	handler(ctx)

	encoding := ctx.Response.Header.Peek("Content-Encoding")
	if string(encoding) != "gzip" {
		t.Errorf("Expected Content-Encoding 'gzip', got %s", encoding)
	}

	// 响应应该被压缩（更小）
	body := ctx.Response.Body()
	if len(body) >= len(longResponse) {
		t.Errorf("Expected compressed body smaller than original, got %d >= %d", len(body), len(longResponse))
	}
}

func TestProcessWithBrotli(t *testing.T) {
	m, _ := New(&config.CompressionConfig{
		Type:    "brotli",
		Level:   4,
		MinSize: 10,
		Types:   []string{"text/html"},
	})

	longResponse := bytes.Repeat([]byte("Hello World! "), 100)

	nextHandler := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/html")
		_, _ = ctx.Write(longResponse)
	}

	handler := m.Process(nextHandler)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("Accept-Encoding", "br")

	handler(ctx)

	encoding := ctx.Response.Header.Peek("Content-Encoding")
	if string(encoding) != "br" {
		t.Errorf("Expected Content-Encoding 'br', got %s", encoding)
	}
}

func TestProcessUnsupportedEncoding(t *testing.T) {
	m, _ := New(&config.CompressionConfig{
		Type:  "gzip",
		Level: 6,
	})

	nextHandler := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/html")
		_, _ = ctx.WriteString("Test response")
	}

	handler := m.Process(nextHandler)

	ctx := &fasthttp.RequestCtx{}
	// 不设置 Accept-Encoding 或设置为空
	ctx.Request.Header.Set("Accept-Encoding", "")

	handler(ctx)

	// 不应压缩
	encoding := ctx.Response.Header.Peek("Content-Encoding")
	if string(encoding) != "" {
		t.Errorf("Expected no Content-Encoding, got %s", encoding)
	}
}

func TestProcessNonCompressibleType(t *testing.T) {
	m, _ := New(&config.CompressionConfig{
		Type:    "gzip",
		MinSize: 10,
	})

	longResponse := bytes.Repeat([]byte("data"), 1000)

	nextHandler := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "image/png")
		_, _ = ctx.Write(longResponse)
	}

	handler := m.Process(nextHandler)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("Accept-Encoding", "gzip")

	handler(ctx)

	// 图片不应压缩
	encoding := ctx.Response.Header.Peek("Content-Encoding")
	if string(encoding) != "" {
		t.Errorf("Expected no Content-Encoding for image, got %s", encoding)
	}
}

func TestName(t *testing.T) {
	m, _ := New(nil)
	if m.Name() != "compression" {
		t.Errorf("Expected name 'compression', got %s", m.Name())
	}
}

func TestGetters(t *testing.T) {
	cfg := &config.CompressionConfig{
		Type:    "gzip",
		Level:   5,
		MinSize: 500,
		Types:   []string{"text/html"},
	}
	m, _ := New(cfg)

	if m.Level() != 5 {
		t.Errorf("Expected Level 5, got %d", m.Level())
	}
	if m.MinSize() != 500 {
		t.Errorf("Expected MinSize 500, got %d", m.MinSize())
	}
	if len(m.Types()) != 1 {
		t.Errorf("Expected 1 type, got %d", len(m.Types()))
	}
}

func TestProcessStreamingGzip(t *testing.T) {
	m, _ := New(&config.CompressionConfig{
		Type:    "gzip",
		Level:   6,
		MinSize: 10,
		Types:   []string{"text/html"},
	})

	// 创建大于 streamingThreshold 的响应
	largeResponse := bytes.Repeat([]byte("Hello World! This is streaming test data. "), 2000) // ~80KB

	nextHandler := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/html")
		_, _ = ctx.Write(largeResponse)
	}

	handler := m.Process(nextHandler)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("Accept-Encoding", "gzip")

	handler(ctx)

	encoding := ctx.Response.Header.Peek("Content-Encoding")
	if string(encoding) != "gzip" {
		t.Errorf("Expected Content-Encoding 'gzip', got %s", encoding)
	}

	// Content-Length 应该被移除（使用 chunked encoding）
	contentLength := ctx.Response.Header.Peek("Content-Length")
	if string(contentLength) != "" {
		t.Errorf("Expected no Content-Length for streaming, got %s", contentLength)
	}

	// 读取 body 并解压验证
	body := ctx.Response.Body()
	if len(body) == 0 {
		t.Fatal("Expected non-empty body")
	}

	// 解压验证
	gr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Failed to create gzip reader: %v", err)
	}
	defer gr.Close()

	decompressed, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("Failed to decompress: %v", err)
	}

	if !bytes.Equal(decompressed, largeResponse) {
		t.Errorf("Decompressed body does not match original")
	}
}

func TestProcessStreamingBrotli(t *testing.T) {
	m, _ := New(&config.CompressionConfig{
		Type:    "brotli",
		Level:   4,
		MinSize: 10,
		Types:   []string{"text/html"},
	})

	// 创建大于 streamingThreshold 的响应
	largeResponse := bytes.Repeat([]byte("Hello World! This is brotli streaming test data. "), 2000) // ~100KB

	nextHandler := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/html")
		_, _ = ctx.Write(largeResponse)
	}

	handler := m.Process(nextHandler)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("Accept-Encoding", "br")

	handler(ctx)

	encoding := ctx.Response.Header.Peek("Content-Encoding")
	if string(encoding) != "br" {
		t.Errorf("Expected Content-Encoding 'br', got %s", encoding)
	}

	// Content-Length 应该被移除
	contentLength := ctx.Response.Header.Peek("Content-Length")
	if string(contentLength) != "" {
		t.Errorf("Expected no Content-Length for streaming, got %s", contentLength)
	}

	// 读取 body 并解压验证
	body := ctx.Response.Body()
	if len(body) == 0 {
		t.Fatal("Expected non-empty body")
	}

	// 解压验证
	br := brotli.NewReader(bytes.NewReader(body))
	decompressed, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("Failed to decompress: %v", err)
	}

	if !bytes.Equal(decompressed, largeResponse) {
		t.Errorf("Decompressed body does not match original")
	}
}

func TestProcessSmallResponseBuffered(t *testing.T) {
	m, _ := New(&config.CompressionConfig{
		Type:    "gzip",
		Level:   6,
		MinSize: 10,
		Types:   []string{"text/html"},
	})

	// 创建小于 streamingThreshold 但大于 MinSize 的响应
	smallResponse := bytes.Repeat([]byte("Hello World! "), 100) // ~1.3KB

	nextHandler := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/html")
		_, _ = ctx.Write(smallResponse)
	}

	handler := m.Process(nextHandler)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("Accept-Encoding", "gzip")

	handler(ctx)

	encoding := ctx.Response.Header.Peek("Content-Encoding")
	if string(encoding) != "gzip" {
		t.Errorf("Expected Content-Encoding 'gzip', got %s", encoding)
	}

	// 小响应应该被压缩且 body 更小
	body := ctx.Response.Body()
	if len(body) >= len(smallResponse) {
		t.Errorf("Expected compressed body smaller than original, got %d >= %d", len(body), len(smallResponse))
	}
}

// TestMiddleware_SkipPrecompressed 验证预压缩响应不被再次压缩。
// 当上游处理器（如 gzip_static）已设置 Content-Encoding 时，
// compression 中间件应跳过压缩，避免双重编码导致数据损坏。
func TestMiddleware_SkipPrecompressed(t *testing.T) {
	m, _ := New(&config.CompressionConfig{
		Type:    "gzip",
		Level:   6,
		MinSize: 1,
	})

	// 模拟预压缩响应（如 gzip_static 设置的）
	originalBody := []byte("precompressed data")
	nextHandler := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.SetBody(originalBody)
		ctx.Response.Header.Set("Content-Encoding", "gzip")
		ctx.Response.Header.SetContentType("application/json")
	}

	handler := m.Process(nextHandler)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("Accept-Encoding", "gzip")

	handler(ctx)

	// 验证 Content-Encoding 保持不变
	encoding := ctx.Response.Header.Peek("Content-Encoding")
	if string(encoding) != "gzip" {
		t.Errorf("Content-Encoding = %q, want %q", string(encoding), "gzip")
	}

	// 验证 body 内容未被修改（未被再次压缩）
	body := ctx.Response.Body()
	if !bytes.Equal(body, originalBody) {
		t.Errorf("Body was modified, should remain unchanged")
	}
}

// TestMiddleware_CompressWhenNoPrecompressed 验证无预压缩文件的响应仍正常压缩。
func TestMiddleware_CompressWhenNoPrecompressed(t *testing.T) {
	m, _ := New(&config.CompressionConfig{
		Type:    "gzip",
		Level:   6,
		MinSize: 1,
		Types:   []string{"application/json"},
	})

	// 使用足够大的数据确保压缩后更小
	originalBody := bytes.Repeat([]byte(`{"message": "test"}`), 100) // ~1700 bytes
	nextHandler := func(ctx *fasthttp.RequestCtx) {
		ctx.Response.SetBody(originalBody)
		ctx.Response.Header.SetContentType("application/json")
	}

	handler := m.Process(nextHandler)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("Accept-Encoding", "gzip")

	handler(ctx)

	// 验证 Content-Encoding 被设置
	encoding := ctx.Response.Header.Peek("Content-Encoding")
	if string(encoding) != "gzip" {
		t.Errorf("Content-Encoding = %q, want %q", string(encoding), "gzip")
	}

	// 验证 body 被压缩（大小应该变小）
	body := ctx.Response.Body()
	if len(body) >= len(originalBody) {
		t.Errorf("Expected compressed body smaller than original, got %d >= %d", len(body), len(originalBody))
	}
}

// TestCompressorPoolConcurrentGet 验证并发从池中获取 writer 不会产生竞态。
//
// 该测试针对 pool.New 在构造函数中初始化的场景，确保多个 goroutine 同时
// Get/Put 时 sync.Pool 的行为是安全的。
func TestCompressorPoolConcurrentGet(t *testing.T) {
	m, err := New(&config.CompressionConfig{
		Type:    "gzip",
		Level:   6,
		MinSize: 10,
		Types:   []string{"text/html"},
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, ok := m.gzipPool.Get()
			if ok {
				m.gzipPool.Put(w)
			}
		}()
	}
	wg.Wait()
}

// TestProcessEncodedETag 压缩后的强 ETag 追加编码后缀；未压缩与弱 ETag 保持不变。
func TestProcessEncodedETag(t *testing.T) {
	body := bytes.Repeat([]byte("etag body "), 100)
	for _, tc := range []struct {
		name, alg, ae, in, want string
	}{
		{"gzip强", "gzip", "gzip", `"abc"`, `"abc-gzip"`},
		{"br强", "brotli", "br", `"abc"`, `"abc-br"`},
		{"弱ETag不变", "gzip", "gzip", `W/"abc"`, `W/"abc"`},
		{"未压缩不变", "gzip", "", `"abc"`, `"abc"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mw, err := New(&config.CompressionConfig{Type: tc.alg, Level: 4, MinSize: 10, Types: []string{"text/plain"}})
			if err != nil {
				t.Fatal(err)
			}
			h := mw.Process(func(ctx *fasthttp.RequestCtx) {
				ctx.Response.Header.SetContentType("text/plain")
				ctx.Response.Header.Set("ETag", tc.in)
				ctx.Response.SetBody(body)
			})
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.SetRequestURI("/")
			if tc.ae != "" {
				ctx.Request.Header.Set("Accept-Encoding", tc.ae)
			}
			h(ctx)
			if got := string(ctx.Response.Header.Peek("ETag")); got != tc.want {
				t.Errorf("ETag = %q, want %q", got, tc.want)
			}
		})
	}
}
