// Package foxhttp 是一个高性能原生 HTTP 入口框架。
//
// 目标：在同一套统一抽象之上无缝支撑 HTTP/1.1、HTTP/2、HTTP/3 与国密(GM/TLCP)，
// 并为上层 WAF (waf.go 的 80/443 入口) 预留干净的挂载接口。
//
// 设计要点：
//   - 协议解析复用经过实战检验、零额外分配热路径的标准库 net/http (h1/h2) 与
//     quic-go/http3 (h3)、tjfoc/gmsm/gmtls (国密 TLCP)，把精力集中在“统一抽象 +
//     WAF 钩子 + 国密自适应”这一真正的衔接价值上。
//   - 443 端口做 ClientHello 探测分流：国密客户端走 gmtls，国际客户端走标准
//     crypto/tls (自动协商 h2 / http1.1)，互不干扰、各自最优。
//   - 三种协议在入口处统一归一为 *Ctx，WAF 只需实现 Inspector 接口即可，
//     完全不感知底层协议差异。
//
// 单文件结构（按 section 顺序）：
//  1. 协议版本与统一上下文 Ctx
//  2. 决策 Decision 与 WAF 钩子接口 Inspector
//  3. 配置 Config
//  4. Server 生命周期 (New/Start/Shutdown)
//  5. 入口处理链 (协议 -> Ctx -> 钩子 -> 上游)
//  6. 443 国密/国际自适应分流
//  7. 反向代理与默认处理器
//  8. 性能基础设施 (buffer pool / chanListener / peekedConn)
//  9. 工具 (自签证书、证书加载)
package foxhttp

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/tjfoc/gmsm/gmtls"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// =============================================================================
// 1. 协议版本与统一上下文
// =============================================================================

// Proto 表示请求所使用的应用层协议。
type Proto uint8

const (
	ProtoUnknown Proto = iota
	ProtoHTTP11        // HTTP/1.1 (含明文)
	ProtoH2C           // HTTP/2 明文 (h2c)
	ProtoHTTP2         // HTTP/2 over TLS
	ProtoHTTP3         // HTTP/3 over QUIC
)

func (p Proto) String() string {
	switch p {
	case ProtoHTTP11:
		return "HTTP/1.1"
	case ProtoH2C:
		return "h2c"
	case ProtoHTTP2:
		return "HTTP/2"
	case ProtoHTTP3:
		return "HTTP/3"
	default:
		return "unknown"
	}
}

// Ctx 是所有协议归一后的统一请求上下文，是 WAF 与上游业务唯一需要面对的对象。
// 热路径对象：通过 sync.Pool 复用，禁止在请求结束后继续持有。
type Ctx struct {
	W http.ResponseWriter // 响应写入器（已按协议适配）
	R *http.Request       // 标准请求对象

	Proto      Proto  // 协议版本
	ClientIP   string // 对端 IP（不含端口）
	ServerName string // SNI / Host
	IsGM       bool   // 是否经由国密 TLCP 握手
	TLSVersion uint16 // TLS 版本（0 表示明文）
	StartedAt  time.Time

	srv   *Server
	attrs map[string]any

	// 响应钩子与响应元信息内嵌于此，随 Ctx 一起池化复用，避免每请求堆分配。
	hook     respHook
	respInfo ResponseInfo
}

// Set/Get 用于在钩子与上游之间传递请求级数据。
func (c *Ctx) Set(k string, v any) {
	if c.attrs == nil {
		c.attrs = make(map[string]any, 4)
	}
	c.attrs[k] = v
}

func (c *Ctx) Get(k string) (any, bool) {
	if c.attrs == nil {
		return nil, false
	}
	v, ok := c.attrs[k]
	return v, ok
}

// Elapsed 返回从进入入口到当前的耗时。
func (c *Ctx) Elapsed() time.Duration { return time.Since(c.StartedAt) }

func (c *Ctx) reset() {
	c.W = nil
	c.R = nil
	c.Proto = ProtoUnknown
	c.ClientIP = ""
	c.ServerName = ""
	c.IsGM = false
	c.TLSVersion = 0
	c.srv = nil
	c.hook = respHook{}
	c.respInfo = ResponseInfo{}
	for k := range c.attrs {
		delete(c.attrs, k)
	}
}

// =============================================================================
// 2. 决策与 WAF 钩子接口
// =============================================================================

// Action 是钩子的裁决动作。
type Action uint8

