# NGINX 反向代理与负载均衡指南

## 1. 反向代理基础

### 什么是反向代理

反向代理服务器接收客户端请求，将请求转发给后端服务器，获取响应后返回给客户端。NGINX 作为反向代理可以：

- 隐藏后端服务器真实地址
- 实现负载均衡
- 缓存响应内容
- SSL 终端加密
- 压缩响应内容
- 请求路由与重写

### 基础配置示例

```nginx
server {
    listen 80;
    server_name example.com;

    location / {
        proxy_pass http://backend.example.com:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

---

## 2. proxy_pass 指令详解

### 语法

`proxy_pass URL;`

URL 可以是：
- HTTP 地址：`http://backend:8080`
- HTTPS 地址：`https://backend:8443`
- Unix Socket：`unix:/tmp/backend.socket`
- upstream 组：`http://backend_group`
- 变量：`http://$backend`

### URI 传递规则

**带 URI 的 proxy_pass**：请求 URI 中匹配 location 的部分会被替换。

```nginx
location /name/ {
    proxy_pass http://127.0.0.1/remote/;
    # /name/test -> /remote/test
}

location /api/ {
    proxy_pass http://backend/v1/;
    # /api/users -> /v1/users
}
```

**不带 URI 的 proxy_pass**：请求 URI 以原始形式传递。

```nginx
location /some/path/ {
    proxy_pass http://127.0.0.1;
    # /some/path/test -> /some/path/test
}
```

**使用变量**：

```nginx
location / {
    proxy_pass http://$backend;
    # 需要配合 resolver 指令解析域名
}

resolver 10.0.0.1 valid=300s;
```

---

## 3. 请求头设置

### proxy_set_header

设置传递给后端服务器的请求头。

```nginx
proxy_set_header Host $host;                   # 传递原始 Host
proxy_set_header Host $http_host;              # 传递 Host 头（含端口）
proxy_set_header Host backend.example.com;     # 固定 Host 值

proxy_set_header X-Real-IP $remote_addr;       # 客户端真实 IP
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;  # 代理链
proxy_set_header X-Forwarded-Proto $scheme;    # 原始协议

# 删除请求头
proxy_set_header Accept-Encoding "";           # 删除该字段
```

### 默认行为

| 头字段 | 默认值 |
|--------|--------|
| `Host` | `$proxy_host`（proxy_pass 中的地址） |
| `Connection` | `close` |

---

## 4. 负载均衡配置

### upstream 块定义

```nginx
upstream backend {
    server backend1.example.com weight=5;
    server backend2.example.com:8080;
    server 192.168.0.1:8080 max_fails=3 fail_timeout=30s;
    server backend3.example.com backup;        # 备份服务器
    server unix:/tmp/backend4;
}

server {
    location / {
        proxy_pass http://backend;
    }
}
```

### 负载均衡算法

| 算法 | 指令 | 说明 |
|------|------|------|
| **轮询** | 默认 | 请求依次分发（加权） |
| **最少连接** | `least_conn;` | 分配给活动连接最少的服务器 |
| **IP Hash** | `ip_hash;` | 同一客户端 IP 始终路由到同一服务器 |
| **Hash** | `hash key [consistent];` | 基于指定键哈希，支持一致性哈希 |
| **随机** | `random [two [method]];` | 随机选择，two 表示选两台再择优 |

### 配置示例

**轮询（默认）**：
```nginx
upstream backend {
    server srv1.example.com;
    server srv2.example.com;
    server srv3.example.com;
}
```

**加权轮询**：
```nginx
upstream backend {
    server srv1.example.com weight=5;  # 5/7 的请求
    server srv2.example.com weight=2;  # 2/7 的请求
    server srv3.example.com;           # 1/7 的请求（默认 weight=1）
}
```

**最少连接**：
```nginx
upstream backend {
    least_conn;
    server srv1.example.com;
    server srv2.example.com;
    server srv3.example.com;
}
```

**IP Hash（会话持久性）**：
```nginx
upstream backend {
    ip_hash;
    server srv1.example.com;
    server srv2.example.com;
    server srv3.example.com;
}
```

**一致性哈希**：
```nginx
upstream backend {
    hash $request_uri consistent;
    server srv1.example.com;
    server srv2.example.com;
    server srv3.example.com;
}
```

**随机负载均衡（1.15.1+）**：
```nginx
upstream backend {
    random;                              # 纯随机选择
    server srv1.example.com;
    server srv2.example.com;
    server srv3.example.com;
}

# Power of Two Choices 算法（更智能）
upstream backend {
    random two;                          # 随机选两台，按权重择优
    server srv1.example.com;
    server srv2.example.com;
    server srv3.example.com;
}

# 结合最少连接策略
upstream backend {
    random two least_conn;               # 随机选两台，选连接数少的
    server srv1.example.com;
    server srv2.example.com;
}
```

**random 算法参数说明**：

| 参数 | 说明 |
|------|------|
| `two` | 随机选择两台服务器，再根据策略择优 |
| `least_conn` | 与 `two` 配合，选择连接数较少的服务器 |
| `least_time=header` | 与 `two` 配合，选择响应头时间最短的服务器（NGINX Plus）|
| `least_time=last_byte` | 与 `two` 配合，选择完整响应时间最短的服务器（NGINX Plus）|

**适用场景**：
- 多个负载均衡器共享后端时避免锁竞争
- 对一致性要求不高但需要低延迟的场景
- 配合 `zone` 实现无锁负载均衡

### server 指令参数

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `weight=N` | 权重值 | 1 |
| `max_conns=N` | 最大并发连接数 | 0（无限制） |
| `max_fails=N` | 失败次数阈值 | 1 |
| `fail_timeout=T` | 失败统计时间及不可用持续时间 | 10s |
| `backup` | 备份服务器（主服务器不可用时使用） | - |
| `down` | 标记为永久不可用 | - |
| `resolve` | 监控域名 IP 变化（需 zone + resolver） | - |

```nginx
upstream backend {
    zone backend 64k;
    resolver 10.0.0.1;

    server backend1.example.com weight=5 max_fails=3 fail_timeout=30s;
    server backend2.example.com resolve;
    server backup1.example.com backup;
}
```

---

## 5. 健康检查

### 被动健康检查（内置）

```nginx
upstream backend {
    server srv1.example.com max_fails=3 fail_timeout=30s;
    server srv2.example.com max_fails=3 fail_timeout=30s;
}
```

