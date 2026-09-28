// Package config 提供 YAML 配置文件的解析、验证和默认配置生成功能。
//
// 该文件包含配置验证相关的核心逻辑，包括：
//   - 服务器配置验证（监听地址、静态文件、代理）
//   - SSL/TLS 配置验证（证书、协议、加密套件）
//   - 安全配置验证（访问控制、认证、速率限制）
//   - 压缩配置验证（类型、级别、最小大小）
//
// 主要用途：
//
//	用于验证用户提供的配置是否符合要求，确保服务器启动前配置有效。
//
// 注意事项：
//   - 验证失败时返回详细的错误信息
//   - 支持默认服务器和虚拟主机两种模式的验证
//
// 作者：xfy
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"rua.plus/lolly/internal/loadbalance"
	"rua.plus/lolly/internal/variable"
)

// validateDefaultServer 验证 servers 中最多只有一个 default: true 服务器。
//
// 参数：
//   - servers: 服务器配置列表
//
// 返回值：
//   - error: 超过一个 default 时返回错误信息，成功返回 nil
func validateDefaultServer(servers []ServerConfig) error {
	count := 0
	for _, s := range servers {
		if s.Default {
			count++
		}
	}
	if count > 1 {
		return errors.New("只能有一个 default: true 服务器")
	}
	return nil
}

// validateMode 验证服务器运行模式有效值。
//
// 检查 Mode 是否为 ServerModeSingle, ServerModeVHost,
// ServerModeMultiServer, ServerModeAuto 之一。
// 空值视为 auto（合法）。
//
// 参数：
//   - mode: 服务器运行模式
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
func validateMode(mode ServerMode) error {
	if mode == "" || mode == ServerModeAuto {
		return nil
	}
	validModes := []string{
		string(ServerModeSingle),
		string(ServerModeVHost),
		string(ServerModeMultiServer),
		string(ServerModeAuto),
	}
	return ValidateEnum(string(mode), validModes, "mode")
}

// validateListenConflicts 检测 servers 中监听地址冲突。
//
// 在 multi_server 模式下，每个 server 必须有 listen 配置。
// 允许相同 listen 地址但不同 server_name 的配置（nginx 虚拟主机风格）。
// 只有当 listen 和 server_name 都相同时才报冲突。
//
// 参数：
//   - servers: 服务器配置列表
//   - mode: 服务器运行模式
//
// 返回值：
//   - error: 发现冲突或缺失时返回错误信息，成功返回 nil
func validateListenConflicts(servers []ServerConfig, mode ServerMode) error {
	if mode != ServerModeMultiServer {
		return nil
	}

	// 使用 listen+name 组合作为唯一标识
	// 允许相同 listen 但不同 name（虚拟主机）
	seen := make(map[string]int)
	for i, s := range servers {
		if s.Listen == "" {
			return fmt.Errorf("servers[%d]: multi_server 模式下每个 server 必须配置 listen 地址", i)
		}
		// 使用 listen + name 作为唯一键
		key := s.Listen + "|" + s.Name
		if idx, exists := seen[key]; exists {
			return fmt.Errorf("监听地址冲突: servers[%d] 和 servers[%d] 都使用 %s 且 server_name 相同", idx, i, s.Listen)
		}
		seen[key] = i
	}
	return nil
}

// ValidateEnum 验证值是否在有效枚举列表中
func ValidateEnum(value string, validValues []string, fieldName string) error {
	if slices.Contains(validValues, value) {
		return nil
	}
	return fmt.Errorf("无效的 %s: %s（仅支持 %v）", fieldName, value, validValues)
}

// SignedInteger 约束：有符号整数类型
type SignedInteger interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64
}

// ValidateNonNegative 验证值为非负数（泛型版本）
func ValidateNonNegative[T SignedInteger](value T, fieldName string) error {
	if value < 0 {
		return fmt.Errorf("%s 不能为负数", fieldName)
	}
	return nil
}

// ValidateNonNegativeDuration 验证 time.Duration 值为非负数
func ValidateNonNegativeDuration(value time.Duration, fieldName string) error {
	return ValidateNonNegative(int64(value), fieldName)
}

// ValidateNoNullByte 验证字符串不包含 null byte
func ValidateNoNullByte(s string, fieldName string) error {
	if strings.Contains(s, "\x00") {
		return fmt.Errorf("%s 不能包含 null byte", fieldName)
	}
	return nil
}

// ValidatePathTraversal 验证路径不包含路径遍历 '..'
func ValidatePathTraversal(path string, fieldName string) error {
	if strings.Contains(path, "..") {
		return fmt.Errorf("%s不能包含 '..'", fieldName)
	}
	return nil
}