const (
	ActionAllow Action = iota // 放行
	ActionBlock               // 拦截
)

// Decision 是钩子返回的裁决。
type Decision struct {
	Action  Action
	Status  int               // 拦截时的 HTTP 状态码，默认 403
	Body    []byte            // 拦截时返回的响应体
	Headers map[string]string // 附加响应头（放行/拦截均可用于注入安全头）
}

// Allow 是放行裁决的快捷值。
var Allow = Decision{Action: ActionAllow}

// Block 构造一个拦截裁决。
func Block(status int, body string) Decision {
	if status == 0 {
		status = http.StatusForbidden
	}
	return Decision{Action: ActionBlock, Status: status, Body: []byte(body)}
}

// ResponseInfo 在响应阶段提供给钩子检查的元信息（头部阶段，不含完整 body，
// 以保证流式响应的性能）。
type ResponseInfo struct {
	Status int
	Header http.Header
}

// Inspector 是 WAF 的核心挂载接口（请求阶段，必选能力）。
// waf.go 实现本接口即可在转发上游前接管所有协议的请求流量。
// 方法在热路径上，实现必须高性能、无阻塞。
type Inspector interface {
	// InspectRequest 在读取到请求头(及可选 body)后、转发上游前调用。
	InspectRequest(c *Ctx) Decision
}

// ResponseInspector 是可选的响应阶段钩子。
// 仅当传入的 Inspector 同时实现本接口时，框架才会包装 ResponseWriter；
// 否则响应路径零开销、完整保留 sendfile / Flush 等标准库快路径。
type ResponseInspector interface {
	// InspectResponse 在上游返回响应头、写回客户端前调用，可注入安全头或改写状态。
	InspectResponse(c *Ctx, resp *ResponseInfo) Decision
}

// NopInspector 是一个全放行的空实现，方便未接入 WAF 时占位。
type NopInspector struct{}

func (NopInspector) InspectRequest(*Ctx) Decision { return Allow }

// InspectorFunc 允许用函数快速实现请求阶段钩子。
type InspectorFunc func(c *Ctx) Decision

func (f InspectorFunc) InspectRequest(c *Ctx) Decision { return f(c) }

// =============================================================================
// 3. 配置
// =============================================================================

// Config 描述一个 Server 的全部配置。零值不可用，请用 New 构造。
type Config struct {
	// 监听地址（为空表示不启用对应入口）。
	HTTPAddr  string // 明文入口，如 ":80"
	HTTPSAddr string // TLS(TCP) 入口，如 ":443"，国密/国际自适应
	QUICAddr  string // HTTP/3(UDP) 入口，如 ":443"
	EnableH2C bool   // 明文入口是否支持 h2c (HTTP/2 over cleartext)

	// 国际证书 (PEM 文件路径)。HTTPSAddr/QUICAddr 启用时需要；为空则自动生成自签证书。
	TLSCert string
	TLSKey  string

	// 国密双证书 (PEM 文件路径)。提供后 443 入口即支持国密 TLCP 客户端。
	GMSignCert string // 签名证书
	GMSignKey  string
	GMEncCert  string // 加密证书
	GMEncKey   string

	// WAF 钩子与上游业务。
	Inspector Inspector    // 为空使用 NopInspector
	Handler   http.Handler // 上游业务/反代；为空使用内置回显处理器

	// 性能与超时调优。
	ReadTimeout       time.Duration // 默认 15s
	ReadHeaderTimeout time.Duration // 默认 5s
	WriteTimeout      time.Duration // 默认 30s
	IdleTimeout       time.Duration // 默认 90s
	HandshakeTimeout  time.Duration // TLS 握手/探测超时，默认 5s
	MaxHeaderBytes    int           // 默认 1MB

	// 回调。
	OnError func(error)
}

func (c *Config) withDefaults() {
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 15 * time.Second
	}
	if c.ReadHeaderTimeout == 0 {
		c.ReadHeaderTimeout = 5 * time.Second
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = 30 * time.Second
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 90 * time.Second
	}
	if c.HandshakeTimeout == 0 {
		c.HandshakeTimeout = 5 * time.Second
	}
	if c.MaxHeaderBytes == 0 {
		c.MaxHeaderBytes = 1 << 20
	}
	if c.Handler == nil {
		c.Handler = http.HandlerFunc(defaultEchoHandler)
	}
	if c.OnError == nil {
		c.OnError = func(error) {}
	}
}

// =============================================================================
// 4. Server 生命周期
// =============================================================================