**机制**：
- 在 `fail_timeout` 时间内连续失败 `max_fails` 次，服务器标记为不可用
- `fail_timeout` 时间后再次尝试

### 主动健康检查（NGINX Plus）

```nginx
upstream backend {
    zone backend 64k;

    server srv1.example.com;
    server srv2.example.com;

    health_check interval=5s fails=3 passes=2;
    health_check uri=/health;
}
```

---

## 6. 超时配置

### 主要超时指令

| 指令 | 说明 | 默认值 |
|------|------|--------|
| `proxy_connect_timeout` | 建立连接超时 | 60s |
| `proxy_send_timeout` | 传输请求超时 | 60s |
| `proxy_read_timeout` | 读取响应超时 | 60s |

```nginx
location / {
    proxy_connect_timeout 5s;
    proxy_send_timeout 10s;
    proxy_read_timeout 30s;
    proxy_pass http://backend;
}
```

---

## 7. 缓冲配置

### 响应缓冲

```nginx
proxy_buffering on;                  # 默认 on
proxy_buffer_size 4k;                # 响应头缓冲区大小
proxy_buffers 8 16k;                 # 响应体缓冲区数量和大小
proxy_busy_buffers_size 32k;         # 同时发送给客户端的缓冲区总大小
proxy_max_temp_file_size 1024m;      # 临时文件最大大小
proxy_temp_file_write_size 64k;      # 每次写入临时文件大小
```

### 禁用缓冲（实时传输）

```nginx
location /stream/ {
    proxy_buffering off;
    proxy_pass http://backend;
}
```

---

## 8. 缓存配置

### 缓存路径定义

```nginx
http {
    proxy_cache_path /data/nginx/cache
        levels=1:2                    # 目录层级（1:2 表示 16*256 个子目录）
        keys_zone=one:10m             # 共享内存区名称和大小（1MB 约 8000 个键）
        inactive=60m                  # 非活动数据保留时间
        max_size=1g                   # 缓存最大大小
        use_temp_path=off;            # 临时文件存放位置
}
```

### 启用缓存

```nginx
server {
    location / {
        proxy_cache one;              # 使用定义的缓存区
        proxy_cache_key "$host$request_uri";  # 缓存键
        proxy_cache_valid 200 302 10m;        # 200/302 响应缓存 10 分钟
        proxy_cache_valid 404 1m;             # 404 响应缓存 1 分钟
        proxy_cache_valid any 1m;             # 其他响应缓存 1 分钟
        proxy_pass http://backend;
    }
}
```

### 缓存条件控制

```nginx
# 不从缓存获取响应的条件
proxy_cache_bypass $cookie_nocache $arg_nocache;

# 不将响应保存到缓存的条件
proxy_no_cache $http_pragma $http_authorization;
```

### 使用过期缓存

```nginx
proxy_cache_use_stale error timeout updating http_500 http_502 http_503 http_504;
# 在后端错误、超时、正在更新时使用过期缓存
```

### 缓存锁

```nginx
proxy_cache_lock on;                 # 同时刻只允许一个请求填充缓存
proxy_cache_lock_timeout 5s;         # 锁超时时间
```

---

## 9. 故障转移

### proxy_next_upstream

定义在何种情况下将请求传递给下一台服务器。

```nginx
proxy_next_upstream error timeout invalid_header http_500 http_502 http_503 http_504;
proxy_next_upstream_timeout 30s;     # 限制总时间
proxy_next_upstream_tries 3;         # 限制尝试次数
```

**条件类型**：

| 条件 | 说明 |
|------|------|
| `error` | 与后端建立连接出错 |
| `timeout` | 连接、传输或读取超时 |
| `invalid_header` | 后端返回空或无效响应头 |
| `http_XXX` | 后端返回指定状态码 |
| `non_idempotent` | 非幂等请求（POST、LOCK）也进行重试 |

---

## 10. SSL/HTTPS 代理

### 代理到 HTTPS 后端

```nginx
location / {
    proxy_pass https://backend.example.com;
    proxy_ssl_verify on;                         # 验证后端证书
    proxy_ssl_trusted_certificate /path/to/ca.crt;
    proxy_ssl_verify_depth 2;
    proxy_ssl_server_name on;                    # 启用 SNI
}
```

### 代理 SSL 配置

| 指令 | 说明 | 默认值 |
|------|------|--------|
| `proxy_ssl` | 启用 HTTPS 代理 | off |
| `proxy_ssl_protocols` | 启用的协议 | TLSv1.2 TLSv1.3 |
| `proxy_ssl_ciphers` | 加密套件 | DEFAULT |
| `proxy_ssl_verify` | 验证后端证书 | off |
| `proxy_ssl_verify_depth` | 验证深度 | 1 |
| `proxy_ssl_server_name` | 启用 SNI | off |
| `proxy_ssl_certificate` | 客户端证书 | - |
| `proxy_ssl_certificate_key` | 客户端密钥 | - |

---

## 11. WebSocket 代理

### 基础配置

```nginx
location /chat/ {
    proxy_pass http://backend;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
}
```

### 动态处理

```nginx
http {
    map $http_upgrade $connection_upgrade {
        default upgrade;
        ''      close;
    }

    server {
        location /chat/ {
            proxy_pass http://backend;
            proxy_http_version 1.1;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection $connection_upgrade;
            proxy_read_timeout 3600s;  # 增加超时时间
        }
    }
}
```

---

## 12. 高级代理指令

### proxy_bind

指定连接后端时使用的源地址，用于多网卡服务器选择出口IP。

```nginx
语法: proxy_bind address [transparent];
默认: —
上下文: http, server, location
```

```nginx
proxy_bind $server_addr;              # 使用服务器IP
proxy_bind 192.168.1.1 transparent;   # 透明代理（需要root权限）
```

### proxy_intercept_errors

拦截后端错误响应，配合 error_page 自定义错误页面。

```nginx
语法: proxy_intercept_errors on | off;
默认: off
上下文: http, server, location
```

```nginx
proxy_intercept_errors on;
error_page 500 502 503 504 /50x.html;
```

### proxy_hide_header / proxy_pass_header

控制后端响应头的传递行为：

```nginx
# 隐藏后端返回的特定头
proxy_hide_header X-Powered-By;
proxy_hide_header X-Runtime;

# 传递被默认隐藏的头
proxy_pass_header X-Accel-Redirect;
proxy_pass_header X-Accel-Limit-Rate;
```

### proxy_ignore_headers

