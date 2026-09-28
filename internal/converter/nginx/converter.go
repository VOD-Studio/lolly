// Package nginx provides a converter from nginx configuration to lolly configuration.
package nginx

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"rua.plus/lolly/internal/config"
)

const (
	gzipType        = "gzip"
	offValue        = "off"
	redirectType    = "redirect"
	staticType      = "static"
	returnDirective = "return"
)

// Warning represents a conversion warning for unsupported or partially supported directives.
type Warning struct {
	Directive string
	Line      int
	File      string
	Message   string
}

// String returns a string representation of the warning.
func (w Warning) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", w.File, w.Line, w.Directive, w.Message)
}

// ConvertResult holds the conversion output.
type ConvertResult struct {
	Config   *config.Config
	Warnings []Warning
}

// upstreamInfo holds parsed upstream data for later reference.
type upstreamInfo struct {
	Targets     []config.ProxyTarget
	LoadBalance string
}

// locationClassification classifies a location block for conversion.
type locationClassification struct {
	LocType    string      // "proxy", "static", "redirect", "unsupported"
	Path       string      // location path (without modifier)
	Modifier   string      // "=", "^~", "~", "~*", "@"
	Directives []Directive // original directives in the location block
}

// listenInfo holds parsed listen directive information.
type listenInfo struct {
	Addr      string
	IsSSL     bool
	IsDefault bool
}

// unsupportedDirectives are known nginx directives that have no lolly equivalent.
var unsupportedDirectives = map[string]string{
	"if":               "the 'if' directive is not supported; consider using map or rewrite",
	"map":              "the 'map' directive is not supported; use variables config instead",
	"set":              "the 'set' directive is not supported; use variables config instead",
	"limit_req":        "the 'limit_req' directive is not supported; use rate_limit config instead",
	"limit_conn":       "the 'limit_conn' directive is not supported",
	"add_header":       "the 'add_header' directive is not supported; use security.headers config instead",
	"more_set_headers": "the 'more_set_headers' directive is not supported; use security.headers config instead",
	"auth_request":     "the 'auth_request' directive is not supported; use security.auth_request config instead",
	"split_clients":    "the 'split_clients' directive is not supported",
	"geo":              "the 'geo' directive is not supported; use access.geoip config instead",
	"range":            "the 'range' directive is not supported",
	returnDirective:    "the 'return' directive is not supported for non-redirect status codes; only 301/302 are supported",
}

// Convert converts a parsed nginx configuration to a lolly configuration.
func Convert(nginxCfg *NginxConfig) (*ConvertResult, error) {
	result := &ConvertResult{
		Config: &config.Config{},
	}

	// 1. Build upstream map from top-level and http-level upstream blocks.
	upstreams := make(map[string]*upstreamInfo)
	for i := range nginxCfg.Directives {
		d := &nginxCfg.Directives[i]
		if d.Name == "upstream" {
			info := convertUpstream(d, result)
			if len(d.Args) > 0 {
				upstreams[d.Args[0]] = info
			}
		}
		if d.Name == "http" {
			for j := range d.Block {
				bd := &d.Block[j]
				if bd.Name == "upstream" {
					info := convertUpstream(bd, result)
					if len(bd.Args) > 0 {
						upstreams[bd.Args[0]] = info
					}
				}
			}
		}
	}

	// 2. Find all server blocks: inside http blocks, or at top level.
	var serverBlocks []Directive
	for i := range nginxCfg.Directives {
		d := &nginxCfg.Directives[i]
		switch d.Name {
		case "http":
			// Check for unsupported directives at the http level.
			for j := range d.Block {
				bd := &d.Block[j]
				if bd.Name == "server" {
					serverBlocks = append(serverBlocks, d.Block[j])
				} else if msg, ok := unsupportedDirectives[bd.Name]; ok {
					result.Warnings = append(result.Warnings, Warning{
						Directive: bd.Name,
						Line:      bd.Line,
						File:      bd.File,
						Message:   msg,
					})
				}
			}
		case "server":
			serverBlocks = append(serverBlocks, *d)
		}
	}

	// 3. Convert each server block.
	for i := range serverBlocks {
		serverCfgs := convertServerBlock(&serverBlocks[i], upstreams, result)
		result.Config.Servers = append(result.Config.Servers, serverCfgs...)
	}

	return result, nil
}

