// Package handler 提供 HTTP 请求处理器，包括路由、静态文件服务和零拷贝传输。
//
// 该文件实现静态文件的 HTTP Range 请求（RFC 9110 14）支持，包括：
//   - 单区间 Range：bytes=a-b、bytes=a-、bytes=-n
//   - 206 Partial Content + Content-Range，不可满足时返回 416 + Content-Range: bytes */size
//   - If-Range（强 ETag 或 HTTP 日期）
//   - 大文件区间的 sendfile 零拷贝传输
//
// 设计取舍：
//   - 仅支持单区间；多区间（含逗号）、非 bytes 单位、语法非法的 Range 一律忽略，
//     按普通 200 返回完整内容（RFC 9110 14.2 允许服务端忽略 Range）。
//   - 仅对 GET 生效；HEAD 等其他方法忽略 Range（RFC 9110 14.2）。
//   - Range 始终作用于未压缩（identity）内容：存在有效 Range 时跳过预压缩文件，
//     且压缩中间件不会压缩 206 响应。
//
// 作者：xfy
package handler

import (
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

// rangeState 表示 Range 请求的评估结果。
type rangeState int

const (
	// rangeNone 表示无 Range、Range 被忽略或 If-Range 不匹配，按 200 返回完整内容。
	rangeNone rangeState = iota
	// rangePartial 表示区间有效，按 206 返回部分内容。
	rangePartial
	// rangeUnsatisfiable 表示区间无法满足，返回 416。
	rangeUnsatisfiable
)

// rangeSpec 是一次 Range 评估的结果。
type rangeSpec struct {
	state  rangeState
	start  int64 // 起始偏移（含）
	length int64 // 区间长度
	size   int64 // 资源完整长度
}

// end 返回区间的最后一个字节偏移（含）。
func (s rangeSpec) end() int64 { return s.start + s.length - 1 }

// slice 按区间截取内存中的完整内容；非 rangePartial 时返回原数据。
func (s rangeSpec) slice(data []byte) []byte {
	if s.state != rangePartial {
		return data
	}
	return data[s.start : s.start+s.length]
}

// evalRange 评估请求的 Range 头。
//
// 参数：
//   - ctx: fasthttp 请求上下文
//   - size: 资源（未压缩）完整长度
//   - etag: 资源 identity（未压缩）表示的强 ETag（用于 If-Range）
//   - modTime: 资源修改时间（用于 If-Range 日期比较）
func evalRange(ctx *fasthttp.RequestCtx, size int64, etag string, modTime time.Time) rangeSpec {
	spec := rangeSpec{state: rangeNone, size: size}
	if !hasRangeRequest(ctx) {
		return spec
	}
	if !ifRangeMatches(ctx, etag, modTime) {
		return spec
	}
	start, length, state := parseByteRange(string(ctx.Request.Header.Peek("Range")), size)
	spec.state, spec.start, spec.length = state, start, length
	return spec
}

// hasRangeRequest 判断请求是否携带 Range 头且方法为 GET。
//
// RFC 9110 14.2：Range 仅对 GET 定义，其他方法（含 HEAD）必须忽略它，
// HEAD 应返回与 GET 完整响应一致的元数据（200、完整 Content-Length）。
func hasRangeRequest(ctx *fasthttp.RequestCtx) bool {
	if !ctx.IsGet() {
		return false
	}
	return len(ctx.Request.Header.Peek("Range")) > 0
}

// ifRangeMatches 检查 If-Range 条件，无 If-Range 时视为匹配。
//
// If-Range 为 ETag 时要求与当前 identity（未压缩）表示的强 ETag 完全一致
// （弱 ETag 不匹配；压缩表示的 ETag 带编码后缀，因此同样不匹配，回退完整响应）；
// 为 HTTP 日期时要求与 Last-Modified 精确相等（秒级）。
func ifRangeMatches(ctx *fasthttp.RequestCtx, etag string, modTime time.Time) bool {
	v := strings.TrimSpace(string(ctx.Request.Header.Peek("If-Range")))
	if v == "" {
		return true
	}
	if strings.HasPrefix(v, "\"") || strings.HasPrefix(v, "W/") {
		return v == etag
	}
	t, err := fasthttp.ParseHTTPDate([]byte(v))
	if err != nil {
		return false
	}
	return modTime.UTC().Truncate(time.Second).Equal(t.UTC())
}

// parseByteRange 解析 Range 头值，返回起始偏移、长度与状态。
//
// 规则：
//   - 单位必须为 bytes（不区分大小写），否则忽略；
//   - 含逗号（多区间）或语法非法（含 start > end）则忽略；
//   - a-b / a-：a >= size 时不可满足；b 超出末尾则截断到 size-1；
//   - -n：取末尾 n 字节，n 大于 size 时取整个资源，n == 0 不可满足；
//   - size == 0 时任何区间均不可满足。
func parseByteRange(header string, size int64) (start, length int64, state rangeState) {
	unit, spec, ok := strings.Cut(strings.TrimSpace(header), "=")
	if !ok || !strings.EqualFold(strings.TrimSpace(unit), "bytes") {
		return 0, 0, rangeNone
	}
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.Contains(spec, ",") {
		return 0, 0, rangeNone
	}
	first, last, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, rangeNone
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)

	if first == "" {
		// 后缀区间 -n
		n, valid := parseRangeInt(last)
		if !valid {
			return 0, 0, rangeNone
		}
		if n == 0 || size == 0 {
			return 0, 0, rangeUnsatisfiable
		}
		if n > size {
			n = size
		}
		return size - n, n, rangePartial
	}

	a, valid := parseRangeInt(first)
	if !valid {
		return 0, 0, rangeNone
	}
	b := int64(math.MaxInt64)
	if last != "" {
		b, valid = parseRangeInt(last)
		if !valid || b < a {
			return 0, 0, rangeNone
		}
	}
	if a >= size {
		return 0, 0, rangeUnsatisfiable
	}
	if b >= size {
		b = size - 1
	}
	return a, b - a + 1, rangePartial
}

