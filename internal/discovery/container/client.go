// Package container 实现容器引擎兼容 API 的 Unix 套接字客户端。
//
// 该文件只依赖标准库 HTTP 协议，避免绑定特定 Docker 或 Podman SDK 版本。
//
// 作者：xfy
package container

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// apiClient 封装兼容 API 所需的三个端点。
type apiClient struct {
	http           *http.Client
	requestTimeout time.Duration
}

// containerSummary 是运行中容器列表的最小响应模型。
type containerSummary struct {
	// ID 是后续详情查询所需的容器标识。
	ID string `json:"Id"`
}

// containerInspect 是标签与网络解析所需的详情响应模型。
type containerInspect struct {
	// Config 包含环境变量与镜像暴露端口。
	Config struct {
		Env          []string            `json:"Env"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	} `json:"Config"`
	// NetworkSettings 包含容器加入的网络及地址。
	NetworkSettings struct {
		Networks map[string]networkAttachment `json:"Networks"`
	} `json:"NetworkSettings"`
}

// networkAttachment 表示容器在一个网络中的地址。
type networkAttachment struct {
	// IPAddress 是网络分配的 IPv4 地址。
	IPAddress string `json:"IPAddress"`
	// GlobalIPv6Address 是网络分配的 IPv6 地址。
	GlobalIPv6Address string `json:"GlobalIPv6Address"`
}

// newAPIClient 创建通过 Unix 套接字通信的标准 HTTP 客户端。
func newAPIClient(socketPath string, requestTimeout time.Duration) *apiClient {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		panic("默认 HTTP 传输类型不受支持")
	}
	transport := base.Clone()
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", socketPath)
	}
	client := new(http.Client)
	client.Transport = transport
	return &apiClient{http: client, requestTimeout: requestTimeout}
}

// listRunning 查询所有运行中的容器。
func (c *apiClient) listRunning(ctx context.Context) ([]containerSummary, error) {
	var result []containerSummary
	if err := c.getJSON(ctx, "/containers/json?all=0", &result); err != nil {
		return nil, err
	}
	return result, nil
}

// inspect 查询一个容器的环境、端口与网络信息。
func (c *apiClient) inspect(ctx context.Context, id string) (containerInspect, error) {
	var result containerInspect
	path := "/containers/" + url.PathEscape(id) + "/json"
	if err := c.getJSON(ctx, path, &result); err != nil {
		return containerInspect{}, err
	}
	return result, nil
}

// events 消费容器事件流，每读到一个完整事件便通知调用方。
func (c *apiClient) events(ctx context.Context, notify func()) error {
	filters := url.QueryEscape(`{"type":["container","network"]}`)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://container/events?filters="+filters, nil)
	if err != nil {
		return fmt.Errorf("创建容器事件请求失败: %w", err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("连接容器事件流失败: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("容器事件 API 返回状态 %s", response.Status)
	}

	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		if len(scanner.Bytes()) != 0 {
			notify()
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取容器事件流失败: %w", err)
	}
	return nil
}

// getJSON 执行兼容 API 请求并严格处理非成功状态。
func (c *apiClient) getJSON(ctx context.Context, path string, target any) error {
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://container"+path, nil)
	if err != nil {
		return fmt.Errorf("创建容器 API 请求失败: %w", err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("调用容器 API 失败: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("容器 API 返回状态 %s", response.Status)
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("解析容器 API 响应失败: %w", err)
	}
	return nil
}