// convertUpstream converts an upstream block to upstreamInfo.
func convertUpstream(d *Directive, result *ConvertResult) *upstreamInfo {
	info := &upstreamInfo{}

	for i := range d.Block {
		bd := &d.Block[i]

		switch bd.Name {
		case "server":
			target := convertUpstreamServer(bd)
			info.Targets = append(info.Targets, target)
		case "least_conn":
			info.LoadBalance = "least_conn"
		case "ip_hash":
			info.LoadBalance = "ip_hash"
		case "hash":
			// hash $variable consistent → consistent_hash
			if len(bd.Args) > 0 {
				info.LoadBalance = "consistent_hash"
			}
		case "random":
			info.LoadBalance = "random"
		default:
			result.Warnings = append(result.Warnings, Warning{
				Directive: bd.Name,
				Line:      bd.Line,
				File:      bd.File,
				Message:   fmt.Sprintf("unsupported directive in upstream block: %s", bd.Name),
			})
		}
	}

	return info
}

// convertUpstreamServer parses a server directive inside an upstream block.
func convertUpstreamServer(d *Directive) config.ProxyTarget {
	target := config.ProxyTarget{}

	if len(d.Args) > 0 {
		url := d.Args[0]
		// Add http:// prefix if no scheme present (nginx upstream servers don't have schemes)
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			url = "http://" + url
		}
		target.URL = url
	}

	for _, arg := range d.Args[1:] {
		if after, ok := strings.CutPrefix(arg, "weight="); ok {
			if v, err := strconv.Atoi(after); err == nil {
				target.Weight = v
			}
		} else if after, ok := strings.CutPrefix(arg, "max_fails="); ok {
			if v, err := strconv.Atoi(after); err == nil {
				target.MaxFails = v
			}
		} else if after, ok := strings.CutPrefix(arg, "fail_timeout="); ok {
			target.FailTimeout = parseDuration(after)
		} else if arg == "backup" {
			target.Backup = true
		} else if arg == "down" {
			target.Down = true
		}
	}

	return target
}