// Stats 是轻量原子计数器，用于观测与性能报告。
type Stats struct {
	Requests   atomic.Uint64
	Blocked    atomic.Uint64
	GMRequests atomic.Uint64
	ByProto    [5]atomic.Uint64 // 以 Proto 为下标
}

// Server 是框架主体。
type Server struct {
	cfg Config

	stdTLS *tls.Config   // 国际 TLS 配置 (h2/http1.1)
	gmTLS  *gmtls.Config // 国密 TLCP 配置
	hasGM  bool

	// 预解析钩子，避免每请求做接口/类型断言。
	reqInspector  Inspector         // 请求阶段（nil 表示无）
	respInspector ResponseInspector // 响应阶段（nil 表示无，则不包装 ResponseWriter）

	h1h2 *http.Server  // 服务 h1/h2 (标准 TLS 与明文)
	h3   *http3.Server // 服务 h3

	// 自适应分流用的虚拟监听器。
	stdLn *chanListener // 国际 TLS 连接（已包装 *tls.Conn）
	gmLn  *chanListener // 国密连接（gmtls.Conn）

	rawTLSLn net.Listener // 443 物理 TCP 监听器
	plainLn  net.Listener // 80 物理 TCP 监听器

	Stats Stats

	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup
	ctxPool sync.Pool
}

// New 构造一个 Server 并完成证书加载与内部组件初始化（不启动监听）。
func New(cfg Config) (*Server, error) {
	cfg.withDefaults()
	s := &Server{cfg: cfg}
	s.ctxPool.New = func() any { return new(Ctx) }

	// 预解析钩子能力：请求钩子可选，响应钩子按是否实现 ResponseInspector 决定是否包装。
	s.reqInspector = cfg.Inspector
	if ri, ok := cfg.Inspector.(ResponseInspector); ok {
		s.respInspector = ri
	}

	needTLS := cfg.HTTPSAddr != "" || cfg.QUICAddr != ""
	if needTLS {
		cert, err := loadOrGenStdCert(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("加载国际证书失败: %w", err)
		}
		s.stdTLS = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{http2.NextProtoTLS, "http/1.1"}, // h2, http/1.1
		}
	}

	if cfg.GMSignCert != "" && cfg.GMEncCert != "" {
		sig, err := gmtls.LoadX509KeyPair(cfg.GMSignCert, cfg.GMSignKey)
		if err != nil {
			return nil, fmt.Errorf("加载国密签名证书失败: %w", err)
		}
		enc, err := gmtls.LoadX509KeyPair(cfg.GMEncCert, cfg.GMEncKey)
		if err != nil {
			return nil, fmt.Errorf("加载国密加密证书失败: %w", err)
		}
		s.gmTLS = &gmtls.Config{
			GMSupport:    gmtls.NewGMSupport(),
			Certificates: []gmtls.Certificate{sig, enc}, // [0]签名 [1]加密
		}
		s.hasGM = true
	}

	// h1/h2 服务器：一个实例服务多个 listener（明文 / 标准TLS / 国密）。
	entry := s.entryHandler()
	h1h2 := &http.Server{
		Handler:           entry,
		TLSConfig:         s.stdTLS, // 让 Serve 自动装配 h2 的 TLSNextProto
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		ConnContext:       connContext, // 标记国密连接
		// 默认丢弃底层连接级噪声（如客户端中断的 TLS 握手）；业务错误经 OnError 上报。
		ErrorLog: log.New(io.Discard, "", 0),
	}
	if s.stdTLS != nil {
		// 显式装配 HTTP/2，确保 ALPN=h2 时进入 http2 处理。
		_ = http2.ConfigureServer(h1h2, &http2.Server{})
	}
	s.h1h2 = h1h2

	if cfg.QUICAddr != "" {
		h3TLS := s.stdTLS.Clone()
		s.h3 = &http3.Server{
			Addr:      cfg.QUICAddr,
			Handler:   s.h3Handler(),
			TLSConfig: http3.ConfigureTLSConfig(h3TLS),
		}
	}

	return s, nil
}

