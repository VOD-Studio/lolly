package nginx

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"rua.plus/lolly/internal/config"
)

// helper: parse nginx config string and convert.
func convertString(t *testing.T, input string) (*ConvertResult, error) {
	t.Helper()
	cfg, err := ParseString(input)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	return Convert(cfg)
}

// helper: check if any warning contains substring.
func hasWarningContaining(warnings []Warning, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w.Message, substr) {
			return true
		}
	}
	return false
}

func TestConvertServerBlock(t *testing.T) {
	input := `
http {
    server {
        listen 8080;
        server_name example.com www.example.com;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	if len(result.Config.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(result.Config.Servers))
	}

	s := result.Config.Servers[0]
	if s.Listen != ":8080" {
		t.Errorf("expected listen :8080, got %s", s.Listen)
	}
	if s.Name != "example.com" {
		t.Errorf("expected name example.com, got %s", s.Name)
	}
	if len(s.ServerNames) != 2 {
		t.Fatalf("expected 2 server_names, got %d", len(s.ServerNames))
	}
	if s.ServerNames[0] != "example.com" || s.ServerNames[1] != "www.example.com" {
		t.Errorf("expected [example.com www.example.com], got %v", s.ServerNames)
	}
}

func TestConvertLocationProxyPass(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        location /api/ {
            proxy_pass http://backend:8080;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if len(s.Proxy) != 1 {
		t.Fatalf("expected 1 proxy, got %d", len(s.Proxy))
	}

	p := s.Proxy[0]
	if p.Path != "/api/" {
		t.Errorf("expected path /api/, got %s", p.Path)
	}
	if len(p.Targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(p.Targets))
	}
	if p.Targets[0].URL != "http://backend:8080" {
		t.Errorf("expected target URL http://backend:8080, got %s", p.Targets[0].URL)
	}
}

func TestConvertLocationRoot(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        location /static/ {
            root /var/www/html;
            index index.html index.htm;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if len(s.Static) != 1 {
		t.Fatalf("expected 1 static, got %d", len(s.Static))
	}

	st := s.Static[0]
	if st.Path != "/static/" {
		t.Errorf("expected path /static/, got %s", st.Path)
	}
	if st.Root != "/var/www/html" {
		t.Errorf("expected root /var/www/html, got %s", st.Root)
	}
	if len(st.Index) != 2 || st.Index[0] != "index.html" || st.Index[1] != "index.htm" {
		t.Errorf("expected [index.html index.htm], got %v", st.Index)
	}
}

func TestConvertServerLevelRoot(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        root /var/www/html;
        index index.html index.htm index.php;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if len(s.Static) != 1 {
		t.Fatalf("expected 1 static, got %d", len(s.Static))
	}

	st := s.Static[0]
	if st.Path != "/" {
		t.Errorf("expected path /, got %s", st.Path)
	}
	if st.Root != "/var/www/html" {
		t.Errorf("expected root /var/www/html, got %s", st.Root)
	}
	if len(st.Index) != 3 || st.Index[0] != "index.html" || st.Index[1] != "index.htm" || st.Index[2] != "index.php" {
		t.Errorf("expected [index.html index.htm index.php], got %v", st.Index)
	}
}

func TestConvertServerLevelRootWithLocation(t *testing.T) {
	// When both server-level root and location /static exist, both should be converted
	input := `
http {
    server {
        listen 80;
        root /var/www/html;
        index index.html;
        location /static/ {
            root /var/www/static;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if len(s.Static) != 2 {
		t.Fatalf("expected 2 static, got %d", len(s.Static))
	}

	// Find the root location
	var rootStatic, staticLoc *config.StaticConfig
	for i := range s.Static {
		if s.Static[i].Path == "/" {
			rootStatic = &s.Static[i]
		} else if s.Static[i].Path == "/static/" {
			staticLoc = &s.Static[i]
		}
	}

	if rootStatic == nil {
		t.Error("expected root static config at path /")
	} else if rootStatic.Root != "/var/www/html" {
		t.Errorf("expected root /var/www/html, got %s", rootStatic.Root)
	}

	if staticLoc == nil {
		t.Error("expected static config at path /static/")
	} else if staticLoc.Root != "/var/www/static" {
		t.Errorf("expected root /var/www/static, got %s", staticLoc.Root)
	}
}

func TestConvertServerLevelRootNotCreatedForProxy(t *testing.T) {
	// When location / is a proxy, server-level root should NOT create a static config
	input := `
http {
    server {
        listen 80;
        root /var/www/html;
        location / {
            proxy_pass http://backend;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if len(s.Static) != 0 {
		t.Errorf("expected 0 static (location / is proxy), got %d", len(s.Static))
	}
	if len(s.Proxy) != 1 {
		t.Errorf("expected 1 proxy, got %d", len(s.Proxy))
	}
}

func TestConvertUpstream(t *testing.T) {
	input := `
upstream backend {
    server 10.0.0.1:8080 weight=3;
    server 10.0.0.2:8080 weight=1 max_fails=3 fail_timeout=30s;
    server 10.0.0.3:8080 backup;
}

http {
    server {
        listen 80;
        location / {
            proxy_pass http://backend;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if len(s.Proxy) != 1 {
		t.Fatalf("expected 1 proxy, got %d", len(s.Proxy))
	}

	p := s.Proxy[0]
	if len(p.Targets) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(p.Targets))
	}

	if p.Targets[0].URL != "http://10.0.0.1:8080" {
		t.Errorf("target[0] URL = %s, want http://10.0.0.1:8080", p.Targets[0].URL)
	}
	if p.Targets[0].Weight != 3 {
		t.Errorf("target[0] Weight = %d, want 3", p.Targets[0].Weight)
	}
	if p.Targets[1].MaxFails != 3 {
		t.Errorf("target[1] MaxFails = %d, want 3", p.Targets[1].MaxFails)
	}
	if p.Targets[1].FailTimeout != 30*time.Second {
		t.Errorf("target[1] FailTimeout = %v, want 30s", p.Targets[1].FailTimeout)
	}
	if !p.Targets[2].Backup {
		t.Error("target[2] Backup = false, want true")
	}
}

func TestConvertLocationModifiers(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantType string
	}{
		{
			name: "exact",
			input: `
http {
    server {
        listen 80;
        location = /exact {
            proxy_pass http://backend;
        }
    }
}`,
			wantType: "exact",
		},
		{
			name: "prefix_priority",
			input: `
http {
    server {
        listen 80;
        location ^~ /prefix {
            proxy_pass http://backend;
        }
    }
}`,
			wantType: "prefix_priority",
		},
		{
			name: "regex",
			input: `
http {
    server {
        listen 80;
        location ~ \.php$ {
            proxy_pass http://backend;
        }
    }
}`,
			wantType: "regex",
		},
		{
			name: "regex_caseless",
			input: `
http {
    server {
        listen 80;
        location ~* \.php$ {
            proxy_pass http://backend;
        }
    }
}`,
			wantType: "regex_caseless",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := convertString(t, tt.input)
			if err != nil {
				t.Fatalf("convert error: %v", err)
			}

			s := result.Config.Servers[0]
			if len(s.Proxy) != 1 {
				t.Fatalf("expected 1 proxy, got %d", len(s.Proxy))
			}
			if s.Proxy[0].LocationType != tt.wantType {
				t.Errorf("LocationType = %s, want %s", s.Proxy[0].LocationType, tt.wantType)
			}
		})
	}
}

func TestConvertGzipConfig(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        gzip on;
        gzip_types text/plain text/css application/json;
        gzip_min_length 1024;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if s.Compression.Type != "gzip" {
		t.Errorf("Compression.Type = %s, want gzip", s.Compression.Type)
	}
	if len(s.Compression.Types) != 3 {
		t.Fatalf("Compression.Types length = %d, want 3", len(s.Compression.Types))
	}
	if s.Compression.Types[0] != "text/plain" {
		t.Errorf("Compression.Types[0] = %s, want text/plain", s.Compression.Types[0])
	}
	if s.Compression.MinSize != 1024 {
		t.Errorf("Compression.MinSize = %d, want 1024", s.Compression.MinSize)
	}
}

func TestConvertRewrite(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        rewrite ^/old/(.*)$ /new/$1 last;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if len(s.Rewrite) != 1 {
		t.Fatalf("expected 1 rewrite rule, got %d", len(s.Rewrite))
	}

	r := s.Rewrite[0]
	if r.Pattern != "^/old/(.*)$" {
		t.Errorf("Pattern = %s, want ^/old/(.*)$", r.Pattern)
	}
	if r.Replacement != "/new/$1" {
		t.Errorf("Replacement = %s, want /new/$1", r.Replacement)
	}
	if r.Flag != "last" {
		t.Errorf("Flag = %s, want last", r.Flag)
	}
}

func TestConvertReturn301(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        location /old {
            return 301 https://example.com/new;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if len(s.Rewrite) != 1 {
		t.Fatalf("expected 1 rewrite rule, got %d", len(s.Rewrite))
	}

	r := s.Rewrite[0]
	if r.Pattern != "^/old$" {
		t.Errorf("Pattern = %s, want ^/old$", r.Pattern)
	}
	if r.Replacement != "https://example.com/new" {
		t.Errorf("Replacement = %s, want https://example.com/new", r.Replacement)
	}
	if r.Flag != "permanent" {
		t.Errorf("Flag = %s, want permanent", r.Flag)
	}

	if !hasWarningContaining(result.Warnings, "return 301") {
		t.Error("expected warning about return 301 conversion")
	}
}

func TestConvertReturn302(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        location /temp {
            return 302 https://example.com/temp-new;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if len(s.Rewrite) != 1 {
		t.Fatalf("expected 1 rewrite rule, got %d", len(s.Rewrite))
	}

	r := s.Rewrite[0]
	if r.Flag != "redirect" {
		t.Errorf("Flag = %s, want redirect", r.Flag)
	}

	if !hasWarningContaining(result.Warnings, "return 302") {
		t.Error("expected warning about return 302 conversion")
	}
}

func TestConvertSSLConfig(t *testing.T) {
	input := `
http {
    server {
        listen 443 ssl;
        ssl_certificate /etc/ssl/server.crt;
        ssl_certificate_key /etc/ssl/server.key;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if s.SSL.Cert != "/etc/ssl/server.crt" {
		t.Errorf("SSL.Cert = %s, want /etc/ssl/server.crt", s.SSL.Cert)
	}
	if s.SSL.Key != "/etc/ssl/server.key" {
		t.Errorf("SSL.Key = %s, want /etc/ssl/server.key", s.SSL.Key)
	}
	if s.Listen != ":443" {
		t.Errorf("Listen = %s, want :443", s.Listen)
	}
}

func TestConvertSSLRejectHandshake(t *testing.T) {
	input := `
http {
    server {
        listen 443 ssl default_server;
        ssl_reject_handshake on;
    }
    server {
        listen 443 ssl;
        server_name example.com;
        ssl_certificate /etc/ssl/server.crt;
        ssl_certificate_key /etc/ssl/server.key;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}
	if len(result.Config.Servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(result.Config.Servers))
	}

	// default_server 在前
	def := result.Config.Servers[0]
	if !def.SSL.RejectHandshake {
		t.Errorf("expected SSL.RejectHandshake = true for default_server")
	}
	if def.SSL.Cert != "" {
		t.Errorf("expected no cert for reject_handshake server, got %q", def.SSL.Cert)
	}
}

func TestConvertProxyAddXForwardedFor(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        location / {
            proxy_pass http://backend;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if len(s.Proxy) != 1 {
		t.Fatalf("expected 1 proxy, got %d", len(s.Proxy))
	}

	val := s.Proxy[0].Headers.SetRequest["X-Forwarded-For"]
	if val != "$remote_addr" {
		t.Errorf("X-Forwarded-For = %s, want $remote_addr", val)
	}

	if !hasWarningContaining(result.Warnings, "$proxy_add_x_forwarded_for") {
		t.Error("expected warning about $proxy_add_x_forwarded_for replacement")
	}
}

func TestConvertUnsupportedDirective(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        if ($host = example.com) {
            return 301 https://example.com;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	if !hasWarningContaining(result.Warnings, "'if' directive") {
		t.Error("expected warning about unsupported 'if' directive")
	}
}

func TestConvertEmptyLocation(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        location /empty {
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	if !hasWarningContaining(result.Warnings, "no content") {
		t.Error("expected warning about empty location")
	}
}

func TestConvertConflictingLocation(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        location /conflict {
            proxy_pass http://backend;
            root /var/www;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	// Should be classified as proxy, not static.
	if len(s.Proxy) != 1 {
		t.Fatalf("expected 1 proxy, got %d", len(s.Proxy))
	}
	if len(s.Static) != 0 {
		t.Errorf("expected 0 static, got %d", len(s.Static))
	}

	if !hasWarningContaining(result.Warnings, "proxy_pass takes priority") {
		t.Error("expected warning about proxy_pass taking priority over root/alias")
	}
}

func TestConvertAliasDirective(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        location /images/ {
            alias /data/photos/;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if len(s.Static) != 1 {
		t.Fatalf("expected 1 static, got %d", len(s.Static))
	}

	st := s.Static[0]
	if st.Alias != "/data/photos/" {
		t.Errorf("Alias = %s, want /data/photos/", st.Alias)
	}
	// alias 和 root 互斥，root 应为空
	if st.Root != "" {
		t.Errorf("Root = %s, want empty (alias is set)", st.Root)
	}
}

func TestConvertReturnNonRedirect(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        location /health {
            return 200 "OK";
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	if len(result.Warnings) == 0 {
		t.Fatal("expected at least one warning, got none")
	}
	if !hasWarningContaining(result.Warnings, "'return' directive") && !hasWarningContaining(result.Warnings, "return") {
		t.Errorf("expected warning about non-redirect return, got warnings: %v", result.Warnings)
	}
}

func TestConvertServerLevelReturn(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        return 301 https://example.com;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if len(s.Rewrite) != 1 {
		t.Fatalf("expected 1 rewrite rule, got %d", len(s.Rewrite))
	}

	r := s.Rewrite[0]
	if r.Pattern != "^/" {
		t.Errorf("Pattern = %s, want ^/", r.Pattern)
	}
	if r.Replacement != "https://example.com" {
		t.Errorf("Replacement = %s, want https://example.com", r.Replacement)
	}
	if r.Flag != "permanent" {
		t.Errorf("Flag = %s, want permanent", r.Flag)
	}
}

func TestConvertProxySetHeader(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        location / {
            proxy_pass http://backend;
            proxy_set_header Host $host;
            proxy_set_header X-Real-IP $remote_addr;
            proxy_set_header X-Forwarded-Proto $scheme;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	p := s.Proxy[0]

	if p.Headers.SetRequest["Host"] != "$host" {
		t.Errorf("Host = %s, want $host", p.Headers.SetRequest["Host"])
	}
	if p.Headers.SetRequest["X-Real-IP"] != "$remote_addr" {
		t.Errorf("X-Real-IP = %s, want $remote_addr", p.Headers.SetRequest["X-Real-IP"])
	}
	if p.Headers.SetRequest["X-Forwarded-Proto"] != "$scheme" {
		t.Errorf("X-Forwarded-Proto = %s, want $scheme", p.Headers.SetRequest["X-Forwarded-Proto"])
	}
}

func TestConvertProxyTimeout(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        location / {
            proxy_pass http://backend;
            proxy_connect_timeout 5s;
            proxy_read_timeout 30s;
            proxy_send_timeout 15s;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	p := s.Proxy[0]

	if p.Timeout.Connect != 5*time.Second {
		t.Errorf("Connect = %v, want 5s", p.Timeout.Connect)
	}
	if p.Timeout.Read != 30*time.Second {
		t.Errorf("Read = %v, want 30s", p.Timeout.Read)
	}
	if p.Timeout.Write != 15*time.Second {
		t.Errorf("Write = %v, want 15s", p.Timeout.Write)
	}
}

func TestConvertClientMaxBodySize(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        client_max_body_size 10m;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if s.ClientMaxBodySize != "10m" {
		t.Errorf("ClientMaxBodySize = %s, want 10m", s.ClientMaxBodySize)
	}
}

func TestConvertMultipleServerBlocks(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        server_name example.com;
    }
    server {
        listen 443 ssl;
        server_name secure.example.com;
        ssl_certificate /etc/ssl/cert.pem;
        ssl_certificate_key /etc/ssl/key.pem;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	if len(result.Config.Servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(result.Config.Servers))
	}

	if result.Config.Servers[0].Listen != ":80" {
		t.Errorf("server[0] Listen = %s, want :80", result.Config.Servers[0].Listen)
	}
	if result.Config.Servers[1].Listen != ":443" {
		t.Errorf("server[1] Listen = %s, want :443", result.Config.Servers[1].Listen)
	}
	if result.Config.Servers[1].Name != "secure.example.com" {
		t.Errorf("server[1] Name = %s, want secure.example.com", result.Config.Servers[1].Name)
	}
}

func TestConvertDefaultServer(t *testing.T) {
	input := `
http {
    server {
        listen 80 default_server;
        server_name _;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if !s.Default {
		t.Error("Default = false, want true")
	}
	if s.Listen != ":80" {
		t.Errorf("Listen = %s, want :80", s.Listen)
	}
}

func TestConvertProxyRedirect(t *testing.T) {
	tests := []struct {
		name  string
		input string
		mode  string
		rules int
	}{
		{
			name: "off",
			input: `
http {
    server {
        listen 80;
        location / {
            proxy_pass http://backend;
            proxy_redirect off;
        }
    }
}`,
			mode:  "off",
			rules: 0,
		},
		{
			name: "default",
			input: `
http {
    server {
        listen 80;
        location / {
            proxy_pass http://backend;
            proxy_redirect default;
        }
    }
}`,
			mode:  "default",
			rules: 0,
		},
		{
			name: "custom",
			input: `
http {
    server {
        listen 80;
        location / {
            proxy_pass http://backend;
            proxy_redirect http://backend/ /;
        }
    }
}`,
			mode:  "custom",
			rules: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := convertString(t, tt.input)
			if err != nil {
				t.Fatalf("convert error: %v", err)
			}

			p := result.Config.Servers[0].Proxy[0]
			if p.RedirectRewrite == nil {
				t.Fatal("RedirectRewrite is nil")
			}
			if p.RedirectRewrite.Mode != tt.mode {
				t.Errorf("Mode = %s, want %s", p.RedirectRewrite.Mode, tt.mode)
			}
			if len(p.RedirectRewrite.Rules) != tt.rules {
				t.Errorf("Rules count = %d, want %d", len(p.RedirectRewrite.Rules), tt.rules)
			}
		})
	}
}

func TestConvertErrorPage(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        error_page 404 /404.html;
        error_page 500 502 503 /50x.html;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	pages := s.Security.ErrorPage.Pages

	if pages[404] != "/404.html" {
		t.Errorf("error_page[404] = %s, want /404.html", pages[404])
	}
	if pages[500] != "/50x.html" {
		t.Errorf("error_page[500] = %s, want /50x.html", pages[500])
	}
	if pages[502] != "/50x.html" {
		t.Errorf("error_page[502] = %s, want /50x.html", pages[502])
	}
	if pages[503] != "/50x.html" {
		t.Errorf("error_page[503] = %s, want /50x.html", pages[503])
	}
}

func TestConvertAuthBasic(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        auth_basic "Restricted Area";
        auth_basic_user_file /etc/nginx/.htpasswd;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if s.Security.Auth.Type != "basic" {
		t.Errorf("Auth.Type = %s, want basic", s.Security.Auth.Type)
	}
	if s.Security.Auth.Realm != "Restricted Area" {
		t.Errorf("Auth.Realm = %s, want Restricted Area", s.Security.Auth.Realm)
	}

	if !hasWarningContaining(result.Warnings, "auth_basic_user_file") {
		t.Error("expected warning about auth_basic_user_file migration")
	}
}

func TestConvertTopLevelServer(t *testing.T) {
	// Server block without http wrapper.
	input := `
server {
    listen 8080;
    server_name direct.example.com;
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	if len(result.Config.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(result.Config.Servers))
	}
	s := result.Config.Servers[0]
	if s.Listen != ":8080" {
		t.Errorf("Listen = %s, want :8080", s.Listen)
	}
	if s.Name != "direct.example.com" {
		t.Errorf("Name = %s, want direct.example.com", s.Name)
	}
}

func TestConvertLoggingConfig(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        access_log /var/log/nginx/access.log combined;
        error_log /var/log/nginx/error.log warn;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	if result.Config.Logging.Access.Path != "/var/log/nginx/access.log" {
		t.Errorf("Access.Path = %s, want /var/log/nginx/access.log", result.Config.Logging.Access.Path)
	}
	if result.Config.Logging.Access.Format != "combined" {
		t.Errorf("Access.Format = %s, want combined", result.Config.Logging.Access.Format)
	}
	if result.Config.Logging.Error.Path != "/var/log/nginx/error.log" {
		t.Errorf("Error.Path = %s, want /var/log/nginx/error.log", result.Config.Logging.Error.Path)
	}
	if result.Config.Logging.Error.Level != "warn" {
		t.Errorf("Error.Level = %s, want warn", result.Config.Logging.Error.Level)
	}
}

func TestConvertServerTokens(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        server_tokens off;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if s.ServerTokens {
		t.Error("ServerTokens = true, want false")
	}
}

func TestConvertUpstreamLoadBalance(t *testing.T) {
	tests := []struct {
		name     string
		upstream string
		wantLB   string
	}{
		{
			name:     "least_conn",
			upstream: "least_conn;",
			wantLB:   "least_conn",
		},
		{
			name:     "ip_hash",
			upstream: "ip_hash;",
			wantLB:   "ip_hash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := fmt.Sprintf(`
upstream backend {
    server 10.0.0.1:8080;
    %s
}

http {
    server {
        listen 80;
        location / {
            proxy_pass http://backend;
        }
    }
}
`, tt.upstream)
			result, err := convertString(t, input)
			if err != nil {
				t.Fatalf("convert error: %v", err)
			}

			p := result.Config.Servers[0].Proxy[0]
			if p.LoadBalance != tt.wantLB {
				t.Errorf("LoadBalance = %s, want %s", p.LoadBalance, tt.wantLB)
			}
		})
	}
}

func TestConvertTryFiles(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        location / {
            root /var/www;
            try_files $uri $uri/ /index.html;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	st := s.Static[0]
	if len(st.TryFiles) != 3 {
		t.Fatalf("TryFiles length = %d, want 3", len(st.TryFiles))
	}
	if st.TryFiles[0] != "$uri" || st.TryFiles[1] != "$uri/" || st.TryFiles[2] != "/index.html" {
		t.Errorf("TryFiles = %v, want [$uri $uri/ /index.html]", st.TryFiles)
	}
}

func TestConvertProxyHidePassHeaders(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        location / {
            proxy_pass http://backend;
            proxy_hide_header X-Powered-By;
            proxy_pass_header X-My-Custom;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	p := result.Config.Servers[0].Proxy[0]
	if len(p.Headers.HideResponse) != 1 || p.Headers.HideResponse[0] != "X-Powered-By" {
		t.Errorf("HideResponse = %v, want [X-Powered-By]", p.Headers.HideResponse)
	}
	if len(p.Headers.PassResponse) != 1 || p.Headers.PassResponse[0] != "X-My-Custom" {
		t.Errorf("PassResponse = %v, want [X-My-Custom]", p.Headers.PassResponse)
	}
}

func TestConvertProxyCache(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        location / {
            proxy_pass http://backend;
            proxy_cache my_cache;
            proxy_cache_valid 200 10m;
            proxy_cache_valid 404 1m;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	p := result.Config.Servers[0].Proxy[0]
	if !p.Cache.Enabled {
		t.Error("Cache.Enabled = false, want true")
	}
	if p.CacheValid == nil {
		t.Fatal("CacheValid is nil")
	}
	if p.CacheValid.OK != 10*time.Minute {
		t.Errorf("CacheValid.OK = %v, want 10m", p.CacheValid.OK)
	}
	if p.CacheValid.NotFound != time.Minute {
		t.Errorf("CacheValid.NotFound = %v, want 1m", p.CacheValid.NotFound)
	}
}

func TestConvertNamedLocation(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        location @fallback {
            proxy_pass http://fallback-backend;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	p := result.Config.Servers[0].Proxy[0]
	if p.LocationType != "named" {
		t.Errorf("LocationType = %s, want named", p.LocationType)
	}
	if p.LocationName != "fallback" {
		t.Errorf("LocationName = %s, want fallback", p.LocationName)
	}
}

func TestConvertWarningString(t *testing.T) {
	w := Warning{
		Directive: "if",
		Line:      10,
		File:      "test.conf",
		Message:   "unsupported",
	}
	s := w.String()
	if !strings.Contains(s, "test.conf:10") {
		t.Errorf("Warning.String() = %s, should contain test.conf:10", s)
	}
	if !strings.Contains(s, "unsupported") {
		t.Errorf("Warning.String() = %s, should contain 'unsupported'", s)
	}
}

func TestConvertEmptyConfig(t *testing.T) {
	input := ``
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	if len(result.Config.Servers) != 0 {
		t.Errorf("expected 0 servers, got %d", len(result.Config.Servers))
	}
}

func TestConvertServerTokensOn(t *testing.T) {
	input := `
http {
    server {
        listen 80;
        server_tokens on;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if !s.ServerTokens {
		t.Error("ServerTokens = false, want true")
	}
}

func TestConvertMapDirectiveWarning(t *testing.T) {
	input := `
http {
    map $host $backend {
        default backend1;
    }
    server {
        listen 80;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	if !hasWarningContaining(result.Warnings, "'map' directive") {
		t.Error("expected warning about unsupported 'map' directive")
	}
}

func TestConvertUpstreamDownServer(t *testing.T) {
	input := `
upstream backend {
    server 10.0.0.1:8080;
    server 10.0.0.2:8080 down;
}

http {
    server {
        listen 80;
        location / {
            proxy_pass http://backend;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	p := result.Config.Servers[0].Proxy[0]
	if len(p.Targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(p.Targets))
	}
	if !p.Targets[1].Down {
		t.Error("target[1].Down = false, want true")
	}
}

func TestConvertUpstreamInsideHttpBlock(t *testing.T) {
	// Upstream blocks inside http block should be found.
	input := `
http {
    upstream backend {
        server 10.0.0.1:8080;
        server 10.0.0.2:8080;
    }
    server {
        listen 80;
        location / {
            proxy_pass http://backend;
        }
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if len(s.Proxy) != 1 {
		t.Fatalf("expected 1 proxy, got %d", len(s.Proxy))
	}

	p := s.Proxy[0]
	if len(p.Targets) != 2 {
		t.Fatalf("expected 2 targets from upstream inside http block, got %d", len(p.Targets))
	}
	if p.Targets[0].URL != "http://10.0.0.1:8080" {
		t.Errorf("target[0] URL = %s, want http://10.0.0.1:8080", p.Targets[0].URL)
	}
	if p.Targets[1].URL != "http://10.0.0.2:8080" {
		t.Errorf("target[1] URL = %s, want http://10.0.0.2:8080", p.Targets[1].URL)
	}
}

func TestConvertSSLWithoutCertKey(t *testing.T) {
	// SSL enabled via listen directive but no ssl_certificate/key should produce a warning.
	input := `
http {
    server {
        listen 443 ssl;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if s.SSL.Cert != "" {
		t.Errorf("SSL.Cert = %s, want empty string (no auto)", s.SSL.Cert)
	}
	if s.SSL.Key != "" {
		t.Errorf("SSL.Key = %s, want empty string (no auto)", s.SSL.Key)
	}
	if !hasWarningContaining(result.Warnings, "ssl_certificate") {
		t.Error("expected warning about missing ssl_certificate/ssl_certificate_key")
	}
}

func TestConvertSSLWithCertKeyNoWarning(t *testing.T) {
	// SSL enabled with both cert and key should NOT produce a warning.
	input := `
http {
    server {
        listen 443 ssl;
        ssl_certificate /etc/ssl/server.crt;
        ssl_certificate_key /etc/ssl/server.key;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if s.SSL.Cert != "/etc/ssl/server.crt" {
		t.Errorf("SSL.Cert = %s, want /etc/ssl/server.crt", s.SSL.Cert)
	}
	if s.SSL.Key != "/etc/ssl/server.key" {
		t.Errorf("SSL.Key = %s, want /etc/ssl/server.key", s.SSL.Key)
	}
	if hasWarningContaining(result.Warnings, "ssl_certificate") {
		t.Error("unexpected warning about missing ssl_certificate when both are provided")
	}
}

func TestConvertServerNoListenDefault(t *testing.T) {
	// Server block without listen directive should get default 0.0.0.0:80.
	input := `
http {
    server {
        server_name example.com;
    }
}
`
	result, err := convertString(t, input)
	if err != nil {
		t.Fatalf("convert error: %v", err)
	}

	s := result.Config.Servers[0]
	if s.Listen != "0.0.0.0:80" {
		t.Errorf("Listen = %s, want 0.0.0.0:80 when no listen directive", s.Listen)
	}
}
