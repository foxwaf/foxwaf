package main

import (
	"bytes"
	_ "embed"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed block.html
var blockPageHTML string

const (
	maxBodyScan   = 32 * 1024
	maxTrackedIPs = 50000
	burstWindow   = 30                  // 秒
	burstThresh   = 5                   // 30s 内命中 5 次 → ACL 封
	aclBlockSec   = 30 * 60             // 30 分钟
	idleEvictSec  = int64(3600)
	sweepInterval = 2 * time.Minute
)

// 云元数据/内网地址字符串黑名单（小写子串）
var ssrfHostBlacklist = []string{
	"169.254.169.254",       // AWS/Azure/GCP IMDS
	"metadata.google.internal",
	"metadata.goog",
	"100.100.100.200",       // 阿里云
	"169.254.169.253",       // OpenStack 变种
	"fd00:ec2::254",         // AWS IPv6 IMDS
	"[fd00:ec2::254]",
	"[::ffff:169.254.169.254]",
}

// 危险 scheme
var dangerousSchemes = []string{
	"gopher://", "dict://", "file://", "ftp://", "ldap://", "ldaps://",
	"jar://", "netdoc://", "sftp://", "tftp://", "ssh://", "telnet://",
}

// 内网 CIDR（命中即告警）
var internalCIDRs = func() []*net.IPNet {
	cidrs := []string{
		"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"169.254.0.0/16", "100.64.0.0/10",
		"::1/128", "fc00::/7", "fe80::/10",
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, e := net.ParseCIDR(c); e == nil {
			out = append(out, n)
		}
	}
	return out
}()

var whitelistCIDRs = func() []*net.IPNet {
	cidrs := []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, e := net.ParseCIDR(c); e == nil {
			out = append(out, n)
		}
	}
	return out
}()

// ---------------- HostAPI ----------------
var (
	hostAddACLBlock   func(ip, source, desc string, expireUnix int64) error
	hostIsWhitelisted func(ip string) bool
	hostGetClientIP   func(r *http.Request) string
)

func SetHostAPI(
	addACLBlock func(ip, source, desc string, expireUnix int64) error,
	isWhitelisted func(ip string) bool,
	getClientIP func(r *http.Request) string,
) {
	hostAddACLBlock = addACLBlock
	hostIsWhitelisted = isWhitelisted
	hostGetClientIP = getClientIP
}

type ipState struct {
	hits      atomic.Int32
	windowTs  atomic.Int64
	lastTouch atomic.Int64
	blocked   atomic.Int64
}

var (
	ipMap       sync.Map
	trackedN    atomic.Int64
	sweeperOnce sync.Once
)

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

func isInternalClient(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range whitelistCIDRs {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// --------------- 检测 ---------------
// normalizeHost 尝试把 hex/oct/dec/unicode 编码的 IP 解析成标准 IP
func normalizeHost(h string) string {
	h = strings.TrimSpace(h)
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return ""
	}
	// 去掉方括号（IPv6）
	if strings.HasPrefix(h, "[") && strings.Contains(h, "]") {
		h = strings.TrimPrefix(h, "[")
		h = strings.SplitN(h, "]", 2)[0]
	}
	// 处理形如 http://user@host 里的 host 部分 —— 这里 h 已是纯 host
	// 十进制整数 IP：2130706433 → 127.0.0.1
	if n, err := strconv.ParseUint(h, 10, 32); err == nil && !strings.ContainsAny(h, ".") {
		return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n)).String()
	}
	// 十六进制 0x7f000001
	if strings.HasPrefix(h, "0x") || strings.HasPrefix(h, "0X") {
		if n, err := strconv.ParseUint(h[2:], 16, 32); err == nil {
			return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n)).String()
		}
	}
	return h
}

