// Package config 提供 YAML 配置文件的解析、验证和默认配置生成功能。
//
// 该文件包含根配置结构体和加载/保存功能，包括：
//   - 根配置结构体
//   - 配置文件的加载、保存和验证方法
//
// 主要用途：
//
//	用于定义和管理服务器的完整配置，支持单服务器和多虚拟主机两种模式。
//
// 注意事项：
//   - 配置文件使用 YAML 格式
//   - 所有配置项都有合理的默认值
//   - 配置加载后会自动验证
//
// 作者：xfy
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// 默认配置常量。
const (
	// DefaultPprofPath pprof 端点的默认路径。
	DefaultPprofPath = "/debug/pprof"
)

// ServerMode 服务器运行模式类型。
//
// 定义服务器的工作模式，支持显式配置或自动推断。
type ServerMode string

// ServerMode 枚举值。
const (
	// ServerModeSingle 单服务器模式 - 只运行一个服务器实例。
	ServerModeSingle ServerMode = "single"
	// ServerModeVHost 虚拟主机模式 - 多个服务器共享相同的监听地址。
	ServerModeVHost ServerMode = "vhost"
	// ServerModeMultiServer 多服务器模式 - 多个服务器监听不同的地址。
	ServerModeMultiServer ServerMode = "multi_server"
	// ServerModeAuto 自动模式 - 根据配置自动推断运行模式。
	ServerModeAuto ServerMode = "auto"
)

// Config 根配置结构，支持单服务器和多虚拟主机两种模式。
//
// 包含服务器配置、日志配置、性能配置和监控配置等模块。
// 是配置文件的顶级结构体，所有其他配置都作为其子结构。
//
// 注意事项：
//   - 必须配置 servers 列表中的至少一个
//   - 加载后会自动进行配置验证
//   - Stream 配置为可选，用于 TCP/UDP 层代理
//   - HTTP/3 配置为可选，需 SSL 配置配合才能生效
//
// 使用示例：
//
//	cfg, err := config.Load("config.yaml")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	// 使用多虚拟主机模式
//	for _, s := range cfg.Servers {
//	    // 处理每个服务器配置
//	}
type Config struct {
	Mode        ServerMode        `yaml:"mode"`
	Variables   VariablesConfig   `yaml:"variables"`
	Logging     LoggingConfig     `yaml:"logging"`
	Servers     []ServerConfig    `yaml:"servers"`
	Stream      []StreamConfig    `yaml:"stream"`
	Monitoring  MonitoringConfig  `yaml:"monitoring"`
	HTTP3       HTTP3Config       `yaml:"http3"`
	Resolver    ResolverConfig    `yaml:"resolver"`
	Performance PerformanceConfig `yaml:"performance"`
	Shutdown    ShutdownConfig    `yaml:"shutdown"`
	Include     []IncludeConfig   `yaml:"include"` // 配置引入，支持从其他文件引入配置片段
}

// parseSize 解析大小字符串（支持 k, m 单位）。
func parseSize(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, strconv.ErrSyntax
	}

	// 提取单位
	unit := strings.ToLower(s[len(s)-1:])
	multiplier := 1
	numStr := s

	switch unit {
	case "k":
		multiplier = 1024
		numStr = s[:len(s)-1]
	case "m":
		multiplier = 1024 * 1024
		numStr = s[:len(s)-1]
	}

	value, err := strconv.Atoi(numStr)
	if err != nil {
		return 0, err
	}

	return value * multiplier, nil
}

