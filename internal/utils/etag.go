// Package utils 提供通用的工具函数和辅助类型。
//
// 包含 ETag 生成相关的工具函数，用于处理缓存验证。
//
// 作者：xfy
package utils

import (
	"strconv"
	"strings"
	"time"
)

// GenerateETag 基于 ModTime 和 Size 生成 ETag。
// 使用 strconv.AppendInt 避免 fmt.Sprintf 分配。
// 格式: "<modtime-unix-hex>-<size-hex>"
func GenerateETag(modTime time.Time, size int64) string {
	var buf [32]byte
	b := buf[:0]
	b = append(b, '"')
	b = strconv.AppendInt(b, modTime.Unix(), 16)
	b = append(b, '-')
	b = strconv.AppendInt(b, size, 16)
	b = append(b, '"')
	return string(b)
}

// ETagForEncoding 返回指定内容编码对应表示的 ETag。
//
// 不同 Content-Encoding 的表示字节不同，必须携带不同的强 ETag（RFC 9110 8.8.3），
// 否则 If-Range / 缓存验证会把压缩表示与未压缩表示混为一谈。
// 强 ETag 在结尾引号前追加 "-<encoding>"，如 "abc" -> "abc-gzip"；
// encoding 为空或 identity、ETag 为空或弱 ETag（W/ 前缀）时原样返回，
// 已带相同编码后缀时保持幂等。
func ETagForEncoding(etag, encoding string) string {
	if etag == "" || encoding == "" || strings.EqualFold(encoding, "identity") ||
		strings.HasPrefix(etag, "W/") || len(etag) < 2 || !strings.HasSuffix(etag, `"`) {
		return etag
	}
	suffix := "-" + strings.ToLower(encoding)
	if strings.HasSuffix(etag, suffix+`"`) {
		return etag
	}
	return etag[:len(etag)-1] + suffix + `"`
}
