# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- **ssl**: 内置 ACME（Let's Encrypt）自动证书签发/续期，支持 `ssl.acme` 配置块
  - 无需 certbot/acme-companion 等外部工具即可申请与续期证书
  - 支持 `tls-alpn-01`（默认，无 80 端口依赖）与 `http-01` 挑战
  - 支持自定义 ACME 目录（生产/测试环境）、联系邮箱、状态持久化目录
  - 支持 EAB（外部账户绑定）与域名白名单（防止任意 SNI 刷证书）
  - 证书状态持久化到 `state_path`，进程重启后复用，避免触发 CA 速率限制
  - 静态证书优先：同时配置 `cert`/`key` 时忽略 ACME 并给出告警
- **config**: `ssl.acme` 配置校验与默认配置模板说明
- **ssl**: ACME 证书到期监控，临近过期（默认 30 天）/ 已过期时输出分级告警
- **server**: ACME http-01 挑战缺少 80 端口监听器时启动告警

## [0.4.1] - 2026-06-10

### Fixed

- **stream**: 修复上游选择逻辑，添加优雅关闭支持
- **stream**: 修复 Stop 后 stopCh 未重置导致的重启问题

## [0.4.0] - 2026-06-09

### Added

#### 负载均衡

- **Least Time 负载均衡器**：基于 EWMA（指数加权移动平均）统计的最少响应时间算法，自动选择响应最快上游
- **Session Sticky 负载均衡器**：基于 Cookie 的会话粘性，一致性哈希分片，支持 Cookie 过期、域名、路径、Secure/HttpOnly/SameSite 属性
- 对应 YAML 配置支持：`least_time`、`sticky` 策略及参数

#### 平台与构建

- FreeBSD 部署示例

### Fixed

- Least Time 响应时间记录修正
- Sticky Cookie 格式、分片键、过期检查修复
- Sticky 双重 Stop 防护和重启支持
- 配置验证：`least_time` 的 `default_time` 不允许负值

### Tests

- Least Time 和 Sticky 负载均衡器集成测试
- Least Time 和 Sticky 基准测试

## [0.3.0] - 2026-06-05

### Added

#### 核心功能

- Lua gjson 模块：JSON 编码/解码库，支持 sort_keys 选项
- Lua 路由注册：支持 route 字段的 Lua 脚本按路由执行
- Config include 指令：支持 glob 模式的配置文件拆分和包含
- SIGHUP 热重载：配置变更后通过信号触发无中断重载
- FreeBSD / OpenBSD 构建目标
- X-Forwarded-Host 和 X-Forwarded-Proto 可配置代理头

#### 平台与构建

- MIT 许可证
- `docs/plans/` 添加到 .gitignore
- Makefile 优化和一致性修复

### Performance

- matcher RadixTree 搜索 sync.Pool 零分配
- loadbalance filterHealthy 池化 + atomic.Bool 重建锁
- FileInfoCache 近似 LRU 替代读写锁升级
- resolver O(1) LRU（container/list）
- proxy 内联 FNV-1a 哈希、消除 WebSocket 字符串分配、缓存键单次遍历
- compression MIME 预计算字节切片、GeoIP 查找去重、gzip_static 预建扩展名集合
- ConsistentHash 双锁消除、Balancer healthy slice 池化

### Fixed

- 修复 9 个包的并发数据竞争（`go test -count=3 -race` 零 race）
  - lua: TCPSocket.currentOp 锁保护、schedulerMu 调度器同步
  - server: buildMiddlewareChain 并发写、multi-server 模式 goroutine 安全
  - handler/app/http3/stream: 测试同步和原子操作修复
- 多服务器模式 status/pprof 注册
- config include 循环检测
- atomic.Bool 替代 running 标志消除 data race
- OCSP 刷新逻辑、DNS resolver double-close panic
- SSL DER 提取改用 encoding/pem
- 根路径规范化修复

### Changed

- 删除 ~16.8k 行死代码（disk_cache、tiered_cache、tempfile、upgrade、lua mock engine 等）
- 提取公共函数到 testutil/netutil/utils
- 合并冗余 switch、移除 50+ 子目录 AGENTS.md
- fasthttp 升级至 1.71.0，compress 升级至 1.18.6

### Tests