忽略后端的特定响应头（如缓存控制），允许NGINX处理这些头：

```nginx
proxy_ignore_headers Cache-Control Expires X-Accel-Redirect X-Accel-Expires;
```

### proxy_cookie_* 系列

修改后端返回的 Set-Cookie 头：

```nginx
# 修改域名
proxy_cookie_domain localhost example.com;
proxy_cookie_domain off;              # 禁用域名修改

# 修改路径
proxy_cookie_path /foo/ /bar/;
proxy_cookie_path off;                # 禁用路径修改

# 添加安全标志
proxy_cookie_flags session httponly secure samesite=strict;
proxy_cookie_flags * samesite=lax;    # 应用到所有cookie
```

### proxy_limit_rate

限制从后端读取响应的传输速率：

```nginx
proxy_limit_rate 100k;                # 100KB/s
```

### proxy_request_buffering

控制请求是否先完整缓冲再发送到后端：

```nginx
proxy_request_buffering on;           # 默认，完整缓冲
proxy_request_buffering off;          # 流式传输，支持上传进度
```

lolly 对应配置为 `buffering.request_mode`（取值 `default`/`on`/`off`）。
`off` 时请求体在 客户端→lolly→上游 全程不落全量内存，适合大文件上传。
注意：带请求体的请求会禁用 `next_upstream` 重试（流不可回放），
`$request_body` 变量为空（与 nginx 行为一致）。

### proxy_redirect

修改后端返回的重定向头 Location 和 Refresh：

```nginx
proxy_redirect default;                                    # 使用默认替换
proxy_redirect off;                                        # 禁用替换
proxy_redirect http://localhost:8080/ http://$host/;       # 自定义替换
proxy_redirect ~^http://([^/]+)/(.+)$ http://$host/$2;      # 使用正则
```

---

## 13. SSL 客户端证书认证 (proxy_ssl_*)

用于 mTLS 双向认证场景，NGINX 作为客户端向后端提供证书：

```nginx
location / {
    proxy_pass https://backend.example.com;

    # mTLS 双向认证
    proxy_ssl_certificate /path/to/client.crt;
    proxy_ssl_certificate_key /path/to/client.key;

    # 验证后端证书
    proxy_ssl_verify on;
    proxy_ssl_trusted_certificate /path/to/ca.crt;
    proxy_ssl_verify_depth 2;

    # SSL协议和加密套件
    proxy_ssl_protocols TLSv1.2 TLSv1.3;
    proxy_ssl_ciphers HIGH:!aNULL;

    # 会话复用
    proxy_ssl_session_reuse on;

    # SNI支持
    proxy_ssl_server_name on;
    proxy_ssl_name backend.example.com;
}
```

| 指令 | 说明 | 默认值 |
|------|------|--------|
| `proxy_ssl_certificate` | 客户端证书路径 | — |
| `proxy_ssl_certificate_key` | 客户端私钥路径 | — |
| `proxy_ssl_verify` | 验证后端证书 | off |
| `proxy_ssl_trusted_certificate` | 受信任CA证书 | — |
| `proxy_ssl_verify_depth` | 验证深度 | 1 |
| `proxy_ssl_protocols` | 启用的协议 | TLSv1.2 TLSv1.3 |
| `proxy_ssl_ciphers` | 加密套件 | DEFAULT |
| `proxy_ssl_session_reuse` | 会话复用 | on |
| `proxy_ssl_name` | SNI名称 | — |

---

## 14. 高级缓存指令

### proxy_cache_methods

指定可缓存的请求方法：

```nginx
proxy_cache_methods GET HEAD POST;    # 可缓存 POST 请求
```

### proxy_cache_min_uses

设置最小访问次数才开始缓存，避免缓存低频请求：

```nginx
proxy_cache_min_uses 3;               # 第3次访问才开始缓存
```

### proxy_cache_background_update

后台异步更新过期缓存（类似 stale-while-revalidate）：

```nginx
proxy_cache_background_update on;
```

### proxy_cache_revalidate

使用 If-Modified-Since 和 If-None-Match 重新验证缓存：

```nginx
proxy_cache_revalidate on;            # 减少数据传输
```

### proxy_cache_convert_head

自动将 HEAD 请求转为 GET 以获取响应体：

```nginx
proxy_cache_convert_head on;          # 默认启用
```

### proxy_cache_purge

支持 PURGE 方法清除缓存（需编译时启用）：

```nginx
location ~ /purge(/.*) {
    proxy_cache_purge cache_zone $1;
}
```

---

## 15. FastCGI 代理

### 基础配置

```nginx
location ~ \.php$ {
    fastcgi_pass  localhost:9000;
    fastcgi_index index.php;
    fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
    include fastcgi_params;
}
```

### FastCGI 指令完整列表

| 指令 | 语法 | 默认值 | 上下文 |
|------|------|--------|--------|
| `fastcgi_pass` | fastcgi_pass address; | — | location |
| `fastcgi_index` | fastcgi_index name; | — | http, server, location |
| `fastcgi_param` | fastcgi_param parameter value [if_not_empty]; | — | http, server, location |
| `fastcgi_split_path_info` | fastcgi_split_path_info regex; | — | location |
| `fastcgi_buffer_size` | fastcgi_buffer_size size; | 4k/8k | http, server, location |
| `fastcgi_buffers` | fastcgi_buffers number size; | 8 4k/8k | http, server, location |
| `fastcgi_busy_buffers_size` | fastcgi_busy_buffers_size size; | 8k/16k | http, server, location |
| `fastcgi_temp_file_write_size` | fastcgi_temp_file_write_size size; | 8k/16k | http, server, location |
| `fastcgi_temp_path` | fastcgi_temp_path path [level1 [level2 [level3]]]; | — | http, server, location |
| `fastcgi_cache` | fastcgi_cache zone; | — | http, server, location |
| `fastcgi_cache_key` | fastcgi_cache_key string; | — | http, server, location |
| `fastcgi_cache_valid` | fastcgi_cache_valid [code...] time; | — | http, server, location |
| `fastcgi_cache_methods` | fastcgi_cache_methods method...; | GET HEAD | http, server, location |
| `fastcgi_cache_min_uses` | fastcgi_cache_min_uses number; | 1 | http, server, location |
| `fastcgi_cache_bypass` | fastcgi_cache_bypass string...; | — | http, server, location |
| `fastcgi_no_cache` | fastcgi_no_cache string...; | — | http, server, location |
| `fastcgi_cache_use_stale` | fastcgi_cache_use_stale condition...; | — | http, server, location |
| `fastcgi_cache_background_update` | fastcgi_cache_background_update on/off; | off | http, server, location |
| `fastcgi_cache_revalidate` | fastcgi_cache_revalidate on/off; | off | http, server, location |
| `fastcgi_cache_lock` | fastcgi_cache_lock on/off; | off | http, server, location |
| `fastcgi_cache_lock_timeout` | fastcgi_cache_lock_timeout time; | 5s | http, server, location |
| `fastcgi_cache_convert_head` | fastcgi_cache_convert_head on/off; | on | http, server, location |
| `fastcgi_connect_timeout` | fastcgi_connect_timeout time; | 60s | http, server, location |
| `fastcgi_send_timeout` | fastcgi_send_timeout time; | 60s | http, server, location |
| `fastcgi_read_timeout` | fastcgi_read_timeout time; | 60s | http, server, location |
| `fastcgi_send_lowat` | fastcgi_send_lowat size; | 0 | http, server, location |
| `fastcgi_request_buffering` | fastcgi_request_buffering on/off; | on | http, server, location |
| `fastcgi_intercept_errors` | fastcgi_intercept_errors on/off; | off | http, server, location |
| `fastcgi_hide_header` | fastcgi_hide_header field; | — | http, server, location |
| `fastcgi_pass_header` | fastcgi_pass_header field; | — | http, server, location |
| `fastcgi_ignore_headers` | fastcgi_ignore_headers field...; | — | http, server, location |
| `fastcgi_limit_rate` | fastcgi_limit_rate rate; | 0 | http, server, location |

