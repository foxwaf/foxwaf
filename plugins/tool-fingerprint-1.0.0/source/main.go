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
	aclBlockSec    = 60 * 60          // 扫描器高置信度 → 封 1 小时
	localDedupSec  = 60
	maxTrackedIPs  = 50000
	idleEvictSec   = int64(3600)
	sweepInterval  = 2 * time.Minute
)

// 扫描器 UA 子串（小写）
var toolUASigs = []string{
	"sqlmap",
	"nuclei",
	"nmap scripting engine",
	"masscan",
	"acunetix",
	"nikto",
	"wpscan",
	"ffuf",
	"feroxbuster",
	"gobuster",
	"dirbuster",
	"dirbuster-",
	"wfuzz",
	"burpsuite", "burp",
	"w3af",
	"arachni",
	"netsparker",
	"appscan",
	"qualys",
	"openvas",
	"zaproxy", "owasp zap",
	"xray",
	"crawlergo",
	"pocsuite",
	"hydra",
	"medusa",
	"skipfish",
	"httpx - open source tool",
	"naabu",
	"katana",
	"subfinder",
	"whatweb",
}

// 特征头/cookie（小写 key 子串）
var toolHeaderSigs = map[string]string{
	"x-scanner":        "x-scanner-header",
	"x-wpscan-scan-id": "wpscan-header",
	"acunetix-aspect":  "acunetix-header",
	"nuclei":           "nuclei-header",
}

// URL 路径里的扫描器特征
var toolPathSigs = []string{
	"acunetix-wvs-test-for-some-inexistent-file",
	"/nuclei-template",
	"{{baseurl}}",
	".sqlmapproject.",
	"/wp-content/plugins/../",
}

// 常见 OOB 域名（命中 header / body / url）
var oobDomains = []string{
	".burpcollaborator.net",
	".oast.fun",
	".oast.live",
	".oast.pro",
	".oast.me",
	".oast.site",
	".oast.online",
	".interact.sh",
	".dnslog.cn",
	".ceye.io",
	".requestbin.net",
	".pipedream.net",
}

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

type ipState struct {
	lastBlock atomic.Int64
	lastTouch atomic.Int64
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

// detect 返回命中的特征描述，或空串
func detect(r *http.Request) string {
	// 1) User-Agent
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	for _, s := range toolUASigs {
		if strings.Contains(ua, s) {
			return "ua:" + s
		}
	}

	// 2) 特征 header key
	for k := range r.Header {
		lk := strings.ToLower(k)
		for sig, desc := range toolHeaderSigs {
			if strings.Contains(lk, sig) {
				return "header:" + desc
			}
		}
	}

	// 3) URL 路径 / query
	target := strings.ToLower(r.URL.Path + "?" + r.URL.RawQuery)
	for _, s := range toolPathSigs {
		if strings.Contains(target, s) {
			return "path:" + s
		}
	}

	// 4) OOB 域名（出现在 url 或 Referer / 常见 header 里）
	candidates := []string{target, strings.ToLower(r.Header.Get("Referer")),
		strings.ToLower(r.Header.Get("Host")),
		strings.ToLower(r.Header.Get("X-Forwarded-Host"))}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		for _, d := range oobDomains {
			if strings.Contains(c, d) {
				return "oob:" + d
			}
		}
	}

	return ""
}

func respondBlock(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Blocked-By", "tool-fingerprint")
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
	reason := detect(r)
	if reason == "" {
		return r, false
	}
	logEvt("block", "scanner", ip, map[string]any{"reason": reason, "ua": r.Header.Get("User-Agent"), "path": r.URL.Path})

	// 高置信度 → 一次命中即 ACL 封 1 小时
	now := time.Now().Unix()
	st := loadOrCreate(ip, now)
	if st == nil {
		respondBlock(w, reason)
		return nil, true
	}
	if st.lastBlock.Load()+localDedupSec < now {
		st.lastBlock.Store(now)
		if hostAddACLBlock != nil {
			go func(p, rs string) {
				defer func() { _ = recover() }()
				_ = hostAddACLBlock(p, "tool-fingerprint",
					"scanner: "+rs, now+aclBlockSec)
			}(ip, reason)
		}
	}
	respondBlock(w, reason)
	return nil, true
}

func loadOrCreate(ip string, now int64) *ipState {
	if v, ok := ipMap.Load(ip); ok {
		st := v.(*ipState)
		st.lastTouch.Store(now)
		return st
	}
	if trackedN.Load() >= maxTrackedIPs {
		return nil
	}
	st := &ipState{}
	st.lastTouch.Store(now)
	actual, loaded := ipMap.LoadOrStore(ip, st)
	if !loaded {
		trackedN.Add(1)
	}
	return actual.(*ipState)
}

func Init() (string, int, bool, func(http.ResponseWriter, *http.Request) (*http.Request, bool)) {
	return "tool-fingerprint", 8, true, Handler
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