// convertServerBlock converts a server block directive to one or more ServerConfigs.
// If a server block has multiple listen directives (e.g., listen 80; listen 443 ssl;),
// it will be split into multiple ServerConfigs, one for each listen address.
func convertServerBlock(d *Directive, upstreams map[string]*upstreamInfo, result *ConvertResult) []config.ServerConfig {
	var listens []listenInfo
	baseServer := config.ServerConfig{}
	var serverRoot string
	var serverIndex []string

	// First pass: collect all listen directives
	for i := range d.Block {
		bd := &d.Block[i]
		if bd.Name == "listen" {
			info := parseListenInfo(bd)
			listens = append(listens, info)
		}
	}

	// Default if no listen directive
	if len(listens) == 0 {
		listens = append(listens, listenInfo{Addr: "0.0.0.0:80"})
	}

	// Second pass: process other directives into baseServer
	for i := range d.Block {
		bd := &d.Block[i]

		switch bd.Name {
		case "listen":
			// Already processed above
		case "server_name":
			parseServerName(bd, &baseServer)
		case "root":
			if len(bd.Args) > 0 {
				serverRoot = bd.Args[0]
			}
		case "index":
			serverIndex = append(serverIndex, bd.Args...)
		case "ssl_certificate":
			if len(bd.Args) > 0 {
				baseServer.SSL.Cert = bd.Args[0]
			}
		case "ssl_certificate_key":
			if len(bd.Args) > 0 {
				baseServer.SSL.Key = bd.Args[0]
			}
		case "ssl_reject_handshake":
			// nginx: ssl_reject_handshake on|off
			if len(bd.Args) > 0 && bd.Args[0] == "on" {
				baseServer.SSL.RejectHandshake = true
			}
		case gzipType:
			parseGzip(bd, &baseServer)
		case "gzip_types":
			baseServer.Compression.Types = bd.Args
		case "gzip_min_length":
			if len(bd.Args) > 0 {
				if v, err := strconv.Atoi(bd.Args[0]); err == nil {
					baseServer.Compression.MinSize = v
				}
			}
		case "client_max_body_size":
			if len(bd.Args) > 0 {
				baseServer.ClientMaxBodySize = bd.Args[0]
			}
		case "server_tokens":
			if len(bd.Args) > 0 {
				baseServer.ServerTokens = bd.Args[0] != offValue
			}
		case "access_log":
			parseAccessLog(bd, result)
		case "error_log":
			parseErrorLog(bd, result)
		case returnDirective:
			parseServerReturn(bd, &baseServer, result)
		case "rewrite":
			parseRewrite(bd, &baseServer)
		case "location":
			classification := classifyLocation(bd, serverRoot, result)
			convertLocation(classification, &baseServer, upstreams, result)
		case "error_page":
			parseErrorPage(bd, &baseServer)
		case "auth_basic":
			parseAuthBasic(bd, &baseServer)
		case "auth_basic_user_file":
			if len(bd.Args) > 0 {
				result.Warnings = append(result.Warnings, Warning{
					Directive: "auth_basic_user_file",
					Line:      bd.Line,
					File:      bd.File,
					Message:   fmt.Sprintf("auth_basic_user_file (%s) cannot be directly converted; htpasswd file must be manually migrated to auth.users", bd.Args[0]),
				})
			}
		default:
			if msg, ok := unsupportedDirectives[bd.Name]; ok {
				result.Warnings = append(result.Warnings, Warning{
					Directive: bd.Name,
					Line:      bd.Line,
					File:      bd.File,
					Message:   msg,
				})
			}
		}
	}

	// Handle server-level root for static files
	if serverRoot != "" {
		hasRootLocation := false
		for i := range baseServer.Static {
			if baseServer.Static[i].Root == "" {
				baseServer.Static[i].Root = serverRoot
			}
			if len(baseServer.Static[i].Index) == 0 && len(serverIndex) > 0 {
				baseServer.Static[i].Index = serverIndex
			}
			if baseServer.Static[i].Path == "/" {
				hasRootLocation = true
			}
		}
		if !hasRootLocation {
			for _, p := range baseServer.Proxy {
				if p.Path == "/" {
					hasRootLocation = true
					break
				}
			}
		}
		if !hasRootLocation {
			baseServer.Static = append(baseServer.Static, config.StaticConfig{
				Path:  "/",
				Root:  serverRoot,
				Index: serverIndex,
			})
		}
	}

	// Create servers for each listen address
	var servers []config.ServerConfig
	for _, li := range listens {
		server := baseServer // Copy base config
		server.Listen = li.Addr
		server.Default = li.IsDefault

		// Warn if SSL listen but no cert/key
		if li.IsSSL && (server.SSL.Cert == "" || server.SSL.Key == "") {
			result.Warnings = append(result.Warnings, Warning{
				Directive: "listen",
				Message:   "SSL is enabled via listen directive but ssl_certificate and/or ssl_certificate_key are not configured; SSL config will be incomplete",
			})
		}

		servers = append(servers, server)
	}

	return servers
}

// parseListenInfo parses a listen directive and returns structured info.
func parseListenInfo(d *Directive) listenInfo {
	info := listenInfo{}
	if len(d.Args) == 0 {
		return info
	}

	addr := d.Args[0]
	for _, arg := range d.Args[1:] {
		if arg == "ssl" {
			info.IsSSL = true
		}
		if arg == "default_server" {
			info.IsDefault = true
		}
	}

	// Format address: if just a port number, prefix with ":".
	if port, err := strconv.Atoi(addr); err == nil {
		info.Addr = fmt.Sprintf(":%d", port)
	} else if strings.Contains(addr, ":") {
		info.Addr = addr
	} else {
		info.Addr = ":" + addr
	}

	return info
}

// parseServerName parses a server_name directive.
func parseServerName(d *Directive, server *config.ServerConfig) {
	if len(d.Args) == 0 {
		return
	}

	server.Name = d.Args[0]
	server.ServerNames = append(server.ServerNames, d.Args...)
}