// Start 启动所有已配置的入口（非阻塞）。任一物理监听器创建失败将返回错误。
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("server 已关闭")
	}

	// 明文入口 (80)。
	if s.cfg.HTTPAddr != "" {
		ln, err := net.Listen("tcp", s.cfg.HTTPAddr)
		if err != nil {
			return fmt.Errorf("监听 %s 失败: %w", s.cfg.HTTPAddr, err)
		}
		s.plainLn = ln
		s.serve(func() error { return s.h1h2.Serve(ln) }, "http(plain)")
	}

	// TLS 入口 (443) —— 国密/国际自适应分流。
	if s.cfg.HTTPSAddr != "" {
		ln, err := net.Listen("tcp", s.cfg.HTTPSAddr)
		if err != nil {
			return fmt.Errorf("监听 %s 失败: %w", s.cfg.HTTPSAddr, err)
		}
		s.rawTLSLn = ln
		s.stdLn = newChanListener(ln.Addr())
		s.gmLn = newChanListener(ln.Addr())

		// 两个虚拟监听器分别由同一个 h1/h2 server 服务。
		s.serve(func() error { return s.h1h2.Serve(s.stdLn) }, "https(intl)")
		s.serve(func() error { return s.h1h2.Serve(s.gmLn) }, "https(gm)")
		// 分流主循环。
		s.serve(func() error { return s.dispatchTLS(ln) }, "tls-dispatch")
	}

	// HTTP/3 入口 (443/udp)。
	if s.h3 != nil {
		s.serve(func() error { return s.h3.ListenAndServe() }, "http3")
	}

	return nil
}

func (s *Server) serve(fn func() error, name string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := fn(); err != nil && !isClosedErr(err) {
			s.cfg.OnError(fmt.Errorf("%s: %w", name, err))
		}
	}()
}

// Shutdown 优雅关闭所有入口。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	var firstErr error
	if s.rawTLSLn != nil {
		_ = s.rawTLSLn.Close()
	}
	if s.stdLn != nil {
		_ = s.stdLn.Close()
	}
	if s.gmLn != nil {
		_ = s.gmLn.Close()
	}
	if err := s.h1h2.Shutdown(ctx); err != nil {
		firstErr = err
	}
	if s.h3 != nil {
		if err := s.h3.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		if firstErr == nil {
			firstErr = ctx.Err()
		}
	}
	return firstErr
}

// Wait 阻塞直到所有服务 goroutine 退出（配合 Start 使用）。
func (s *Server) Wait() { s.wg.Wait() }

// =============================================================================
// 5. 入口处理链：协议 -> Ctx -> 钩子 -> 上游
// =============================================================================

// entryHandler 构造 h1/h2/明文 入口的处理器（含 h2c 包装）。
func (s *Server) entryHandler() http.Handler {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handle(w, r, detectProto(r))
	})
	if s.cfg.EnableH2C {
		return h2c.NewHandler(h, &http2.Server{})
	}
	return h
}

// h3Handler 构造 HTTP/3 入口的处理器。
func (s *Server) h3Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handle(w, r, ProtoHTTP3)
	})
}

// handle 是所有协议汇聚后的统一入口。
func (s *Server) handle(w http.ResponseWriter, r *http.Request, proto Proto) {
	s.Stats.Requests.Add(1)
	s.Stats.ByProto[proto].Add(1)

	c := s.ctxPool.Get().(*Ctx)
	c.reset()
	c.W = w
	c.R = r
	c.Proto = proto
	c.ClientIP = clientIP(r.RemoteAddr)
	c.ServerName = r.Host
	c.StartedAt = time.Now()
	c.srv = s

	// 国密标记 + TLS 信息。
	if isGMConn(r.Context()) {
		c.IsGM = true
		s.Stats.GMRequests.Add(1)
	}
	if r.TLS != nil {
		c.TLSVersion = r.TLS.Version
		if r.TLS.ServerName != "" {
			c.ServerName = r.TLS.ServerName
		}
	}

	defer s.ctxPool.Put(c)

	// 请求阶段钩子（未配置则跳过）。
	if s.reqInspector != nil {
		if d := s.reqInspector.InspectRequest(c); d.Action == ActionBlock {
			s.Stats.Blocked.Add(1)
			writeDecision(w, d)
			return
		}
	}

	// 上游：仅当存在响应钩子时才包装 ResponseWriter，否则直通以保留 sendfile/Flush 快路径。
	rw := w
	if s.respInspector != nil {
		c.hook = respHook{ResponseWriter: w, ctx: c}
		rw = &c.hook
	}
	s.cfg.Handler.ServeHTTP(rw, r)
}