// parseRangeInt 解析非负十进制整数；仅接受数字，数值溢出时按 MaxInt64 处理。
func parseRangeInt(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return math.MaxInt64, true
	}
	return n, true
}

// apply 把评估结果写入响应头/状态码。
//
//   - 始终设置 Accept-Ranges: bytes；
//   - rangePartial：状态码 206 与 Content-Range（Content-Length 由 body 决定）；
//   - rangeUnsatisfiable：状态码 416 与 Content-Range: bytes */size，并清空 body。
func (s rangeSpec) apply(ctx *fasthttp.RequestCtx) {
	ctx.Response.Header.Set("Accept-Ranges", "bytes")
	switch s.state {
	case rangePartial:
		ctx.Response.SetStatusCode(fasthttp.StatusPartialContent)
		ctx.Response.Header.Set("Content-Range",
			"bytes "+strconv.FormatInt(s.start, 10)+"-"+strconv.FormatInt(s.end(), 10)+"/"+strconv.FormatInt(s.size, 10))
	case rangeUnsatisfiable:
		ctx.Response.SetStatusCode(fasthttp.StatusRequestedRangeNotSatisfiable)
		ctx.Response.Header.Set("Content-Range", "bytes */"+strconv.FormatInt(s.size, 10))
		ctx.Response.ResetBody()
		ctx.Response.Header.SetContentType("text/plain; charset=utf-8")
		ctx.Response.SetBodyString("Range Not Satisfiable")
	case rangeNone:
	}
}

// rangeFileReader 从文件指定偏移读取固定长度的内容，用作 SetBodyStream 的 body。
//
// 实现 io.WriterTo：fasthttp 写响应体时会调用 WriteTo，且写入目标（bufio.Writer）
// 会把 ReadFrom 下传给底层 TCP 连接；此处把 *io.LimitedReader 交给它，
// 使 Go 运行时仍可使用 sendfile 传输区间内容。
type rangeFileReader struct {
	lr *io.LimitedReader
	f  *os.File
}

// newRangeFileReader 定位到 start 并返回长度为 length 的读取器。
func newRangeFileReader(f *os.File, start, length int64) (*rangeFileReader, error) {
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	return &rangeFileReader{lr: &io.LimitedReader{R: f, N: length}, f: f}, nil
}

// Read 实现 io.Reader。
func (r *rangeFileReader) Read(p []byte) (int, error) { return r.lr.Read(p) }

// Close 关闭底层文件。
func (r *rangeFileReader) Close() error { return r.f.Close() }

// WriteTo 实现 io.WriterTo。
func (r *rangeFileReader) WriteTo(w io.Writer) (int64, error) {
	if rf, ok := w.(io.ReaderFrom); ok {
		return rf.ReadFrom(r.lr)
	}
	return io.Copy(w, r.lr)
}