// parseGzip parses a gzip directive.
func parseGzip(d *Directive, server *config.ServerConfig) {
	if len(d.Args) > 0 && d.Args[0] == "on" {
		server.Compression.Type = "gzip"
	}
}

// parseAccessLog parses an access_log directive.
func parseAccessLog(d *Directive, result *ConvertResult) {
	if len(d.Args) > 0 {
		result.Config.Logging.Access.Path = d.Args[0]
	}
	if len(d.Args) > 1 {
		result.Config.Logging.Access.Format = d.Args[1]
	}
}

// parseErrorLog parses an error_log directive.
func parseErrorLog(d *Directive, result *ConvertResult) {
	if len(d.Args) > 0 {
		result.Config.Logging.Error.Path = d.Args[0]
	}
	if len(d.Args) > 1 {
		result.Config.Logging.Error.Level = d.Args[1]
	}
}

// parseServerReturn parses a return directive at server level.
func parseServerReturn(d *Directive, server *config.ServerConfig, result *ConvertResult) {
	if len(d.Args) == 0 {
		return
	}

	code, err := strconv.Atoi(d.Args[0])
	if err != nil {
		return
	}

	switch code {
	case 301:
		url := ""
		if len(d.Args) > 1 {
			url = d.Args[1]
		}
		server.Rewrite = append(server.Rewrite, config.RewriteRule{
			Pattern:     "^/",
			Replacement: url,
			Flag:        "permanent",
		})
	case 302:
		url := ""
		if len(d.Args) > 1 {
			url = d.Args[1]
		}
		server.Rewrite = append(server.Rewrite, config.RewriteRule{
			Pattern:     "^/",
			Replacement: url,
			Flag:        "redirect",
		})
	default:
		result.Warnings = append(result.Warnings, Warning{
			Directive: returnDirective,
			Line:      d.Line,
			File:      d.File,
			Message:   fmt.Sprintf("return %d is not a redirect; only 301/302 are supported at server level", code),
		})
	}
}

// parseRewrite parses a rewrite directive.
func parseRewrite(d *Directive, server *config.ServerConfig) {
	if len(d.Args) < 2 {
		return
	}

	rule := config.RewriteRule{
		Pattern:     d.Args[0],
		Replacement: d.Args[1],
	}

	if len(d.Args) > 2 {
		rule.Flag = d.Args[2]
	}

	server.Rewrite = append(server.Rewrite, rule)
}

// parseErrorPage parses an error_page directive.
func parseErrorPage(d *Directive, server *config.ServerConfig) {
	// error_page 404 500 50x.html
	// error_page 404 /404.html
	if len(d.Args) < 2 {
		return
	}

	// Last arg is the page path.
	pagePath := d.Args[len(d.Args)-1]

	if server.Security.ErrorPage.Pages == nil {
		server.Security.ErrorPage.Pages = make(map[int]string)
	}

	for _, arg := range d.Args[:len(d.Args)-1] {
		if code, err := strconv.Atoi(arg); err == nil {
			server.Security.ErrorPage.Pages[code] = pagePath
		}
	}
}

// parseAuthBasic parses an auth_basic directive.
func parseAuthBasic(d *Directive, server *config.ServerConfig) {
	if len(d.Args) > 0 {
		if d.Args[0] != offValue {
			server.Security.Auth.Type = "basic"
			server.Security.Auth.Realm = d.Args[0]
		}
	}
}