// validateServer 验证服务器配置。
//
// 检查服务器配置的各项参数是否符合要求，包括监听地址、
// 静态文件、代理、SSL、安全和压缩等配置。
//
// 参数：
//   - s: 服务器配置对象
//   - isDefault: 是否为默认服务器，默认服务器可省略部分配置
//
// 返回值：
//   - error: 验证失败时返回具体错误信息，成功返回 nil
//
// 注意事项：
//   - 默认服务器可省略监听地址
//   - 验证错误信息包含字段路径，便于定位问题
func validateServer(s *ServerConfig, isDefault bool) error {
	// 监听地址必填（默认服务器可省略，使用默认值）
	if s.Listen == "" && !isDefault {
		return errors.New("listen 地址必填")
	}

	// 验证监听地址格式
	if s.Listen != "" {
		if _, err := net.ResolveTCPAddr("tcp", s.Listen); err != nil {
			return fmt.Errorf("无效的监听地址 %s: %w", s.Listen, err)
		}
	}

	// 验证静态文件配置
	if err := validateStatics(s.Static); err != nil {
		return fmt.Errorf("static: %w", err)
	}

	// 验证代理配置
	for i := range s.Proxy {
		if err := validateProxy(&s.Proxy[i]); err != nil {
			return fmt.Errorf("proxy[%d]: %w", i, err)
		}
	}

	// 检查 static 和 proxy 路径冲突
	if err := validatePathConflicts(s); err != nil {
		return err
	}

	// 验证重写规则
	for i := range s.Rewrite {
		if err := validateRewrite(&s.Rewrite[i]); err != nil {
			return fmt.Errorf("rewrite[%d]: %w", i, err)
		}
	}

	// 验证 SSL 配置
	if err := validateSSL(&s.SSL); err != nil {
		return fmt.Errorf("ssl: %w", err)
	}

	// 验证 ACME 配置（需要 server_name 上下文以推导申请域名）
	if err := validateACME(&s.SSL.ACME, s.ServerNames, s.Name); err != nil {
		return fmt.Errorf("ssl.acme: %w", err)
	}

	// 验证安全配置
	if err := validateSecurity(&s.Security); err != nil {
		return fmt.Errorf("security: %w", err)
	}

	// 验证压缩配置
	if err := validateCompression(&s.Compression); err != nil {
		return fmt.Errorf("compression: %w", err)
	}

	// 验证 Lua 中间件配置
	if err := validateLua(s.Lua); err != nil {
		return fmt.Errorf("lua: %w", err)
	}

	return nil
}

// validateStatics 验证静态文件配置数组。
//
// 检查静态文件配置的路径重复和根目录路径安全性。
//
// 参数：
//   - statics: 静态文件配置数组
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
func validateStatics(statics []StaticConfig) error {
	if len(statics) == 0 {
		return nil
	}

	paths := make(map[string]int)
	for i, s := range statics {
		// Path 默认为 "/"
		path := s.Path
		if path == "" {
			path = "/"
		}

		// 检查路径重复
		if idx, exists := paths[path]; exists {
			return fmt.Errorf("路径 %s 重复定义 (static[%d] 和 static[%d])", path, idx, i)
		}
		paths[path] = i

		// root 和 alias 互斥检查
		if s.Root != "" && s.Alias != "" {
			return fmt.Errorf("static[%d]: root 和 alias 不能同时设置", i)
		}

		// 验证根目录路径安全
		if err := ValidatePathTraversal(s.Root, fmt.Sprintf("static[%d]: 根目录路径", i)); err != nil {
			return err
		}

		// 验证 alias 路径安全
		if err := ValidatePathTraversal(s.Alias, fmt.Sprintf("static[%d]: alias 路径", i)); err != nil {
			return err
		}

		// 验证 try_files 模式
		for j, pattern := range s.TryFiles {
			if err := validateTryFilesPattern(pattern); err != nil {
				return fmt.Errorf("static[%d].try_files[%d]: %w", i, j, err)
			}
		}

		// 验证 auto_index_format
		if s.AutoIndex {
			validFormats := []string{"", "html", "json", "xml"}
			if err := ValidateEnum(s.AutoIndexFormat, validFormats, "auto_index_format"); err != nil {
				return fmt.Errorf("static[%d]: %w", i, err)
			}
		}
	}
	return nil
}

// validateTryFilesPattern 验证 try_files 模式的安全性。
//
// 检查 try_files 配置项是否包含安全风险的模式。
// 支持的模式格式：
//   - $uri - 请求路径
//   - $uri/ - 请求路径加斜杠
//   - $uri.<ext> - 请求路径加扩展名（如 $uri.html）
//   - /path - 绝对路径回退
//   - filename - 相对路径回退
//
// 验证规则：
//   - 拒绝 null byte（\x00）
//   - 拒绝路径分隔符（/ 和 \）在扩展名中
//   - 扩展名仅允许字母、数字、点、下划线、连字符
//   - 拒绝危险后缀（.php, .exe, .bat, .sh, .cgi 等）
//
// 参数：
//   - pattern: try_files 配置项
//
// 返回值：
//   - error: 验证失败时返回具体错误信息
func validateTryFilesPattern(pattern string) error {
	if pattern == "" {
		return errors.New("try_files 模式不能为空")
	}

	// 检查 null byte
	if err := ValidateNoNullByte(pattern, "try_files 模式"); err != nil {
		return err
	}

	// 定义支持的模式类型
	// 1. $uri 占位符
	if pattern == "$uri" || pattern == "$uri/" {
		return nil
	}

	// 2. $uri.<ext> 动态后缀
	if strings.HasPrefix(pattern, "$uri.") {
		ext := pattern[5:] // 提取扩展名部分

		// 检查扩展名安全性
		if err := validateTryFilesExtension(ext); err != nil {
			return fmt.Errorf("try_files 模式 %q: %w", pattern, err)
		}
		return nil
	}

	// 3. 绝对路径回退（以 / 开头）
	if strings.HasPrefix(pattern, "/") {
		// 验证路径不包含危险字符
		if strings.Contains(pattern, "..") {
			return fmt.Errorf("try_files 模式 %q 不能包含路径遍历", pattern)
		}
		return nil
	}

	// 4. 相对路径回退（文件名）
	// 检查是否为安全文件名
	if err := validateTryFilesFilename(pattern); err != nil {
		return fmt.Errorf("try_files 模式 %q: %w", pattern, err)
	}

	return nil
}

