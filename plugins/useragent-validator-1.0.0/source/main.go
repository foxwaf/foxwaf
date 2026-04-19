package main

import (
	_ "embed"
	"net"
	"net/http"
	"strings"
)

// 拦截页（与 filename-validator 同款，不计入数据库）
//go:embed block.html
var blockPageHTML string

// User-Agent 长度上限，超长视为异常，避免 ToLower 大块分配
const maxUserAgentLen = 2048

// 扫描器/脚本 UA 关键词（小写）
var badUserAgentSubstrings = []string{
	"python", // Python-urllib, python-requests
	"go-http-client",
	"curl", "wget",
	"java/", // Java/1.8 等
	"libwww", "perl", "php",
	"masscan", "nmap", "nikto", "sqlmap",
	"acunetix", "nessus", "qualys",
	"zgrab", "zeek",
	"metasploit", "dirbuster",
	"httpclient", // Apache-HttpClient
	"scanner", "postman",
}

// ---------------- HostAPI ----------------
// 仅注入 getClientIP；签名与其它插件保持一致。
var hostGetClientIP func(r *http.Request) string

func SetHostAPI(
	_ func(ip, source, desc string, expireUnix int64) error,
	_ func(ip string) bool,
	getClientIP func(r *http.Request) string,
) {
	hostGetClientIP = getClientIP
}

// ---------------- PluginLogger (WebSocket 实时事件) ----------------
var hostLogEvent func(level, event, ip string, fields map[string]any)

func SetPluginLogger(emit func(level, event, ip string, fields map[string]any)) {
	hostLogEvent = emit
}

func logEvt(level, event, ip string, fields map[string]any) {
	if f := hostLogEvent; f != nil {
		f(level, event, ip, fields)
	}
}

// clientIPFromRequest 优先用主进程注入的可信 IP 解析，兜底使用常见代理头/RemoteAddr
func clientIPFromRequest(r *http.Request) string {
	if hostGetClientIP != nil {
		if ip := strings.TrimSpace(hostGetClientIP(r)); ip != "" {
			return ip
		}
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i >= 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(trunc)"
}

func isUAAlphaNum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

func hasOnlyUAWordChars(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isUAAlphaNum(s[i]) {
			return false
		}
	}
	return true
}

// matchKeywordWithBoundary:
// 1) 纯字母数字关键词启用边界判断，避免 "bot" 误匹配 "robot"；
// 2) 含符号关键词（如 "go-http-client"）按子串匹配，避免漏检。
func matchKeywordWithBoundary(uaLower, keywordLower string) bool {
	if uaLower == "" || keywordLower == "" {
		return false
	}
	if !hasOnlyUAWordChars(keywordLower) {
		return strings.Contains(uaLower, keywordLower)
	}

	start := 0
	for {
		idx := strings.Index(uaLower[start:], keywordLower)
		if idx < 0 {
			return false
		}
		idx += start
		end := idx + len(keywordLower)

		leftBoundary := idx == 0 || !isUAAlphaNum(uaLower[idx-1])
		rightBoundary := end == len(uaLower) || !isUAAlphaNum(uaLower[end])
		if leftBoundary && rightBoundary {
			return true
		}

		start = idx + 1
		if start >= len(uaLower) {
			return false
		}
	}
}

// classifyUA: 第二个返回值给出命中的具体原因，第三个返回值给出命中的关键词（若有）。
func classifyUA(ua string) (blocked bool, reason, keyword string) {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return true, "empty_ua", ""
	}
	if len(ua) > maxUserAgentLen {
		return true, "overlong_ua", ""
	}
	lower := strings.ToLower(ua)
	for _, sub := range badUserAgentSubstrings {
		if matchKeywordWithBoundary(lower, sub) {
			return true, "bad_keyword", sub
		}
	}
	return false, "", ""
}

func Handler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	ua := r.Header.Get("User-Agent")
	blocked, reason, kw := classifyUA(ua)
	if !blocked {
		return r, false
	}
	ip := clientIPFromRequest(r)
	fields := map[string]any{
		"path":   r.URL.Path,
		"host":   r.Host,
		"reason": reason,
		"ua":     truncate(ua, 256),
		"ua_len": len(ua),
	}
	if kw != "" {
		fields["keyword"] = kw
	}
	logEvt("block", "bad_user_agent", ip, fields)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Blocked-By", "useragent-validator")
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(blockPageHTML))
	return nil, true
}

func Init() (string, int, bool, func(http.ResponseWriter, *http.Request) (*http.Request, bool)) {
	return "useragent-validator", 5, true, Handler
}