// classifyLocation classifies a location block based on its directives.
func classifyLocation(d *Directive, serverRoot string, result *ConvertResult) locationClassification {
	class := locationClassification{
		Directives: d.Block,
	}

	// Parse location path and modifier.
	if len(d.Args) > 0 {
		first := d.Args[0]
		switch first {
		case "=", "^~", "~", "~*":
			class.Modifier = first
			if len(d.Args) > 1 {
				class.Path = d.Args[1]
			}
		default:
			if strings.HasPrefix(first, "@") {
				class.Modifier = "@"
				class.Path = first[1:]
			} else {
				class.Path = first
			}
		}
	}

	// Classify based on content.
	hasProxyPass := false
	hasRootOrAlias := false
	hasTryFiles := false
	hasRedirect := false

	for i := range d.Block {
		switch d.Block[i].Name {
		case "proxy_pass":
			hasProxyPass = true
		case "root", "alias":
			hasRootOrAlias = true
		case "try_files":
			hasTryFiles = true
		case returnDirective:
			if len(d.Block[i].Args) > 0 {
				code, err := strconv.Atoi(d.Block[i].Args[0])
				if err == nil && (code == 301 || code == 302) {
					hasRedirect = true
				}
			}
		}
	}

	switch {
	case hasProxyPass:
		class.LocType = "proxy"
		if hasRootOrAlias {
			result.Warnings = append(result.Warnings, Warning{
				Directive: "location",
				Line:      d.Line,
				File:      d.File,
				Message:   "location has both proxy_pass and root/alias; proxy_pass takes priority",
			})
		}
	case hasRootOrAlias || hasTryFiles:
		class.LocType = staticType
	case hasRedirect:
		class.LocType = redirectType
	default:
		// If no explicit root/alias/try_files but server-level root exists,
		// classify as static (will inherit server root)
		if serverRoot != "" {
			class.LocType = staticType
		} else {
			class.LocType = "unsupported"
		}
	}

	return class
}

// convertLocation converts a classified location to the appropriate config entries.
func convertLocation(class locationClassification, server *config.ServerConfig, upstreams map[string]*upstreamInfo, result *ConvertResult) {
	locType := modifierToLocationType(class.Modifier)

	switch class.LocType {
	case "proxy":
		proxy := config.ProxyConfig{
			Path:         class.Path,
			LocationType: locType,
		}

		if class.Modifier == "@" {
			proxy.LocationName = class.Path
		}

		convertProxyDirectives(class.Directives, &proxy, upstreams, result)
		server.Proxy = append(server.Proxy, proxy)

	case staticType:
		static := config.StaticConfig{
			Path:         class.Path,
			LocationType: locType,
		}

		convertStaticDirectives(class.Directives, &static, result)
		server.Static = append(server.Static, static)

	case "redirect":
		convertRedirectDirectives(class.Directives, class.Path, server, result)

	case "unsupported":
		if len(class.Directives) == 0 {
			result.Warnings = append(result.Warnings, Warning{
				Directive: "location",
				Message:   fmt.Sprintf("location %s has no content and is unsupported", class.Path),
			})
		}
		for i := range class.Directives {
			bd := &class.Directives[i]
			result.Warnings = append(result.Warnings, Warning{
				Directive: bd.Name,
				Line:      bd.Line,
				File:      bd.File,
				Message:   fmt.Sprintf("unsupported directive in location: %s", bd.Name),
			})
		}
	}
}

// modifierToLocationType maps nginx location modifiers to lolly location types.
func modifierToLocationType(modifier string) string {
	switch modifier {
	case "=":
		return "exact"
	case "^~":
		return "prefix_priority"
	case "~":
		return "regex"
	case "~*":
		return "regex_caseless"
	case "@":
		return "named"
	default:
		return ""
	}
}