// validateTryFilesExtension 验证动态后缀扩展名的安全性。
//
// 检查扩展名是否包含危险字符或属于危险后缀列表。
//
// 参数：
//   - ext: 扩展名字符串（不包含前导点）
//
// 返回值：
//   - error: 验证失败时返回错误信息
func validateTryFilesExtension(ext string) error {
	if ext == "" {
		return errors.New("扩展名不能为空")
	}

	// 检查路径分隔符
	if strings.ContainsAny(ext, "/\\") {
		return errors.New("扩展名不能包含路径分隔符")
	}

	// 检查 null byte
	if err := ValidateNoNullByte(ext, "扩展名"); err != nil {
		return err
	}

	// 白名单字符检查：仅允许字母、数字、点、下划线、连字符
	for i, c := range ext {
		isLetter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		isAllowed := c == '.' || c == '_' || c == '-'
		if !isLetter && !isDigit && !isAllowed {
			return fmt.Errorf("扩展名包含非法字符 %q (位置 %d)", c, i)
		}
	}

	// 危险后缀黑名单（不含前导点，与 ext 格式一致）
	dangerousExtensions := []string{
		"php", "php3", "php4", "php5", "phtml",
		"exe", "bat", "cmd", "sh", "bash",
		"cgi", "pl", "py", "rb",
		"asp", "aspx", "jsp",
	}

	extLower := strings.ToLower(ext)
	for _, dangerous := range dangerousExtensions {
		if extLower == dangerous || strings.HasSuffix(extLower, "."+dangerous) {
			return fmt.Errorf("扩展名 %q 被禁止（潜在安全风险）", ext)
		}
	}

	return nil
}

// validateTryFilesFilename 验证回退文件名的安全性。
//
// 检查文件名是否包含路径遍历或危险字符。
//
// 参数：
//   - filename: 文件名字符串
//
// 返回值：
//   - error: 验证失败时返回错误信息
func validateTryFilesFilename(filename string) error {
	if filename == "" {
		return errors.New("文件名不能为空")
	}

	// 检查路径遍历
	if strings.Contains(filename, "..") {
		return errors.New("文件名不能包含路径遍历")
	}

	// 检查路径分隔符
	if strings.ContainsAny(filename, "/\\") {
		return errors.New("文件名不能包含路径分隔符")
	}

	// 检查 null byte
	if err := ValidateNoNullByte(filename, "文件名"); err != nil {
		return err
	}

	return nil
}

// validatePathConflicts 检查 static 和 proxy 路径冲突。
//
// 确保 static 和 proxy 没有相同的 path 前缀。
//
// 参数：
//   - s: 服务器配置对象
//
// 返回值：
//   - error: 发现冲突时返回错误信息，成功返回 nil
func validatePathConflicts(s *ServerConfig) error {
	staticPaths := make(map[string]int)
	for i, st := range s.Static {
		path := st.Path
		if path == "" {
			path = "/"
		}
		staticPaths[path] = i
	}

	for i, p := range s.Proxy {
		if idx, exists := staticPaths[p.Path]; exists {
			return fmt.Errorf("路径 %s 同时定义在 static[%d] 和 proxy[%d]", p.Path, idx, i)
		}
	}
	return nil
}

// validateProxy 验证代理配置。
//
// 检查代理路径、目标地址和负载均衡算法的有效性。
//
// 参数：
//   - p: 代理配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
//
// 验证规则：
//   - path 必填
//   - targets 至少需要一个目标
//   - 目标 URL 必须以 http:// 或 https:// 开头
//   - load_balance 必须是有效的负载均衡算法
func validateProxy(p *ProxyConfig) error {
	// 路径必填
	if p.Path == "" {
		return errors.New("path 必填")
	}

	// 至少需要一个目标
	if len(p.Targets) == 0 {
		return errors.New("targets 至少需要一个目标地址")
	}

	// 验证每个目标地址
	for i, t := range p.Targets {
		if t.URL == "" {
			return fmt.Errorf("targets[%d].url 必填", i)
		}
		if !strings.HasPrefix(t.URL, "http://") && !strings.HasPrefix(t.URL, "https://") {
			return fmt.Errorf("targets[%d].url 必须以 http:// 或 https:// 开头", i)
		}
	}

	// 验证负载均衡算法
	if !loadbalance.IsValidAlgorithm(p.LoadBalance) {
		return fmt.Errorf("无效的负载均衡算法：%s", p.LoadBalance)
	}

	// validate least_time config
	if p.LoadBalance == "least_time" {
		if p.LeastTime.Metric != "" && p.LeastTime.Metric != "header" && p.LeastTime.Metric != "last_byte" {
			return fmt.Errorf("无效的 least_time metric: %s（有效值: header, last_byte）", p.LeastTime.Metric)
		}
		if p.LeastTime.DefaultTime < 0 {
			return fmt.Errorf("least_time default_time 不能为负数")
		}
	}

	// validate sticky config
	if p.LoadBalance == "sticky" {
		if !p.Sticky.Enabled {
			return fmt.Errorf("load_balance=sticky 时 sticky.enabled 必须为 true")
		}
		if p.Sticky.FallbackAlgo != "" && !loadbalance.IsValidAlgorithm(p.Sticky.FallbackAlgo) {
			return fmt.Errorf("无效的 sticky fallback_balance: %s", p.Sticky.FallbackAlgo)
		}
		if p.Sticky.SameSite != "" {
			validSameSites := []string{"Lax", "Strict", "None"}
			if !slices.Contains(validSameSites, p.Sticky.SameSite) {
				return fmt.Errorf("无效的 sticky same_site: %s（有效值: Lax, Strict, None）", p.Sticky.SameSite)
			}
		}
	}

	// 验证故障转移配置
	if err := validateNextUpstream(&p.NextUpstream); err != nil {
		return fmt.Errorf("next_upstream: %w", err)
	}

	// 验证一致性哈希键格式
	if p.HashKey != "" {
		validHashKeys := []string{"ip", "uri"}
		if !strings.HasPrefix(p.HashKey, "header:") {
			if err := ValidateEnum(p.HashKey, validHashKeys, "hash_key"); err != nil {
				return fmt.Errorf("无效的 hash_key: %s（仅支持 ip, uri 或 header:X-Name 格式）", p.HashKey)
			}
		}
	}

	// 验证 redirect_rewrite 配置
	if err := validateRedirectRewrite(p.RedirectRewrite); err != nil {
		return fmt.Errorf("redirect_rewrite: %w", err)
	}

	// 验证 location_type 和 path 组合
	validLocationTypes := []string{"", "exact", "prefix_priority", "regex", "regex_caseless", "prefix", "named"}
	if p.LocationType != "" {
		if err := ValidateEnum(p.LocationType, validLocationTypes, "location_type"); err != nil {
			return fmt.Errorf("无效的 location_type: %s", p.LocationType)
		}
	}

	// 当 location_type 为 regex 类型时，验证 path 是否是有效正则
	if p.LocationType == "regex" || p.LocationType == "regex_caseless" {
		// Path 必填且必须能编译为有效正则
		if p.Path == "" {
			return errors.New("location_type 为 regex/regex_caseless 时，path 必填")
		}
		// 使用 regexp.Compile 验证正则语法有效性
		if _, err := regexp.Compile(p.Path); err != nil {
			return fmt.Errorf("location_type 为 '%s' 时，path '%s' 不是有效正则: %w", p.LocationType, p.Path, err)
		}
	}

	// 当 location_type 为 named 时，验证 location_name 必填
	if p.LocationType == "named" {
		if p.LocationName == "" {
			return errors.New("location_type 为 named 时，location_name 必填")
		}
	}

	return nil
}

