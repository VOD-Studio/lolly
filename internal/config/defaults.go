// Package config 提供 YAML 配置文件的解析、验证和默认配置生成功能。
//
// 该文件包含默认配置生成功能，包括：
//   - DefaultConfig 函数：返回带合理默认值的配置结构体
//   - GenerateConfigYAML 函数：生成带注释的示例配置文件
//
// 主要用途：
//
//	用于生成默认配置和示例配置文件，便于用户快速上手。
//
// 注意事项：
//   - 默认值经过优化，适合大多数常见场景
//   - 生成的 YAML 包含详细注释说明
//
// 作者：xfy
package config

import (
	"bytes"
	"fmt"
	"time"
)

// DefaultConfig 返回带默认值的配置结构体。
//
// 返回一个预填充了合理默认值的配置对象，包含服务器、
// SSL、安全、压缩、日志、性能和监控等各模块的默认设置。
//
// 返回值：
//   - *Config: 带默认值的配置对象
//
// 默认值说明：
//   - 监听地址: :8080
//   - 超时: 读取/写入 30s，空闲 120s
//   - TLS: 仅支持 TLSv1.2 和 TLSv1.3
//   - 安全头部: X-Frame-Options=DENY, X-Content-Type-Options=nosniff
//   - 压缩: gzip，级别 6，最小 1024 字节
func DefaultConfig() *Config {
	return &Config{
		Servers: []ServerConfig{{
			Listen:             ":8080",
			Name:               "localhost",
			ReadTimeout:        30 * time.Second,
			WriteTimeout:       30 * time.Second,
			IdleTimeout:        120 * time.Second,
			MaxConnsPerIP:      1000,
			MaxRequestsPerConn: 10000,
			ClientMaxBodySize:  "10MB",
			// 高并发优化配置默认值
			Concurrency:       256 * 1024, // 256K 最大并发连接
			ReadBufferSize:    16 * 1024,  // 16KB 读缓冲
			WriteBufferSize:   16 * 1024,  // 16KB 写缓冲
			ReduceMemoryUsage: false,      // 优先性能
			CacheAPI: &CacheAPIConfig{
				Enabled: false,
				Path:    "/_cache/purge",
				Allow:   []string{"127.0.0.1"},
				Auth: CacheAPIAuthConfig{
					Type:  "none",
					Token: "",
				},
			},
			Lua: &LuaMiddlewareConfig{
				Enabled: false,
				Scripts: []LuaScriptConfig{},
				GlobalSettings: LuaGlobalSettings{
					MaxConcurrentCoroutines: 1000,
					CoroutineTimeout:        30 * time.Second,
					CodeCacheSize:           1000,
					EnableFileWatch:         true,
					MaxExecutionTime:        30 * time.Second,
					LStatePoolInitialSize:   100,
					LStatePoolMaxSize:       1000,
				},
			},
			Static: []StaticConfig{{
				Path:  "/",
				Root:  "/var/www/html",
				Index: []string{"index.html", "index.htm"},
			}},
			SSL: SSLConfig{
				Protocols:    []string{"TLSv1.2", "TLSv1.3"},
				OCSPStapling: false,
				HSTS: HSTSConfig{
					MaxAge:            31536000,
					IncludeSubDomains: true,
					Preload:           false,
				},
				HTTP2: HTTP2Config{
					Enabled:                 true,
					MaxConcurrentStreams:    128,
					MaxHeaderListSize:       1048576, // 1MB
					IdleTimeout:             120 * time.Second,
					PushEnabled:             false,
					H2CEnabled:              false,
					GracefulShutdownTimeout: 30 * time.Second,
				},
				SessionTickets: SessionTicketsConfig{
					Enabled:        false,
					KeyFile:        "",
					RotateInterval: 1 * time.Hour,
					RetainKeys:     3,
				},
				ClientVerify: ClientVerifyConfig{
					Enabled:     false,
					Mode:        "none",
					ClientCA:    "",
					VerifyDepth: 1,
					CRL:         "",
				},
			},
			Security: SecurityConfig{
				Access: AccessConfig{
					Allow:   []string{},
					Deny:    []string{},
					Default: "allow",
					GeoIP: GeoIPConfig{
						CacheSize: 10000,
						CacheTTL:  3600 * time.Second,
					},
				},
				RateLimit: RateLimitConfig{
					RequestRate:       0,
					Burst:             0,
					ConnLimit:         0,
					Key:               "ip",
					Algorithm:         "token_bucket",
					SlidingWindowMode: "approximate",
					SlidingWindow:     60,
				},
				Auth: AuthConfig{
					RequireTLS:        true,
					Algorithm:         "bcrypt",
					Realm:             "Restricted Area",
					MinPasswordLength: 8,
				},
				Headers: SecurityHeaders{
					XFrameOptions:       "DENY",
					XContentTypeOptions: "nosniff",
					ReferrerPolicy:      "strict-origin-when-cross-origin",
				},
				AuthRequest: AuthRequestConfig{
					Timeout: 5 * time.Second,
				},
				CORS: CORSConfig{
					Enabled: false,
				},
			},
			Compression: CompressionConfig{
				Type:                 "gzip",
				Level:                6,
				MinSize:              1024,
				GzipStatic:           true,
				GzipStaticExtensions: []string{".br", ".gz"},
				Types: []string{
					"text/html",
					"text/css",
					"text/javascript",
					"application/json",
					"application/javascript",
				},
			},
		}},
		Logging: LoggingConfig{
			Format: "text",
			Access: AccessLogConfig{
				// 近似 nginx combined 格式
				// nginx: $remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent"
				// lolly: $request 不含 HTTP 版本，$time 为 RFC3339 格式
				Format: "$remote_addr - $remote_user [$time] \"$request\" $status $body_bytes_sent \"$http_referer\" \"$http_user_agent\"",
			},
			Error: ErrorLogConfig{
				Level: "info",
			},
		},
		Performance: PerformanceConfig{
			GoroutinePool: GoroutinePoolConfig{
				Enabled:     false,
				MaxWorkers:  1000,
				MinWorkers:  10,
				IdleTimeout: 60 * time.Second,
			},
			FileCache: FileCacheConfig{
				MaxEntries: 10000,
				MaxSize:    256 * 1024 * 1024, // 256MB
				Inactive:   20 * time.Second,
			},
			Transport: TransportConfig{
				IdleConnTimeout: 90 * time.Second,
				MaxConnsPerHost: 512, // fasthttp 推荐值
			},
		},
		Monitoring: MonitoringConfig{
			Status: StatusConfig{
				Enabled: false,
				Path:    "/_status",
				Format:  "json",
				Allow:   []string{"127.0.0.1", "localhost"},
			},
			Pprof: PprofConfig{
				Enabled: false,
				Path:    DefaultPprofPath,
				Allow:   []string{"127.0.0.1"},
			},
			Healthz: HealthzConfig{
				Enabled: true,
				Path:    "/healthz",
			},
			Readyz: ReadyzConfig{
				Enabled: true,
				Path:    "/readyz",
			},
		},
		HTTP3: HTTP3Config{
			Enabled:     false,
			Listen:      ":443",
			MaxStreams:  100,
			IdleTimeout: 60 * time.Second,
			Enable0RTT:  false,
		},
		Resolver: ResolverConfig{
			Enabled:   false,
			Addresses: []string{"8.8.8.8:53", "8.8.4.4:53"},
			Valid:     30 * time.Second,
			Timeout:   5 * time.Second,
			IPv4:      true,
			IPv6:      false,
			CacheSize: 1024,
		},
		Variables: VariablesConfig{
			Set: map[string]string{},
		},
		Shutdown: ShutdownConfig{
			GracefulTimeout: 30 * time.Second,
			FastTimeout:     5 * time.Second,
			ReloadTimeout:   5 * time.Second,
		},
	}
}

