# Nginx 配置示例

本目录包含 nginx 常用配置示例，用于展示 lolly 需要兼容的功能特性。

## 目录结构

```
docs/config/
├── README.md                    # 本说明文件
├── basic/                       # 基础配置
│   ├── static-server.conf       # 静态文件服务器
│   ├── reverse-proxy.conf       # 反向代理基础配置
│   └── virtual-host.conf        # 虚拟主机配置
├── ssl/                         # SSL/TLS 配置
│   ├── basic-ssl.conf           # 基础 HTTPS 配置
│   ├── mtls.conf                # 双向 TLS 认证
│   ├── ocsp-stapling.conf       # OCSP Stapling 配置
│   └── hsts.conf                # HSTS 安全配置
├── load-balancing/              # 负载均衡配置
│   ├── round-robin.conf         # 轮询负载均衡
│   ├── weighted.conf            # 加权负载均衡
│   ├── least-conn.conf          # 最少连接负载均衡
│   ├── ip-hash.conf             # IP 哈希会话保持
│   └── consistent-hash.conf     # 一致性哈希
├── advanced/                    # 高级功能配置
│   ├── websocket.conf           # WebSocket 代理
│   ├── grpc.conf                # gRPC 代理
│   ├── http2.conf               # HTTP/2 配置
│   ├── http3.conf               # HTTP/3 (QUIC) 配置
│   ├── stream-tcp.conf          # TCP Stream 代理
│   └── stream-udp.conf          # UDP Stream 代理
├── security/                    # 安全配置
│   ├── rate-limit.conf          # 请求速率限制
│   ├── conn-limit.conf          # 连接数限制
│   ├── access-control.conf      # IP 访问控制
│   ├── basic-auth.conf          # Basic 认证
│   ├── security-headers.conf    # 安全响应头
│   └── auth-request.conf        # 外部认证子请求
├── caching/                     # 缓存配置
│   ├── proxy-cache.conf         # 代理响应缓存
│   ├── gzip.conf                # Gzip 压缩
│   └── brotli.conf              # Brotli 压缩
├── rewriting/                   # URL 重写配置
│   ├── rewrite-rules.conf       # URL 重写规则
│   ├── redirect.conf            # 重定向配置
└── lua/                         # Lua 脚本配置
    ├── basic-lua.conf           # 基础 Lua 使用
    ├── access-by-lua.conf       # access_by_lua 认证
    ├── content-by-lua.conf      # content_by_lua 内容生成
    ├── balancer-by-lua.conf     # balancer_by_lua 动态负载均衡
    └── shared-dict.conf         # lua_shared_dict 共享字典
```

## 配置对照说明

每个 nginx 配置文件都配有对应的 lolly YAML 配置注释，说明 lolly 如何实现相同功能。

## 使用目的

1. **功能对照**: 明确 nginx 配置指令与 lolly 配置项的对应关系
2. **兼容性测试**: 用于验证 lolly 实现的功能是否符合预期
3. **迁移参考**: 帮助用户从 nginx 配置迁移到 lolly 配置

## 配置来源

基于 nginx 官方文档和最佳实践整理，参考：
- nginx 官方文档: https://nginx.org/en/docs/
- OpenResty 文档: https://openresty.org/

## 功能对照表

### 负载均衡

| nginx 指令 | Lolly 配置 |
|-----------|-----------|
| `upstream { server ...; }` | `proxy.targets` 列表 |
| `weight=N` | `targets[].weight: N` |
| `least_conn;` | `load_balance: "least_conn"` |
| `ip_hash;` | `load_balance: "ip_hash"` |
| `hash $key consistent;` | `load_balance: "consistent_hash"` |

### SSL/TLS

| nginx 指令 | Lolly 配置 |
|-----------|-----------|
| `ssl_certificate` | `ssl.cert` |
| `ssl_certificate_key` | `ssl.key` |
| `ssl_protocols` | `ssl.protocols` |
| `ssl_ciphers` | `ssl.ciphers` |
| `ssl_stapling on` | `ssl.ocsp_stapling: true` |
| `add_header Strict-Transport-Security` | `ssl.hsts` |
| `ssl_verify_client` | `ssl.client_verify.mode` |
| `ssl_client_certificate` | `ssl.client_verify.client_ca` |
| `acme_issuer` / `acme_certificate` | `ssl.acme`（内置 ACME 自动签发/续期，无需 certbot） |