// Load 从文件加载配置。
//
// 读取指定路径的 YAML 配置文件，解析并验证配置内容。
//
// 参数：
//   - path: 配置文件路径
//
// 返回值：
//   - *Config: 解析后的配置对象
//   - error: 读取、解析或验证失败时的错误信息
//
// 注意事项：
//   - 加载后会自动调用 Validate 进行配置验证
//   - 文件不存在或格式错误都会返回错误
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	data = ExpandEnv(data)

	// 从默认值开始，YAML 只覆盖显式配置的字段。
	// 注意：yaml.v3 对 slice 会整体替换，因此用户显式配置的 Servers[]
	// 元素不会继承 server-level 默认值；但顶层 struct 字段（Performance、
	// Monitoring、Resolver）的默认值会被保留。
	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	if len(cfg.Include) > 0 {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("获取配置文件绝对路径失败: %w", err)
		}
		visited := map[string]bool{absPath: true}
		if err := processIncludes(cfg, filepath.Dir(path), 0, visited); err != nil {
			return nil, fmt.Errorf("处理配置引入失败: %w", err)
		}
	}

	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("配置验证失败: %w", err)
	}

	return cfg, nil
}

const maxIncludeDepth = 10

func processIncludes(cfg *Config, baseDir string, depth int, visited map[string]bool) error {
	if depth >= maxIncludeDepth {
		return fmt.Errorf("配置引入嵌套深度超过 %d 层", maxIncludeDepth)
	}

	for _, inc := range cfg.Include {
		pattern := inc.Path
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(baseDir, pattern)
		}

		matches, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("展开引入路径 %q 失败: %w", inc.Path, err)
		}
		if len(matches) == 0 {
			return fmt.Errorf("引入路径 %q 未匹配到任何文件", inc.Path)
		}

		for _, match := range matches {
			absMatch, err := filepath.Abs(match)
			if err != nil {
				return fmt.Errorf("获取引入文件绝对路径失败 %q: %w", match, err)
			}
			if visited[absMatch] {
				return fmt.Errorf("检测到循环引入: %s", absMatch)
			}
			visited[absMatch] = true

			info, err := os.Stat(match)
			if err != nil {
				return fmt.Errorf("读取引入文件 %q 失败: %w", match, err)
			}
			if info.IsDir() {
				delete(visited, absMatch)
				continue
			}

			data, err := os.ReadFile(match)
			if err != nil {
				return fmt.Errorf("读取引入文件 %q 失败: %w", match, err)
			}

			data = ExpandEnv(data)

			var included Config
			if err := yaml.Unmarshal(data, &included); err != nil {
				return fmt.Errorf("解析引入文件 %q 失败: %w", match, err)
			}

			if len(included.Include) > 0 {
				if err := processIncludes(&included, filepath.Dir(match), depth+1, visited); err != nil {
					return err
				}
			}

			cfg.Servers = append(cfg.Servers, included.Servers...)
			cfg.Stream = append(cfg.Stream, included.Stream...)
			for k, v := range included.Variables.Set {
				if _, exists := cfg.Variables.Set[k]; !exists {
					if cfg.Variables.Set == nil {
						cfg.Variables.Set = make(map[string]string)
					}
					cfg.Variables.Set[k] = v
				}
			}

			delete(visited, absMatch)
		}
	}

	cfg.Include = nil
	return nil
}

// HasServers 检查是否为多虚拟主机模式。
//
// 返回值：
//   - bool: 如果配置了 servers 列表且非空，返回 true
func (c *Config) HasServers() bool {
	return len(c.Servers) > 0
}

// GetDefaultServerFromList 从 servers 列表中获取默认服务器配置。
//
// 遍历 servers 列表，返回第一个 Default 标记为 true 的服务器。
// 用于在虚拟主机模式下获取默认服务器的配置作为 fallback。
//
// 返回值：
//   - *ServerConfig: 默认服务器配置，如无则返回 nil
func (c *Config) GetDefaultServerFromList() *ServerConfig {
	for i := range c.Servers {
		if c.Servers[i].Default {
			return &c.Servers[i]
		}
	}
	return nil
}