// validateSSL 验证 SSL 配置。
//
// 检查 SSL 证书、私钥、TLS 协议版本和加密套件的有效性。
// 同时验证 HTTP/2 配置的有效性。
//
// 参数：
//   - s: SSL 配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
//
// 验证规则：
//   - cert 和 key 必须同时配置或同时为空
//   - TLS 协议仅允许 TLSv1.2 和 TLSv1.3
//   - 拒绝不安全的加密套件（RC4、DES、3DES、CBC）
//   - HTTP/2 配置仅在配置了 SSL 时生效
func validateSSL(s *SSLConfig) error {
	// 是否具备 TLS 能力：静态证书或 ACME 自动证书
	hasTLS := (s.Cert != "" && s.Key != "") || s.ACME.Enabled

	// 验证 HTTP/2 配置
	if err := validateHTTP2(&s.HTTP2, hasTLS); err != nil {
		return fmt.Errorf("http2: %w", err)
	}

	// 证书和私钥必须同时配置
	if (s.Cert == "") != (s.Key == "") {
		return errors.New("cert 和 key 必须同时配置")
	}

	// 未配置静态证书时跳过后续校验；启用 ACME 时证书由 CA 动态签发
	if s.Cert == "" && s.Key == "" {
		return nil
	}

	// 验证 TLS 版本
	for _, proto := range s.Protocols {
		if proto == "TLSv1.0" || proto == "TLSv1.1" {
			return fmt.Errorf("不安全的 TLS 版本: %s（仅允许 TLSv1.2 和 TLSv1.3）", proto)
		}
		if proto != "TLSv1.2" && proto != "TLSv1.3" {
			return fmt.Errorf("未知的 TLS 版本: %s", proto)
		}
	}

	// 验证加密套件（拒绝不安全的）
	insecureCiphers := []string{"RC4", "DES", "3DES", "CBC"}
	for _, cipher := range s.Ciphers {
		for _, insecure := range insecureCiphers {
			if strings.Contains(cipher, insecure) {
				return fmt.Errorf("不安全的加密套件: %s", cipher)
			}
		}
	}

	return nil
}

// validateACME 验证 ACME 自动证书配置。
//
// 校验挑战类型、目录 URL、EAB 配对以及申请域名的可推导性。
// ACME 的域名来源优先顺序与运行时一致：hosts 显式配置 > server_names > name。
//
// 参数：
//   - a: ACME 配置对象
//   - serverNames: 服务器配置的 server_names 列表
//   - name: 服务器名称（无 server_names 时作为域名来源）
//
// 返回值：
//   - error: 验证失败时返回具体错误信息，成功返回 nil
//
// 验证规则：
//   - challenge 仅允许 tls-alpn-01 或 http-01（空值等价于 tls-alpn-01）
//   - directory 非空时必须是合法的 http(s) URL
//   - eab_kid 和 eab_hmac_key 必须同时配置或同时为空
//   - 必须能从 hosts、server_names 或 name 中推导出至少一个域名
func validateACME(a *ACMEConfig, serverNames []string, name string) error {
	if !a.Enabled {
		return nil
	}

	// 挑战类型
	validChallenges := []string{"", ACMEChallengeTLSALPN01, ACMEChallengeHTTP01}
	if err := ValidateEnum(a.Challenge, validChallenges, "challenge"); err != nil {
		return fmt.Errorf("无效的 challenge: %s（仅支持 %s 或 %s）",
			a.Challenge, ACMEChallengeTLSALPN01, ACMEChallengeHTTP01)
	}

	// 目录 URL
	if a.Directory != "" {
		u, err := url.Parse(a.Directory)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("无效的 directory URL: %s", a.Directory)
		}
	}

	// EAB 必须成对配置
	if (a.EABKid == "") != (a.EABHmacKey == "") {
		return errors.New("eab_kid 和 eab_hmac_key 必须同时配置")
	}

	// 域名来源缺失时无法申请证书
	if len(a.ResolveHosts(serverNames, name)) == 0 {
		return errors.New("启用 ACME 时必须配置 hosts 或 server_names")
	}

	return ValidateNoNullByte(a.StatePath, "state_path")
}

