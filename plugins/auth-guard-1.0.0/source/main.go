package main

import (
	_ "embed"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed block.html
var blockPageHTML string

const (
	windowSec      = 10
	threshold      = 10                // 10s 内 10 次登录失败 → 封
	bucketCount    = 10
	bucketWidthSec = windowSec / bucketCount
	blockSec       = 60 * 60           // ACL 封禁 1 小时
	maxTrackedIPs  = 50000
	sweepInterval  = 1 * time.Minute
	idleEvictSec   = int64(windowSec + blockSec + 120)
)

// 认证端点匹配子串（小写 path 包含即命中）
var authEndpoints = []string{
	"/login", "/signin", "/sign-in",
	"/oauth/token", "/oauth2/token",
	"/api/auth", "/api/login", "/api/signin",
	"/wp-login.php", "/user/login",
	"/admin/login", "/adminer",
	"/.well-known/webfinger",
	"/auth/realms",
}

// 统计的失败状态
// 认证端点上的任意 4xx 都视为失败尝试（含 404 —— 扫 wp-login.php / /admin/login 等不存在路径也是典型撞库特征）
var countedStatuses = map[int]struct{}{
	400: {}, 401: {}, 403: {}, 404: {}, 422: {}, 429: {},
}

var whitelistCIDRs = func() []*net.IPNet {
	cidrs := []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}
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

type ipStat struct {
	buckets    [bucketCount]atomic.Int32
	bucketTs   [bucketCount]atomic.Int64
	blockUntil atomic.Int64
	lastTouch  atomic.Int64
}

var (
	ipStats     sync.Map
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

func isAuthEndpoint(path string) bool {
	p := strings.ToLower(path)
	for _, s := range authEndpoints {
		if strings.Contains(p, s) {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ---------------- 插件导出 ----------------
// Handler：HostAPI 已注入时全部依赖中央 ACL，自身 O(1) 放行
func Handler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	sweeperOnce.Do(startSweeper)
	if hostAddACLBlock != nil {
		return r, false
	}
	// 兜底：HostAPI 未注入时的本地拦截
	ip := clientIPFromRequest(r)
	if ip == "" || isInternal(ip) {
		return r, false
	}
	v, ok := ipStats.Load(ip)
	if !ok {
		return r, false
	}
	st := v.(*ipStat)
	now := time.Now().Unix()
	if st.blockUntil.Load() > now {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Blocked-By", "auth-guard")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(blockPageHTML))
		return nil, true
	}
	return r, false
}

// AfterResponse：仅对认证端点上的 400/401/403/422 计数
func AfterResponse(r *http.Request, statusCode int, _ http.Header) {
	if _, hit := countedStatuses[statusCode]; !hit {
		return
	}
	if !isAuthEndpoint(r.URL.Path) {
		return
	}
	ip := clientIPFromRequest(r)
	if ip == "" || isInternal(ip) {
		return
	}
	now := time.Now().Unix()
	logEvt("hit", "auth_fail", ip, map[string]any{"path": r.URL.Path, "status": statusCode})

	v, ok := ipStats.Load(ip)
	if !ok {
		if trackedN.Load() >= maxTrackedIPs {
			return
		}
		st := &ipStat{}
		st.lastTouch.Store(now)
		actual, loaded := ipStats.LoadOrStore(ip, st)
		if !loaded {
			trackedN.Add(1)
		}
		v = actual
	}
	st := v.(*ipStat)
	st.lastTouch.Store(now)
	if st.blockUntil.Load() > now {
		return
	}

	slot := int(now/int64(bucketWidthSec)) % bucketCount
	expectTs := now - (now % int64(bucketWidthSec))
	if st.bucketTs[slot].Load() != expectTs {
		st.bucketTs[slot].Store(expectTs)
		st.buckets[slot].Store(0)
	}
	st.buckets[slot].Add(1)

	var sum int32
	cutoff := now - int64(windowSec)
	for i := 0; i < bucketCount; i++ {
		if st.bucketTs[i].Load() > cutoff {
			sum += st.buckets[i].Load()
		}
	}
	if sum >= threshold {
		for i := 0; i < bucketCount; i++ {
			st.buckets[i].Store(0)
		}
		logEvt("block", "brute_force", ip, map[string]any{"fails": sum, "window": windowSec, "ttl": blockSec})
		const localDedupSec = 30
		if hostAddACLBlock == nil {
			st.blockUntil.Store(now + blockSec)
		} else {
			st.blockUntil.Store(now + localDedupSec)
		}
		if hostAddACLBlock != nil {
			if hostIsWhitelisted == nil || !hostIsWhitelisted(ip) {
				desc := "auth-guard: " + itoa(int(sum)) + " 次认证失败 / " + itoa(windowSec) + "s"
				go func(p, d string, e int64) {
					defer func() { _ = recover() }()
					_ = hostAddACLBlock(p, "auth-guard", d, e)
				}(ip, desc, now+blockSec)
			}
		}
	}
}

func Init() (string, int, bool, func(http.ResponseWriter, *http.Request) (*http.Request, bool)) {
	return "auth-guard", 6, true, Handler
}

func startSweeper() {
	go func() {
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for range t.C {
			now := time.Now().Unix()
			ipStats.Range(func(k, v any) bool {
				st := v.(*ipStat)
				if now-st.lastTouch.Load() > idleEvictSec && st.blockUntil.Load() <= now {
					ipStats.Delete(k)
					trackedN.Add(-1)
				}
				return true
			})
		}
	}()
}
