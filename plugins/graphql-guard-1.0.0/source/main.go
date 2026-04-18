package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
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
	maxBodyScan        = 128 * 1024 // GraphQL query 一般不会太大
	maxDepth           = 10
	maxAliases         = 50
	maxFields          = 500
	disallowIntrospect = true

	burstWindow   = 60
	burstThresh   = 5
	aclBlockSec   = 30 * 60
	maxTrackedIPs = 50000
	idleEvictSec  = int64(3600)
	sweepInterval = 2 * time.Minute
)

// graphql 端点匹配子串
var gqlEndpoints = []string{
	"/graphql", "/graphiql", "/api/graphql", "/v1/graphql", "/v2/graphql",
	"/query", "/gql",
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

func isGQLEndpoint(p string) bool {
	lp := strings.ToLower(p)
	for _, s := range gqlEndpoints {
		if strings.Contains(lp, s) {
			return true
		}
	}
	return false
}

// snapshotBody 读取 body 前 maxBodyScan 并 restore
func snapshotBody(r *http.Request) []byte {
	if r.Body == nil {
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

// extractQuery 从 GraphQL 请求中提取 query 字符串
func extractQuery(r *http.Request) string {
	// GET: ?query=...
	if r.Method == "GET" {
		return r.URL.Query().Get("query")
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	body := snapshotBody(r)
	if len(body) == 0 {
		return ""
	}
	// application/graphql：body 本身就是 query
	if strings.Contains(ct, "application/graphql") {
		return string(body)
	}
	// application/json：{"query":"...", ...}
	if strings.Contains(ct, "json") || body[0] == '{' || body[0] == '[' {
		// 批量查询数组：取第一条足以判定 introspection / 深度
		var obj map[string]interface{}
		if err := json.Unmarshal(body, &obj); err == nil {
			if q, ok := obj["query"].(string); ok {
				return q
			}
		}
		// 数组形式
		var arr []map[string]interface{}
		if err := json.Unmarshal(body, &arr); err == nil && len(arr) > 0 {
			parts := make([]string, 0, len(arr))
			for _, item := range arr {
				if q, ok := item["query"].(string); ok {
					parts = append(parts, q)
				}
			}
			return strings.Join(parts, "\n")
		}
	}
	// form-urlencoded：query=...
	if strings.Contains(ct, "urlencoded") {
		_ = r.ParseForm()
		if q := r.Form.Get("query"); q != "" {
			return q
		}
	}
	return ""
}

// analyzeQuery 简易词法扫描：统计深度（未配对的 `{`）、alias 数量（`label:`）、字段数（非空标识符）
type gqlMetrics struct {
	depth      int
	aliases    int
	fields     int
	introspect bool
}

func analyzeQuery(q string) gqlMetrics {
	var m gqlMetrics
	curDepth := 0
	// introspection 关键字
	lq := strings.ToLower(q)
	if strings.Contains(lq, "__schema") || strings.Contains(lq, "__type(") ||
		strings.Contains(lq, "__typename") && strings.Contains(lq, "introspectionquery") {
		m.introspect = true
	}
	// 粗略扫描
	i := 0
	inStr := byte(0)
	for i < len(q) {
		c := q[i]
		if inStr != 0 {
			if c == '\\' && i+1 < len(q) {
				i += 2
				continue
			}
			if c == inStr {
				inStr = 0
			}
			i++
			continue
		}
		switch c {
		case '"', '\'':
			inStr = c
		case '#':
			// 注释到行尾
			for i < len(q) && q[i] != '\n' {
				i++
			}
			continue
		case '{':
			curDepth++
			if curDepth > m.depth {
				m.depth = curDepth
			}
		case '}':
			if curDepth > 0 {
				curDepth--
			}
		case ':':
			// 判别 alias: 左侧是标识符字符且右侧不是 `:`（避免 :: 之类）
			if i > 0 && i+1 < len(q) {
				prev := q[i-1]
				if isIdent(prev) {
					m.aliases++
				}
			}
		}
		if isIdent(c) {
			// 跳过整个标识符，统计为一个 field（粗略）
			m.fields++
			for i < len(q) && isIdent(q[i]) {
				i++
			}
			continue
		}
		i++
	}
	return m
}

func isIdent(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') || b == '_'
}

func respondBlock(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Blocked-By", "graphql-guard")
	w.Header().Set("X-Block-Reason", reason)
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(blockPageHTML))
}

func Handler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	sweeperOnce.Do(startSweeper)

	if !isGQLEndpoint(r.URL.Path) {
		return r, false
	}
	ip := clientIPFromRequest(r)
	if ip == "" || isInternal(ip) {
		return r, false
	}
	if hostIsWhitelisted != nil && hostIsWhitelisted(ip) {
		return r, false
	}

	q := extractQuery(r)
	if q == "" {
		return r, false
	}
	m := analyzeQuery(q)

	if disallowIntrospect && m.introspect {
		onHit(ip, "introspection")
		respondBlock(w, "introspection")
		return nil, true
	}
	if m.depth > maxDepth {
		onHit(ip, "depth")
		respondBlock(w, "depth-exceeded")
		return nil, true
	}
	if m.aliases > maxAliases {
		onHit(ip, "alias-flood")
		respondBlock(w, "alias-flood")
		return nil, true
	}
	if m.fields > maxFields {
		onHit(ip, "field-flood")
		respondBlock(w, "field-flood")
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
					_ = hostAddACLBlock(p, "graphql-guard",
						"graphql: "+rs+" x"+itoa(int(h)), now+aclBlockSec)
				}(ip, reason, hits)
			}
		}
	}
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

func Init() (string, int, bool, func(http.ResponseWriter, *http.Request) (*http.Request, bool)) {
	return "graphql-guard", 9, true, Handler
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
