// Package container 提供基于 Docker/Podman 兼容 API 的容器服务发现。
//
// 该文件定义稳定的发现模型与 Watcher 生命周期，并保证失败时保留最后成功快照。
// 所有公开方法均可并发调用。
//
// 作者：xfy
package container

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

const (
	// defaultSocketPath 是容器引擎未显式配置时使用的标准套接字。
	defaultSocketPath = "/var/run/docker.sock"
	// defaultResyncInterval 用于弥补事件流断开期间可能遗漏的变更。
	defaultResyncInterval = 30 * time.Second
	// defaultRequestTimeout 限制列表与详情 API 的单次等待时间。
	defaultRequestTimeout = 5 * time.Second
)

// Config 控制容器引擎连接与发现范围。
type Config struct {
	// SocketPath 是 Docker 或 Podman 兼容 API 的 Unix 套接字路径。
	SocketPath string
	// Network 指定读取容器地址的网络名称。
	Network string
	// ResyncInterval 是全量同步周期，零值使用默认的三十秒周期。
	ResyncInterval time.Duration
	// RequestTimeout 是列表与详情请求的超时，零值使用默认的五秒。
	RequestTimeout time.Duration
	// Debounce 是容器事件合并等待时间；零值表示立即同步。
	Debounce time.Duration
}

// Endpoint 表示一个可连接的容器后端。
type Endpoint struct {
	// ContainerID 是容器引擎分配的标识。
	ContainerID string
	// Address 是指定网络中的 IPv4 或 IPv6 地址。
	Address string
	// Port 是容器内服务端口。
	Port int
}

// Route 表示按主机与路径聚合的动态路由。
type Route struct {
	// Host 是请求匹配的主机名。
	Host string
	// Path 是请求匹配的路径前缀。
	Path string
	// Protocol 是访问后端使用的 http 或 https。
	Protocol string
	// Destination 是转发给后端时使用的目标路径。
	Destination string
	// Endpoints 包含共享该主机与路径的全部后端。
	Endpoints []Endpoint
	// ExternalHTTPPort 是该路由声明的外部 HTTP 端口。
	ExternalHTTPPort int
	// ExternalHTTPSPort 是该路由声明的外部 HTTPS 端口。
	ExternalHTTPSPort int
	// HTTPSMethod 是 HTTPS 请求处理策略。
	HTTPSMethod string
	// ACMEHosts 是申请证书时使用的主机名。
	ACMEHosts []string
	// ACMEEmail 是申请证书时使用的邮箱。
	ACMEEmail string
}

// Issue 表示单个容器配置无法转换为路由的问题。
type Issue struct {
	// ContainerID 标识问题来源。
	ContainerID string
	// Message 描述可供日志记录的原因。
	Message string
}

// Snapshot 是一次成功全量同步得到的不可变视图。
type Snapshot struct {
	// Routes 是按主机与路径稳定排序的路由集合。
	Routes []Route
	// Issues 是被跳过配置的问题集合。
	Issues []Issue
}

// Watcher 管理兼容 API、事件监听与最后成功快照。
type Watcher struct {
	client         *apiClient
	network        string
	resyncInterval time.Duration
	debounce       time.Duration

	mu       sync.RWMutex
	snapshot Snapshot
}

// New 创建容器发现观察器。
//
// 空套接字与零同步周期分别使用 Docker 标准路径和默认周期；负同步周期会返回错误。
func New(config Config) (*Watcher, error) {
	if config.SocketPath == "" {
		config.SocketPath = defaultSocketPath
	}
	if config.ResyncInterval < 0 {
		return nil, errors.New("容器发现同步周期不能为负数")
	}
	if config.RequestTimeout < 0 {
		return nil, errors.New("容器发现请求超时不能为负数")
	}
	if config.Debounce < 0 {
		return nil, errors.New("容器发现事件防抖时间不能为负数")
	}
	if config.ResyncInterval == 0 {
		config.ResyncInterval = defaultResyncInterval
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultRequestTimeout
	}
	watcher := new(Watcher)
	watcher.client = newAPIClient(config.SocketPath, config.RequestTimeout)
	watcher.network = config.Network
	watcher.resyncInterval = config.ResyncInterval
	watcher.debounce = config.Debounce
	return watcher, nil
}

