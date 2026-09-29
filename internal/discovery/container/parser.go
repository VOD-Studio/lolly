// Package container 将容器详情转换为稳定的动态路由快照。
//
// 该文件兼容 nginx-proxy 常用环境变量，并将单个容器的配置错误隔离为问题报告。
//
// 作者：xfy
package container

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

const (
	defaultContainerPort   = 80
	defaultVirtualPath     = "/"
	defaultVirtualProtocol = "http"
	defaultHTTPSMethod     = "redirect"
)

// snapshotBuilder 跨容器聚合相同主机和路径的端点。
type snapshotBuilder struct {
	routes map[string]*Route
	issues []Issue
}

// routeSpec 是从一个容器环境中解析出的单条完整路由声明。
type routeSpec struct {
	host, path, protocol, destination         string
	port, externalHTTPPort, externalHTTPSPort int
	httpsMethod, acmeEmail                    string
	acmeHosts                                 []string
}

// multiportSpec 对应 VIRTUAL_HOST_MULTIPORTS 中一个路径的设置。
type multiportSpec struct {
	Port        *int   `yaml:"port"`
	Protocol    string `yaml:"proto"`
	Destination string `yaml:"dest"`
}

// multiportHost 对应多端口声明中的主机级设置和动态路径映射。
type multiportHost struct {
	ExternalHTTPPort  *int                     `yaml:"external_http_port"`
	ExternalHTTPSPort *int                     `yaml:"external_https_port"`
	Paths             map[string]multiportSpec `yaml:",inline"`
}

// newSnapshotBuilder 创建空的聚合器。
func newSnapshotBuilder() *snapshotBuilder {
	return &snapshotBuilder{routes: make(map[string]*Route)}
}

// issue 记录容器级问题，使其余容器仍可组成快照。
func (b *snapshotBuilder) issue(id, message string) {
	b.issues = append(b.issues, Issue{ContainerID: id, Message: message})
}

// preserve 从上一快照保留详情暂时不可用容器的路由与端点。
func (b *snapshotBuilder) preserve(snapshot Snapshot, failed map[string]struct{}) {
	for _, oldRoute := range snapshot.Routes {
		key := oldRoute.Host + "\x00" + oldRoute.Path
		for _, endpoint := range oldRoute.Endpoints {
			if _, ok := failed[endpoint.ContainerID]; !ok {
				continue
			}
			route := b.routes[key]
			if route == nil {
				copy := oldRoute
				copy.Endpoints = nil
				copy.ACMEHosts = append([]string(nil), oldRoute.ACMEHosts...)
				route = &copy
				b.routes[key] = route
			}
			route.Endpoints = append(route.Endpoints, endpoint)
		}
	}
}

// add 将一个容器详情转换并合并到快照中。
func (b *snapshotBuilder) add(id string, inspect containerInspect, networkName string) {
	environment := parseEnvironment(inspect.Config.Env)
	address := selectAddress(inspect.NetworkSettings.Networks, networkName)
	if address == "" {
		b.issue(id, "指定网络中没有可用地址")
		return
	}
	specs, err := parseRouteSpecs(environment, inspect.Config.ExposedPorts)
	if err != nil {
		b.issue(id, err.Error())
		return
	}
	for _, spec := range specs {
		key := spec.host + "\x00" + spec.path
		route := b.routes[key]
		if route == nil {
			route = &Route{
				Host: spec.host, Path: spec.path, Protocol: spec.protocol, Destination: spec.destination,
				ExternalHTTPPort: spec.externalHTTPPort, ExternalHTTPSPort: spec.externalHTTPSPort,
				HTTPSMethod: spec.httpsMethod, ACMEHosts: spec.acmeHosts, ACMEEmail: spec.acmeEmail,
			}
			b.routes[key] = route
		} else if !routeMatchesSpec(route, spec) {
			b.issue(id, fmt.Sprintf("主机 %q 路径 %q 的路由声明与已有容器冲突，已忽略", spec.host, spec.path))
			continue
		}
		route.Endpoints = append(route.Endpoints, Endpoint{ContainerID: id, Address: address, Port: spec.port})
	}
}

// routeMatchesSpec 判断可以安全共享后端池的声明是否完全一致。
func routeMatchesSpec(route *Route, spec routeSpec) bool {
	return route.Protocol == spec.protocol && route.Destination == spec.destination &&
		route.ExternalHTTPPort == spec.externalHTTPPort && route.ExternalHTTPSPort == spec.externalHTTPSPort &&
		route.HTTPSMethod == spec.httpsMethod && route.ACMEEmail == spec.acmeEmail && slices.Equal(route.ACMEHosts, spec.acmeHosts)
}