// GenerateConfigYAML 生成带注释的默认配置 YAML。
//
// 根据配置对象生成带详细注释的 YAML 配置文件内容，
// 包含各配置项的说明和有效值提示。
//
// 参数：
//   - cfg: 配置对象，用于填充 YAML 中的默认值
//
// 返回值：
//   - []byte: 生成的 YAML 内容（包含注释）
//   - error: 当前实现始终返回 nil
//
// 注意事项：
//   - 生成的 YAML 包含中文注释，便于理解各配置项用途
//   - 注释中包含有效值范围和默认值说明
func GenerateConfigYAML(cfg *Config) ([]byte, error) {
	// 手动构建带注释的 YAML
	var buf bytes.Buffer

	buf.WriteString("# Lolly 配置文件\n")
	buf.WriteString("\n")

	// mode 配置
	buf.WriteString("# 运行模式配置\n")
	buf.WriteString("# mode: auto              # 运行模式（有效值: single, vhost, multi_server, auto）\n")
	buf.WriteString("# auto: 自动推断模式（根据 servers 配置自动选择，默认）\n")
	buf.WriteString("# single: 单服务器模式（只有一个 server）\n")
	buf.WriteString("# vhost: 虚拟主机模式（多个 server 共享相同监听地址）\n")
	buf.WriteString("# multi_server: 多服务器模式（多个 server 监听不同地址）\n")
	buf.WriteString("\n")

	// servers 配置
	buf.WriteString("# 服务器配置（多服务器模式）\n")
	buf.WriteString("servers:\n")
	buf.WriteString("  - # 服务器配置\n")
	fmt.Fprintf(&buf, "    listen: \"%s\"           # 监听地址\n", cfg.Servers[0].Listen)
	fmt.Fprintf(&buf, "    name: \"%s\"             # 服务器名称（虚拟主机匹配）\n", cfg.Servers[0].Name)
	buf.WriteString("    # server_names:                 # 多个 server_name（支持通配符和正则）\n")
	buf.WriteString("    #   - \"example.com\"             # 精确匹配\n")
	buf.WriteString("    #   - \"*.example.com\"           # 前缀通配（匹配 xxx.example.com）\n")
	buf.WriteString("    #   - \"~^www\\.\"                 # 正则匹配（以 www. 开头）\n")
	buf.WriteString("    # unix_socket:                  # Unix socket 配置（监听 Unix 域套接字）\n")
	buf.WriteString("    #   mode: 0666                  # 文件权限\n")
	buf.WriteString("    #   user: \"\"                    # 文件所有者（空表示当前用户）\n")
	buf.WriteString("    #   group: \"\"                   # 文件组（空表示当前组）\n")
	buf.WriteString("    # default: false           # 虚拟主机模式下标记为默认服务器（接收未匹配的请求）\n")
	fmt.Fprintf(&buf, "    read_timeout: %ds            # 读取超时（0 表示不限制）\n", int(cfg.Servers[0].ReadTimeout.Seconds()))
	fmt.Fprintf(&buf, "    write_timeout: %ds           # 写入超时（0 表示不限制）\n", int(cfg.Servers[0].WriteTimeout.Seconds()))
	fmt.Fprintf(&buf, "    idle_timeout: %ds            # 空闲超时（0 表示不限制）\n", int(cfg.Servers[0].IdleTimeout.Seconds()))
	fmt.Fprintf(&buf, "    max_conns_per_ip: %d         # 每 IP 最大连接数（0 表示不限制）\n", cfg.Servers[0].MaxConnsPerIP)
	fmt.Fprintf(&buf, "    max_requests_per_conn: %d    # 每连接最大请求数（0 表示不限制）\n", cfg.Servers[0].MaxRequestsPerConn)
	fmt.Fprintf(&buf, "    client_max_body_size: \"%s\"  # 请求体大小限制（支持单位: b, kb, mb, gb）\n", cfg.Servers[0].ClientMaxBodySize)
	buf.WriteString("    # 高并发优化配置（可选，未配置时使用默认值）\n")
	fmt.Fprintf(&buf, "    # concurrency: %d           # 最大并发连接数（默认 %d）\n", cfg.Servers[0].Concurrency, cfg.Servers[0].Concurrency)
	fmt.Fprintf(&buf, "    # read_buffer_size: %d       # 读缓冲区大小（字节，默认 %d）\n", cfg.Servers[0].ReadBufferSize, cfg.Servers[0].ReadBufferSize)
	fmt.Fprintf(&buf, "    # write_buffer_size: %d      # 写缓冲区大小（字节，默认 %d）\n", cfg.Servers[0].WriteBufferSize, cfg.Servers[0].WriteBufferSize)
	fmt.Fprintf(&buf, "    # reduce_memory_usage: %v    # 是否优先减少内存使用（默认 %v）\n", cfg.Servers[0].ReduceMemoryUsage, cfg.Servers[0].ReduceMemoryUsage)
	buf.WriteString("    # server_tokens: true          # 是否在响应头中显示版本号（默认 true，false 隐藏 Server 头版本信息）\n")
	buf.WriteString("\n")

	// types 配置
	buf.WriteString("    # MIME 类型配置\n")
	buf.WriteString("    # types:\n")
	buf.WriteString("    #   default_type: \"application/octet-stream\"  # 默认 MIME 类型（无法识别扩展名时使用）\n")
	buf.WriteString("    #   map:                                     # 自定义扩展名到 MIME 类型的映射\n")
	buf.WriteString("    #     \".html\": \"text/html\"\n")
	buf.WriteString("    #     \".css\": \"text/css\"\n")
	buf.WriteString("    #     \".js\": \"application/javascript\"\n")
	buf.WriteString("\n")

	// limit_rate 配置
	buf.WriteString("    # 响应速率限制配置（限制响应数据发送速率，防止单连接占用过多带宽）\n")
	buf.WriteString("    # limit_rate:\n")
	buf.WriteString("    #   rate: 0                    # 字节/秒（0 表示不限速）\n")
	buf.WriteString("    #   burst: 0                   # 突发流量字节数\n")
	buf.WriteString("    #   large_file_threshold: 0    # 大文件阈值（字节），超过此大小采用特殊策略（0 表示不区分）\n")
	buf.WriteString("    #   large_file_strategy: \"\"    # 大文件策略（有效值: skip 跳过限速, coarse 粗粒度限速）\n")
	buf.WriteString("\n")

	// cache_api 配置
	buf.WriteString("    # 缓存清理 API 配置（用于主动清理代理缓存）\n")
	buf.WriteString("    # cache_api:\n")
	fmt.Fprintf(&buf, "    #   enabled: %v              # 是否启用缓存清理 API\n", cfg.Servers[0].CacheAPI.Enabled)
	fmt.Fprintf(&buf, "    #   path: \"%s\"         # API 端点路径\n", cfg.Servers[0].CacheAPI.Path)
	buf.WriteString("    #   allow:                 # 允许访问的 IP\n")
	for _, ip := range cfg.Servers[0].CacheAPI.Allow {
		fmt.Fprintf(&buf, "    #     - \"%s\"\n", ip)
	}
	buf.WriteString("    #   auth:                  # 认证配置\n")
	fmt.Fprintf(&buf, "    #     type: \"%s\"          # 认证类型（有效值: none, token）\n", cfg.Servers[0].CacheAPI.Auth.Type)
	buf.WriteString("    #     token: \"\"            # 认证令牌（支持环境变量 ${CACHE_API_TOKEN}）\n")
	buf.WriteString("\n")

	// lua 配置
	buf.WriteString("    # Lua 中间件配置（在请求处理流程中嵌入 Lua 脚本）\n")
	buf.WriteString("    # lua:\n")
	fmt.Fprintf(&buf, "    #   enabled: %v              # 是否启用 Lua 中间件\n", cfg.Servers[0].Lua.Enabled)
	buf.WriteString("    #   scripts:               # Lua 脚本列表\n")
	buf.WriteString("    #     - path: \"/scripts/auth.lua\"  # 脚本路径\n")
	buf.WriteString("    #       phase: \"access\"          # 执行阶段（有效值: rewrite, access, content, log, header_filter, body_filter）\n")
	buf.WriteString("    #       # 或者使用路由匹配模式（与 phase 互斥）\n")
	buf.WriteString("    #       # route: \"/api/.*\"     # 路由匹配路径/模式（设置后按路由匹配执行，而非阶段执行）\n")
	buf.WriteString("    #       # route_type: \"regex\"   # 路由匹配类型（有效值: exact, prefix, prefix_priority, regex, regex_caseless；默认 prefix）\n")
	buf.WriteString("    #       timeout: 10s              # 执行超时\n")
	buf.WriteString("    #       enabled: true             # 是否启用此脚本\n")
	buf.WriteString("    #   global_settings:       # 全局设置\n")
	fmt.Fprintf(&buf, "    #     max_concurrent_coroutines: %d  # 最大并发协程数\n", cfg.Servers[0].Lua.GlobalSettings.MaxConcurrentCoroutines)
	fmt.Fprintf(&buf, "    #     coroutine_timeout: %ds          # 协程执行超时\n", int(cfg.Servers[0].Lua.GlobalSettings.CoroutineTimeout.Seconds()))
	fmt.Fprintf(&buf, "    #     code_cache_size: %d           # 字节码缓存条目数\n", cfg.Servers[0].Lua.GlobalSettings.CodeCacheSize)
	fmt.Fprintf(&buf, "    #     enable_file_watch: %v         # 启用文件变更检测\n", cfg.Servers[0].Lua.GlobalSettings.EnableFileWatch)
	fmt.Fprintf(&buf, "    #     max_execution_time: %ds         # 最大执行时间\n", int(cfg.Servers[0].Lua.GlobalSettings.MaxExecutionTime.Seconds()))
	fmt.Fprintf(&buf, "    #     coroutine_stack_size: %d         # 协程栈大小（0=使用引擎默认值 64，最大 256）\n", cfg.Servers[0].Lua.GlobalSettings.CoroutineStackSize)
	fmt.Fprintf(&buf, "    #     coroutine_pool_warmup: %d        # 协程池预热数量（0=不预热）\n", cfg.Servers[0].Lua.GlobalSettings.CoroutinePoolWarmup)
	fmt.Fprintf(&buf, "    #     minimize_stack_memory: %v    # 最小化栈内存使用\n", cfg.Servers[0].Lua.GlobalSettings.MinimizeStackMemory)
	fmt.Fprintf(&buf, "    #     lstate_pool_initial_size: %d   # LState池初始大小\n", cfg.Servers[0].Lua.GlobalSettings.LStatePoolInitialSize)
	fmt.Fprintf(&buf, "    #     lstate_pool_max_size: %d       # LState池最大大小\n", cfg.Servers[0].Lua.GlobalSettings.LStatePoolMaxSize)
	buf.WriteString("\n")

	// static 配置
	buf.WriteString("    # 静态文件服务配置（支持多个目录）\n")
	buf.WriteString("    static:\n")
	for _, st := range cfg.Servers[0].Static {
		buf.WriteString("      - path: \"/\"              # 匹配路径前缀\n")
		fmt.Fprintf(&buf, "        root: \"%s\"  # 静态文件根目录\n", st.Root)
		buf.WriteString("        # alias: \"\"             # 替换路径（与 root 互斥，nginx alias 语义）\n")
		buf.WriteString("        # root: 请求路径追加到 root 后面\n")
		buf.WriteString("        # alias: 请求路径替换 location 为 alias\n")
		buf.WriteString("        # 示例: path=/images/, alias=/var/www/files/\n")
		buf.WriteString("        #   /images/logo.png → /var/www/files/logo.png\n")
		buf.WriteString("        index:                 # 索引文件\n")
		for _, idx := range st.Index {
			fmt.Fprintf(&buf, "          - \"%s\"\n", idx)
		}
		buf.WriteString("        try_files: []             # SPA 部署示例: [\"$uri\", \"$uri/\", \"/index.html\"]\n")
		buf.WriteString("        try_files_pass: false     # 内部重定向是否触发中间件\n")
		buf.WriteString("        symlink_check: false      # 是否检查符号链接安全（防止路径遍历攻击）\n")
		buf.WriteString("        # expires: \"\"              # 缓存过期时间（nginx 兼容格式：30d, 1h, max, epoch）\n")
		buf.WriteString("        #   30d → Cache-Control: max-age=2592000\n")
		buf.WriteString("        #   max → Cache-Control: max-age=315360000, immutable\n")
		buf.WriteString("        #   epoch → Cache-Control: no-cache\n")
		buf.WriteString("        # location_type: \"\"         # 位置匹配类型（有效值: exact, prefix, regex, regex_caseless, prefix_priority, named）\n")
		buf.WriteString("        # internal: false           # 仅允许内部重定向访问\n")
		buf.WriteString("        # auto_index: false         # 启用目录列表（当目录无索引文件时）\n")
		buf.WriteString("        # auto_index_format: html   # 输出格式（html/json/xml）\n")
		buf.WriteString("        # auto_index_localtime: false # 使用本地时间（默认 GMT）\n")
		buf.WriteString("        # auto_index_exact_size: false # 显示精确大小（默认人类可读 K/M/G）\n")
	}
	buf.WriteString("    # 示例：静态资源缓存配置\n")
	buf.WriteString("    # - path: \"/assets/\"\n")
	buf.WriteString("    #   root: \"/var/www/assets\"\n")
	buf.WriteString("    #   expires: \"30d\"  # 缓存 30 天\n")
	buf.WriteString("\n")

	// proxy 配置示例
	buf.WriteString("    # 反向代理配置\n")
	buf.WriteString("    # proxy:\n")
	buf.WriteString("    #   - path: /api                  # 匹配路径前缀\n")
	buf.WriteString("    #     location_type: \"prefix\"     # 匹配类型（有效值: exact, prefix_priority, regex, regex_caseless, prefix, named）\n")
	buf.WriteString("    #     location_name: \"\"           # 命名 location 名称（仅 location_type=named 时使用，如 @fallback）\n")
	buf.WriteString("    #     targets:                    # 后端目标列表\n")
	buf.WriteString("    #       - url: http://backend1:8080\n")
	buf.WriteString("    #         weight: 3               # 权重（加权轮询时有效）\n")
	buf.WriteString("    #       - url: http://backend2:8080\n")
	buf.WriteString("    #         weight: 1\n")
	buf.WriteString("    #         max_conns: 0           # 单目标最大并发连接（0 为不限制）\n")
	buf.WriteString("    #         max_fails: 1           # 最大失败次数\n")
	buf.WriteString("    #         fail_timeout: \"10s\"    # 失败超时时间\n")
	buf.WriteString("    #         backup: false          # 备份服务器标记（仅当主服务器不可用时使用）\n")
	buf.WriteString("    #         down: false            # 永久不可用标记\n")
	buf.WriteString("    #         proxy_uri: \"\"          # 代理传递的 URI 路径\n")
	buf.WriteString("    #     load_balance: round_robin   # 负载均衡算法（有效值: round_robin, weighted_round_robin, least_conn, ip_hash, consistent_hash, random, least_time, sticky）\n")
	buf.WriteString("    #     hash_key: ip                # 一致性哈希键（仅 load_balance=consistent_hash 时有效，有效值: ip, uri, header:X-Name）\n")
	buf.WriteString("    #     virtual_nodes: 150          # 一致性哈希虚拟节点数（仅 load_balance=consistent_hash 时有效）\n")
	buf.WriteString("    #     health_check:               # 健康检查\n")
	buf.WriteString("    #       interval: 10s\n")
	buf.WriteString("    #       path: /health\n")
	buf.WriteString("    #       timeout: 5s\n")
	buf.WriteString("    #       slow_start: 0s           # 慢启动时间（0 表示禁用）\n")
	buf.WriteString("    #       match:                   # 健康检查匹配条件（可选，未配置时仅检查状态码 2xx）\n")
	buf.WriteString("    #         status: [\"200-299\"]    # 匹配的状态码范围列表\n")
	buf.WriteString("    #         body: \"ok\"             # 响应体正则匹配（可选）\n")
	buf.WriteString("    #         headers:               # 响应头匹配（可选）\n")
	buf.WriteString("    #           X-Health: \"ok\"\n")
	buf.WriteString("    #     timeout:                    # 超时配置\n")
	buf.WriteString("    #       dial: 5s                  # TCP 连接建立超时（不含 DNS/TLS）\n")
	buf.WriteString("    #       connect: 5s               # 总连接超时（含 DNS/TLS）\n")
	buf.WriteString("    #       read: 30s                 # 读取超时\n")
	buf.WriteString("    #       write: 30s                # 写入超时\n")
	buf.WriteString("    #     buffering:                # 代理缓冲配置\n")
	buf.WriteString("    #       mode: \"default\"        # 缓冲模式（有效值: default, on, off；off 为流式转发）\n")
	buf.WriteString("    #       buffer_size: 0         # 响应缓冲区大小（字节，0 表示使用默认值）\n")
	buf.WriteString("    #       buffers: \"8 16k\"       # 多缓冲区配置（格式: \"数量 大小\"，如 \"8 16k\" 或 \"4 4k 8 16k\"）\n")
	buf.WriteString("    #     proxy_bind: \"\"           # 代理拨号绑定本地地址\n")
	buf.WriteString("    #     internal: false          # 仅允许内部重定向访问\n")
	buf.WriteString("    #     headers:                    # 头部修改\n")
	buf.WriteString("    #       set_request: {X-Custom: value}\n")
	buf.WriteString("    #       set_response: {X-Server: lolly}\n")
	buf.WriteString("    #       remove: [X-Powered-By]\n")
	buf.WriteString("    #       hide_response: []        # 隐藏的响应头列表\n")
	buf.WriteString("    #       pass_response: []        # 白名单传递的响应头\n")
	buf.WriteString("    #       ignore_headers: []       # 完全忽略的头部（不传递给客户端也不记录）\n")
	buf.WriteString("    #       set_forwarded_host: true # 是否设置 X-Forwarded-Host（nil/true=设置，false=不设置）\n")
	buf.WriteString("    #       set_forwarded_proto: true # 是否设置 X-Forwarded-Proto（nil/true=设置，false=不设置）\n")
	buf.WriteString("    #       cookie_domain: \"\"        # Cookie 域重写\n")
	buf.WriteString("    #       cookie_path: \"\"          # Cookie 路径重写\n")
	buf.WriteString("    #     cache:                      # 代理缓存\n")
	buf.WriteString("    #       enabled: false\n")
	buf.WriteString("    #       max_age: 60s\n")
	buf.WriteString("    #       methods: [GET, HEAD]     # 可缓存的 HTTP 方法（默认 GET, HEAD）\n")
	buf.WriteString("    #       min_uses: 1              # 缓存阈值，请求次数达到此值才缓存（默认 1）\n")
	buf.WriteString("    #       cache_lock: true          # 防止缓存击穿\n")
	buf.WriteString("    #       cache_lock_timeout: 5s   # 缓存锁超时时间（默认 5s）\n")
	buf.WriteString("    #       stale_while_revalidate: 30s\n")
	buf.WriteString("    #       stale_if_error: 1m       # 上游错误时使用过期缓存的时间窗口\n")
	buf.WriteString("    #       stale_if_timeout: 30s    # 上游超时时使用过期缓存的时间窗口\n")
	buf.WriteString("    #       background_update_disable: false  # 禁用后台更新（默认启用）\n")
	buf.WriteString("    #       cache_ignore_headers: [] # 缓存时忽略的响应头\n")
	buf.WriteString("    #       revalidate: false        # 启用条件请求（默认关闭）\n")
	buf.WriteString("    #     cache_valid:                # 按 HTTP 状态码细分缓存时间（可选，未配置时使用 max_age）\n")
	buf.WriteString("    #       ok: 10m                   # 200-299 缓存 10 分钟\n")
	buf.WriteString("    #       redirect: 1h              # 301/302 缓存 1 小时\n")
	buf.WriteString("    #       not_found: 1m             # 404 缓存 1 分钟\n")
	buf.WriteString("    #       client_error: 0           # 其他 4xx 不缓存\n")
	buf.WriteString("    #       server_error: 0           # 5xx 不缓存\n")
	buf.WriteString("    #     proxy_ssl:                  # 上游 SSL 配置（加密到后端的连接）\n")
	buf.WriteString("    #       enabled: false            # 是否启用上游 TLS\n")
	buf.WriteString("    #       server_name: \"\"           # SNI 服务器名称\n")
	buf.WriteString("    #       trusted_ca: \"\"            # 信任的 CA 证书\n")
	buf.WriteString("    #       client_cert: \"\"           # 客户端证书（mTLS）\n")
	buf.WriteString("    #       client_key: \"\"            # 客户端私钥（mTLS）\n")
	buf.WriteString("    #       min_version: \"TLSv1.2\"    # 最小 TLS 版本\n")
	buf.WriteString("    #       max_version: \"\"           # 最大 TLS 版本（空表示不限制）\n")
	buf.WriteString("    #       insecure_skip_verify: false  # 跳过证书验证（仅测试用）\n")
	buf.WriteString("    #     client_max_body_size: \"50MB\"  # 此代理路径的请求体限制\n")
	buf.WriteString("    #     next_upstream:              # 故障转移配置\n")
	buf.WriteString("    #       tries: 1                  # 最大尝试次数（1 表示禁用故障转移）\n")
	buf.WriteString("    #       http_codes: [502, 503, 504]  # 触发重试的 HTTP 状态码\n")
	buf.WriteString("    #     balancer_by_lua:         # Lua 动态负载均衡（在 load_balance 基础上自定义选择逻辑）\n")
	buf.WriteString("    #       enabled: false         # 是否启用 Lua 负载均衡\n")
	buf.WriteString("    #       script: \"\"            # Lua 脚本路径，返回目标索引\n")
	buf.WriteString("    #       fallback: \"round_robin\"  # Lua 失败时的备用算法（有效值: round_robin, weighted_round_robin, least_conn, ip_hash, consistent_hash, random）\n")
	buf.WriteString("    #       timeout: 5s            # Lua 执行超时\n")
	buf.WriteString("    #     redirect_rewrite:        # Location/Refresh 头改写配置（代理响应重定向时改写 Location 头）\n")
	buf.WriteString("    #       mode: \"default\"       # 运行模式（有效值: default, off, custom）\n")
	buf.WriteString("    #       # default: 自动从选中的 target URL 生成规则（运行时）\n")
	buf.WriteString("    #       # off: 禁用改写\n")
	buf.WriteString("    #       # custom: 使用 rules 列表（预编译）\n")
	buf.WriteString("    #       rules: []               # 改写规则列表（仅 mode=\"custom\" 时使用）\n")
	buf.WriteString("    #       # 示例规则（custom 模式）:\n")
	buf.WriteString("    #       # - pattern: \"http://localhost:8000/\"  # 匹配模式（无 ~ 前缀为前缀匹配，~ 开头为正则）\n")
	buf.WriteString("    #       #   replacement: \"$scheme://$host:$server_port/\"  # 替换目标（支持 $host, $scheme, $server_port 等变量）\n")
	buf.WriteString("    #       # - pattern: \"~^http://[^/]+:8000/(.*)$\"  # 正则匹配示例\n")
	buf.WriteString("    #       #   replacement: \"$scheme://$host/$1\"  # 使用捕获组 $1\n")
	buf.WriteString("    #     least_time:              # 最小时间负载均衡配置\n")
	buf.WriteString("    #       metric: last_byte      # 指标类型（header | last_byte）\n")
	buf.WriteString("    #       default_time: 1ms      # 无统计样本时的默认响应时间\n")
	buf.WriteString("    #     sticky:                  # Session Sticky 配置\n")
	buf.WriteString("    #       enabled: false         # 是否启用\n")
	buf.WriteString("    #       name: lolly_route      # cookie 名称\n")
	buf.WriteString("    #       expires: 1h            # session 有效期\n")
	buf.WriteString("    #       path: /                # cookie 路径\n")
	buf.WriteString("    #       http_only: true        # HttpOnly flag\n")
	buf.WriteString("    #       same_site: Lax         # SameSite 属性\n")
	buf.WriteString("    #       fallback_balance: round_robin  # fallback 算法\n")
	buf.WriteString("\n")

	// SSL 配置
	buf.WriteString("    # SSL/TLS 配置\n")
	buf.WriteString("    # ssl:\n")
	buf.WriteString("    #   cert: /path/to/cert.pem        # 证书文件\n")
	buf.WriteString("    #   key: /path/to/key.pem          # 私钥文件\n")
	buf.WriteString("    #   cert_chain: /path/to/chain.pem # 证书链文件\n")
	buf.WriteString("    #   protocols:                     # TLS 版本（有效值: TLSv1.2, TLSv1.3）\n")
	for _, proto := range cfg.Servers[0].SSL.Protocols {
		fmt.Fprintf(&buf, "    #     - \"%s\"\n", proto)
	}
	buf.WriteString("    #   ciphers:                       # 加密套件（仅 TLS 1.2 有效，TLS 1.3 使用内置套件）\n")
	buf.WriteString("    #     - ECDHE-ECDSA-AES256-GCM-SHA384\n")
	buf.WriteString("    #     - ECDHE-RSA-AES256-GCM-SHA384\n")
	buf.WriteString("    #     - ECDHE-ECDSA-CHACHA20-POLY1305\n")
	buf.WriteString("    #     - ECDHE-RSA-CHACHA20-POLY1305\n")
	buf.WriteString("    #   # 拒绝不安全套件：含 RC4、DES、3DES、CBC 的配置将报错\n")
	fmt.Fprintf(&buf, "    #   ocsp_stapling: %v              # OCSP Stapling\n", cfg.Servers[0].SSL.OCSPStapling)
	buf.WriteString("    #   hsts:                          # HTTP Strict Transport Security\n")
	fmt.Fprintf(&buf, "    #     max_age: %d                  # 过期时间（秒）\n", cfg.Servers[0].SSL.HSTS.MaxAge)
	fmt.Fprintf(&buf, "    #     include_sub_domains: %v      # 包含子域名\n", cfg.Servers[0].SSL.HSTS.IncludeSubDomains)
	fmt.Fprintf(&buf, "    #     preload: %v                  # 加入 HSTS 预加载列表\n", cfg.Servers[0].SSL.HSTS.Preload)
	buf.WriteString("    #   session_tickets:               # TLS Session Tickets 配置（TLS 1.3 会话恢复）\n")
	fmt.Fprintf(&buf, "    #     enabled: %v                  # 是否启用 Session Tickets\n", cfg.Servers[0].SSL.SessionTickets.Enabled)
	buf.WriteString("    #     key_file: \"\"                # 密钥存储文件路径（用于持久化密钥）\n")
	fmt.Fprintf(&buf, "    #     rotate_interval: %d          # 密钥轮换间隔（秒），建议 1-24 小时\n", int(cfg.Servers[0].SSL.SessionTickets.RotateInterval.Seconds()))
	fmt.Fprintf(&buf, "    #     retain_keys: %d              # 保留的历史密钥数量，建议 3-5 个\n", cfg.Servers[0].SSL.SessionTickets.RetainKeys)
	buf.WriteString("    #   client_verify:                 # mTLS 客户端证书验证配置\n")
	buf.WriteString("    #     enabled: false               # 是否启用客户端证书验证\n")
	buf.WriteString("    #     mode: \"none\"                # 验证模式（有效值: none, request, require, optional_no_ca）\n")
	buf.WriteString("    #     client_ca: \"\"               # 客户端 CA 证书文件路径\n")
	buf.WriteString("    #     verify_depth: 1              # 证书链验证深度\n")
	buf.WriteString("    #     crl: \"\"                     # 证书撤销列表文件路径（可选）\n")
	buf.WriteString("    #   acme:                          # ACME（Let's Encrypt）自动证书签发/续期\n")
	buf.WriteString("    #     enabled: false               # 是否启用（未配置 cert/key 时由 CA 自动签发）\n")
	buf.WriteString("    #     directory: \"https://acme-v02.api.letsencrypt.org/directory\"  # ACME 目录 URL\n")
	buf.WriteString("    #     email: \"admin@example.com\"   # 联系邮箱（账户注册与到期提醒）\n")
	buf.WriteString("    #     state_path: \"/var/lib/lolly/acme\"  # 账户与证书持久化目录（权限 0700）\n")
	buf.WriteString("    #     challenge: \"tls-alpn-01\"     # 挑战类型（有效值: tls-alpn-01, http-01）\n")
	buf.WriteString("    #     hosts: []                    # 申请域名（为空时使用 server_names，再次之 name）\n")
	buf.WriteString("    #     eab_kid: \"\"                  # 外部账户绑定 Key ID（可选，部分 CA 需要）\n")
	buf.WriteString("    #     eab_hmac_key: \"\"             # 外部账户绑定 HMAC 密钥（base64url，可选）\n")
	buf.WriteString("    # 说明：cert/key 已配置时优先使用静态证书；通配符域名需 DNS-01 挑战，暂不支持\n")
	buf.WriteString("    #   http2:                         # HTTP/2 配置（需 SSL 证书）\n")
	fmt.Fprintf(&buf, "    #     enabled: %v                  # 是否启用 HTTP/2\n", cfg.Servers[0].SSL.HTTP2.Enabled)
	fmt.Fprintf(&buf, "    #     max_concurrent_streams: %d   # 最大并发流数\n", cfg.Servers[0].SSL.HTTP2.MaxConcurrentStreams)
	fmt.Fprintf(&buf, "    #     max_header_list_size: %d     # 最大头部列表大小（字节）\n", cfg.Servers[0].SSL.HTTP2.MaxHeaderListSize)
	fmt.Fprintf(&buf, "    #     idle_timeout: %ds            # 空闲超时\n", int(cfg.Servers[0].SSL.HTTP2.IdleTimeout.Seconds()))
	fmt.Fprintf(&buf, "    #     push_enabled: %v             # 是否启用 Server Push\n", cfg.Servers[0].SSL.HTTP2.PushEnabled)
	fmt.Fprintf(&buf, "    #     h2c_enabled: %v              # 是否启用 H2C（明文 HTTP/2）\n", cfg.Servers[0].SSL.HTTP2.H2CEnabled)
	fmt.Fprintf(&buf, "    #     graceful_shutdown_timeout: %ds  # HTTP/2 优雅关闭超时\n", int(cfg.Servers[0].SSL.HTTP2.GracefulShutdownTimeout.Seconds()))
	buf.WriteString("\n")

	// SSL 默认值说明（即使不启用也展示默认配置）
	buf.WriteString("    # SSL/TLS 默认配置说明（未配置证书时不启用）\n")
	buf.WriteString("    # 默认 TLS 协议: TLSv1.2, TLSv1.3（不支持 TLSv1.0/1.1）\n")
	buf.WriteString("    # 默认 HSTS 配置: max_age=31536000（1年）, include_sub_domains=true\n")
	buf.WriteString("\n")

	// security 配置
	buf.WriteString("    # 安全配置\n")
	buf.WriteString("    security:\n")
	buf.WriteString("      # IP 访问控制\n")
	buf.WriteString("      access:\n")
	buf.WriteString("        allow: []                   # 允许的 IP/CIDR 列表\n")
	buf.WriteString("        deny: []                    # 拒绝的 IP/CIDR 列表\n")
	fmt.Fprintf(&buf, "        default: \"%s\"             # 默认动作（有效值: allow, deny）\n", cfg.Servers[0].Security.Access.Default)
	buf.WriteString("        trusted_proxies: []         # 可信代理 CIDR 列表，用于 X-Forwarded-For 解析\n")
	buf.WriteString("\n")
	buf.WriteString("      # GeoIP 地理访问控制（基于 IP 所属国家/地区）\n")
	buf.WriteString("      geoip:\n")
	buf.WriteString("        enabled: false            # 是否启用 GeoIP 访问控制\n")
	buf.WriteString("        database: \"\"             # GeoIP 数据库文件路径（如 /usr/share/GeoIP/GeoLite2-Country.mmdb）\n")
	buf.WriteString("        default: \"allow\"         # 未匹配时的默认动作（有效值: allow, deny）\n")
	buf.WriteString("        private_ip_behavior: \"bypass\"  # 私有 IP 处理方式（有效值: bypass, apply_default, deny）\n")
	buf.WriteString("        allow_countries: []       # 允许的国家代码列表（如 [\"CN\", \"US\"]）\n")
	buf.WriteString("        deny_countries: []        # 拒绝的国家代码列表\n")
	fmt.Fprintf(&buf, "        cache_size: %d         # GeoIP 查询缓存大小\n", cfg.Servers[0].Security.Access.GeoIP.CacheSize)
	fmt.Fprintf(&buf, "        cache_ttl: %d           # 缓存有效期（秒）\n", int(cfg.Servers[0].Security.Access.GeoIP.CacheTTL.Seconds()))
	buf.WriteString("\n")
	buf.WriteString("      # 速率限制\n")
	buf.WriteString("      rate_limit:\n")
	fmt.Fprintf(&buf, "        request_rate: %d            # 每秒请求数（0 表示不限制）\n", cfg.Servers[0].Security.RateLimit.RequestRate)
	fmt.Fprintf(&buf, "        burst: %d                   # 突发上限\n", cfg.Servers[0].Security.RateLimit.Burst)
	fmt.Fprintf(&buf, "        conn_limit: %d              # 连接数限制\n", cfg.Servers[0].Security.RateLimit.ConnLimit)
	fmt.Fprintf(&buf, "        key: \"%s\"                  # 限流 key 来源（有效值: ip, header）\n", cfg.Servers[0].Security.RateLimit.Key)
	fmt.Fprintf(&buf, "        algorithm: \"%s\"             # 限流算法（有效值: token_bucket, sliding_window）\n", cfg.Servers[0].Security.RateLimit.Algorithm)
	fmt.Fprintf(&buf, "        sliding_window_mode: \"%s\"   # 滑动窗口模式（有效值: approximate, precise，仅 algorithm=sliding_window 时有效）\n", cfg.Servers[0].Security.RateLimit.SlidingWindowMode)
	fmt.Fprintf(&buf, "        sliding_window: %d          # 滑动窗口大小（秒，仅 algorithm=sliding_window 时有效）\n", cfg.Servers[0].Security.RateLimit.SlidingWindow)
	buf.WriteString("\n")
	buf.WriteString("      # 认证配置（type 为空时禁用）\n")
	buf.WriteString("      auth:\n")
	buf.WriteString("        type: \"\"                    # 认证类型（有效值: basic，空表示禁用）\n")
	fmt.Fprintf(&buf, "        require_tls: %v             # 启用时强制 HTTPS\n", cfg.Servers[0].Security.Auth.RequireTLS)
	fmt.Fprintf(&buf, "        algorithm: \"%s\"            # 密码哈希算法（有效值: bcrypt, argon2id）\n", cfg.Servers[0].Security.Auth.Algorithm)
	buf.WriteString("        users: []                   # 用户列表\n")
	fmt.Fprintf(&buf, "        realm: \"%s\"                # 认证域\n", cfg.Servers[0].Security.Auth.Realm)
	fmt.Fprintf(&buf, "        min_password_length: %d     # 密码最小长度\n", cfg.Servers[0].Security.Auth.MinPasswordLength)
	buf.WriteString("\n")
	buf.WriteString("      # 安全头部\n")
	buf.WriteString("      headers:\n")
	fmt.Fprintf(&buf, "        x_frame_options: \"%s\"        # 防止点击劫持（有效值: DENY, SAMEORIGIN, 空表示禁用）\n", cfg.Servers[0].Security.Headers.XFrameOptions)
	fmt.Fprintf(&buf, "        x_content_type_options: \"%s\" # 防止 MIME 嗅探（有效值：nosniff，空表示禁用）\n", cfg.Servers[0].Security.Headers.XContentTypeOptions)
	fmt.Fprintf(&buf, "        referrer_policy: \"%s\"        # 引用策略（有效值: no-referrer, no-referrer-when-downgrade, origin, origin-when-cross-origin, same-origin, strict-origin, strict-origin-when-cross-origin, unsafe-url）\n", cfg.Servers[0].Security.Headers.ReferrerPolicy)
	buf.WriteString("        content_security_policy: \"\"  # 内容安全策略 CSP（空表示禁用）\n")
	buf.WriteString("        permissions_policy: \"\"       # 权限策略（空表示禁用）\n")
	buf.WriteString("\n")
	buf.WriteString("      # 自定义错误页面\n")
	buf.WriteString("      error_page:\n")
	buf.WriteString("        pages: {}                  # 状态码到页面映射，如 {404: \"/errors/404.html\"}\n")
	buf.WriteString("        default: \"\"                # 默认错误页面\n")
	buf.WriteString("        response_code: 0          # 响应状态码覆盖（0 表示使用原始状态码）\n")
	buf.WriteString("\n")
	buf.WriteString("      # 外部认证子请求配置（将认证委托给外部服务）\n")
	buf.WriteString("      auth_request:\n")
	buf.WriteString("        enabled: false              # 是否启用外部认证\n")
	buf.WriteString("        uri: \"\"                     # 认证服务地址（支持相对路径或完整 URL）\n")
	buf.WriteString("        method: \"GET\"               # 认证请求方法（有效值: GET, POST, HEAD）\n")
	fmt.Fprintf(&buf, "        auth_timeout: %ds            # 认证请求超时时间\n", int(cfg.Servers[0].Security.AuthRequest.Timeout.Seconds()))
	buf.WriteString("        headers: {}                 # 自定义认证请求头，如 {X-Original-Uri: \"$request_uri\"}\n")
	buf.WriteString("        forward_headers: []         # 需要转发的原请求头，默认包含 Cookie, Authorization, X-Forwarded-For\n")
	buf.WriteString("\n")

	// rewrite 配置示例
	buf.WriteString("    # URL 重写规则\n")
	buf.WriteString("    # rewrite:\n")
	buf.WriteString("    #   - pattern: \"^/old/(.*)$\"     # 匹配模式（正则表达式）\n")
	buf.WriteString("    #     replacement: /new/$1       # 替换目标\n")
	buf.WriteString("    #     flag: last                 # 标志（有效值: last, redirect, permanent, break）\n")
	buf.WriteString("\n")

	// compression 配置
	buf.WriteString("    # 响应压缩配置\n")
	buf.WriteString("    compression:\n")
	fmt.Fprintf(&buf, "      type: \"%s\"            # 压缩类型（有效值: gzip, brotli, both，空表示禁用）\n", cfg.Servers[0].Compression.Type)
	fmt.Fprintf(&buf, "      level: %d              # 压缩级别（范围 0-9，0=不压缩，1=最快，9=最高压缩率）\n", cfg.Servers[0].Compression.Level)
	fmt.Fprintf(&buf, "      min_size: %d        # 最小压缩大小（字节，小于此值不压缩）\n", cfg.Servers[0].Compression.MinSize)
	fmt.Fprintf(&buf, "      gzip_static: %v        # 启用预压缩文件支持（自动查找 .gz/.br 文件）\n", cfg.Servers[0].Compression.GzipStatic)
	buf.WriteString("      gzip_static_extensions:  # 预压缩文件扩展名\n")
	for _, ext := range cfg.Servers[0].Compression.GzipStaticExtensions {
		fmt.Fprintf(&buf, "        - \"%s\"\n", ext)
	}
	buf.WriteString("      types:                 # 可压缩的 MIME 类型\n")
	for _, t := range cfg.Servers[0].Compression.Types {
		fmt.Fprintf(&buf, "        - \"%s\"\n", t)
	}
	buf.WriteString("\n")

	// shutdown 配置
	buf.WriteString("# 服务器关闭配置\n")
	buf.WriteString("shutdown:\n")
	fmt.Fprintf(&buf, "  graceful_timeout: %ds    # 优雅停止超时（SIGQUIT），等待活跃请求完成（0=使用默认30s）\n", int(cfg.Shutdown.GracefulTimeout.Seconds()))
	fmt.Fprintf(&buf, "  fast_timeout: %ds         # 快速停止超时（SIGINT/SIGTERM，0=使用默认5s）\n", int(cfg.Shutdown.FastTimeout.Seconds()))
	fmt.Fprintf(&buf, "  reload_timeout: %ds        # 热重载启动等待超时（SIGHUP，0=使用默认5s）\n", int(cfg.Shutdown.ReloadTimeout.Seconds()))
	buf.WriteString("\n")

	// stream 配置
	buf.WriteString("# TCP/UDP Stream 代理配置（可选）\n")
	buf.WriteString("# stream:\n")
	buf.WriteString("#   - listen: \"3306\"              # 监听地址\n")
	buf.WriteString("#     protocol: \"tcp\"             # 协议类型（有效值: tcp, udp）\n")
	buf.WriteString("#     upstream:\n")
	buf.WriteString("#       targets:                   # 上游目标列表\n")
	buf.WriteString("#         - addr: \"mysql1:3306\"   # 目标地址\n")
	buf.WriteString("#           weight: 3              # 权重（加权轮询时有效）\n")
	buf.WriteString("#         - addr: \"mysql2:3306\"\n")
	buf.WriteString("#           weight: 1\n")
	buf.WriteString("#       load_balance: \"round_robin\"  # 负载均衡算法（有效值: round_robin, weighted_round_robin, least_conn, ip_hash, consistent_hash, random）\n")
	buf.WriteString("#     ssl:                        # 服务端 SSL 配置（仅 TCP 支持）\n")
	buf.WriteString("#       enabled: false            # 是否启用 TLS 终端\n")
	buf.WriteString("#       cert: \"/path/to/cert.pem\" # 服务器证书文件\n")
	buf.WriteString("#       key: \"/path/to/key.pem\"   # 服务器私钥文件\n")
	buf.WriteString("#       protocols: [\"TLSv1.2\", \"TLSv1.3\"]  # TLS 协议版本\n")
	buf.WriteString("#       ciphers: []               # 加密套件（仅 TLS 1.2 有效）\n")
	buf.WriteString("#       client_ca: \"\"             # 客户端 CA 证书（mTLS）\n")
	buf.WriteString("#       verify_depth: 1           # 证书链验证深度\n")
	buf.WriteString("#     proxy_ssl:                  # 上游 SSL 配置（加密到后端的连接）\n")
	buf.WriteString("#       enabled: false            # 是否启用上游 TLS\n")
	buf.WriteString("#       verify: false             # 是否验证上游证书\n")
	buf.WriteString("#       trusted_ca: \"\"            # 信任的 CA 证书\n")
	buf.WriteString("#       server_name: \"\"           # SNI 服务器名称\n")
	buf.WriteString("#       cert: \"\"                  # 客户端证书（mTLS）\n")
	buf.WriteString("#       key: \"\"                   # 客户端私钥（mTLS）\n")
	buf.WriteString("#       protocols: []             # TLS 协议版本\n")
	buf.WriteString("#       session_reuse: false      # 是否复用 SSL 会话\n")
	buf.WriteString("\n")

	// logging 配置
	buf.WriteString("# 日志配置\n")
	buf.WriteString("logging:\n")
	fmt.Fprintf(&buf, "  format: \"%s\"           # 全局日志格式（有效值: text, json），控制启动/停止日志格式\n", cfg.Logging.Format)
	buf.WriteString("  access:\n")
	buf.WriteString("    path: \"\"                   # 日志文件路径（空表示输出到 stdout）\n")
	fmt.Fprintf(&buf, "    format: '%s'  # 访问日志格式，近似 nginx combined\n", cfg.Logging.Access.Format)
	buf.WriteString("    # 支持变量: $remote_addr, $remote_user, $request, $status, $body_bytes_sent, $request_time, $http_referer, $http_user_agent, $time\n")
	buf.WriteString("    # 特殊值 \"json\" 输出结构化 JSON\n")
	buf.WriteString("  error:\n")
	buf.WriteString("    path: \"\"                   # 日志文件路径（空表示输出到 stderr）\n")
	fmt.Fprintf(&buf, "    level: \"%s\"           # 日志级别（有效值: debug, info, warn, error，级别越高日志越少）\n", cfg.Logging.Error.Level)
	buf.WriteString("\n")

	// performance 配置
	buf.WriteString("# 性能配置\n")
	buf.WriteString("performance:\n")
	buf.WriteString("  goroutine_pool:              # Goroutine 池（处理并发请求）\n")
	fmt.Fprintf(&buf, "    enabled: %v             # 是否启用\n", cfg.Performance.GoroutinePool.Enabled)
	fmt.Fprintf(&buf, "    max_workers: %d          # 最大 worker 数\n", cfg.Performance.GoroutinePool.MaxWorkers)
	fmt.Fprintf(&buf, "    min_workers: %d          # 最小 worker 数（预热）\n", cfg.Performance.GoroutinePool.MinWorkers)
	fmt.Fprintf(&buf, "    idle_timeout: %ds        # 空闲超时\n", int(cfg.Performance.GoroutinePool.IdleTimeout.Seconds()))
	buf.WriteString("  file_cache:                  # 静态文件缓存\n")
	fmt.Fprintf(&buf, "    max_entries: %d          # 最大缓存条目\n", cfg.Performance.FileCache.MaxEntries)
	fmt.Fprintf(&buf, "    max_size: %d              # 内存上限（字节，%dMB）\n", cfg.Performance.FileCache.MaxSize, cfg.Performance.FileCache.MaxSize/1024/1024)
	fmt.Fprintf(&buf, "    inactive: %ds             # 未访问淘汰时间\n", int(cfg.Performance.FileCache.Inactive.Seconds()))
	buf.WriteString("  transport:                   # HTTP Transport 连接池\n")
	fmt.Fprintf(&buf, "    idle_conn_timeout: %ds        # 空闲超时\n", int(cfg.Performance.Transport.IdleConnTimeout.Seconds()))
	fmt.Fprintf(&buf, "    max_conns_per_host: %d        # 每主机最大连接（0 表示不限制）\n", cfg.Performance.Transport.MaxConnsPerHost)
	buf.WriteString("\n")

	// HTTP3 配置
	buf.WriteString("# HTTP/3 (QUIC) 配置（需要 SSL 证书）\n")
	buf.WriteString("http3:\n")
	fmt.Fprintf(&buf, "  enabled: %v              # 是否启用 HTTP/3\n", cfg.HTTP3.Enabled)
	fmt.Fprintf(&buf, "  listen: \"%s\"             # UDP 监听地址\n", cfg.HTTP3.Listen)
	fmt.Fprintf(&buf, "  max_streams: %d          # 最大并发流\n", cfg.HTTP3.MaxStreams)
	fmt.Fprintf(&buf, "  idle_timeout: %ds        # 空闲超时\n", int(cfg.HTTP3.IdleTimeout.Seconds()))
	fmt.Fprintf(&buf, "  enable_0rtt: %v          # 启用 0-RTT（早期数据，可能存在安全风险）\n", cfg.HTTP3.Enable0RTT)
	buf.WriteString("\n")

	// monitoring 配置
	buf.WriteString("# 监控配置\n")
	buf.WriteString("monitoring:\n")
	buf.WriteString("  status:\n")
	fmt.Fprintf(&buf, "    enabled: %v            # 是否启用状态端点\n", cfg.Monitoring.Status.Enabled)
	fmt.Fprintf(&buf, "    path: \"%s\"        # 状态端点路径\n", cfg.Monitoring.Status.Path)
	fmt.Fprintf(&buf, "    format: \"%s\"      # 输出格式（有效值: text, json, html）\n", cfg.Monitoring.Status.Format)
	buf.WriteString("    allow:                 # 允许访问的 IP\n")
	for _, ip := range cfg.Monitoring.Status.Allow {
		fmt.Fprintf(&buf, "      - \"%s\"\n", ip)
	}
	buf.WriteString("  pprof:                  # pprof 性能分析端点（用于 PGO 优化）\n")
	fmt.Fprintf(&buf, "    enabled: %v           # 是否启用（生产环境仅在收集 profile 时启用）\n", cfg.Monitoring.Pprof.Enabled)
	fmt.Fprintf(&buf, "    path: \"%s\"      # 端点路径前缀\n", cfg.Monitoring.Pprof.Path)
	buf.WriteString("    allow:                 # 允许访问的 IP\n")
	for _, ip := range cfg.Monitoring.Pprof.Allow {
		fmt.Fprintf(&buf, "      - \"%s\"\n", ip)
	}
	buf.WriteString("\n")

	// resolver 配置
	buf.WriteString("# DNS 解析器配置（用于后端域名动态解析）\n")
	buf.WriteString("resolver:\n")
	fmt.Fprintf(&buf, "  enabled: %v              # 是否启用 DNS 解析器\n", cfg.Resolver.Enabled)
	buf.WriteString("  addresses:               # DNS 服务器地址列表\n")
	buf.WriteString("    - \"8.8.8.8:53\"\n")
	buf.WriteString("    - \"8.8.4.4:53\"\n")
	fmt.Fprintf(&buf, "  valid: %ds               # 缓存有效期（TTL），建议 30s-300s\n", int(cfg.Resolver.Valid.Seconds()))
	fmt.Fprintf(&buf, "  timeout: %ds             # DNS 查询超时\n", int(cfg.Resolver.Timeout.Seconds()))
	fmt.Fprintf(&buf, "  ipv4: %v                 # 是否查询 IPv4 地址\n", cfg.Resolver.IPv4)
	fmt.Fprintf(&buf, "  ipv6: %v                 # 是否查询 IPv6 地址\n", cfg.Resolver.IPv6)
	fmt.Fprintf(&buf, "  cache_size: %d           # 缓存最大条目数（0 表示不限制）\n", cfg.Resolver.CacheSize)
	buf.WriteString("\n")

	// variables 配置
	buf.WriteString("# 自定义变量配置（全局变量，应用于所有虚拟主机）\n")
	buf.WriteString("variables:\n")
	buf.WriteString("  set: {}                  # 自定义变量集合，如 {app_name: \"lolly\"}\n")
	buf.WriteString("  # 注意：变量名只允许字母、数字、下划线，不能与内置变量冲突\n")
	buf.WriteString("  # 不能以 arg_、http_、cookie_ 开头（这些是动态变量前缀）\n")
	buf.WriteString("\n")

	// include 配置
	buf.WriteString("# 配置文件拆分（include 机制）\n")
	buf.WriteString("# include:\n")
	buf.WriteString("#   - path: \"conf.d/*.yaml\"       # 相对路径 + glob 模式\n")
	buf.WriteString("#   - path: \"sites/example.yaml\"  # 单个文件引入\n")
	buf.WriteString("# 支持循环检测和深度限制（最大 10 层）\n")
	buf.WriteString("# 注意：只有 servers、stream、variables 会被合并，其他字段忽略\n")

	return buf.Bytes(), nil
}