### FastCGI 缓存完整配置示例

```nginx
http {
    # 缓存路径定义
    fastcgi_cache_path /var/cache/nginx/php
        levels=1:2
        keys_zone=php:10m
        max_size=100m
        inactive=60m
        use_temp_path=off;

    server {
        listen 80;
        server_name example.com;

        location ~ \.php$ {
            # 启用缓存
            fastcgi_cache php;
            fastcgi_cache_key "$scheme$request_method$host$request_uri";

            # 缓存有效期
            fastcgi_cache_valid 200 302 1h;
            fastcgi_cache_valid 404 1m;
            fastcgi_cache_valid any 5m;

            # 使用过期缓存
            fastcgi_cache_use_stale error timeout updating http_500 http_503;

            # 后台更新
            fastcgi_cache_background_update on;

            # 重新验证
            fastcgi_cache_revalidate on;

            # 缓存锁
            fastcgi_cache_lock on;
            fastcgi_cache_lock_timeout 5s;

            # 绕过缓存条件
            fastcgi_cache_bypass $cookie_nocache $arg_nocache;
            fastcgi_no_cache $http_pragma $http_authorization;

            # FastCGI 后端
            fastcgi_pass unix:/run/php/php-fpm.sock;
            fastcgi_index index.php;
            fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;

            # 超时设置
            fastcgi_connect_timeout 5s;
            fastcgi_send_timeout 60s;
            fastcgi_read_timeout 60s;

            # 缓冲配置
            fastcgi_buffer_size 16k;
            fastcgi_buffers 8 16k;
            fastcgi_busy_buffers_size 32k;

            # 错误处理
            fastcgi_intercept_errors on;

            include fastcgi_params;
        }
    }
}
```

---

## 16. 内置变量

### 代理相关变量

| 变量 | 说明 |
|------|------|
| `$proxy_host` | proxy_pass 中的服务器名称和端口 |
| `$proxy_port` | proxy_pass 中的端口 |
| `$proxy_add_x_forwarded_for` | X-Forwarded-For 头 + 客户端 IP |

### Upstream 响应时间变量（用于性能监控）

| 变量 | 说明 | 单位 |
|------|------|------|
| `$upstream_addr` | 上游服务器地址（IP:端口）| - |
| `$upstream_connect_time` | 与上游建立连接的时间（含 SSL 握手）| 秒 |
| `$upstream_header_time` | 接收到上游响应头的时间 | 秒 |
| `$upstream_response_time` | 完整响应时间（从建立连接到接收完成）| 秒 |
| `$upstream_response_length` | 上游响应体长度 | 字节 |
| `$upstream_bytes_received` | 从上游接收的总字节数 | 字节 |
| `$upstream_bytes_sent` | 发送到上游的总字节数 | 字节 |
| `$upstream_status` | 上游返回的 HTTP 状态码 | - |
| `$upstream_cache_status` | 缓存命中状态（HIT/MISS/EXPIRED 等）| - |
| `$upstream_queue_time` | 请求在队列中等待的时间（NGINX Plus）| 秒 |

**日志格式中使用响应时间变量**：

```nginx
log_format detailed '$remote_addr - $remote_user [$time_local] '
                    '"$request" $status $body_bytes_sent '
                    '"$http_referer" "$http_user_agent" '
                    'rt=$request_time '
                    'uct="$upstream_connect_time" '
                    'uht="$upstream_header_time" '
                    'urt="$upstream_response_time" '
                    'upstream=$upstream_addr '
                    'upstream_status=$upstream_status '
                    'upstream_bytes=$upstream_response_length';

access_log /var/log/nginx/access.log detailed;
```

**响应时间变量解读**：

```
请求时间线:
客户端 ──▶ NGINX ──▶ 连接上游 ──▶ 发送请求 ──▶ 接收响应头 ──▶ 接收响应体 ──▶ 客户端
              │           │              │                │                │
              │           │              │                │                │
              └───────────┴──────────────┴────────────────┴────────────────┘
                          │              │                │
                    $upstream_     $upstream_       $upstream_
                    connect_time   header_time      response_time
```

- `$upstream_connect_time`：TCP 连接 + SSL 握手时间
- `$upstream_header_time`：从开始到收到响应头
- `$upstream_response_time`：完整请求处理时间
- `$request_time`：从客户端发起请求到响应完成（包含所有上游）

**基于响应时间的告警配置示例**：

```nginx
# 慢请求日志
map $upstream_response_time $slow_log {
    default 0;
    "~^[2-9]\." 1;    # 2秒以上
    "~^[0-9]{2,}" 1;  # 10秒以上
}

server {
    location /api/ {
        # 记录慢请求
        access_log /var/log/nginx/slow.log detailed if=$slow_log;
        proxy_pass http://backend;
    }
}
```

---

## 17. 综合配置示例