- 安全中间件覆盖率 75.9% → 88.5%
- 服务器模块覆盖率 78.6% → 83.3%
- proxy 覆盖率 71.1% → 80%+
- variable 覆盖率 74.5% → 85%+
- http3 覆盖率 46% → 93.1%
- sslutil 覆盖率 29.2% → 85%+
- adapter 覆盖率 0% → 80%+
- stream 覆盖率 57% → 75%+

### Build

- 删除 GitHub Actions workflow 文件
- 添加 `docs/skills/release.md` 发布流程技能

## [0.2.2] - 2026-04-30

### Added

#### 核心功能

- autoindex 模块：自动目录列表功能
- nginx 配置导入功能（converter），支持 listen、server 级别 root/index、alias、try_files 等指令转换
- internal 指令：防止外部直接访问特定 location，支持 X-Accel-Redirect 内部重定向
- server_tokens 配置：控制 Server 响应头是否显示版本号
- types 配置块：自定义 MIME 类型映射，线程安全实现 (mimeutil)

#### 静态文件服务

- ETag 和 304 Not Modified 支持，减少带宽消耗
- expires 缓存控制支持
- alias 配置支持，路径别名映射
- gzip_static 默认值改为 true

#### 代理与缓存

- 分层缓存架构实现
- stale_if_error 和 stale_if_timeout 缓存回退逻辑
- 健康检查增强和缓存配置扩展
- ProxyBind / ProxyBuffering / ProxyURI 和响应头控制
- 上游服务器参数和 random 负载均衡算法
- 慢启动 (slow start) 负载均衡功能

#### 中间件

- limit_rate 响应速率限制中间件，支持令牌桶算法和大文件回退策略

#### 平台与构建

- act 命令支持本地运行 CI
- E2E 测试框架集成 testcontainers
- 分层测试 CI 工作流和基准测试 GitHub Actions workflow

### Changed

- 日志格式参数从布尔值改为字符串，支持 json / text / console 格式
- 版本信息模块化重构
- 代码现代化：采用 Go 1.22+ 特性（slices/maps 等）
- 提取 HTTP/2/3 适配器公共逻辑为 CommonAdapter
- 提取 TLS 配置生成到 sslutil
- 提取 createFastServer / createProxyForConfig 辅助函数
- 统一 IP 白名单解析和 excludeSet 构建
- 提取公共逻辑、消除重复代码、加强错误处理
- 缓存处理中消除 goto 语句，用互斥锁替代原子操作保护 Target 失败状态
- 增强预压缩配置灵活性，提取常量提高代码可读性
- 精简并重构 README 文档结构

### Performance

- FileInfo 缓存：handleTryFiles 和 handleInternalRedirect 使用缓存减少 stat 调用
- 缓存 ETag 复用：避免重复生成 ETag
- FileInfoCache 使用 RWMutex 替代 Mutex 提升并发读性能
- 预压缩文件存在性缓存：避免重复 os.Stat 调用
- 压缩中间件使用 bytes 操作避免字符串分配
- ConsistentHash 和 RateLimiter 并发优化
- Balancer healthy slice 池化减少分配
- FileEntry 池化减少 GC 压力
- 零分配优化与 Dial timeout 支持
- Cache.Get 方法读锁优先，double-check 升级写锁
- 静态文件服务性能优化

### Fixed

- Lua 引擎并发安全问题
- sync.Pool 指针包装消除装箱分配
- 2 个 CRITICAL + 6 个 HIGH 安全与代码质量问题
- nginx 配置解析空字符串和 upstream URL 问题
- 多 listen 指令的 server 块转换
- 跳过已有 Content-Encoding 的响应压缩
- ProxyBind 拨号超时和 Cookie 属性匹配
- HealthChecker 重启支持
- shutdownServers nil ctx 防御性检查
- dictReplace key 存在性检查逻辑
- ParseRegexPattern nginx 风格正则解析
- remote_port 和 server_port Lua 变量返回空值
- 配置默认值同步 GenerateConfigYAML 输出
- coverage.html gitignore 规则
- 根路径规范化修复相对路径性能问题

### Documentation

- 为多个模块添加标准化 godoc 注释（lua / matcher / proxy / middleware / logging 等）
- 更新 README.md 配置示例和文档
- 更新 AGENTS.md 文档，添加新模块说明
- 添加子目录 AGENTS.md 文档

### Tests

