package main

import (
	"bytes"
	_ "embed"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed block.html
var blockPageHTML string

// ---------------- 配置 ----------------
const (
	maxBodyScan    = 32 * 1024          // body 最多扫描 32KB
	aclBlockSec    = 15 * 60            // AI 爬虫拉黑时长 15 分钟
	promptDedupSec = 60                 // 同 IP prompt injection 本地去重
	maxTrackedIPs  = 50000
	idleEvictSec   = int64(3600)
	sweepInterval  = 2 * time.Minute
)

// AI 爬虫 User-Agent 子串（全部小写比较）
// 覆盖：OpenAI/Anthropic/Google/Meta/字节/Common Crawl/Apple 等主流训练爬虫
var aiBotSignatures = []string{
	"gptbot", "chatgpt-user", "oai-searchbot",
	"claudebot", "claude-web", "anthropic-ai",
	"google-extended", "googleother",
	"bytespider", "bytedance",
	"perplexitybot", "perplexity-user",
	"ccbot",
	"facebookbot", "meta-externalagent", "meta-externalfetcher",
	"applebot-extended",
	"amazonbot",
	"diffbot",
	"duckassistbot",
	"youbot",
	"img2dataset",
	"timpibot",
	"omgili", "omgilibot",
	"ai2bot",
	"cohere-ai",
	"petalbot",
	"mj12bot",
}

// Prompt injection 特征（小写子串），覆盖中英文常见越狱词
var promptInjectionSigs = []string{
	"ignore previous instructions",
	"ignore all previous",
	"disregard previous",
	"disregard the above",
	"forget previous instructions",
	"you are now dan",
	"developer mode",
	"jailbreak",
	"system prompt:",
	"<|im_start|>", "<|im_end|>",
	"<|system|>", "<|user|>", "<|assistant|>",
	"[inst]", "[/inst]",
	"###instruction",
	"pretend you are",
	"act as an ai",
	"从现在开始你是",
	"忽略之前的指令",
	"忽略以上指令",
	"忽略上面的",
	"请扮演",
	"开发者模式",
	"越狱模式",
}

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

// ---------------- IP / 本地去重 ----------------
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

// ---------------- 检测 ----------------
func matchAIBot(uaLower string) string {
	for _, s := range aiBotSignatures {
		if strings.Contains(uaLower, s) {
			return s
		}
	}
	return ""
}

func matchPromptInjection(lower string) string {
	for _, s := range promptInjectionSigs {
		if strings.Contains(lower, s) {
			return s
		}
	}
	return ""
}

// 读取 body 的前 maxBodyScan 字节并 restore（保证主流程能继续读）
func snapshotBody(r *http.Request) []byte {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	buf := make([]byte, maxBodyScan)
	n, _ := io.ReadFull(io.LimitReader(r.Body, maxBodyScan), buf)
	if n <= 0 {
		// 把原 body 包回去
		r.Body = io.NopCloser(bytes.NewReader(nil))
		return nil
	}
	snap := buf[:n]
	// 将剩余 body 与已读拼回
	rest, _ := io.ReadAll(r.Body)
	combined := make([]byte, 0, n+len(rest))
	combined = append(combined, snap...)
	combined = append(combined, rest...)
	r.Body = io.NopCloser(bytes.NewReader(combined))
	return snap
}

func respondBlock(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Blocked-By", "ai-shield")
	w.Header().Set("X-Block-Reason", reason)
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(blockPageHTML))
}

// ---------------- 插件导出 ----------------
func Handler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	sweeperOnce.Do(startSweeper)

	ip := clientIPFromRequest(r)
	if ip == "" || isInternal(ip) {
		return r, false
	}
	if hostIsWhitelisted != nil && hostIsWhitelisted(ip) {
		return r, false
	}

	// 1) AI 爬虫 UA 检测（高置信度 → 直接 ACL 封 15 分钟）
	ua := r.Header.Get("User-Agent")
	if ua != "" {
		uaLower := strings.ToLower(ua)
		if sig := matchAIBot(uaLower); sig != "" {
			if hostAddACLBlock != nil {
				go func(pIP, pSig string) {
					defer func() { _ = recover() }()
					_ = hostAddACLBlock(pIP, "ai-shield", "ai-bot UA: "+pSig, time.Now().Unix()+aclBlockSec)
				}(ip, sig)
			}
			respondBlock(w, "ai-bot:"+sig)
			return nil, true
		}
	}

	// 2) Prompt Injection 检测：URL + 关键头部 + (POST/PUT/PATCH 时扫描 body)
	now := time.Now().Unix()
	// 本地去重：同 IP 60s 内不重复扫描 body（防止巨大 body 反复消耗 CPU）
	st := loadIPState(ip, now)
	if st != nil && st.lastBlock.Load()+promptDedupSec > now {
		respondBlock(w, "prompt-injection-dedup")
		return nil, true
	}

	// URL 与关键头部（User-Agent / Referer / 自定义头）
	// 注意：RawQuery 里空格会是 `+` 或 `%20`，必须解码后再匹配 "ignore previous instructions" 这种含空格的签名
	rawQS := r.URL.RawQuery
	decodedQS, err := url.QueryUnescape(rawQS)
	if err != nil {
		decodedQS = rawQS
	}
	urlLower := strings.ToLower(decodedQS + " " + rawQS + " " + r.URL.Path)
	if sig := matchPromptInjection(urlLower); sig != "" {
		markBlock(ip, now)
		respondBlock(w, "prompt-injection:url:"+sig)
		return nil, true
	}
	// 扫描前 8 个常见 header 值
	for _, h := range []string{"Referer", "Origin", "X-Prompt", "X-Ai-Input", "X-Query", "X-Chat"} {
		if v := r.Header.Get(h); v != "" {
			if sig := matchPromptInjection(strings.ToLower(v)); sig != "" {
				markBlock(ip, now)
				respondBlock(w, "prompt-injection:header:"+sig)
				return nil, true
			}
		}
	}
	// Body（仅 POST/PUT/PATCH 且 Content-Type 看着像文本）
	if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" {
		ct := strings.ToLower(r.Header.Get("Content-Type"))
		if ct == "" || strings.Contains(ct, "json") || strings.Contains(ct, "text") ||
			strings.Contains(ct, "xml") || strings.Contains(ct, "urlencoded") ||
			strings.Contains(ct, "form-data") {
			body := snapshotBody(r)
			if len(body) > 0 {
				if sig := matchPromptInjection(strings.ToLower(bytesToString(body))); sig != "" {
					markBlock(ip, now)
					respondBlock(w, "prompt-injection:body:"+sig)
					return nil, true
				}
			}
		}
	}

	return r, false
}

func bytesToString(b []byte) string { return string(b) }

func loadIPState(ip string, now int64) *ipState {
	if v, ok := ipMap.Load(ip); ok {
		st := v.(*ipState)
		st.lastTouch.Store(now)
		return st
	}
	return nil
}

func markBlock(ip string, now int64) {
	v, ok := ipMap.Load(ip)
	if !ok {
		if trackedN.Load() >= maxTrackedIPs {
			return
		}
		st := &ipState{}
		st.lastTouch.Store(now)
		actual, loaded := ipMap.LoadOrStore(ip, st)
		if !loaded {
			trackedN.Add(1)
		}
		v = actual
	}
	st := v.(*ipState)
	st.lastBlock.Store(now)
	st.lastTouch.Store(now)
}

func Init() (string, int, bool, func(http.ResponseWriter, *http.Request) (*http.Request, bool)) {
	return "ai-shield", 4, true, Handler
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