// GetMode 获取服务器运行模式。
//
// 如果 Mode 显式设置（非 auto），返回设置的值。
// 如果 Mode 是 auto 或未设置，根据配置自动推断：
//   - servers 数量 == 1 → single
//   - servers 数量 > 1 且所有 listen 地址相同 → vhost
//   - servers 数量 > 1 且 listen 地址不同 → multi_server
//
// 返回值：
//   - ServerMode: 推断后的服务器运行模式
func (c *Config) GetMode() ServerMode {
	// 如果显式设置了非 auto 模式，直接返回
	if c.Mode != "" && c.Mode != ServerModeAuto {
		return c.Mode
	}

	// 自动推断模式
	serverCount := len(c.Servers)

	// servers 为空 → auto（配置验证会确保至少有一个服务器）
	if serverCount == 0 {
		return ServerModeAuto
	}

	// servers 数量 == 1 → single
	if serverCount == 1 {
		return ServerModeSingle
	}

	// servers 数量 > 1，检查 listen 地址
	firstListen := c.Servers[0].Listen
	allSameListen := true
	for i := 1; i < serverCount; i++ {
		if c.Servers[i].Listen != firstListen {
			allSameListen = false
			break
		}
	}

	// 所有 listen 地址相同 → vhost，否则 → multi_server
	if allSameListen {
		return ServerModeVHost
	}
	return ServerModeMultiServer
}

// Validate 配置验证入口。
//
// 验证配置的完整性和有效性，检查是否至少配置了一个服务器，
// 并递归验证所有服务器配置。
//
// 参数：
//   - cfg: 配置对象
//
// 返回值：
//   - error: 验证失败时的错误信息，包含具体字段路径
//
// 验证规则：
//   - 必须配置 servers 数组且至少包含一个服务器
//   - 所有服务器配置必须通过 validateServer 验证
func Validate(cfg *Config) error {
	// 必须配置 servers 且至少包含一个服务器
	if !cfg.HasServers() {
		return errors.New("必须配置 servers 且至少包含一个服务器")
	}

	// 验证模式
	if err := validateMode(cfg.Mode); err != nil {
		return err
	}

	// 验证模式对服务器数量和监听地址的约束
	if err := validateModeConstraints(cfg); err != nil {
		return err
	}

	// 验证监听分组内的协议与主机名约束
	if err := validateListenGroups(cfg.Servers); err != nil {
		return err
	}

	// 验证 default 服务器在各监听分组内唯一
	if err := validateDefaultServer(cfg.Servers); err != nil {
		return err
	}

	// 验证所有服务器
	for i := range cfg.Servers {
		if err := validateServer(&cfg.Servers[i], false); err != nil {
			return fmt.Errorf("servers[%d]: %w", i, err)
		}
	}

	// 验证 Stream 配置
	for i := range cfg.Stream {
		if err := validateStream(&cfg.Stream[i]); err != nil {
			return fmt.Errorf("stream[%d]: %w", i, err)
		}
	}

	// 验证日志配置
	if err := validateLogging(&cfg.Logging); err != nil {
		return err
	}

	// 验证性能配置
	if err := validatePerformance(&cfg.Performance); err != nil {
		return fmt.Errorf("performance: %w", err)
	}

	// 验证 Resolver 配置
	if err := cfg.Resolver.Validate(); err != nil {
		return fmt.Errorf("resolver: %w", err)
	}

	// 验证变量配置
	if err := validateVariables(&cfg.Variables); err != nil {
		return fmt.Errorf("variables: %w", err)
	}

	// 验证关闭配置
	if err := validateShutdown(&cfg.Shutdown); err != nil {
		return err
	}

	return nil
}

// validateShutdown 验证关闭配置。
func validateShutdown(cfg *ShutdownConfig) error {
	if cfg.GracefulTimeout < 0 {
		return errors.New("shutdown.graceful_timeout 不能为负数")
	}
	if cfg.FastTimeout < 0 {
		return errors.New("shutdown.fast_timeout 不能为负数")
	}
	// 0 值表示使用默认值，在应用层处理
	return nil
}