// convertProxyDirectives converts directives within a proxy location block.
func convertProxyDirectives(directives []Directive, proxy *config.ProxyConfig, upstreams map[string]*upstreamInfo, result *ConvertResult) {
	for i := range directives {
		d := &directives[i]

		switch d.Name {
		case "proxy_pass":
			if len(d.Args) > 0 {
				url := d.Args[0]
				// Check if URL references an upstream name (no scheme).
				if upstreamName := extractUpstreamName(url); upstreamName != "" {
					if info, ok := upstreams[upstreamName]; ok {
						proxy.Targets = append(proxy.Targets, info.Targets...)
						if info.LoadBalance != "" && proxy.LoadBalance == "" {
							proxy.LoadBalance = info.LoadBalance
						}
					} else {
						// Upstream not found; use URL as-is.
						proxy.Targets = append(proxy.Targets, config.ProxyTarget{URL: url})
					}
				} else {
					proxy.Targets = append(proxy.Targets, config.ProxyTarget{URL: url})
				}
			}
		case "proxy_set_header":
			if len(d.Args) >= 2 {
				if proxy.Headers.SetRequest == nil {
					proxy.Headers.SetRequest = make(map[string]string)
				}
				value := mapVariable(d.Args[1], result, d)
				// Convert the empty string marker back to actual empty string
				if value == `""` {
					value = ""
				}
				proxy.Headers.SetRequest[d.Args[0]] = value
			}
		case "proxy_hide_header":
			if len(d.Args) > 0 {
				proxy.Headers.HideResponse = append(proxy.Headers.HideResponse, d.Args[0])
			}
		case "proxy_pass_header":
			if len(d.Args) > 0 {
				proxy.Headers.PassResponse = append(proxy.Headers.PassResponse, d.Args[0])
			}
		case "proxy_redirect":
			convertProxyRedirect(d, proxy)
		case "proxy_connect_timeout":
			if len(d.Args) > 0 {
				proxy.Timeout.Connect = parseDuration(d.Args[0])
			}
		case "proxy_read_timeout":
			if len(d.Args) > 0 {
				proxy.Timeout.Read = parseDuration(d.Args[0])
			}
		case "proxy_send_timeout":
			if len(d.Args) > 0 {
				proxy.Timeout.Write = parseDuration(d.Args[0])
			}
		case "proxy_cache":
			proxy.Cache.Enabled = true
		case "proxy_cache_valid":
			parseProxyCacheValid(d, proxy)
		default:
			if msg, ok := unsupportedDirectives[d.Name]; ok {
				result.Warnings = append(result.Warnings, Warning{
					Directive: d.Name,
					Line:      d.Line,
					File:      d.File,
					Message:   msg,
				})
			}
		}
	}
}

// extractUpstreamName extracts an upstream name from a proxy_pass URL.
// If the URL has no scheme (e.g., "http://upstream_name" where upstream_name
// has no port), it returns the host portion. Otherwise returns empty string.
func extractUpstreamName(url string) string {
	if _, rest, ok := strings.Cut(url, "://"); ok {
		host := rest
		if slashIdx := strings.IndexAny(host, "/?"); slashIdx >= 0 {
			host = host[:slashIdx]
		}
		// Check if this is an upstream reference by looking up known upstream names.
		// An upstream name is a host with no port and no dot (not an IP or domain).
		if !strings.Contains(host, ":") && !strings.Contains(host, ".") && host != "" {
			return host
		}
	}
	return ""
}

// mapVariable replaces nginx variables with lolly equivalents.
func mapVariable(value string, result *ConvertResult, d *Directive) string {
	if strings.Contains(value, "$proxy_add_x_forwarded_for") {
		result.Warnings = append(result.Warnings, Warning{
			Directive: "proxy_set_header",
			Line:      d.Line,
			File:      d.File,
			Message:   "$proxy_add_x_forwarded_for is replaced with $remote_addr; lolly automatically appends to X-Forwarded-For",
		})
		return strings.ReplaceAll(value, "$proxy_add_x_forwarded_for", "$remote_addr")
	}
	return value
}

// convertProxyRedirect handles proxy_redirect directive.
func convertProxyRedirect(d *Directive, proxy *config.ProxyConfig) {
	if len(d.Args) == 0 {
		return
	}

	rr := &config.RedirectRewriteConfig{}

	switch d.Args[0] {
	case "off":
		rr.Mode = "off"
	case "default":
		rr.Mode = "default"
	default:
		rr.Mode = "custom"
		if len(d.Args) >= 2 {
			rr.Rules = append(rr.Rules, config.RedirectRewriteRule{
				Pattern:     d.Args[0],
				Replacement: d.Args[1],
			})
		}
	}

	proxy.RedirectRewrite = rr
}