```nginx
http {
    upstream backend {
        zone backend 64k;
        least_conn;

        server backend1.example.com weight=5 max_fails=3 fail_timeout=30s;
        server backend2.example.com resolve;
        server backup.example.com backup;

        keepalive 32;
    }

    proxy_cache_path /data/nginx/cache levels=1:2 keys_zone=main:10m inactive=60m max_size=1g;

    server {
        listen 80;
        server_name example.com;

        location / {
            proxy_pass http://backend;
            proxy_http_version 1.1;
            proxy_set_header Connection "";

            proxy_set_header Host $host;
            proxy_set_header X-Real-IP $remote_addr;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;

            proxy_connect_timeout 5s;
            proxy_read_timeout 30s;

            proxy_cache main;
            proxy_cache_key "$host$request_uri";
            proxy_cache_valid 200 10m;

            proxy_next_upstream error timeout http_502 http_503;
        }

        location /api/ {
            proxy_pass http://backend;
            proxy_set_header Host $host;
            proxy_buffering off;
        }

        location /ws/ {
            proxy_pass http://backend;
            proxy_http_version 1.1;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection $connection_upgrade;
            proxy_read_timeout 3600s;
        }
    }
}
```

---

## 18. 主动健康检查详解

### 18.1 被动检查 vs 主动检查

| 特性 | 被动健康检查 (Passive) | 主动健康检查 (Active) |
|------|----------------------|---------------------|
| **实现方式** | 基于真实客户端请求响应判断 | 独立的探测请求周期性检测 |
| **触发时机** | 实际请求失败时 | 按配置间隔主动发起 |
| **资源占用** | 无额外开销 | 需要额外的连接和请求 |
| **发现速度** | 慢（依赖真实流量） | 快（独立探测） |
| **可用性** | 开源 NGINX 内置 | NGINX Plus 商业版 / 第三方模块 |
| **配置位置** | `server` 指令参数 | `upstream` 块或 `location` 指令 |
| **典型参数** | `max_fails`, `fail_timeout` | `interval`, `fails`, `passes`, `match` |

**被动检查机制**：
```nginx
upstream backend {
    # 在 fail_timeout(30s) 内连续失败 max_fails(3) 次，标记为不可用
    server srv1.example.com max_fails=3 fail_timeout=30s;
}
```

**主动检查优势**：
- 不依赖真实客户端流量即可检测后端状态
- 可以检测特定的健康检查端点（如 `/health`）
- 支持自定义匹配规则验证响应内容
- 支持 gRPC、TCP、UDP 等多种协议

### 18.2 HTTP 健康检查指令详解 (NGINX Plus)

**注意**：HTTP 主动健康检查模块 (`ngx_http_upstream_hc_module`) 是 NGINX Plus 商业订阅的一部分。

#### health_check 指令

**语法**：`health_check [parameters];`
**上下文**：`location`
**功能**：启用 upstream 服务器组的定期健康检查

**参数说明**：

| 参数 | 语法 | 默认值 | 说明 |
|------|------|--------|------|
| `interval` | `interval=time` | `5s` | 检查间隔时间 |
| `jitter` | `jitter=time` | — | 随机延迟时间，避免多个服务器同时检查 |
| `fails` | `fails=number` | `1` | 连续失败次数判定为不健康 |
| `passes` | `passes=number` | `1` | 连续成功次数判定为健康 |
| `uri` | `uri=uri` | `/` | 健康检查请求的 URI |
| `port` | `port=number` | 服务器端口 | 健康检查使用的端口 |
| `match` | `match=name` | — | 引用 `match` 块进行响应验证 |
| `mandatory` | `mandatory [persistent]` | — | 初始状态为 "checking"；`persistent` 在 reload 后保持状态 |
| `keepalive_time` | `keepalive_time=time` | — | 启用健康检查连接的 keepalive |
| `type=grpc` | `type=grpc [grpc_service=name] [grpc_status=code]` | — | 启用 gRPC 健康检查 |

#### match 指令

**语法**：`match name { ... }`
**上下文**：`http`
**功能**：定义响应验证测试集

**测试项**：

| 测试项 | 语法 | 说明 |
|--------|------|------|
| `status` | `status [!] code [code...]` | 状态码匹配，支持范围如 `200-399` |
| `header` | `header header [operator] value` | 响应头匹配，`=` 精确匹配，`~` 正则匹配 |
| `body` | `body ~ "regex"` | 响应体正则匹配（只检查前 256KB） |
| `require` | `require $variable` | 变量非空且不为 "0" |

**header 操作符**：
- `=` 或 `==`：精确相等
- `!=`：不相等
- `~`：正则匹配（区分大小写）
- `~*`：正则匹配（不区分大小写）

#### 配置示例

**基础健康检查**：
```nginx
upstream dynamic {
    zone upstream_dynamic 64k;  # 共享内存区必须

    server backend1.example.com weight=5;
    server backend2.example.com:8080 fail_timeout=5s slow_start=30s;
}

server {
    location / {
        proxy_pass http://dynamic;
        health_check;  # 使用默认配置
    }
}
```

**高级配置**：
```nginx
server {
    location / {
        proxy_pass http://backend;
        health_check interval=10s jitter=2s fails=3 passes=2
                     uri=/health port=8080 match=server_ok
                     keepalive_time=60s;
    }
}

match server_ok {
    status 200;                              # 状态码必须是 200
    header Content-Type = application/json;  # Content-Type 精确匹配
    header X-Health-Status ~ ^ok$;          # 正则匹配头值
    body ~ "\"status\":\\s*\"healthy\"";      # 响应体包含状态标记
}
```

**gRPC 健康检查**（不兼容 `uri` 和 `match`）：
```nginx
upstream grpc_backend {
    zone grpc_zone 64k;
    server grpc1.example.com:50051;
    server grpc2.example.com:50051;
}

server {
    location / {
        grpc_pass grpc://grpc_backend;
        health_check mandatory type=grpc grpc_service=myapp.HealthCheck grpc_status=12;
    }
}
```

### 18.3 Stream 健康检查指令详解 (NGINX Plus)

**注意**：Stream 主动健康检查模块 (`ngx_stream_upstream_hc_module`) 是 NGINX Plus 商业订阅的一部分。

#### 指令概览

| 指令 | 上下文 | 默认值 | 说明 |
|------|--------|--------|------|
| `health_check` | `server` | — | 启用健康检查 |
| `health_check_timeout` | `stream`, `server` | `5s` | 健康检查超时 |
| `match` | `stream` | — | 定义响应验证规则 |

