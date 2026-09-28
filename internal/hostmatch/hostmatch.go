// Package hostmatch 提供 nginx server_name 风格的主机名匹配。
//
// 该包被 HTTP 虚拟主机路由（internal/server.VHostManager）和 TLS SNI
// 证书选择（internal/ssl.SNIManager）共用，确保两者对同一份 server_name
// 配置的匹配结果始终一致：TLS 握手阶段选中的证书对应的虚拟主机，
// 必须与后续 HTTP 请求实际被路由到的虚拟主机相同。
//
// 支持的 server_name 格式：
//   - 精确匹配："example.com"
//   - 前缀通配："*.example.com"（匹配任意子域名）
//   - 后缀通配："example.*"（匹配任意 TLD）
//   - 正则匹配："~regex"（以 ~ 开头，后面是正则表达式）
//
// 匹配优先级（与 nginx server_name 规则一致）：
//  1. 精确匹配
//  2. 最长前缀通配
//  3. 后缀通配
//  4. 正则匹配（按添加顺序）
//  5. 默认值
package hostmatch

import (
	"fmt"
	"regexp"
	"strings"
)

// Matcher 按 server_name 规则匹配主机名并返回关联的值。
//
// 零值不可用，必须通过 New 创建。并发读取是安全的，但不支持并发写入
// （所有 Add/SetDefault 调用应在 Find 之前完成）。
type Matcher[T any] struct {
	exact             map[string]T
	wildcardSuffixMap map[string]T // "*.example.com" -> suffix "example.com"
	wildcardTLDMap    map[string]T // "example.*" -> tld "example"
	regexHosts        []regexEntry[T]
	defaultValue      T
	hasDefault        bool
}

type regexEntry[T any] struct {
	pattern *regexp.Regexp
	value   T
}

// New 创建一个空的 Matcher。
func New[T any]() *Matcher[T] {
	return &Matcher[T]{
		exact:             make(map[string]T),
		wildcardSuffixMap: make(map[string]T),
		wildcardTLDMap:    make(map[string]T),
	}
}

// Add 注册一个 server_name 模式及其关联值。
//
// 参数：
//   - pattern: server_name 模式（精确/前缀通配/后缀通配/正则）
//   - value: 匹配成功时关联的值
//
// 返回值：
//   - error: 正则模式无效时返回错误
func (m *Matcher[T]) Add(pattern string, value T) error {
	switch {
	case strings.HasPrefix(pattern, "~"):
		re, err := regexp.Compile(pattern[1:])
		if err != nil {
			return fmt.Errorf("invalid regex pattern %q: %w", pattern, err)
		}
		m.regexHosts = append(m.regexHosts, regexEntry[T]{pattern: re, value: value})
	case strings.HasPrefix(pattern, "*."):
		m.wildcardSuffixMap[pattern[2:]] = value
	case strings.HasSuffix(pattern, ".*"):
		m.wildcardTLDMap[pattern[:len(pattern)-2]] = value
	default:
		m.exact[pattern] = value
	}
	return nil
}

// SetDefault 设置未匹配到任何模式时使用的默认值。
func (m *Matcher[T]) SetDefault(value T) {
	m.defaultValue = value
	m.hasDefault = true
}

// findLongestWildcardPrefix 查找最长的前缀通配匹配。
//
// 例如 "a.b.example.com" 优先匹配 "*.b.example.com"，其次 "*.example.com"。
func (m *Matcher[T]) findLongestWildcardPrefix(host string) (T, bool) {
	parts := strings.Split(host, ".")
	for i := 1; i < len(parts); i++ {
		suffix := strings.Join(parts[i:], ".")
		if v, ok := m.wildcardSuffixMap[suffix]; ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}

// Find 按优先级匹配主机名，返回关联值。
//
// 返回值：
//   - T: 匹配到的值（未匹配且无默认值时为零值）
//   - bool: 是否匹配成功（含默认值命中）
func (m *Matcher[T]) Find(host string) (T, bool) {
	if v, ok := m.exact[host]; ok {
		return v, true
	}

	if v, ok := m.findLongestWildcardPrefix(host); ok {
		return v, true
	}

	parts := strings.Split(host, ".")
	if len(parts) >= 2 {
		if v, ok := m.wildcardTLDMap[parts[0]]; ok {
			return v, true
		}
	}

	for _, e := range m.regexHosts {
		if e.pattern.MatchString(host) {
			return e.value, true
		}
	}

	if m.hasDefault {
		return m.defaultValue, true
	}

	var zero T
	return zero, false
}