- E2E 测试完整覆盖：代理、静态文件、SSL/TLS、缓存、健康检查、负载均衡、WebSocket
- 缓存和代理集成测试
- DNS 解析器 Mock 测试
- Lua 边界场景和 Scheduler 模式测试
- HTTP/2 E2E 和 TLS 集成测试
- 正则 location 配置集成测试
- 多模块单元测试覆盖率扩展
- 分片缓存原型与扩展性对比测试
- 多项专项基准测试（缓存键零分配、FileCache Set 分配热点、变量展开分配追踪等）

### Build

- 分层测试 CI 工作流
- 基准测试 GitHub Actions workflow
- check 目标改为使用 test-all
- bench 目标限制到 internal 目录

## [0.2.1] - 2026-04-17

### Added

#### 核心功能

- 多服务器模式支持，可在单个配置中定义多个独立 server
- Location 匹配引擎，支持 nginx 风格的精确/前缀/正则匹配
- server_name 通配符和正则匹配支持
- Unix socket 监听支持
- 配置引入 (include) 支持，可拆分配置到多个文件
- 缓存清理 API 端点
- 状态端点 HTML/Text 输出格式，支持 localhost IP 访问
- 上游 SSL 配置，支持代理连接使用自定义证书
- 高并发优化配置项
- 缓存 TTL 新鲜度验证优化
- Location/Refresh 头改写功能
- DNS 缓存 LRU 淘汰机制

#### Lua 中间件系统

- 完整 ngx API 实现：ngx.var/ngx.ctx/ngx.req/ngx.resp/ngx.log
- 共享字典 API (ngx.shared.DICT)
- 定时器 API (ngx.timer)，含线程隔离和回调限制
- 子请求 API (ngx.location.capture)
- Cosocket API 和响应拦截器
- balancer_by_lua 动态负载均衡
- Lua 脚本嵌入支持，含沙箱安全检查

#### 安全与访问控制

- GeoIP 国家代码访问控制
- 符号链接安全检查
- 静态文件处理符号链接安全检查

#### 平台支持

- Docker 容器化部署支持
- Windows 平台构建和运行兼容性

#### 可观测性

- Prometheus 状态输出端点缓存清理 API

### Changed

- 移除旧版单 server 配置，统一使用 servers 数组格式
- 结构体内存布局优化减少 padding
- 提取公共模块：mimeutil、sslutil、netutil、testutil
- 使用标准库 slices/maps 替代自定义函数
- gofumpt 替代 goimports 格式化代码
- 禁用 govet fieldalignment 检查

### Performance

- 零拷贝字节路径减少内存分配
- EphemeralGet/PersistentGet 零拷贝变量访问 API
- LuaContext/协程池对象池化
- 缓存键哈希计算零分配优化
- 全局变量惰性加载降低每请求开销
- 滑动窗口限流器分段锁优化并发性能
- FileCache.Get() 锁粒度优化
- HTTP/3 移除冗余 ctxPool 提升性能

### Fixed

- ClientMaxBodySize 默认值设置
- sendfile 高并发下连接断开处理
- WebSocket 升级失败时的资源泄漏
- 代理请求 URI 和 Host 设置
- BodyStream nil 检查防止空指针
- 负载均衡器 hostnameOnce 并发安全
- SSL 证书验证禁用的安全警告
- 大量 golangci-lint 错误修复

### Documentation

- nginx 源码架构分析文档
- fasthttp 源码分析任务规划
- 重构文档目录结构，nginx 文档移至子目录
- Lua 配置示例和示例脚本
- 更新 README 添加 GeoIP 和 Lua 功能说明

### Tests

- 端到端集成性能基准测试
- HTTP/2/HTTP/3/WebSocket 性能基准测试
- Lua 协程和字节编译性能基准测试
- DNS 解析、状态端点、负载均衡器完整测试
- 静态文件发送处理测试
- 基准测试迁移到 Go 1.24 b.Loop() API

### Build

- Docker 构建支持
- Windows 平台构建兼容性
- golangci-lint 配置重构，重新启用多项检查

---

## [0.2.0] - 2026-04-10

### Added

#### 核心基础设施

- 变量系统模块，支持内置变量、SSL 客户端证书变量、上游变量，自动注入请求上下文
- DNS 解析器模块，支持自定义 DNS 服务器配置，集成到代理请求处理
- HTTP/2 协议支持，集成到服务器和应用层

#### SSL/TLS 增强

- Session Tickets 支持，含密钥轮换和内存/文件存储后端
- mTLS 客户端证书验证，支持可选/强制验证模式
- TCP/UDP Stream SSL/TLS 支持，完整证书配置