#### health_check 参数（Stream）

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `interval` | `5s` | 检查间隔 |
| `jitter` | — | 随机延迟 |
| `fails` | `1` | 失败次数阈值 |
| `passes` | `1` | 成功次数阈值 |
| `match` | — | 引用 match 块 |
| `port` | 服务器端口 | 检查端口 |
| `udp` | — | 使用 UDP 协议 |
| `mandatory` | — | 初始状态为 "checking" |
| `persistent` | — | reload 后保持状态 |

#### match 块（Stream）

| 测试项 | 语法 | 说明 |
|--------|------|------|
| `send` | `send "string"` | 发送给服务器的字符串（支持 `\x` 十六进制） |
| `expect` | `expect "string"` / `expect ~ "regex"` | 期望的响应 |

**注意**：只检查服务器返回数据的前 `proxy_buffer_size` 字节。

#### 配置示例

**TCP 基础检查**：
```nginx
upstream tcp_backend {
    zone tcp_zone 64k;
    server backend1.example.com:12345 weight=5;
    server backend2.example.com:12345;
}

server {
    listen 12346;
    proxy_pass tcp_backend;
    health_check interval=5s;
}
```

**UDP 健康检查**：
```nginx
upstream dns_upstream {
    zone dns_zone 64k;
    server dns1.example.com:53;
}

server {
    listen 53 udp;
    proxy_pass dns_upstream;
    health_check udp interval=3s;  # 发送探测并期望无 ICMP 不可达回复
}
```

**自定义匹配规则（MySQL 检查）**：
```nginx
upstream mysql_backend {
    zone mysql_zone 10m;
    server db1.example.com:3306;
    server db2.example.com:3306;
}

match mysql_handshake {
    # 发送 MySQL 握手包（十六进制）
    send "\x3a\x00\x00\x01\x0a\x35\x2e\x35\x2e\x32\x2d\x6d\x32\x00\x01...";
    # 期望收到包含版本信息的响应
    expect ~ "\x4a\x00\x00\x00\x0a";
}

server {
    listen 3307;
    proxy_pass mysql_backend;
    health_check match=mysql_handshake interval=5s;
    health_check_timeout 10s;
}
```

**HTTP 风格的 TCP 检查**：
```nginx
match http_check {
    send     "GET /health HTTP/1.0\r\nHost: localhost\r\n\r\n";
    expect ~ "200 OK";
}

server {
    listen 80;
    proxy_pass backend;
    health_check match=http_check interval=5s fails=3 passes=2;
}
```

### 18.4 自定义健康检查配置示例

#### 场景一：API 网关健康检查

```nginx
http {
    upstream api_backend {
        zone api_zone 64k;

        server api1.example.com:8080;
        server api2.example.com:8080;
        server api3.example.com:8080;
    }

    # 健康检查匹配规则
    match api_healthy {
        status 200;
        header Content-Type = application/json;
        body ~ "\"status\":\\s*\"up\"";
    }

    server {
        listen 80;
        server_name api.example.com;

        location / {
            proxy_pass http://api_backend;
            proxy_set_header Host $host;
            proxy_set_header X-Real-IP $remote_addr;

            # 健康检查配置
            health_check interval=5s jitter=1s fails=3 passes=2
                         uri=/api/health
                         match=api_healthy;
        }

        # 健康检查状态页（NGINX Plus）
        location /upstream_status {
            upstream_status;
            access_log off;
            allow 10.0.0.0/8;
            deny all;
        }
    }
}
```

#### 场景二：多协议混合检查

```nginx
# TCP 服务健康检查
stream {
    upstream redis_backend {
        zone redis_zone 64k;
        server redis1.example.com:6379;
        server redis2.example.com:6379;
    }

    match redis_ping {
        send "PING\r\n";
        expect ~ "\+PONG";
    }

    server {
        listen 6379;
        proxy_pass redis_backend;
        health_check match=redis_ping interval=10s fails=2 passes=2;
        health_check_timeout 3s;
    }
}

# HTTP 服务健康检查
http {
    upstream web_backend {
        zone web_zone 64k;
        server web1.example.com:80;
        server web2.example.com:80;
    }

    server {
        location / {
            proxy_pass http://web_backend;
            health_check interval=5s uri=/nginx_health;
        }
    }
}
```

#### 场景三：微服务 gRPC 健康检查

```nginx
upstream grpc_services {
    zone grpc_zone 64k;
    server service1.example.com:50051;
    server service2.example.com:50051;
}

server {
    listen 50051 http2;

    location / {
        grpc_pass grpc://grpc_services;
        # gRPC 健康检查：使用标准 gRPC Health Checking Protocol
        # grpc_status=12 (UNIMPLEMENTED) 表示服务未实现健康检查接口
        # grpc_status=0 (OK) 表示服务健康
        health_check mandatory type=grpc grpc_service=grpc.health.v1.Health
                         interval=5s fails=3 passes=2;
    }
}
```

### 18.5 健康检查与负载均衡配合

#### 状态流转机制

```
         初始状态
            |
            v
      ┌───────────┐     ┌──────────────┐
      │ checking  │────▶│   unhealthy  │
      └─────┬─────┘     └──────────────┘
            │                   │
            │  passes 次成功     │ fails 次失败
            v                   v
      ┌───────────┐     ┌──────────────┐
      │  healthy  │◀────│              │
      └───────────┘     └──────────────┘
```

**关键行为**：
- `checking` 状态：初始或 reload 后，不接收客户端请求
- `mandatory` 参数：强制等待首次健康检查完成才标记为健康
- `persistent` 参数：reload 后如之前是健康状态则保持 healthy

#### 与负载均衡算法结合

```nginx
upstream backend {
    zone backend 64k;
    least_conn;  # 最少连接算法

    server srv1.example.com weight=5;
    server srv2.example.com;
    server srv3.example.com;

    # 被动检查参数与主动检查并存
    # 被动检查作为兜底，主动检查提供快速发现
}

server {
    location / {
        proxy_pass http://backend;
        health_check interval=5s fails=3 passes=2;

        # 故障转移配置
        proxy_next_upstream error timeout http_502 http_503 http_504;
        proxy_next_upstream_timeout 5s;
        proxy_next_upstream_tries 2;
    }
}
```

**监控指标集成**：

