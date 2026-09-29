# Lolly

[![Go Version](https://img.shields.io/badge/Go-1.26%2B-00ADD8?style=flat&logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

高性能 HTTP 服务器与反向代理，使用 Go 语言编写。

基于 [fasthttp](https://github.com/valyala/fasthttp) 构建，提供比标准 net/http 更高的性能。支持 HTTP/3 (QUIC)、WebSocket、虚拟主机、多种负载均衡算法、故障转移、代理缓存、Lua 脚本扩展，以及完整的安全与性能优化特性。

> **代码统计**：154 个源文件 | 171 个测试文件 | ~121,000 行 Go 代码

## 特性

### 核心功能

- **静态文件服务** - 零拷贝传输（sendfile）、文件缓存、预压缩支持、try_files 配置、ETag 和 304 Not Modified
- **反向代理** - 请求头/响应头修改、超时控制、故障转移（next_upstream）、Location/Refresh 头改写
- **HTTP/2** - 完整的 HTTP/2 服务器支持，包含适配器与流控；TLS 上经 ALPN 协商，明文端口可启用 h2c（prior knowledge 与 Upgrade 握手）
- **HTTP/3 (QUIC)** - 基于 quic-go，支持 0-RTT 连接
- **WebSocket** - 完整的 WebSocket 代理支持
- **虚拟主机** - 单进程支持多域名独立配置，server_name 支持通配符和正则匹配
- **Location 匹配** - nginx 风格的精确/前缀/正则匹配引擎
- **Unix Socket** - 支持 Unix socket 监听
- **配置引入** - include 指令支持配置拆分
- **TCP/UDP Stream** - 四层代理，支持 MySQL、Redis 等服务
- **Lua 脚本** - 基于 gopher-lua 的可编程扩展，支持 nginx-lua 兼容 API
- **GeoIP 过滤** - 基于 MaxMind GeoIP2 的国家/地区访问控制
- **自定义错误页面** - 支持为特定状态码配置自定义错误页面
- **nginx 配置导入** - 支持将 nginx 配置文件转换为 lolly YAML 配置
- **变量系统** - nginx 风格变量（`$request_uri`、`$remote_addr`、`$host` 等），支持自定义变量
- **Gzip/Brotli 压缩** - 动态与预压缩（gzip_static/br_static）支持
- **URL 重写** - 正则替换、break/last/redirect/permanent 标记
- **目录索引** - 自动生成目录列表（HTML/JSON/XML）
- **访问日志** - 结构化访问日志，支持自定义格式
- **DNS 解析缓存** - 带 TTL 与统计的缓存 DNS 解析器

### 负载均衡

| 算法                 | 说明                                                     |
| -------------------- | -------------------------------------------------------- |
| Round Robin          | 轮询，均匀分配                                           |
| Weighted Round Robin | 加权轮询，按权重分配                                     |
| Least Connections    | 最少连接，选择活跃连接最少的目标                         |
| IP Hash              | IP 哈希，同一客户端始终路由到同一目标                    |
| Consistent Hash      | 一致性哈希，支持虚拟节点（默认 150），最小化节点变更影响 |
| Random               | Power of Two Choices，随机选择两个后比较，简单高效       |

### 安全

- **访问控制** - IP/CIDR 白名单与黑名单，支持 trusted_proxies 配置
- **速率限制** - 令牌桶与滑动窗口算法，支持精确/近似模式
- **连接限制** - 单 IP 并发连接数限制
- **认证** - Basic Auth，支持 bcrypt 与 argon2id；外部 auth_request 子请求
- **安全头部** - HSTS、X-Frame-Options、CSP、Referrer-Policy
- **SSL/TLS** - OCSP Stapling、TLS 1.2/1.3、加密套件配置、Session Tickets 密钥轮换、客户端证书验证（mTLS）、ACME 自动证书签发/续期（Let's Encrypt，支持 tls-alpn-01 / http-01 挑战）
- **请求体限制** - 可配置全局和路径级别的请求体大小限制

### 性能优化

- **Goroutine 池** - 限制并发 worker 数量，避免 goroutine 爆炸
- **文件缓存** - LRU 淘汰策略，内存上限控制
- **连接池** - 基于 fasthttp HostClient 的空闲连接复用，减少连接建立开销
- **零拷贝** - 大文件（≥8KB）在 Linux 上使用 sendfile 系统调用，其他平台自动回退到高效拷贝
- **代理缓存** - 支持缓存后端响应，cache_lock 防止缓存击穿与并发回源
- **PGO 优化** - 支持 Profile-Guided Optimization 构建

### 运维

- **热升级** - USR2 信号触发，零停机升级，支持 Unix Socket 继承
- **配置热重载** - HUP 信号触发，动态更新配置
- **日志轮转** - USR1 信号触发，重新打开日志文件
- **优雅关闭** - QUIT 信号触发，等待请求完成，支持超时配置
- **状态监控** - 内置 `/status` 端点，统计连接数、请求数、流量、上游健康状态、缓存命中率
- **pprof 端点** - 内置性能分析端点，支持 CPU/heap/goroutine/block 分析
- **ACME 证书监控** - 周期性扫描 ACME 状态目录，证书临近过期或已过期时输出分级告警日志；http-01 挑战缺少 80 端口监听器时启动告警

## 架构

```
                    +------------------+
                    |     Client       |
                    +--------+---------+
                             |
         +-------------------+-------------------+
         |                   |                   |
    +----v----+        +-----v-----+        +----v----+
    | HTTP/1  |        |  HTTP/2   |        | HTTP/3  |
    | (TCP)   |        |  (TLS)    |        | (QUIC)  |
    +----+----+        +-----+-----+        +----+----+
         |                   |                   |
         +-------------------+-------------------+
                             |
                    +--------v---------+
                    |   Middleware     |
                    |     Chain        |
                    +--------+---------+
                             |
         +-------------------+-------------------+
         |                   |                   |
    +----v----+        +-----v-----+        +----v----+
    | Static  |        |   Proxy   |        | Stream  |
    | Handler |        |  Handler  |        | Handler |
    +----+----+        +-----+-----+        +----+----+
         |                   |                   |
    File Cache         Load Balancer        L4 Proxy
         |                   |                   |
    +----+----+        +-----v-----+        +----v----+
    | Disk    |        | Upstream  |        | Backend |
    | I/O     |        | Targets   |        | Servers |
    +---------+        +-----------+        +---------+
```

### 目录结构

```
internal/
├── app/          # 应用入口、信号处理、生命周期
├── config/       # 配置加载、验证、默认值
├── server/       # HTTP 服务器核心
├── handler/      # 请求处理器
├── proxy/        # 反向代理
├── loadbalance/  # 负载均衡算法
├── matcher/      # Location 匹配器
├── middleware/   # 中间件链
│   ├── compression/  # Gzip/Brotli 压缩
│   ├── security/     # 访问控制、认证、限流、安全头部
│   ├── rewrite/      # URL 重写
│   ├── accesslog/    # 访问日志
│   ├── bodylimit/    # 请求体限制
│   └── errorintercept/ # 错误拦截
├── lua/          # Lua 脚本引擎
├── http2/        # HTTP/2 服务器
├── http3/        # HTTP/3 服务器
├── adapter/      # HTTP/2、HTTP/3 适配器
├── stream/       # TCP/UDP Stream 代理
├── ssl/          # TLS 配置
├── cache/        # 缓存系统
├── resolver/     # DNS 解析器
├── variable/     # 变量系统
├── logging/      # 日志系统
├── converter/    # nginx 配置转换器
└── ...           # 其他工具模块
```

## 信号处理

| 信号            | 行为                   |
| --------------- | ---------------------- |
| SIGTERM, SIGINT | 快速停止               |
| SIGQUIT         | 优雅停止，等待请求完成 |
| SIGHUP          | 重载配置               |
| SIGUSR1         | 重新打开日志文件       |
| SIGUSR2         | 热升级                 |

## 依赖

| 库                                                          | 用途             |
| ----------------------------------------------------------- | ---------------- |
| [fasthttp](https://github.com/valyala/fasthttp)             | HTTP 服务器核心  |
| [fasthttp/router](https://github.com/fasthttp/router)       | HTTP 路由器      |
| [quic-go](https://github.com/quic-go/quic-go)               | QUIC/HTTP/3 实现 |
| [zerolog](https://github.com/rs/zerolog)                    | 零分配 JSON 日志 |
| [gopher-lua](https://github.com/yuin/gopher-lua)            | Lua 脚本引擎     |
| [klauspost/compress](https://github.com/klauspost/compress) | Gzip/Brotli 压缩 |
| [geoip2-golang](https://github.com/oschwald/geoip2-golang)  | GeoIP 查询       |
| [golang-lru/v2](https://github.com/hashicorp/golang-lru)    | LRU 缓存         |
| [yaml.v3](https://gopkg.in/yaml.v3)                         | YAML 配置解析    |

## 许可证

MIT License

## 作者

xfy