#### 中间件与处理

- auth_request 外部认证中间件，支持子请求验证流程
- static handler alias 指令，支持路径别名映射
- 代理响应临时文件处理，保护内存避免大响应OOM
- rate limiting 后台自动清理和优雅关闭

#### 可观测性

- Prometheus 格式状态输出支持
- 缓存清理 API 端点
- 分层性能回归检测策略与基准测试套件

#### 配置与构建

- Resolver/SSL 默认值完善及 YAML 配置示例输出
- 负载均衡算法配置验证
- golangci-lint 静态检查配置
- 所有构建命令启用静态链接支持
- goimports 替代 go fmt 格式化代码

### Changed

- resolver/variable/ssl 等核心类型重命名，移除冗余前缀
- HeadersMiddleware 重命名移除冗余前缀
- HTTP/2 使用 textproto.CanonicalMIMEHeaderKey 替代手动实现
- 配置废弃字段标记与移除

### Performance

- 一致性哈希虚拟节点哈希值预计算
- 代理缓存使用 uint64 哈希键优化性能

### Documentation

- 新增 variable/resolver 等 AGENTS.md 模块文档
- nginx 健康检查详解与高级模块文档
- 配置字段完整参考文档

### Tests

- 变量系统单元测试与基准测试
- SSL Session Tickets/Stream SSL 测试
- try_files、错误页面、pprof 单元测试

---

## [0.1.0] - 2026-04-07

### Added

#### 核心功能

- 反向代理与负载均衡模块，支持轮询/加权/最少连接/一致性哈希算法
- 故障转移 (next_upstream) 支持，自动切换备用上游
- 文件缓存模块，支持缓存元数据管理
- SSL/TLS 支持，强制 TLS 1.2+，支持证书配置
- HTTP/3 (QUIC) 支持，含 0-RTT 与性能配置验证
- 静态文件服务，支持多静态目录、路径前缀匹配、try_files 配置

#### 中间件系统

- URL 重写中间件，含 ReDoS 保护与循环检测
- gzip/deflate/Brotli 响应压缩中间件
- 请求体大小限制中间件
- 自定义错误页面支持
- 访问日志中间件，支持 nginx combined 格式
- 安全中间件（访问控制、可信代理配置）

#### 可观测性

- pprof 性能分析端点
- 访问/错误日志分离，支持全局格式配置
- 服务器状态 API

#### 配置与构建

- 配置加载模块，支持 YAML/CLI 参数
- 配置验证功能，多项验证函数
- Makefile 构建脚本，含基准测试基础设施
- 程序信号处理（优雅关闭、热升级）

### Changed

- 统一错误处理风格，空白标识符忽略明确不关心的返回值
- 抽取网络工具函数到 netutil 包，移除冗余代码
- 优化字符串构建方式，使用 fmt.Fprintf 替代冗余写法
- 增强 FlagLast 语义与循环检测

### Fixed

- 配置与代码实现不一致问题修复
- Phase 8 问题修复与功能完善

### Documentation

- 添加项目 README 文档
- 核心模块 GoDoc 文档注释
- nginx 模块翻译文档 (Lua/安全/API网关/动态配置/ACME 指南)
- 模块上下文文档 (AGENTS.md)
- 开发计划文档

### Tests

- handler/logging/middleware/server/proxy/cache/loadbalance/security 等模块单元测试
- 核心模块基准测试与回归检测

---

## Initial Development - 2026-04-02 to 2026-04-03

### Project Initialization

- 项目初始化，添加 nginx 文档作为参考
- Makefile 构建脚本与程序入口
- 配置加载模块与 CLI 参数解析
- 基础 HTTP 服务器核心功能

### Core Modules (Phase 1-4)

- 应用逻辑抽取到 internal/app 包
- 信号处理与配置结构完善
- 反向代理与负载均衡实现
- SSL/TLS 与安全中间件
- 日志模块增强
- 文件缓存实现
- URL 重写与压缩中间件

### Performance & Integration (Phase 5-7)

- 访问日志中间件
- 性能优化与热升级
- 访问控制与可信代理配置
- Phase 6-7 功能完善与测试覆盖

### HTTP/3 & Advanced Features (Phase 8-9)

- HTTP/3 (QUIC) 支持
- 配置验证增强
- Brotli 压缩支持
- pprof 性能分析端点
- 故障转移支持
- 自定义错误页面
- 请求体大小限制
- try_files 配置支持