```nginx
log_format health_log '$remote_addr - $remote_user [$time_local] '
                      '"$request" $status '
                      'upstream=$upstream_addr '
                      'upstream_status=$upstream_status '
                      'health_check=$upstream_health_check_status';

server {
    location / {
        proxy_pass http://backend;
        health_check interval=5s;
        access_log /var/log/nginx/health.log health_log;
    }
}
```

### 18.6 开源 NGINX 的替代方案

由于主动健康检查是 NGINX Plus 商业特性，开源版本需要使用第三方模块。

#### nginx_upstream_check_module (Tengine)

由阿里巴巴 Tengine 团队开发的第三方模块，支持主动健康检查。

**源码地址**：https://github.com/yaoweibin/nginx_upstream_check_module

**安装方法**：
```bash
# 下载模块源码
git clone https://github.com/yaoweibin/nginx_upstream_check_module.git

# 下载 NGINX 源码并解压
cd /usr/local/src
tar -xzvf nginx-1.24.0.tar.gz
cd nginx-1.24.0

# 应用补丁（根据版本选择）
patch -p1 < /path/to/nginx_upstream_check_module/check.patch
# 或 patch -p1 < /path/to/nginx_upstream_check_module/check_1.20.1+.patch

# 编译安装
./configure \
    --prefix=/etc/nginx \
    --add-module=/path/to/nginx_upstream_check_module \
    --with-http_ssl_module \
    --with-http_v2_module

make && make install
```

**指令说明**：

| 指令 | 语法 | 默认值 | 说明 |
|------|------|--------|------|
| `check` | `check interval=ms [fall=N] [rise=N] [timeout=ms] [default_down=true\|false] [type=tcp\|http\|ssl_hello\|mysql\|ajp\|fastcgi]` | 见右侧 | 启用健康检查<br>`interval`: 检查间隔(ms)<br>`fall`: 失败次数<br>`rise`: 成功次数<br>`timeout`: 超时(ms)<br>`default_down`: 默认下线状态<br>`type`: 检查协议 |
| `check_keepalive_requests` | `check_keepalive_requests num` | `1` | 长连接检查次数 |
| `check_http_send` | `check_http_send "packet"` | `GET / HTTP/1.0\r\n\r\n` | HTTP 检查请求包 |
| `check_http_expect_alive` | `check_http_expect_alive [http_2xx] [http_3xx] [http_4xx] [http_5xx]` | `http_2xx` `http_3xx` | 视为健康的 HTTP 状态码 |
| `check_fastcgi_param` | `check_fastcgi_param parameter value` | — | FastCGI 检查参数 |
| `check_status` | `check_status [html\|csv\|json]` | `html` | 状态查看页面格式 |

**配置示例**：
```nginx
upstream backend {
    server 192.168.0.1:80;
    server 192.168.0.2:80;

    # 每 5 秒检查一次，失败 3 次下线，成功 2 次上线，超时 4 秒
    check interval=5000 rise=2 fall=3 timeout=4000 type=http;

    # HTTP 健康检查配置
    check_http_send "GET /health HTTP/1.0\r\n\r\n";
    check_http_expect_alive http_2xx http_3xx;

    # 启用长连接检查（可选）
    check_keepalive_requests 100;
}

server {
    listen 80;

    location / {
        proxy_pass http://backend;
    }

    # 健康检查状态页
    location /status {
        check_status json;  # 可选 html, csv, json
        access_log off;
        allow 10.0.0.0/8;
        deny all;
    }
}
```

**支持的健康检查类型**：
- `tcp`：仅建立 TCP 连接
- `http`：发送 HTTP 请求并验证响应
- `ssl_hello`：发送 SSL Client Hello
- `mysql`：发送 MySQL ping 包
- `ajp`：发送 AJP ping 包
- `fastcgi`：发送 FastCGI 请求

#### 其他替代方案对比

| 方案 | 类型 | 活跃度 | 特点 |
|------|------|--------|------|
| **nginx_upstream_check_module** | 第三方模块 | 中等 | 功能完整，Tengine 使用 |
| **nginx-upsync-module** | 第三方模块 | 低 | 结合 Consul/etcd 动态发现 |
| **OpenResty + lua-resty-healthcheck** | Lua 扩展 | 高 | 灵活可编程 |
| **Traefik** | 替代代理 | 高 | 原生支持主动健康检查 |
| **Envoy** | 替代代理 | 高 | 云原生，功能强大 |

#### OpenResty + Lua 实现示例

```nginx
lua_shared_dict healthcheck 1m;

upstream backend {
    server 127.0.0.1:8081;
    server 127.0.0.1:8082;
    server 127.0.0.1:8083;
}

init_worker_by_lua_block {
    local hc = require "resty.healthcheck"
    local checker = hc.new({
        name = "my-checker",
        shm_name = "healthcheck",
        checks = {
            active = {
                healthy = {
                    interval = 5,
                    successes = 2,
                },
                unhealthy = {
                    interval = 5,
                    http_failures = 3,
                },
            },
        },
    })

    checker:add_target("127.0.0.1", 8081)
    checker:add_target("127.0.0.1", 8082)
    checker:add_target("127.0.0.1", 8083)
}
```

**选型建议**：
- 商业环境且有预算：**NGINX Plus**（完整支持，商业支持）
- 开源替代且功能优先：**nginx_upstream_check_module**
- 需要动态配置：**OpenResty + lua-resty-upstream-healthcheck**
- 新架构选型：**Traefik** 或 **Envoy**（原生支持服务发现）

---

## 19. Upstream 模块源码实现分析

基于 nginx 1.31.0 源码（`lib/nginx/src/http/`）。

### 19.1 核心数据结构

#### ngx_http_upstream_t - 上游请求结构

```c
// src/http/ngx_http_upstream.h:336-420
struct ngx_http_upstream_s {
    ngx_http_upstream_handler_pt     read_event_handler;
    ngx_http_upstream_handler_pt     write_event_handler;

    ngx_peer_connection_t            peer;      // 对端连接

    ngx_event_pipe_t                *pipe;      // 管道（流式传输）

    ngx_chain_t                     *request_bufs;  // 请求缓冲

    ngx_http_upstream_conf_t        *conf;      // upstream 配置
    ngx_http_upstream_srv_conf_t    *upstream;  // upstream 组配置

    ngx_http_upstream_headers_in_t   headers_in;  // 上游响应头

    ngx_buf_t                        buffer;    // 响应缓冲

    // 回调函数
    ngx_int_t                      (*create_request)(ngx_http_request_t *r);
    ngx_int_t                      (*reinit_request)(ngx_http_request_t *r);
    ngx_int_t                      (*process_header)(ngx_http_request_t *r);
    void                           (*finalize_request)(ngx_http_request_t *r, ngx_int_t rc);

    ngx_msec_t                       start_time;

    ngx_http_upstream_state_t       *state;     // 状态信息

    unsigned                         store:1;
    unsigned                         cacheable:1;
    unsigned                         buffering:1;
    unsigned                         keepalive:1;
    unsigned                         upgrade:1;   // WebSocket 升级
};
```

