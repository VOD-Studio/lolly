// Package config 提供 YAML 配置文件的解析、验证和默认配置生成功能。
//
// 该文件定义容器发现配置，供运行时按容器元数据动态创建虚拟主机。
//
// 注意事项：
//   - 当前仅允许通过 Unix socket 连接容器运行时
//   - HTTP 和 HTTPS 字段引用 servers 中唯一的 Name
//
// 作者：xfy
package config

import "time"

// ContainerDiscoveryConfig 定义容器发现及服务器模板配置。
//
// 功能默认关闭；启用后仅发现指定 Network 中的容器。
type ContainerDiscoveryConfig struct {
	// Endpoint 是 Docker-compatible API 的 Unix socket 地址。
	Endpoint string `yaml:"endpoint"`
	// Network 是 Lolly 与后端容器共享的网络名称。
	Network string `yaml:"network"`
	// HTTPServer 是承载动态 HTTP 路由的服务器模板名称。
	HTTPServer string `yaml:"http_server"`
	// HTTPSServer 是承载动态 HTTPS 路由的服务器模板名称。
	HTTPSServer string `yaml:"https_server"`
	// ResyncInterval 是弥补事件丢失的全量同步周期。
	ResyncInterval time.Duration `yaml:"resync_interval"`
	// Debounce 是合并连续容器事件的等待时间。
	Debounce time.Duration `yaml:"debounce"`
	// RequestTimeout 是容器列表和详情 API 的单次超时。
	RequestTimeout time.Duration `yaml:"request_timeout"`
	// ACME 定义动态 HTTPS 域名共用的账户参数。
	ACME ContainerDiscoveryACMEConfig `yaml:"acme"`
	// Enabled 控制容器发现是否启用。
	Enabled bool `yaml:"enabled"`
	// Required 控制首次同步失败是否阻止服务启动。
	Required bool `yaml:"required"`
}

// ContainerDiscoveryACMEConfig 定义动态容器域名的 ACME 参数。
//
// HTTPS 模板负责启用 ACME 和动态域名白名单，本配置提供发现服务使用的
// 账户及挑战参数，避免把尚未发现的域名写入静态 hosts。
type ContainerDiscoveryACMEConfig struct {
	// Email 是动态证书共用的 ACME 账户邮箱。
	Email string `yaml:"email"`
	// Directory 是 ACME 服务目录地址。
	Directory string `yaml:"directory"`
	// StatePath 是账户与证书缓存目录。
	StatePath string `yaml:"state_path"`
	// Challenge 是 tls-alpn-01 或 http-01。
	Challenge string `yaml:"challenge"`
}
