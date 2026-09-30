// Package utils 提供 ETag 生成工具函数的测试。
//
// 该文件测试 GenerateETag 函数，包括：
//   - 正常参数生成
//   - 零值参数
//   - 大数值参数
//   - 格式验证
//
// 作者：xfy
package utils

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeETag(unix int64, size int64) string {
	return fmt.Sprintf(`"%x-%x"`, unix, size)
}

func TestGenerateETag(t *testing.T) {
	tests := []struct {
		name    string
		modTime time.Time
		size    int64
	}{
		{"valid_time_and_size", time.Unix(1609459200, 0), 1024},
		{"zero_time", time.Time{}, 100},
		{"zero_size", time.Unix(1609459200, 0), 0},
		{"large_size", time.Unix(1609459200, 0), 1<<62 - 1},
		{"negative_size", time.Unix(1609459200, 0), -1},
		{"negative_modtime", time.Unix(-1000, 0), 500},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GenerateETag(tt.modTime, tt.size)
			want := makeETag(tt.modTime.Unix(), tt.size)
			assert.Equal(t, want, got)
		})
	}
}

func TestGenerateETag_Format(t *testing.T) {
	etag := GenerateETag(time.Unix(1609459200, 0), 1024)

	assert.True(t, strings.HasPrefix(etag, "\""), "ETag should start with a quote")
	assert.True(t, strings.HasSuffix(etag, "\""), "ETag should end with a quote")
	assert.Equal(t, byte('"'), etag[0])
	assert.Equal(t, byte('"'), etag[len(etag)-1])

	inner := strings.Trim(etag, "\"")
	parts := strings.SplitN(inner, "-", 2)
	require.Len(t, parts, 2, "inner ETag should have exactly one hyphen separator")

	for _, part := range parts {
		for _, c := range part {
			assert.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || c == '-',
				"character %c in ETag part should be hex digit or minus sign", c)
		}
	}
}

func TestGenerateETag_Deterministic(t *testing.T) {
	modTime := time.Unix(1700000000, 0)
	size := int64(2048)

	etag1 := GenerateETag(modTime, size)
	etag2 := GenerateETag(modTime, size)
	assert.Equal(t, etag1, etag2, "same inputs should produce identical ETags")
}

func TestGenerateETag_DifferentInputs(t *testing.T) {
	t1 := time.Unix(1700000000, 0)
	t2 := time.Unix(1700000001, 0)

	assert.NotEqual(t, GenerateETag(t1, 100), GenerateETag(t2, 100),
		"different modtimes should produce different ETags")
	assert.NotEqual(t, GenerateETag(t1, 100), GenerateETag(t1, 200),
		"different sizes should produce different ETags")
}

func TestETagForEncoding(t *testing.T) {
	tests := []struct{ etag, enc, want string }{
		{`"abc"`, "gzip", `"abc-gzip"`},
		{`"abc"`, "br", `"abc-br"`},
		{`"abc"`, "GZIP", `"abc-gzip"`},
		{`"abc"`, "", `"abc"`},
		{`"abc"`, "identity", `"abc"`},
		{`W/"abc"`, "gzip", `W/"abc"`},
		{``, "gzip", ``},
		{`"abc-gzip"`, "gzip", `"abc-gzip"`},
	}
	for _, tt := range tests {
		if got := ETagForEncoding(tt.etag, tt.enc); got != tt.want {
			t.Errorf("ETagForEncoding(%q,%q) = %q, want %q", tt.etag, tt.enc, got, tt.want)
		}
	}
}
