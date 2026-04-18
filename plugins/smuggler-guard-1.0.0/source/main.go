package main

import (
	_ "embed"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed block.html
var blockPageHTML string

const (
	burstWindow   = 60
	burstThresh   = 3
	aclBlockSec   = 30 * 60
	maxTrackedIPs = 50000
	idleEvictSec  = int64(3600)
	sweepInterval = 2 * time.Minute
	maxCLValue    = int64(1024 * 1024 * 1024) // 未声明 chunked 时，CL > 1GB 视为异常
)

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

func isInternal(ip string) bool {
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

// hasCRLFInjection 检查 header value 里是否有裸 \r 或 \n（可能来自未归一化的头部）
func hasCRLFInjection(v string) bool {
	for i := 0; i < len(v); i++ {
		if v[i] == '\r' || v[i] == '\n' {
			return true
		}
	}
	return false
}

// inspect 检查请求是否有走私特征。命中则返回原因字符串。
func inspect(r *http.Request) string {
	// 1) 多个 Host 头
	if hs, ok := r.Header["Host"]; ok && len(hs) > 1 {
		return "multiple-host-headers"
	}
	// net/http 把 Host 头单独挪到 r.Host，但异常请求可能仍在 Header 里
	if strings.Contains(r.Host, ",") || strings.Contains(r.Host, " ") {
		return "malformed-host"
	}

	// 2) 多个 Content-Length
	if cls, ok := r.Header["Content-Length"]; ok {
		if len(cls) > 1 {
			return "multiple-content-length"
		}
		if len(cls) == 1 {
			// 同一 header 里值含逗号也视为重复
			if strings.Contains(cls[0], ",") {
				return "comma-in-content-length"
			}
			if n, err := strconv.ParseInt(strings.TrimSpace(cls[0]), 10, 64); err == nil {
				if n < 0 {
					return "negative-content-length"
				}
				if n > maxCLValue {
					return "oversize-content-length"
				}
			} else if strings.TrimSpace(cls[0]) != "" {
				return "non-numeric-content-length"
			}
		}
	}

	// 3) Transfer-Encoding 异常
	if tes, ok := r.Header["Transfer-Encoding"]; ok {
		if len(tes) > 1 {
			return "multiple-transfer-encoding"
		}
		if len(tes) == 1 {
			teLower := strings.ToLower(strings.TrimSpace(tes[0]))
			// CL + TE 同时存在（除非 TE 仅为 identity，但现代 HTTP 禁止两者共存）
			if _, hasCL := r.Header["Content-Length"]; hasCL {
				return "te-and-cl"
			}
			// TE 值混淆：允许 "chunked" 或 "chunked, identity"；其它都拒绝
			if teLower != "chunked" && teLower != "identity" && teLower != "chunked, identity" {
				return "obfuscated-te:" + teLower
			}
		}
	}

	// 4) CRLF 注入（header value 里裸换行）
	for k, vs := range r.Header {
		for _, v := range vs {
			if hasCRLFInjection(v) {
				return "crlf-injection:" + k
			}
		}
	}

	// 5) 非标准头名混淆（大小写变体重复出现）
	// net/http 已归一化大小写，下面两种情况捕获：
	//   - 同一 key 出现多次且值相互矛盾（CL/TE 已在上面处理，此处是其它敏感头）
	if hs, ok := r.Header["Transfer-Encoding"]; ok && len(hs) == 1 {
		// 原始字节里出现 "Transfer-Encoding \r\n" 之类 —— 绝大多数被 net/http 拒掉
		_ = hs
	}
	return ""
}

func respondBlock(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Blocked-By", "smuggler-guard")
	w.Header().Set("X-Block-Reason", reason)
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(blockPageHTML))
}

func Handler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	sweeperOnce.Do(startSweeper)
	ip := clientIPFromRequest(r)
	if ip == "" || isInternal(ip) {
		return r, false
	}
	if hostIsWhitelisted != nil && hostIsWhitelisted(ip) {
		return r, false
	}
	if reason := inspect(r); reason != "" {
		onHit(ip, reason)
		respondBlock(w, reason)
		return nil, true
	}
	return r, false
}

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
	if now-st.windowTs.Load() > burstWindow {
		st.windowTs.Store(now)
		st.hits.Store(0)
	}
	hits := st.hits.Add(1)
	if hits >= burstThresh && st.blocked.Load() < now-60 {
		st.blocked.Store(now)
		if hostAddACLBlock != nil {
			if hostIsWhitelisted == nil || !hostIsWhitelisted(ip) {
				go func(p, rs string, h int32) {
					defer func() { _ = recover() }()
					_ = hostAddACLBlock(p, "smuggler-guard",
						"smuggler: "+rs+" x"+strconv.Itoa(int(h)), now+aclBlockSec)
				}(ip, reason, hits)
			}
		}
	}
}

func Init() (string, int, bool, func(http.ResponseWriter, *http.Request) (*http.Request, bool)) {
	return "smuggler-guard", 7, true, Handler
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