### 安全

| nginx 指令 | Lolly 配置 |
|-----------|-----------|
| `limit_req zone` | `security.rate_limit.request_rate` |
| `limit_conn zone` | `security.rate_limit.conn_limit` |
| `allow/deny` | `security.access.allow/deny` |
| `auth_basic` | `security.auth.type: "basic"` |
| `auth_request` | `security.auth_request.enabled: true` |
| `add_header X-Frame-Options` | `security.headers.x_frame_options` |

### 代理

| nginx 指令 | Lolly 配置 |
|-----------|-----------|
| `proxy_pass` | `proxy[].targets[].url` |
| `proxy_set_header` | `proxy[].headers.set_request` |
| `proxy_cache` | `proxy[].cache.enabled` |
| `proxy_next_upstream` | `proxy[].next_upstream` |
| `grpc_pass` | HTTP/2 + gRPC 协议支持 |

### 压缩

| nginx 指令 | Lolly 配置 |
|-----------|-----------|
| `gzip on` | `compression.type: "gzip"` |
| `gzip_comp_level` | `compression.level` |
| `gzip_types` | `compression.types` |
| `gzip_static on` | `compression.gzip_static` |
| `brotli on` | `compression.type: "brotli"` |

### 高级功能

| nginx 功能 | Lolly 支持 |
|-----------|-----------|
| WebSocket 代理 | ✓ 自动协议升级 |
| HTTP/2 | ✓ `ssl.http2.enabled`（TLS 上经 ALPN 协商） |
| HTTP/2 明文（h2c） | ✓ `ssl.http2.h2c_enabled`（prior knowledge 与 Upgrade 握手；与 HTTP/1.1 共用 `max_conns_per_ip`；Upgrade 的流 1 请求体会在握手前缓冲） |
| HTTP/3 (QUIC) | ✓ `http3.enabled` |
| TCP/UDP Stream | ✓ `stream` 配置 |
| URL 重写 | ✓ `rewrite` 配置 |
| Lua 脚本 | ✓ 内置 Lua 沙箱 |

## 容器发现

顶层 `container_discovery` 用于根据容器元数据动态创建虚拟主机，默认禁用。当前仅支持通过 Unix socket 连接容器运行时；启用时必须指定容器网络，并可按需引用 HTTP、HTTPS 服务器模板。

```yaml
container_discovery:
  enabled: true
  endpoint: "unix:///var/run/docker.sock"
  network: "frontend"
  http_server: "container-http"
  https_server: "container-https"
  resync_interval: 30s
  debounce: 200ms
  request_timeout: 5s
  required: false
  acme:
    email: "admin@example.com"
    directory: "https://acme-v02.api.letsencrypt.org/directory"
    state_path: "/var/lib/lolly/acme"
    challenge: "tls-alpn-01"

servers:
  - name: "container-http"       # name 必须唯一，且模板必须为明文
    listen: ":80"
  - name: "container-https"      # name 必须唯一，且模板必须启用 TLS
    listen: ":443"
    ssl:
      acme:
        enabled: true
        allow_dynamic_hosts: true # 只允许受信任的发现结果扩充证书域名白名单
```

`resync_interval`、`debounce` 和 `request_timeout` 不得为负数；前两者为 `0` 时分别使用默认 30 秒和不防抖，`request_timeout: 0` 使用默认 5 秒。`required: true` 表示发现服务初始化失败时应阻止启动。HTTPS 模板使用 ACME 时必须显式开启 `ssl.acme.allow_dynamic_hosts`；普通 ACME 服务仍需通过 `hosts`、`server_names` 或 `name` 声明静态域名。

发现器兼容 Docker 和 Podman 的 Docker-compatible Unix socket API。启动时执行一次全量扫描，之后监听容器/网络事件，并通过周期扫描补偿事件断线。API 暂时失败时保留最后一次成功路由。