func ipIsInternal(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range internalCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// scanSSRF 在给定字符串（小写）中查找 SSRF 特征。返回命中描述或空串。
func scanSSRF(s string) string {
	if s == "" {
		return ""
	}
	// 1) 危险 scheme
	for _, sc := range dangerousSchemes {
		if strings.Contains(s, sc) {
			return "scheme:" + strings.TrimSuffix(sc, "://")
		}
	}
	// 2) 云元数据关键字
	for _, h := range ssrfHostBlacklist {
		if strings.Contains(s, h) {
			return "metadata:" + h
		}
	}
	// 3) http(s)://<host> 里的 host 解码后若是内网 IP → 命中
	//    注意 s 是小写的 query 或 body 片段，可能包含 URL 编码
	decoded, err := url.QueryUnescape(s)
	if err != nil {
		decoded = s
	}
	// 扫描每个 http/https URL 中的 host
	for _, prefix := range []string{"http://", "https://"} {
		idx := 0
		for {
			pos := strings.Index(decoded[idx:], prefix)
			if pos < 0 {
				break
			}
			start := idx + pos + len(prefix)
			end := start
			for end < len(decoded) {
				c := decoded[end]
				if c == '/' || c == '?' || c == '#' || c == ' ' || c == '"' || c == '\'' || c == '&' {
					break
				}
				end++
			}
			hostPart := decoded[start:end]
			// 处理 user@host
			if at := strings.LastIndex(hostPart, "@"); at >= 0 {
				hostPart = hostPart[at+1:]
			}
			// 去掉 :port
			if colon := strings.LastIndex(hostPart, ":"); colon >= 0 && !strings.Contains(hostPart, "]") {
				hostPart = hostPart[:colon]
			}
			norm := normalizeHost(hostPart)
			if norm != "" && ipIsInternal(norm) {
				return "internal-ip:" + norm
			}
			idx = end
		}
	}
	return ""
}

func snapshotBody(r *http.Request) []byte {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	buf := make([]byte, maxBodyScan)
	n, _ := io.ReadFull(io.LimitReader(r.Body, maxBodyScan), buf)
	if n <= 0 {
		r.Body = io.NopCloser(bytes.NewReader(nil))
		return nil
	}
	rest, _ := io.ReadAll(r.Body)
	combined := make([]byte, 0, n+len(rest))
	combined = append(combined, buf[:n]...)
	combined = append(combined, rest...)
	r.Body = io.NopCloser(bytes.NewReader(combined))
	return buf[:n]
}

func respondBlock(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Blocked-By", "cloud-ssrf")
	w.Header().Set("X-Block-Reason", reason)
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(blockPageHTML))
}

func Handler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	sweeperOnce.Do(startSweeper)
	ip := clientIPFromRequest(r)
	if ip == "" || isInternalClient(ip) {
		return r, false
	}
	if hostIsWhitelisted != nil && hostIsWhitelisted(ip) {
		return r, false
	}

	// 扫描 URL query + path
	target := strings.ToLower(r.URL.Path + "?" + r.URL.RawQuery)
	if reason := scanSSRF(target); reason != "" {
		onHit(ip, reason)
		respondBlock(w, reason)
		return nil, true
	}
	// 扫描 Referer / X-Forwarded-Host 头（常见 SSRF 附着位置）
	for _, h := range []string{"Referer", "X-Forwarded-Host", "X-Client-IP", "X-Original-URL", "X-Rewrite-URL"} {
		if v := r.Header.Get(h); v != "" {
			if reason := scanSSRF(strings.ToLower(v)); reason != "" {
				onHit(ip, reason)
				respondBlock(w, "header:"+h+":"+reason)
				return nil, true
			}
		}
	}
	// POST/PUT/PATCH 扫描 body 前 32KB
	if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" {
		ct := strings.ToLower(r.Header.Get("Content-Type"))
		if ct == "" || strings.Contains(ct, "json") || strings.Contains(ct, "text") ||
			strings.Contains(ct, "xml") || strings.Contains(ct, "urlencoded") ||
			strings.Contains(ct, "form-data") {
			body := snapshotBody(r)
			if len(body) > 0 {
				if reason := scanSSRF(strings.ToLower(string(body))); reason != "" {
					onHit(ip, reason)
					respondBlock(w, "body:"+reason)
					return nil, true
				}
			}
		}
	}
	return r, false
}

// onHit 累加命中，5 次 / 30s 注入 ACL
func onHit(ip, reason string) {
	now := time.Now().Unix()
	v, ok := ipMap.Load(ip)
	if !ok {
		if trackedN.Load() >= maxTrackedIPs {
			return
		}
		st := &ipState{}
		st.lastTouch.Store(now)
		st.windowTs.Store(now)
		actual, loaded := ipMap.LoadOrStore(ip, st)
		if !loaded {
			trackedN.Add(1)
		}
		v = actual
	}
	st := v.(*ipState)
	st.lastTouch.Store(now)
	// 窗口滑动
	if now-st.windowTs.Load() > burstWindow {
		st.windowTs.Store(now)
		st.hits.Store(0)
	}
	hits := st.hits.Add(1)
	if hits >= burstThresh && st.blocked.Load() < now-60 {
		st.blocked.Store(now)
		if hostAddACLBlock != nil {
			if hostIsWhitelisted == nil || !hostIsWhitelisted(ip) {
				go func(p, rs string) {
					defer func() { _ = recover() }()
					_ = hostAddACLBlock(p, "cloud-ssrf",
						"cloud-ssrf: "+rs+" x"+itoa(int(hits)), now+aclBlockSec)
				}(ip, reason)
			}
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func Init() (string, int, bool, func(http.ResponseWriter, *http.Request) (*http.Request, bool)) {
	return "cloud-ssrf", 5, true, Handler
}

func startSweeper() {
	go func() {
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for range t.C {
			now := time.Now().Unix()
			ipMap.Range(func(k, v any) bool {
				st := v.(*ipState)
				if now-st.lastTouch.Load() > idleEvictSec {
					ipMap.Delete(k)
					trackedN.Add(-1)
				}
				return true
			})
		}
	}()
}
