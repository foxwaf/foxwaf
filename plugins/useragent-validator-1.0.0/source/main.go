package main

import (
	_ "embed"
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
	"python",        // Python-urllib, python-requests
	"go-http-client",
	"curl", "wget",
	"java/",         // Java/1.8 等
	"libwww", "perl", "php",
	"masscan", "nmap", "nikto", "sqlmap",
	"acunetix", "nessus", "qualys",
	"zgrab", "zeek",
	"metasploit", "dirbuster",
	"httpclient",    // Apache-HttpClient
	"scanner", "postman",
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

// isBlockedUserAgent 无 UA、超长 UA 或命中扫描器/脚本 UA 时返回 true
func isBlockedUserAgent(ua string) bool {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return true
	}
	if len(ua) > maxUserAgentLen {
		return true
	}
	lower := strings.ToLower(ua)
	for _, sub := range badUserAgentSubstrings {
		if matchKeywordWithBoundary(lower, sub) {
			return true
		}
	}
	return false
}

func Handler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if isBlockedUserAgent(r.Header.Get("User-Agent")) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(blockPageHTML))
		return nil, true
	}
	return r, false
}

func Init() (string, int, bool, func(http.ResponseWriter, *http.Request) (*http.Request, bool)) {
	return "useragent-validator", 5, true, Handler
}
