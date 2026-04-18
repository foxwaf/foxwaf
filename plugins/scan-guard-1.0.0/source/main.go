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

// --- 可调参数（编译期常量，避免热路径读 map） -----------------------------
const (
	windowSec      = 10                    // 滑动窗口（秒）
	threshold      = 30                    // 窗口内错误响应阈值
	blockSec       = 600                   // 命中阈值后拉黑时长（秒）
	bucketCount    = 10                    // 环形分桶数，1s/桶 → 10s 窗口
	bucketWidthSec = windowSec / bucketCount
	maxTrackedIPs  = 100000                // LRU 上限，防 IP 洪水撑爆内存
	ipv6Prefix     = 64                    // IPv6 按 /64 聚合
	sweepInterval  = 30 * time.Second      // 清理空闲条目的后台频率
	idleEvictSec   = int64(windowSec + blockSec + 60) // 空闲多久回收
)

// 要统计的"错误"状态码（按用户要求：400/401/403/404/405）
// 注意：本插件自己返回的 403 因"已在黑名单"路径不会走 AfterResponse（我们在 Handler 里就返回了，主流程 logTraffic 前没走过上游）；
// 但 WAF/ACL/CC 产生的 403 依然会被计入 —— 这符合"扫描行为往往伴随 403/405 组合"的探测特征。
var countedStatuses = map[int]struct{}{
	400: {}, 401: {}, 403: {}, 404: {}, 405: {},
}