// validateSecurity 验证安全配置。
//
// 验证访问控制、认证、速率限制和安全头部的有效性。
//
// 参数：
//   - s: 安全配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
func validateSecurity(s *SecurityConfig) error {
	if err := validateAccess(&s.Access); err != nil {
		return fmt.Errorf("access: %w", err)
	}

	if err := validateAuth(&s.Auth); err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	if err := validateRateLimit(&s.RateLimit); err != nil {
		return fmt.Errorf("rate_limit: %w", err)
	}

	if err := validateSecurityHeaders(&s.Headers); err != nil {
		return fmt.Errorf("headers: %w", err)
	}

	if err := validateCORS(&s.CORS); err != nil {
		return fmt.Errorf("cors: %w", err)
	}

	return nil
}

func validateCORS(c *CORSConfig) error {
	if !c.Enabled {
		return nil
	}

	if len(c.AllowedOrigins) == 0 {
		return errors.New("启用 CORS 时必须配置 allowed_origins")
	}

	hasWildcard := false
	for _, o := range c.AllowedOrigins {
		if o == "*" {
			hasWildcard = true
			break
		}
	}

	if hasWildcard && c.AllowCredentials {
		return errors.New("allowed_origins 包含 \"*\" 时不能同时启用 allow_credentials（CORS 规范不允许）")
	}

	if c.MaxAge < 0 {
		return errors.New("max_age 不能为负数")
	}

	return nil
}

// validateAccess 验证访问控制配置。
//
// 检查允许和拒绝列表中的 CIDR/IP 格式，以及默认动作的有效性。
//
// 参数：
//   - a: 访问控制配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
//
// 验证规则：
//   - allow 和 deny 列表中的项必须是有效的 CIDR 或 IP 地址
//   - default 动作仅允许 "allow" 或 "deny"
func validateAccess(a *AccessConfig) error {
	// 验证 CIDR 格式
	for _, cidr := range a.Allow {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			// 尝试作为单个 IP 解析
			if ip := net.ParseIP(cidr); ip == nil {
				return fmt.Errorf("无效的 allow CIDR/IP: %s", cidr)
			}
		}
	}

	for _, cidr := range a.Deny {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			if ip := net.ParseIP(cidr); ip == nil {
				return fmt.Errorf("无效的 deny CIDR/IP: %s", cidr)
			}
		}
	}

	// 验证默认动作
	if a.Default != "" && a.Default != "allow" && a.Default != "deny" {
		return fmt.Errorf("无效的 default 动作: %s（仅允许 allow 或 deny）", a.Default)
	}

	// 验证 GeoIP 配置
	if err := validateGeoIP(&a.GeoIP); err != nil {
		return fmt.Errorf("geoip: %w", err)
	}

	return nil
}

// validateGeoIP 验证 GeoIP 配置。
//
// 检查 GeoIP 数据库路径、国家代码格式、缓存设置等。
//
// 参数：
//   - g: GeoIP 配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
func validateGeoIP(g *GeoIPConfig) error {
	// 未启用时跳过验证
	if !g.Enabled {
		return nil
	}

	// 验证数据库路径
	if g.Database == "" {
		return errors.New("database 是必填项（启用 GeoIP 时）")
	}

	// 验证国家代码格式 (ISO 3166-1 alpha-2)
	for _, c := range g.AllowCountries {
		if !isValidCountryCode(c) {
			return fmt.Errorf("无效的 allow_countries 国家代码: %s（应为 2 位大写字母）", c)
		}
	}
	for _, c := range g.DenyCountries {
		if !isValidCountryCode(c) {
			return fmt.Errorf("无效的 deny_countries 国家代码: %s（应为 2 位大写字母）", c)
		}
	}

	// 验证 PrivateIPBehavior
	validBehaviors := []string{"", "allow", "deny", "bypass"}
	if !slices.Contains(validBehaviors, g.PrivateIPBehavior) {
		return fmt.Errorf("无效的 private_ip_behavior: %s（仅支持 allow, deny, bypass）", g.PrivateIPBehavior)
	}

	// 验证缓存大小
	if err := ValidateNonNegative(g.CacheSize, "cache_size"); err != nil {
		return err
	}

	// 验证缓存 TTL
	if err := ValidateNonNegativeDuration(g.CacheTTL, "cache_ttl"); err != nil {
		return err
	}

	// 验证默认动作
	if g.Default != "" && g.Default != "allow" && g.Default != "deny" {
		return fmt.Errorf("无效的 default 动作: %s（仅允许 allow 或 deny）", g.Default)
	}

	return nil
}