// snapshot 生成与映射遍历顺序无关的公开快照。
func (b *snapshotBuilder) snapshot() Snapshot {
	result := Snapshot{Issues: append([]Issue(nil), b.issues...)}
	for _, route := range b.routes {
		result.Routes = append(result.Routes, *route)
	}
	sortSnapshot(&result)
	return result
}

// parseEnvironment 将 KEY=VALUE 列表转换为最后一个值生效的映射。
func parseEnvironment(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		if key, item, found := strings.Cut(value, "="); found {
			result[key] = item
		}
	}
	return result
}

// selectAddress 从指定网络选择地址，并始终优先使用 IPv4。
func selectAddress(networks map[string]networkAttachment, name string) string {
	if name != "" {
		return preferredAddress(networks[name])
	}
	for _, network := range networks {
		if address := preferredAddress(network); address != "" {
			return address
		}
	}
	return ""
}

// preferredAddress 返回附件中的 IPv4，缺失时回退到 IPv6。
func preferredAddress(network networkAttachment) string {
	if net.ParseIP(network.IPAddress) != nil {
		return network.IPAddress
	}
	if net.ParseIP(network.GlobalIPv6Address) != nil {
		return network.GlobalIPv6Address
	}
	return ""
}

// parseRouteSpecs 优先解析多端口标签，否则解析传统单端口标签。
func parseRouteSpecs(environment map[string]string, exposed map[string]struct{}) ([]routeSpec, error) {
	metadata, err := routeMetadata(environment)
	if err != nil {
		return nil, err
	}
	if value := strings.TrimSpace(environment["VIRTUAL_HOST_MULTIPORTS"]); value != "" {
		return parseMultiportSpecs(value, environment, exposed, metadata)
	}
	hosts := splitComma(environment["VIRTUAL_HOST"])
	if len(hosts) == 0 {
		return nil, nil
	}
	port, err := virtualPort(environment["VIRTUAL_PORT"], exposed)
	if err != nil {
		return nil, err
	}
	path, err := normalizePath(valueOrDefault(environment["VIRTUAL_PATH"], defaultVirtualPath))
	if err != nil {
		return nil, err
	}
	protocol, err := normalizeProtocol(environment["VIRTUAL_PROTO"])
	if err != nil {
		return nil, err
	}
	result := make([]routeSpec, 0, len(hosts))
	for _, rawHost := range hosts {
		host, hostErr := normalizeHost(rawHost)
		if hostErr != nil {
			return nil, hostErr
		}
		result = append(result, makeRouteSpec(host, path, protocol, strings.TrimSpace(environment["VIRTUAL_DEST"]), port, metadata))
	}
	return result, nil
}

// parseMultiportSpecs 解析官方主机级端口与路径映射结构。
func parseMultiportSpecs(value string, environment map[string]string, exposed map[string]struct{}, metadata Route) ([]routeSpec, error) {
	var hosts map[string]multiportHost
	if err := yaml.Unmarshal([]byte(value), &hosts); err != nil {
		return nil, fmt.Errorf("解析 VIRTUAL_HOST_MULTIPORTS 失败: %w", err)
	}
	defaultPort, err := virtualPort(environment["VIRTUAL_PORT"], exposed)
	if err != nil {
		return nil, err
	}
	var result []routeSpec
	for rawHost, hostSetting := range hosts {
		hostMetadata := metadata
		host, hostErr := normalizeHost(rawHost)
		if hostErr != nil {
			return nil, hostErr
		}
		if hostSetting.ExternalHTTPPort != nil {
			if err := validatePortNumber(*hostSetting.ExternalHTTPPort, "external_http_port"); err != nil {
				return nil, err
			}
			hostMetadata.ExternalHTTPPort = *hostSetting.ExternalHTTPPort
		}
		if hostSetting.ExternalHTTPSPort != nil {
			if err := validatePortNumber(*hostSetting.ExternalHTTPSPort, "external_https_port"); err != nil {
				return nil, err
			}
			hostMetadata.ExternalHTTPSPort = *hostSetting.ExternalHTTPSPort
		}
		paths := hostSetting.Paths
		if len(paths) == 0 {
			paths = map[string]multiportSpec{defaultVirtualPath: {}}
		}
		for rawPath, setting := range paths {
			path, pathErr := normalizePath(valueOrDefault(rawPath, defaultVirtualPath))
			if pathErr != nil {
				return nil, pathErr
			}
			port := defaultPort
			if setting.Port != nil {
				port = *setting.Port
			}
			if port <= 0 || port > 65535 {
				return nil, fmt.Errorf("VIRTUAL_HOST_MULTIPORTS 中端口无效: %d", port)
			}
			protocol, protocolErr := normalizeProtocol(setting.Protocol)
			if protocolErr != nil {
				return nil, protocolErr
			}
			result = append(result, makeRouteSpec(host, path, protocol, strings.TrimSpace(setting.Destination), port, hostMetadata))
		}
	}
	return result, nil
}