支持以下 nginx-proxy 环境变量：

- `VIRTUAL_HOST`、`VIRTUAL_PORT`、`VIRTUAL_PROTO=http|https`
- `VIRTUAL_PATH`、`VIRTUAL_DEST`
- `VIRTUAL_HOST_MULTIPORTS`（YAML 或 JSON）
- `EXTERNAL_HTTP_PORT`、`EXTERNAL_HTTPS_PORT`
- `HTTPS_METHOD=redirect|noredirect|nohttp|nohttps`
- `LETSENCRYPT_HOST` / `ACME_HOST`
- `LETSENCRYPT_EMAIL` / `ACME_EMAIL`

相同 Host 和 Path 的多个容器会组成轮询上游。静态 `server_name` 始终优先于容器声明。只有显式出现在 `LETSENCRYPT_HOST` 或 `ACME_HOST` 中且同时匹配 `VIRTUAL_HOST` 的域名才会启用动态 HTTPS 和证书签发；未声明证书域名的服务保持 HTTP，避免重定向到不可用的 HTTPS。ACME 账户邮箱在启动时确定：优先使用 `container_discovery.acme.email`，其次使用 HTTPS 模板邮箱，最后使用首次发现结果中的第一个容器邮箱；后续容器邮箱不同时只记录告警。

当前不支持 nginx-proxy 的正则/通配符动态 Host、正则 `VIRTUAL_PATH`、FastCGI/uWSGI、DNS-01 provider 配置及挂载式 nginx 配置片段；这些声明会被拒绝或跳过并记录告警。

## ACME 自动证书（Let's Encrypt）

lolly 内置 ACME 客户端，可直接向 Let's Encrypt 等 CA 申请证书并在到期前
自动续期，无需 certbot / acme-companion 等外部工具。对应 nginx 的
`ngx_http_acme_module`（`acme_issuer` + `acme_certificate`）。

```yaml
servers:
  - listen: ":443"
    name: "example.com"                 # 或 server_names: [example.com, www.example.com]
    ssl:
      acme:
        enabled: true
        email: "admin@example.com"
        directory: "https://acme-v02.api.letsencrypt.org/directory"
        state_path: "/var/lib/lolly/acme"   # 账户与证书持久化目录（0600/0700）
        challenge: "tls-alpn-01"            # 或 http-01（需 80 端口可达）
    # 不要再配置 ssl.cert / ssl.key，否则会优先使用静态证书
```

要点：

- **挑战类型**：`tls-alpn-01`（默认）不依赖 80 端口，适合纯 TLS 服务端；
  `http-01` 要求 CA 能访问 80 端口，并由 lolly 自动响应
  `/.well-known/acme-challenge/` 请求。
- **域名来源**：`ssl.acme.hosts` 显式配置优先，其次 `server_names`，最后 `name`。
- **静态证书优先**：同时配置 `ssl.cert`/`ssl.key` 时忽略 ACME 并告警。
- **速率限制**：证书与账户状态持久化在 `state_path`，进程重启后复用；
  调试时可将 `directory` 改为 `https://acme-staging-v02.api.letsencrypt.org/directory`。
- **限制**：通配符域名需要 DNS-01 挑战，当前暂不支持。
- **多虚拟主机**：`listen` 完全相同的 `server` 共用一个监听器，HTTP 按 Host 分流，TLS 按 SNI 选择各自的静态或 ACME 证书；每个监听地址可独立指定一个 `default: true` 作为未匹配请求的兜底。同一监听地址不能混合 TLS 与明文配置。
- **到期监控**：lolly 每 12 小时扫描 `state_path`，证书剩余天数 < 30 输出 warn、
  < 7 输出 error、已过期输出 error，便于在续期持续失败时及时发现。
- **启动告警**：使用 `http-01` 但没有任何 `server` 监听 80 端口时启动会告警；
  排查时改用 `tls-alpn-01`（默认）或新增 `:80` 监听即可。

## 统计

- **配置文件总数**: 33 个
- **覆盖功能**: 负载均衡、SSL/TLS、安全、代理、缓存、重写、Lua 等