// parseProxyCacheValid parses proxy_cache_valid directive.
func parseProxyCacheValid(d *Directive, proxy *config.ProxyConfig) {
	// proxy_cache_valid 200 10m
	// proxy_cache_valid 301 302 1h
	// proxy_cache_valid any 1m
	if len(d.Args) < 2 {
		return
	}

	if proxy.CacheValid == nil {
		proxy.CacheValid = &config.ProxyCacheValidConfig{}
	}

	// Last arg is the duration.
	dur := parseDuration(d.Args[len(d.Args)-1])

	for _, arg := range d.Args[:len(d.Args)-1] {
		switch arg {
		case "200", "201", "202", "203", "204", "205", "206", "207", "208", "226":
			proxy.CacheValid.OK = dur
		case "301", "302":
			proxy.CacheValid.Redirect = dur
		case "404":
			proxy.CacheValid.NotFound = dur
		case "any":
			proxy.CacheValid.OK = dur
			proxy.CacheValid.Redirect = dur
			proxy.CacheValid.NotFound = dur
			proxy.CacheValid.ClientError = dur
			proxy.CacheValid.ServerError = dur
		default:
			code, err := strconv.Atoi(arg)
			if err == nil {
				switch {
				case code >= 400 && code < 500 && code != 404:
					proxy.CacheValid.ClientError = dur
				case code >= 500:
					proxy.CacheValid.ServerError = dur
				}
			}
		}
	}
}

// convertStaticDirectives converts directives within a static location block.
func convertStaticDirectives(directives []Directive, static *config.StaticConfig, result *ConvertResult) {
	for i := range directives {
		d := &directives[i]

		switch d.Name {
		case "root":
			if len(d.Args) > 0 {
				static.Root = d.Args[0]
			}
		case "alias":
			if len(d.Args) > 0 {
				static.Alias = d.Args[0]
			}
		case "index":
			static.Index = append(static.Index, d.Args...)
		case "try_files":
			static.TryFiles = append(static.TryFiles, d.Args...)
		case "expires":
			if len(d.Args) > 0 {
				static.Expires = d.Args[0]
			}
		default:
			if msg, ok := unsupportedDirectives[d.Name]; ok {
				result.Warnings = append(result.Warnings, Warning{
					Directive: d.Name,
					Line:      d.Line,
					File:      d.File,
					Message:   msg,
				})
			}
		}
	}
}

// convertRedirectDirectives converts redirect directives within a location block.
func convertRedirectDirectives(directives []Directive, locPath string, server *config.ServerConfig, result *ConvertResult) {
	for i := range directives {
		d := &directives[i]

		if d.Name != returnDirective {
			continue
		}

		if len(d.Args) < 2 {
			continue
		}

		code, err := strconv.Atoi(d.Args[0])
		if err != nil {
			continue
		}

		url := d.Args[1]
		pattern := "^" + locPath + "$"

		switch code {
		case 301:
			server.Rewrite = append(server.Rewrite, config.RewriteRule{
				Pattern:     pattern,
				Replacement: url,
				Flag:        "permanent",
			})
			result.Warnings = append(result.Warnings, Warning{
				Directive: returnDirective,
				Line:      d.Line,
				File:      d.File,
				Message:   "return 301 converted to rewrite rule with permanent flag",
			})
		case 302:
			server.Rewrite = append(server.Rewrite, config.RewriteRule{
				Pattern:     pattern,
				Replacement: url,
				Flag:        "redirect",
			})
			result.Warnings = append(result.Warnings, Warning{
				Directive: returnDirective,
				Line:      d.Line,
				File:      d.File,
				Message:   "return 302 converted to rewrite rule with redirect flag",
			})
		default:
			result.Warnings = append(result.Warnings, Warning{
				Directive: returnDirective,
				Line:      d.Line,
				File:      d.File,
				Message:   fmt.Sprintf("return %d in location is not a redirect; only 301/302 are supported", code),
			})
		}
	}
}

// parseDuration parses a time duration string.
// Supports nginx-style durations: "10s", "5m", "1h", "1d".
func parseDuration(s string) time.Duration {
	if s == "" {
		return 0
	}

	// Try standard Go duration first.
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}

	// Handle nginx-style durations without Go support.
	s = strings.TrimSpace(s)
	numStr := s[:len(s)-1]
	unit := s[len(s)-1]

	value, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0
	}

	switch unit {
	case 's':
		return time.Duration(value) * time.Second
	case 'm':
		return time.Duration(value) * time.Minute
	case 'h':
		return time.Duration(value) * time.Hour
	case 'd':
		return time.Duration(value) * 24 * time.Hour
	default:
		return 0
	}
}