// makeRouteSpec 合并路由自身设置与容器级元数据。
func makeRouteSpec(host, path, protocol, destination string, port int, metadata Route) routeSpec {
	return routeSpec{
		host: host, path: path, protocol: protocol, destination: destination, port: port,
		externalHTTPPort: metadata.ExternalHTTPPort, externalHTTPSPort: metadata.ExternalHTTPSPort,
		httpsMethod: metadata.HTTPSMethod, acmeHosts: append([]string(nil), metadata.ACMEHosts...), acmeEmail: metadata.ACMEEmail,
	}
}

// normalizeHost 规范普通 DNS 主机；当前服务端不支持 nginx server_name 通配符和正则。
func normalizeHost(value string) (string, error) {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	if host == "" || strings.ContainsAny(host, "/:\\*") || strings.HasPrefix(host, "~") {
		return "", fmt.Errorf("虚拟主机无效或当前服务端不支持: %q", value)
	}
	for _, item := range host {
		if unicode.IsControl(item) || unicode.IsSpace(item) {
			return "", fmt.Errorf("虚拟主机无效: %q", value)
		}
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", fmt.Errorf("虚拟主机无效: %q", value)
		}
		for _, item := range label {
			if !unicode.IsLetter(item) && !unicode.IsDigit(item) && item != '-' {
				return "", fmt.Errorf("虚拟主机无效: %q", value)
			}
		}
	}
	return host, nil
}

// normalizePath 校验当前服务端可执行的绝对前缀路径。
func normalizePath(value string) (string, error) {
	path := strings.TrimSpace(value)
	if strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("当前服务端不支持正则虚拟路径: %q", value)
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\r\n\x00") {
		return "", fmt.Errorf("虚拟路径无效: %q", value)
	}
	return path, nil
}

// normalizeProtocol 校验当前代理实现支持的后端协议。
func normalizeProtocol(value string) (string, error) {
	protocol := strings.ToLower(valueOrDefault(value, defaultVirtualProtocol))
	if protocol != "http" && protocol != "https" {
		return "", fmt.Errorf("不支持后端协议 %q", protocol)
	}
	return protocol, nil
}

// virtualPort 解析显式端口，未指定时仅在恰有一个暴露端口时自动选择。
func virtualPort(value string, exposed map[string]struct{}) (int, error) {
	if strings.TrimSpace(value) != "" {
		return parsePort(value)
	}
	if len(exposed) == 1 {
		for item := range exposed {
			port, _, _ := strings.Cut(item, "/")
			return parsePort(port)
		}
	}
	return defaultContainerPort, nil
}

// parsePort 将端口文本验证并转换为整数。
func parsePort(value string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("容器端口无效: %q", value)
	}
	return port, nil
}

// routeMetadata 解析容器级外部端口、HTTPS 策略与证书信息。
func routeMetadata(environment map[string]string) (Route, error) {
	var route Route
	var err error
	if route.ExternalHTTPPort, err = parseOptionalPort(environment["EXTERNAL_HTTP_PORT"]); err != nil {
		return Route{}, fmt.Errorf("EXTERNAL_HTTP_PORT 无效: %w", err)
	}
	if route.ExternalHTTPSPort, err = parseOptionalPort(environment["EXTERNAL_HTTPS_PORT"]); err != nil {
		return Route{}, fmt.Errorf("EXTERNAL_HTTPS_PORT 无效: %w", err)
	}
	route.HTTPSMethod = strings.ToLower(valueOrDefault(environment["HTTPS_METHOD"], defaultHTTPSMethod))
	if !slices.Contains([]string{"redirect", "noredirect", "nohttp", "nohttps"}, route.HTTPSMethod) {
		return Route{}, fmt.Errorf("HTTPS_METHOD 无效: %q", route.HTTPSMethod)
	}
	route.ACMEHosts = uniqueStrings(append(splitComma(environment["LETSENCRYPT_HOST"]), splitComma(environment["ACME_HOST"])...))
	route.ACMEEmail = firstNonempty(environment["LETSENCRYPT_EMAIL"], environment["ACME_EMAIL"])
	return route, nil
}

// parseOptionalPort 校验非空外部端口，空值保持未声明语义。
func parseOptionalPort(value string) (int, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	return parsePort(value)
}

// validatePortNumber 校验多端口结构中显式声明的外部端口。
func validatePortNumber(port int, name string) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("VIRTUAL_HOST_MULTIPORTS 中 %s 无效: %d", name, port)
	}
	return nil
}

// splitComma 清理逗号分隔标签并忽略空项。
func splitComma(value string) []string {
	var result []string
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

// uniqueStrings 按声明顺序去重元数据值。
func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}

// firstNonempty 返回第一个非空白值，用于兼容新旧变量别名。
func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// valueOrDefault 仅在值为空时采用默认值。
func valueOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}