// respHook 在响应头阶段触发 InspectResponse，可注入安全头或拦截。
// 仅在响应头写出前介入，并透传 sendfile(ReadFrom)/Flush/Hijack，保证流式与零拷贝不受影响。
// 内嵌于池化的 Ctx（见 Ctx.hook），不产生每请求堆分配。
type respHook struct {
	http.ResponseWriter
	ctx         *Ctx
	wroteHeader bool
	blocked     bool
}

func (h *respHook) WriteHeader(code int) {
	if h.wroteHeader {
		return
	}
	h.wroteHeader = true
	c := h.ctx
	c.respInfo.Status = code
	c.respInfo.Header = h.Header()
	d := c.srv.respInspector.InspectResponse(c, &c.respInfo)
	if d.Action == ActionBlock {
		h.blocked = true
		c.srv.Stats.Blocked.Add(1)
		writeDecision(h.ResponseWriter, d)
		return
	}
	for k, v := range d.Headers {
		h.Header().Set(k, v)
	}
	h.ResponseWriter.WriteHeader(code)
}

func (h *respHook) Write(b []byte) (int, error) {
	if !h.wroteHeader {
		h.WriteHeader(http.StatusOK)
	}
	if h.blocked {
		return len(b), nil // 已拦截，丢弃后续 body
	}
	return h.ResponseWriter.Write(b)
}

// ReadFrom 透传底层的 sendfile 零拷贝能力（如静态文件、socket 到 socket 转发）。
func (h *respHook) ReadFrom(src io.Reader) (int64, error) {
	if !h.wroteHeader {
		h.WriteHeader(http.StatusOK)
	}
	if h.blocked {
		return io.Copy(io.Discard, src)
	}
	if rf, ok := h.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(writerOnly{h.ResponseWriter}, src) // 罕见：底层无 ReaderFrom 时退化
}

// Flush 透传流式刷新（SSE、长连接、h2/h3 分块）。
func (h *respHook) Flush() {
	if !h.wroteHeader {
		h.WriteHeader(http.StatusOK)
	}
	if fl, ok := h.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// Hijack 透传连接劫持（WebSocket 等）。
func (h *respHook) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := h.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("foxhttp: 底层 ResponseWriter 不支持 Hijack")
}

// Unwrap 使 http.ResponseController (SetDeadline 等其余能力) 透传到底层。
func (h *respHook) Unwrap() http.ResponseWriter { return h.ResponseWriter }

// writerOnly 隐藏底层的 ReaderFrom，避免 io.Copy 在退化路径上递归回 respHook.ReadFrom。
type writerOnly struct{ io.Writer }

func writeDecision(w http.ResponseWriter, d Decision) {
	for k, v := range d.Headers {
		w.Header().Set(k, v)
	}
	status := d.Status
	if status == 0 {
		status = http.StatusForbidden
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.WriteHeader(status)
	if len(d.Body) > 0 {
		_, _ = w.Write(d.Body)
	}
}

// =============================================================================
// 6. 443 国密/国际自适应分流
// =============================================================================

// dispatchTLS 接受 443 物理连接，探测 ClientHello 后分流到国密或国际处理。
func (s *Server) dispatchTLS(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.routeTLSConn(conn)
	}
}

func (s *Server) routeTLSConn(raw net.Conn) {
	// 探测前 3 字节 (record type + version)，判定是否国密 ClientHello。
	_ = raw.SetReadDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	hdr := make([]byte, 3)
	if _, err := io.ReadFull(raw, hdr); err != nil {
		_ = raw.Close()
		return
	}
	_ = raw.SetReadDeadline(time.Time{}) // 清除探测超时，交回连接级超时
	pc := &peekedConn{Conn: raw, prefix: hdr}

	// TLS 记录: hdr[0]=0x16(handshake)。国密 GMSSL 的版本为 0x0101。
	isGM := hdr[0] == 0x16 && hdr[1] == 0x01 && hdr[2] == 0x01

	if isGM && s.hasGM {
		gmConn := gmtls.Server(pc, s.gmTLS)
		if !s.gmLn.push(&gmMarkedConn{Conn: gmConn}) {
			_ = gmConn.Close()
		}
		return
	}
	if isGM && !s.hasGM {
		// 收到国密握手但未配置国密证书，直接关闭避免无谓占用。
		_ = pc.Close()
		return
	}
	// 国际客户端：包装为标准 *tls.Conn（h2/http1.1 ALPN 由标准库协商）。
	tlsConn := tls.Server(pc, s.stdTLS)
	if !s.stdLn.push(tlsConn) {
		_ = tlsConn.Close()
	}
}

