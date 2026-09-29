//go:build !windows

// Package app 提供配置重载测试。
//
// 该文件验证重载只比较实际监听地址集合。
//
// 作者：xfy
package app

import (
	"testing"

	"rua.plus/lolly/internal/config"
)

// TestUniqueListenOrder 验证启动日志所需的监听地址保持首次出现顺序。
func TestUniqueListenOrder(t *testing.T) {
	servers := []config.ServerConfig{{Listen: ":443"}, {Listen: ":80"}, {Listen: ":443"}, {Listen: ":8080"}}
	got := uniqueListenOrder(servers)
	want := []string{":443", ":80", ":8080"}
	if len(got) != len(want) {
		t.Fatalf("uniqueListenOrder() = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("uniqueListenOrder() = %v，期望 %v", got, want)
		}
	}
}

// TestRequiresFullRestartUsesUniqueListenSet 验证同一监听内增减虚拟主机无需完整重启。
func TestRequiresFullRestartUsesUniqueListenSet(t *testing.T) {
	application := &App{cfg: &config.Config{Mode: config.ServerModeMultiServer, Servers: []config.ServerConfig{
		{Listen: ":80", Name: "a"},
		{Listen: ":443", Name: "secure"},
	}}}
	newConfig := &config.Config{Mode: config.ServerModeMultiServer, Servers: []config.ServerConfig{
		{Listen: ":443", Name: "secure"},
		{Listen: ":80", Name: "a"},
		{Listen: ":80", Name: "b"},
	}}
	if application.requiresFullRestart(newConfig) {
		t.Fatal("唯一 listen 集合未变化时不应要求完整重启")
	}
	newConfig.Servers[0].Listen = ":8443"
	if !application.requiresFullRestart(newConfig) {
		t.Fatal("唯一 listen 集合变化时应要求完整重启")
	}
}