#### ngx_http_upstream_rr_peer_t - 服务器节点

```c
// src/http/ngx_http_upstream_round_robin.h
struct ngx_http_upstream_rr_peer_s {
    ngx_str_t                       name;       // 服务器名称
    ngx_addr_t                     *addrs;      // 地址数组
    ngx_uint_t                      naddrs;     // 地址数

    ngx_uint_t                      weight;     // 配置权重
    ngx_uint_t                      effective_weight;  // 有效权重（故障恢复）
    ngx_uint_t                      current_weight;    // 当前权重（轮询用）

    ngx_uint_t                      max_conns;  // 最大连接数
    ngx_uint_t                      conns;      // 当前连接数

    ngx_uint_t                      max_fails;  // 最大失败次数
    time_t                          fail_timeout; // 失败超时
    time_t                          accessed;   // 最后访问时间
    time_t                          checked;    // 最后检查时间

    ngx_uint_t                      fails;      // 失败次数

    ngx_uint_t                      down;       // 是否下线

    unsigned                         backup:1;   // 是否备份服务器
};
```

### 19.2 加权轮询算法（平滑分配）

```c
// src/http/ngx_http_upstream_round_robin.c:810-895
ngx_http_upstream_get_peer(ngx_peer_connection_t *pc, void *data)
{
    // 平滑加权轮询：避免突发集中分配
    for (p = peers->peer; p; p++) {
        if (p 已尝试过 || p down || 超过 max_fails || 超过 max_conns)
            continue;

        p->current_weight += p->effective_weight;  // 累加有效权重
        total += p->effective_weight;

        // 慢恢复：失败后逐步恢复权重
        if (p->effective_weight < p->weight)
            p->effective_weight++;

        // 选 current_weight 最高的
        if (best == NULL || p->current_weight > best->current_weight)
            best = p;
    }

    // 选中后扣减总权重
    best->current_weight -= total;
    return best;
}
```

**算法特点**：
- 权重平滑分配，避免请求集中到某台服务器
- 故障恢复时权重逐步回升，而不是直接恢复
- 支持 backup 服务器组切换

### 19.3 一致性哈希实现

```c
// src/http/modules/ngx_http_upstream_hash_module.c
typedef struct {
    uint32_t                            hash;
    ngx_str_t                          *server;
} ngx_http_upstream_chash_point_t;

typedef struct {
    ngx_uint_t                          number;
    ngx_http_upstream_chash_point_t     point[1];  // 虚拟节点数组
} ngx_http_upstream_chash_points_t;

// 虚拟节点分布在 0-2^32 的圆环上
// 根据 key 的 hash 值查找最近的节点（二分查找）
ngx_http_upstream_find_chash_point(points, hash);
```

### 19.4 Upstream 请求处理流程

```
ngx_http_upstream_init(r)
  |
  v
ngx_http_upstream_init_request(r)
  |
  +-- 解析 upstream 名称
  +-- 创建 peer 连接 (peer.get = ngx_http_upstream_get_round_robin_peer)
  |
  v
ngx_http_upstream_connect(r, u)
  |
  +-- ngx_event_connect_peer() 连接到上游服务器
  |
  v
ngx_http_upstream_send_request() --> 发送客户端请求到上游
  |
  v
ngx_http_upstream_process_header(r, u)
  |
  +-- 解析上游响应头
  +-- ngx_http_upstream_process_headers() 处理 set-cookie 等
  |
  v
分支:
  +-- 缓冲模式: ngx_http_upstream_process_request()
  |     (读完整上游响应到临时文件/内存，再转发给客户端)
  |
  +-- 非缓冲模式: ngx_http_upstream_process_upgraded()
        (双向透传，用于 WebSocket 等升级连接)
  |
  v
ngx_http_upstream_finalize_request(r, u, rc)
  |
  +-- peer.free() 记录失败/成功，更新权重
  +-- 清理资源
```

### 19.5 故障转移机制

```c
// src/http/ngx_http_upstream.c
#define NGX_HTTP_UPSTREAM_FT_ERROR           0x00000002
#define NGX_HTTP_UPSTREAM_FT_TIMEOUT         0x00000004
#define NGX_HTTP_UPSTREAM_FT_INVALID_HEADER  0x00000008
#define NGX_HTTP_UPSTREAM_FT_HTTP_500        0x00000010
#define NGX_HTTP_UPSTREAM_FT_HTTP_502        0x00000020
#define NGX_HTTP_UPSTREAM_FT_HTTP_503        0x00000040
#define NGX_HTTP_UPSTREAM_FT_HTTP_504        0x00000080

ngx_http_upstream_next(r, u, ft_type)
{
    if (u->peer.tries > u->conf->next_upstream_tries)
        return finalize;

    if (ft_type & u->conf->next_upstream) {
        // 符合重试条件，选择下一台服务器
        ngx_http_upstream_connect(r, u);
    } else {
        // 不符合条件，直接结束请求
        ngx_http_upstream_finalize_request(r, u, status);
    }
}
```

### 19.6 关键源码路径

| 功能 | 文件路径 | 大小 |
|------|----------|------|
| Upstream 核心 | `src/http/ngx_http_upstream.c` | 187KB |
| Upstream 头文件 | `src/http/ngx_http_upstream.h` | 16KB |
| Round Robin 负载均衡 | `src/http/ngx_http_upstream_round_robin.c` | 32KB |
| Least Connections | `src/http/modules/ngx_http_upstream_least_conn_module.c` | 4KB |
| Hash/一致性哈希 | `src/http/modules/ngx_http_upstream_hash_module.c` | 8KB |
| Random 负载均衡 | `src/http/modules/ngx_http_upstream_random_module.c` | 6KB |
| Proxy 模块 | `src/http/modules/ngx_http_proxy_module.c` | 80KB |

---

*源码分析基于 nginx 1.31.0*
*源码目录：`lib/nginx/src/http/`*