// --- 国密连接标记：通过 ConnContext 把 IsGM 透传给 handler ---

type gmMarkedConn struct{ net.Conn }

type ctxKey int

const gmConnKey ctxKey = 1

func connContext(ctx context.Context, c net.Conn) context.Context {
	if _, ok := c.(*gmMarkedConn); ok {
		return context.WithValue(ctx, gmConnKey, true)
	}
	return ctx
}

func isGMConn(ctx context.Context) bool {
	v, _ := ctx.Value(gmConnKey).(bool)
	return v
}

// detectProto 从标准请求推断协议版本。
func detectProto(r *http.Request) Proto {
	switch r.ProtoMajor {
	case 3:
		return ProtoHTTP3
	case 2:
		if r.TLS == nil {
			return ProtoH2C
		}
		return ProtoHTTP2
	default:
		return ProtoHTTP11
	}
}

// =============================================================================
// 7. 反向代理与默认处理器
// =============================================================================

// NewReverseProxy 构造一个性能调优过的反向代理处理器，可直接作为 Config.Handler。
// target 形如 "http://127.0.0.1:8080"。
func NewReverseProxy(target string) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.Transport = &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	rp.BufferPool = newBufferPool(32 * 1024) // 零分配流式转发
	return rp, nil
}

// defaultEchoHandler 是未配置 Handler 时的内置处理器，回显协议信息，便于联调与压测。
func defaultEchoHandler(w http.ResponseWriter, r *http.Request) {
	proto := detectProto(r)
	gm := isGMConn(r.Context())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Fox-Proto", proto.String())
	if gm {
		w.Header().Set("X-Fox-GM", "1")
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "fox-http ok proto=%s gm=%v path=%s\n", proto, gm, r.URL.Path)
}

// =============================================================================
// 8. 性能基础设施
// =============================================================================

// bufferPool 实现 httputil.BufferPool，供反代零分配复用拷贝缓冲。
type bufferPool struct {
	p sync.Pool
}

func newBufferPool(size int) *bufferPool {
	bp := &bufferPool{}
	bp.p.New = func() any { b := make([]byte, size); return &b }
	return bp
}

func (bp *bufferPool) Get() []byte  { return *(bp.p.Get().(*[]byte)) }
func (bp *bufferPool) Put(b []byte) { bp.p.Put(&b) }

// chanListener 是一个由 channel 驱动的虚拟 net.Listener，
// 用于把分流后的连接喂给标准 http.Server，从而完整复用其 keep-alive / h2 管理。
type chanListener struct {
	ch     chan net.Conn
	addr   net.Addr
	closed chan struct{}
	once   sync.Once
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{
		ch:     make(chan net.Conn, 256),
		addr:   addr,
		closed: make(chan struct{}),
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }

func (l *chanListener) push(c net.Conn) bool {
	select {
	case l.ch <- c:
		return true
	case <-l.closed:
		return false
	}
}

// peekedConn 把已读取的前缀字节重新拼回连接读取流。
type peekedConn struct {
	net.Conn
	prefix []byte
	off    int
}

func (p *peekedConn) Read(b []byte) (int, error) {
	if p.off < len(p.prefix) {
		n := copy(b, p.prefix[p.off:])
		p.off += n
		return n, nil
	}
	return p.Conn.Read(b)
}

func isClosedErr(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) || errors.Is(err, io.EOF)
}

// clientIP 从 "host:port" 中切出 host，避免 net.SplitHostPort 的字符串分配。
// RemoteAddr 形如 "1.2.3.4:5678" 或 IPv6 的 "[::1]:5678"，均返回切片（零分配）。
func clientIP(remoteAddr string) string {
	i := strings.LastIndexByte(remoteAddr, ':')
	if i < 0 {
		return remoteAddr
	}
	host := remoteAddr[:i]
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		return host[1 : len(host)-1] // 去掉 IPv6 方括号
	}
	return host
}

// =============================================================================
// 9. 工具：证书加载与自签生成
// =============================================================================

// loadOrGenStdCert 加载国际 TLS 证书；路径为空时生成一张内存自签证书（便于联调/测试）。
func loadOrGenStdCert(certFile, keyFile string) (tls.Certificate, error) {
	if certFile != "" && keyFile != "" {
		return tls.LoadX509KeyPair(certFile, keyFile)
	}
	return generateSelfSigned()
}

func generateSelfSigned() (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "fox-http self-signed"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}