// Sync 执行一次全量同步并仅在成功后替换快照。
func (w *Watcher) Sync(ctx context.Context) (Snapshot, error) {
	containers, err := w.client.listRunning(ctx)
	if err != nil {
		return w.Snapshot(), err
	}

	sort.Slice(containers, func(left, right int) bool { return containers[left].ID < containers[right].ID })
	builder := newSnapshotBuilder()
	failed := make(map[string]struct{})
	for _, summary := range containers {
		inspect, inspectErr := w.client.inspect(ctx, summary.ID)
		if inspectErr != nil {
			failed[summary.ID] = struct{}{}
			builder.issue(summary.ID, "查询容器详情失败: "+inspectErr.Error())
			continue
		}
		builder.add(summary.ID, inspect, w.network)
	}
	builder.preserve(w.Snapshot(), failed)
	result := builder.snapshot()

	w.mu.Lock()
	w.snapshot = result
	w.mu.Unlock()
	return cloneSnapshot(result), nil
}

// Snapshot 返回最后一次成功同步的独立副本。
func (w *Watcher) Snapshot() Snapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return cloneSnapshot(w.snapshot)
}

// Start 监听容器事件并周期执行全量同步，直到上下文结束。
//
// 瞬时 API 错误不会清空快照，也不会终止监听；每次成功同步都会调用 publish。
func (w *Watcher) Start(ctx context.Context, publish func(Snapshot)) error {
	if publish == nil {
		return errors.New("容器发现发布函数不能为空")
	}
	if snapshot, err := w.Sync(ctx); err == nil {
		publish(snapshot)
	}

	events := make(chan struct{}, 1)
	eventCtx, stopEvents := context.WithCancel(ctx)
	eventsStopped := make(chan struct{})
	go func() {
		defer close(eventsStopped)
		w.watchEvents(eventCtx, events)
	}()
	defer func() {
		stopEvents()
		<-eventsStopped
	}()
	ticker := time.NewTicker(w.resyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-events:
			if !w.waitDebounce(ctx, events) {
				return ctx.Err()
			}
		}
		if snapshot, err := w.Sync(ctx); err == nil {
			publish(snapshot)
		}
	}
}

// waitDebounce 等待事件安静窗口，并在新事件到达时重新计时。
func (w *Watcher) waitDebounce(ctx context.Context, events <-chan struct{}) bool {
	if w.debounce == 0 {
		return true
	}
	timer := time.NewTimer(w.debounce)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-events:
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(w.debounce)
		case <-timer.C:
			return true
		}
	}
}

// watchEvents 持续消费事件流，并在断线后以短延迟重新连接。
func (w *Watcher) watchEvents(ctx context.Context, changed chan<- struct{}) {
	for ctx.Err() == nil {
		_ = w.client.events(ctx, func() {
			select {
			case changed <- struct{}{}:
			default:
			}
		})
		if ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// cloneSnapshot 防止调用方修改观察器内部持有的成功快照。
func cloneSnapshot(source Snapshot) Snapshot {
	result := Snapshot{
		Routes: append([]Route(nil), source.Routes...),
		Issues: append([]Issue(nil), source.Issues...),
	}
	for index := range result.Routes {
		result.Routes[index].Endpoints = append([]Endpoint(nil), source.Routes[index].Endpoints...)
		result.Routes[index].ACMEHosts = append([]string(nil), source.Routes[index].ACMEHosts...)
	}
	return result
}

// sortSnapshot 固定公开结果顺序，避免容器引擎返回顺序影响配置更新。
func sortSnapshot(snapshot *Snapshot) {
	sort.Slice(snapshot.Routes, func(left, right int) bool {
		if snapshot.Routes[left].Host == snapshot.Routes[right].Host {
			return snapshot.Routes[left].Path < snapshot.Routes[right].Path
		}
		return snapshot.Routes[left].Host < snapshot.Routes[right].Host
	})
	for index := range snapshot.Routes {
		sort.Slice(snapshot.Routes[index].Endpoints, func(left, right int) bool {
			return snapshot.Routes[index].Endpoints[left].ContainerID < snapshot.Routes[index].Endpoints[right].ContainerID
		})
	}
	sort.Slice(snapshot.Issues, func(left, right int) bool {
		if snapshot.Issues[left].ContainerID == snapshot.Issues[right].ContainerID {
			return snapshot.Issues[left].Message < snapshot.Issues[right].Message
		}
		return snapshot.Issues[left].ContainerID < snapshot.Issues[right].ContainerID
	})
}