// 白名单：内网/回环不参与统计，不参与拦截
var whitelistCIDRs = func() []*net.IPNet {
	cidrs := []string{
		"127.0.0.0/8", "::1/128",
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"fc00::/7", "fe80::/10",
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// --- 数据结构 -------------------------------------------------------------

// ipStat 单 IP 的环形桶 + 拉黑状态；热路径全 atomic，无锁
type ipStat struct {
	buckets    [bucketCount]atomic.Int32
	bucketTs   [bucketCount]atomic.Int64 // 每桶对应的秒级时间戳
	blockUntil atomic.Int64              // Unix 秒；>now 则处于拉黑状态
	lastTouch  atomic.Int64              // 上次触达时间，用于 sweeper 清理空闲条目
}

var (
	ipStats     sync.Map // key: canonical IP string, value: *ipStat
	trackedN    atomic.Int64
	sweeperOnce sync.Once
)

// --- HostAPI（由主进程通过 SetHostAPI 注入） -----------------------------
// 注意：跨 plugin 边界不能传递命名类型，必须用函数值
var (
	hostAddACLBlock   func(ip, source, desc string, expireUnix int64) error
	hostIsWhitelisted func(ip string) bool
	hostGetClientIP   func(r *http.Request) string // 主进程可信代理感知的客户端 IP
)

// SetHostAPI 由主进程在加载插件时调用，注入中央 ACL 写接口、白名单查询与可信代理感知的 IP 解析。
// 插件在没注入时仍可工作（退化为仅本地 blockUntil 拦截 + 简单 header 取 IP），保持向后兼容。
func SetHostAPI(
	addACLBlock func(ip, source, desc string, expireUnix int64) error,
	isWhitelisted func(ip string) bool,
	getClientIP func(r *http.Request) string,
) {
	hostAddACLBlock = addACLBlock
	hostIsWhitelisted = isWhitelisted
	hostGetClientIP = getClientIP
}

// --- 工具函数 -------------------------------------------------------------

func clientIPFromRequest(r *http.Request) string {
	// 优先使用主进程 HostAPI 提供的 getClientIP —— 它应用了 trustedProxies 白名单，
	// 保证 scan-guard 看到的 IP 与 WAF/ACL/CC 完全一致（避免伪造 X-Real-IP 绕过或测试行为差异）
	if hostGetClientIP != nil {
		if ip := strings.TrimSpace(hostGetClientIP(r)); ip != "" {
			return ip
		}
	}
	// 兜底：HostAPI 未注入时的简化路径（与旧行为兼容）
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

// canonicalIP 把 IPv6 按 /64 聚合，IPv4 原样返回；非法 IP 返回原串
func canonicalIP(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	mask := net.CIDRMask(ipv6Prefix, 128)
	return parsed.Mask(mask).String() + "/" + itoa(ipv6Prefix)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func isWhitelisted(ip string) bool {
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

// --- 插件导出 -------------------------------------------------------------

// Handler 请求前钩子：
//
// 从 v1.1 开始，scan-guard 命中阈值后通过 HostAPI 将 IP 注入中央 ACL（global+ip+block），
// 后续请求由主进程 ACL 层直接短路，**不再**依赖插件本地 blockUntil 做实时拦截。
// 这样做的好处：
//  1. 用户在 ACL 管理页删除 IP，即刻解封（不受插件内存残留影响）
//  2. 拦截页由 ACL 层统一呈现，跨插件一致
//  3. Handler 变成 O(1) passthrough，热路径零开销
//
// 仅保留兜底：若 HostAPI 未被注入（极老版本主进程），退化为本地拦截。
func Handler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	sweeperOnce.Do(startSweeper)

	// 主进程已注入 HostAPI ⇒ 完全交由 ACL 处理；Handler 零成本放行
	if hostAddACLBlock != nil {
		return r, false
	}

	// ------- 兼容兜底路径（HostAPI 未注入） -------
	ip := clientIPFromRequest(r)
	if ip == "" || isWhitelisted(ip) {
		return r, false
	}
	key := canonicalIP(ip)
	v, ok := ipStats.Load(key)
	if !ok {
		return r, false
	}
	st := v.(*ipStat)
	now := time.Now().Unix()
	if st.blockUntil.Load() > now {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Blocked-By", "scan-guard")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(blockPageHTML))
		return nil, true
	}
	return r, false
}

// AfterResponse 响应后钩子：对 400/401/403/404/405 计数，超阈值拉黑
// 为了不影响主路径性能：热路径全 atomic，零分配（除非首次 Store 新 IP）
func AfterResponse(r *http.Request, statusCode int, _ http.Header) {
	if _, hit := countedStatuses[statusCode]; !hit {
		return
	}
	ip := clientIPFromRequest(r)
	if ip == "" || isWhitelisted(ip) {
		return
	}
	key := canonicalIP(ip)
	now := time.Now().Unix()

	v, ok := ipStats.Load(key)
	if !ok {
		if trackedN.Load() >= maxTrackedIPs {
			return // LRU 上限防撑爆，新条目丢弃（已在表里的继续统计）
		}
		st := &ipStat{}
		st.lastTouch.Store(now)
		actual, loaded := ipStats.LoadOrStore(key, st)
		if !loaded {
			trackedN.Add(1)
		}
		v = actual
	}
	st := v.(*ipStat)
	st.lastTouch.Store(now)

	// 如果已拉黑，无需再累加（避免计数噪声）
	if st.blockUntil.Load() > now {
		return
	}

	// 找到当前秒对应的桶；如果桶过期则重置
	slot := int(now/int64(bucketWidthSec)) % bucketCount
	expectTs := now - (now % int64(bucketWidthSec))
	if st.bucketTs[slot].Load() != expectTs {
		st.bucketTs[slot].Store(expectTs)
		st.buckets[slot].Store(0)
	}
	st.buckets[slot].Add(1)

	// 汇总近 windowSec 秒总数
	var sum int32
	cutoff := now - int64(windowSec)
	for i := 0; i < bucketCount; i++ {
		if st.bucketTs[i].Load() > cutoff {
			sum += st.buckets[i].Load()
		}
	}
	if sum >= threshold {
		// 重置所有桶计数 —— 避免阈值一旦跨过，后续每个 4xx 都反复触发 AddACLBlock 压爆 DB
		for i := 0; i < bucketCount; i++ {
			st.buckets[i].Store(0)
		}
		// 本地去重窗口：
		//   - 足够覆盖 ACL 写 DB + 内存重建的时延（通常 <100ms）
		//   - 不能太长，否则用户在 ACL 管理页手动删除后，同 IP 再次扫描要等太久才能重新触发
		//   - 30s 是一个兼顾安全与可管理性的折衷
		const localDedupSec = 30
		// 若 HostAPI 未注入（兜底路径），沿用长拉黑时长（blockSec）供本地 Handler 使用
		if hostAddACLBlock == nil {
			st.blockUntil.Store(now + blockSec)
		} else {
			st.blockUntil.Store(now + localDedupSec)
		}

		// 2) 通过 HostAPI 把 IP 注入中央 ACL（global+ip+block），带 blockSec TTL
		//    —— 后续请求会被 ACL 直接短路在更早阶段，比插件 Handler 更省 CPU
		//    —— 用户可在 ACL 管理页面查看/删除；过期后主进程 sweeper 自动清理
		if hostAddACLBlock != nil {
			// 白名单已在入口 isWhitelisted 里过滤，这里再问一次 HostAPI，保持与用户 ACL allow 规则一致
			if hostIsWhitelisted == nil || !hostIsWhitelisted(ip) {
				desc := "scan-guard: " + itoa(int(sum)) + " 次 4xx / " + itoa(windowSec) + "s"
				// 用 canonical key（IPv4 原样；IPv6 为 /64 CIDR）作为 ACL pattern，与内存统计对齐
				go func(patt, descCopy string, expire int64) {
					defer func() { _ = recover() }()
					_ = hostAddACLBlock(patt, "scan-guard", descCopy, expire)
				}(key, desc, now+blockSec)
			}
		}
	}
}

// Init 注册插件元信息
func Init() (string, int, bool, func(http.ResponseWriter, *http.Request) (*http.Request, bool)) {
	return "scan-guard", 2, true, Handler
}

// --- 后台 sweeper --------------------------------------------------------

func startSweeper() {
	go func() {
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for range t.C {
			now := time.Now().Unix()
			ipStats.Range(func(k, v any) bool {
				st := v.(*ipStat)
				touch := st.lastTouch.Load()
				blockUntil := st.blockUntil.Load()
				// 回收条件：最近触达早于 idleEvictSec，且未处于拉黑态
				if now-touch > idleEvictSec && blockUntil <= now {
					ipStats.Delete(k)
					trackedN.Add(-1)
				}
				return true
			})
		}
	}()
}