// isValidCountryCode 验证 ISO 3166-1 alpha-2 国家代码。
//
// 参数：
//   - code: 国家代码字符串
//
// 返回值：
//   - bool: true 表示有效的国家代码
func isValidCountryCode(code string) bool {
	if len(code) != 2 {
		return false
	}
	for _, c := range code {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// validateAuth 验证认证配置。
//
// 检查认证类型、哈希算法和用户列表的有效性。
//
// 参数：
//   - a: 认证配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
//
// 验证规则：
//   - type 目前仅支持 "basic"
//   - algorithm 仅支持 bcrypt 或 argon2id
//   - 启用认证时至少需要一个用户
func validateAuth(a *AuthConfig) error {
	// 未配置认证时跳过
	if a.Type == "" {
		return nil
	}

	// 仅支持 basic 认证
	if a.Type != "basic" {
		return fmt.Errorf("不支持的认证类型: %s（仅支持 basic）", a.Type)
	}

	// 启用 Basic Auth 时检查是否强制 HTTPS
	// 注意：SSL 配置在 ServerConfig 中，这里无法直接检查
	// 需要在上层验证中检查 SSL 与 Auth 的关联
	_ = a.RequireTLS // 避免空分支警告

	// 验证哈希算法
	validAlgorithms := []string{"", "bcrypt", "argon2id"}
	if err := ValidateEnum(a.Algorithm, validAlgorithms, "哈希算法"); err != nil {
		return fmt.Errorf("不支持的哈希算法: %s（仅支持 bcrypt 或 argon2id）", a.Algorithm)
	}

	// 至少需要一个用户
	if len(a.Users) == 0 {
		return errors.New("启用认证时至少需要一个用户")
	}

	// 验证每个用户
	for i, u := range a.Users {
		if u.Name == "" {
			return fmt.Errorf("users[%d].name 必填", i)
		}
		if u.Password == "" {
			return fmt.Errorf("users[%d].password 必填", i)
		}
	}

	// 验证密码最小长度配置合理性
	if a.MinPasswordLength > 0 && a.MinPasswordLength < 6 {
		return fmt.Errorf("min_password_length 建议至少为 6")
	}
	if a.MinPasswordLength > 128 {
		return fmt.Errorf("min_password_length 上限为 128")
	}

	return nil
}

// validateRateLimit 验证速率限制配置。
//
// 检查请求速率、突发容量和连接限制的有效性。
//
// 参数：
//   - r: 速率限制配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
//
// 验证规则：
//   - request_rate、burst、conn_limit 不能为负数
//   - key 仅支持 "ip" 或 "header"
func validateRateLimit(r *RateLimitConfig) error {
	// 未配置时跳过
	if r.RequestRate == 0 && r.ConnLimit == 0 {
		return nil
	}

	// 验证速率限制值
	if err := ValidateNonNegative(r.RequestRate, "request_rate"); err != nil {
		return err
	}
	if err := ValidateNonNegative(r.Burst, "burst"); err != nil {
		return err
	}
	if err := ValidateNonNegative(r.ConnLimit, "conn_limit"); err != nil {
		return err
	}

	// 验证 key 来源
	validKeys := []string{"", "ip", "header"}
	if err := ValidateEnum(r.Key, validKeys, "key 来源"); err != nil {
		return fmt.Errorf("无效的 key 来源: %s（仅支持 ip 或 header）", r.Key)
	}

	// 验证限流算法
	validAlgorithms := []string{"", "token_bucket", "sliding_window"}
	if err := ValidateEnum(r.Algorithm, validAlgorithms, "限流算法"); err != nil {
		return fmt.Errorf("无效的限流算法: %s（仅支持 token_bucket 或 sliding_window）", r.Algorithm)
	}

	// 验证滑动窗口模式
	validModes := []string{"", "approximate", "precise"}
	if err := ValidateEnum(r.SlidingWindowMode, validModes, "滑动窗口模式"); err != nil {
		return fmt.Errorf("无效的滑动窗口模式: %s（仅支持 approximate 或 precise）", r.SlidingWindowMode)
	}

	return nil
}

// validateHTTP2 验证 HTTP/2 配置。
//
// 检查 HTTP/2 配置的有效性，包括并发流数量和头部大小限制。
//
// 参数：
//   - h: HTTP/2 配置对象
//   - hasSSL: 是否配置了 SSL/TLS
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
//
// 验证规则：
//   - http2.enabled 仅在配置了 SSL 时生效（HTTP/2 over TLS）
//   - max_concurrent_streams 必须大于 0
//   - max_header_list_size 必须是一个有效的字节大小（如 "16KB", "1MB"）或空
func validateHTTP2(h *HTTP2Config, hasSSL bool) error {
	// HTTP/2 配置在 HTTPS 下才有效（除非启用 H2C）
	if h.Enabled && !hasSSL && !h.H2CEnabled {
		// HTTP/2 需要 TLS（h2），明文 HTTP/2（h2c）需要单独启用
		return errors.New("HTTP/2 需要配置 SSL/TLS 证书（http2.enabled 仅在配置 SSL 时生效，或启用 h2c_enabled）")
	}

	// 验证并发流数量
	if err := ValidateNonNegative(h.MaxConcurrentStreams, "max_concurrent_streams"); err != nil {
		return err
	}

	// 验证头部大小限制
	if err := ValidateNonNegative(h.MaxHeaderListSize, "max_header_list_size"); err != nil {
		return err
	}

	// 验证空闲超时
	if err := ValidateNonNegativeDuration(h.IdleTimeout, "idle_timeout"); err != nil {
		return err
	}

	return nil
}

// validateCompression 验证压缩配置。
//
// 检查压缩类型、压缩级别和最小压缩大小的有效性。
//
// 参数：
//   - c: 压缩配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
//
// 验证规则：
//   - type 仅支持 gzip、brotli 或 both
//   - level 范围为 0-9
//   - min_size 不能为负数
func validateCompression(c *CompressionConfig) error {
	// 未配置时跳过
	if c.Type == "" {
		return nil
	}

	// 验证压缩类型
	validTypes := []string{"gzip", "brotli", "both"}
	if err := ValidateEnum(c.Type, validTypes, "压缩类型"); err != nil {
		return fmt.Errorf("无效的压缩类型: %s（仅支持 gzip, brotli 或 both）", c.Type)
	}

	// 验证压缩级别
	if c.Level < 0 || c.Level > 9 {
		return fmt.Errorf("无效的压缩级别: %d（范围 0-9）", c.Level)
	}

	// 验证最小压缩大小
	if err := ValidateNonNegative(c.MinSize, "min_size"); err != nil {
		return err
	}

	return nil
}

// validateRewrite 验证 URL 重写规则。
//
// 检查重写模式、替换目标和标志的有效性。
//
// 参数：
//   - r: 重写规则配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
//
// 验证规则：
//   - pattern 必填
//   - flag 仅允许 last, redirect, permanent, break
func validateRewrite(r *RewriteRule) error {
	// 模式必填
	if r.Pattern == "" {
		return errors.New("pattern 必填")
	}

	// 验证标志
	validFlags := []string{"", "last", "redirect", "permanent", "break"}
	if !slices.Contains(validFlags, r.Flag) {
		return fmt.Errorf("无效的 flag: %s（仅支持 last, redirect, permanent, break）", r.Flag)
	}

	return nil
}

// validateLogging 验证日志配置。
//
// 检查日志格式和级别的有效性。
//
// 参数：
//   - l: 日志配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
//
// 验证规则：
//   - format 仅允许 text 或 json
//   - level 仅允许 debug, info, warn, error
func validateLogging(l *LoggingConfig) error {
	// 验证日志格式
	validFormats := []string{"", "text", "json"}
	if err := ValidateEnum(l.Format, validFormats, "日志格式"); err != nil {
		return fmt.Errorf("无效的日志格式: %s（仅支持 text 或 json）", l.Format)
	}

	// 验证错误日志级别
	validLevels := []string{"", "debug", "info", "warn", "error"}
	if err := ValidateEnum(l.Error.Level, validLevels, "日志级别"); err != nil {
		return fmt.Errorf("无效的日志级别: %s（仅支持 debug, info, warn, error）", l.Error.Level)
	}

	return nil
}

// validateSecurityHeaders 验证安全头部配置。
//
// 检查各安全头部值的有效性。
//
// 参数：
//   - h: 安全头部配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
//
// 验证规则：
//   - x_frame_options 仅允许 DENY, SAMEORIGIN 或空
//   - referrer_policy 仅允许标准 RFC 值
func validateSecurityHeaders(h *SecurityHeaders) error {
	// 验证 X-Frame-Options
	validFrameOptions := []string{"", "DENY", "SAMEORIGIN"}
	if !slices.Contains(validFrameOptions, h.XFrameOptions) {
		return fmt.Errorf("无效的 x_frame_options: %s（仅支持 DENY, SAMEORIGIN 或空）", h.XFrameOptions)
	}

	// 验证 Referrer-Policy
	validReferrerPolicies := []string{
		"", "no-referrer", "no-referrer-when-downgrade", "origin",
		"origin-when-cross-origin", "same-origin", "strict-origin",
		"strict-origin-when-cross-origin", "unsafe-url",
	}
	if !slices.Contains(validReferrerPolicies, h.ReferrerPolicy) {
		return fmt.Errorf("无效的 referrer_policy: %s", h.ReferrerPolicy)
	}

	return nil
}

// validateStream 验证 Stream 代理配置。
//
// 检查监听地址、协议类型和上游配置的有效性。
//
// 参数：
//   - s: Stream 配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
//
// 验证规则：
//   - listen 必填
//   - protocol 仅允许 tcp 或 udp
//   - upstream.targets 至少需要一个目标
func validateStream(s *StreamConfig) error {
	// 监听地址必填
	if s.Listen == "" {
		return errors.New("listen 地址必填")
	}

	// 验证协议类型
	if s.Protocol != "tcp" && s.Protocol != "udp" {
		return fmt.Errorf("无效的协议类型: %s（仅允许 tcp 或 udp）", s.Protocol)
	}

	// 验证上游目标
	if len(s.Upstream.Targets) == 0 {
		return errors.New("upstream.targets 至少需要一个目标地址")
	}

	// 验证每个目标地址
	for i, t := range s.Upstream.Targets {
		if t.Addr == "" {
			return fmt.Errorf("upstream.targets[%d].addr 必填", i)
		}
	}

	// 验证负载均衡算法
	if !loadbalance.IsValidAlgorithm(s.Upstream.LoadBalance) {
		return fmt.Errorf("无效的负载均衡算法：%s", s.Upstream.LoadBalance)
	}

	return nil
}

// validatePerformance 验证性能配置。
//
// 检查性能配置中的废弃选项和潜在问题，输出警告信息。
//
// 参数：
//   - p: 性能配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
func validatePerformance(p *PerformanceConfig) error {
	// 检查 Transport 配置（可能导致性能问题）
	if err := ValidateNonNegative(p.Transport.MaxConnsPerHost, "transport.max_conns_per_host"); err != nil {
		return err
	}

	return nil
}

// validateNextUpstream 验证故障转移配置。
//
// 检查重试次数和 HTTP 状态码的有效性。
//
// 参数：
//   - n: 故障转移配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
//
// 验证规则：
//   - tries 不能为负数，建议不超过后端数量
//   - http_codes 应包含有效的 HTTP 状态码
func validateNextUpstream(n *NextUpstreamConfig) error {
	// 未配置时跳过
	if n.Tries == 0 && len(n.HTTPCodes) == 0 {
		return nil
	}

	// 验证重试次数
	if err := ValidateNonNegative(n.Tries, "tries"); err != nil {
		return err
	}

	// 验证 HTTP 状态码
	for i, code := range n.HTTPCodes {
		if code < 100 || code > 599 {
			return fmt.Errorf("http_codes[%d]: 无效的 HTTP 状态码 %d", i, code)
		}
	}

	return nil
}

// validateVariables 验证自定义变量配置。
//
// 检查变量名的有效性和冲突情况。
//
// 参数：
//   - v: 变量配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
//
// 验证规则：
//   - 变量名不能为空
//   - 变量名只允许字母、数字、下划线
//   - 变量名不能以 arg_、http_、cookie_ 开头（动态变量前缀）
//   - 变量名不能与内置变量冲突
func validateVariables(v *VariablesConfig) error {
	for name := range v.Set {
		// 检查变量名非空
		if name == "" {
			return errors.New("变量名不能为空")
		}

		// 变量名只允许字母、数字、下划线
		for i, c := range name {
			isLetter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
			isDigit := c >= '0' && c <= '9'
			isUnderscore := c == '_'
			if !isLetter && !isDigit && !isUnderscore {
				return fmt.Errorf("变量名 '%s' 包含非法字符（位置 %d）", name, i)
			}
		}

		// 检查动态变量前缀冲突
		if strings.HasPrefix(name, "arg_") || strings.HasPrefix(name, "http_") || strings.HasPrefix(name, "cookie_") {
			return fmt.Errorf("变量名 '%s' 与动态变量前缀冲突（arg_, http_, cookie_）", name)
		}

		// 禁止覆盖内置变量
		if variable.GetBuiltin(name) != nil {
			return fmt.Errorf("变量名 '%s' 与内置变量冲突", name)
		}
	}
	return nil
}

// validateLua 验证 Lua 中间件配置。
//
// 检查 Lua 脚本配置的有效性，包括脚本路径、执行阶段和全局设置。
//
// 参数：
//   - l: Lua 配置对象
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
//
// 验证规则：
//   - scripts[].path 必填
//   - scripts[].phase 和 scripts[].route 互斥，只能设置其一
//   - scripts[].phase 必须是有效阶段（当设置时）
//   - scripts[].route_type 必须是有效类型（当设置 route 时）
//   - scripts[].route 为 regex 类型时必须能编译为有效正则
//   - global_settings.max_concurrent_coroutines 必须 >= 1
func validateLua(l *LuaMiddlewareConfig) error {
	// 未配置时跳过
	if l == nil || !l.Enabled {
		return nil
	}

	// 验证脚本配置
	for i, script := range l.Scripts {
		if script.Path == "" {
			return fmt.Errorf("scripts[%d].path 必填", i)
		}

		// Route 和 Phase 互斥检查
		if script.Route != "" && script.Phase != "" {
			return fmt.Errorf("scripts[%d]: route 和 phase 互斥，只能设置其一", i)
		}

		// 验证阶段值（当设置 phase 时）
		if script.Phase != "" {
			validPhases := []string{"rewrite", "access", "content", "log", "header_filter", "body_filter"}
			if !slices.Contains(validPhases, script.Phase) {
				return fmt.Errorf("scripts[%d].phase 无效: %s（仅支持 rewrite, access, content, log, header_filter, body_filter）", i, script.Phase)
			}
		}

		// 验证路由配置（当设置 route 时）
		if script.Route != "" {
			// 验证 route_type 枚举值
			validRouteTypes := []string{"", "exact", "prefix", "prefix_priority", "regex", "regex_caseless"}
			if !slices.Contains(validRouteTypes, script.RouteType) {
				return fmt.Errorf("scripts[%d].route_type 无效: %s（仅支持 exact, prefix, prefix_priority, regex, regex_caseless）", i, script.RouteType)
			}

			// 当 route_type 为 regex 类型时，验证正则有效性
			if script.RouteType == "regex" || script.RouteType == "regex_caseless" {
				if _, err := regexp.Compile(script.Route); err != nil {
					return fmt.Errorf("scripts[%d].route '%s' 不是有效正则: %w", i, script.Route, err)
				}
			}
		}

		// 超时时间验证
		if err := ValidateNonNegativeDuration(script.Timeout, fmt.Sprintf("scripts[%d].timeout", i)); err != nil {
			return err
		}
	}

	// 验证全局设置
	if err := ValidateNonNegative(l.GlobalSettings.MaxConcurrentCoroutines, "global_settings.max_concurrent_coroutines"); err != nil {
		return err
	}
	if l.GlobalSettings.MaxConcurrentCoroutines > 0 && l.GlobalSettings.MaxConcurrentCoroutines < 1 {
		return errors.New("global_settings.max_concurrent_coroutines 至少为 1")
	}
	if err := ValidateNonNegativeDuration(l.GlobalSettings.CoroutineTimeout, "global_settings.coroutine_timeout"); err != nil {
		return err
	}
	if err := ValidateNonNegative(l.GlobalSettings.CodeCacheSize, "global_settings.code_cache_size"); err != nil {
		return err
	}
	if err := ValidateNonNegativeDuration(l.GlobalSettings.MaxExecutionTime, "global_settings.max_execution_time"); err != nil {
		return err
	}

	return nil
}

// validateRedirectRewrite 验证 redirect_rewrite 配置。
//
// 检查模式有效性、规则完整性和正则表达式编译。
//
// 参数：
//   - cfg: redirect_rewrite 配置对象（nil 时跳过，启用 default 模式）
//
// 返回值：
//   - error: 验证失败时返回错误信息，成功返回 nil
//
// 验证规则：
//   - Mode 仅允许 ""、"default"、"off"、"custom"
//   - custom 模式必须配置至少一条规则
//   - 规则的 pattern 不能为空
//   - 正则模式（~ 前缀）必须能成功编译
func validateRedirectRewrite(cfg *RedirectRewriteConfig) error {
	if cfg == nil {
		return nil // 未配置时默认启用 default 模式
	}

	// Mode 验证
	validModes := []string{"", "default", "off", "custom"}
	if !slices.Contains(validModes, cfg.Mode) {
		return errors.New("redirect_rewrite.mode must be one of: default, off, custom")
	}

	// custom 模式必须有规则
	if cfg.Mode == "custom" && len(cfg.Rules) == 0 {
		return errors.New("redirect_rewrite.rules required when mode is custom")
	}

	// 验证每条规则
	for i, rule := range cfg.Rules {
		if rule.Pattern == "" {
			return fmt.Errorf("redirect_rewrite.rules[%d].pattern cannot be empty", i)
		}

		// 正则模式预编译检查
		if strings.HasPrefix(rule.Pattern, "~") {
			patternStr := rule.Pattern[1:] // 去掉 ~ 前缀
			if strings.HasPrefix(rule.Pattern, "~*") {
				patternStr = rule.Pattern[2:] // 去掉 ~* 前缀（大小写不敏感）
			}
			if _, err := regexp.Compile(patternStr); err != nil {
				return fmt.Errorf("redirect_rewrite.rules[%d].pattern invalid regex: %w", i, err)
			}
		}
	}

	return nil
}
