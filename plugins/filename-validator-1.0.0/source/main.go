package main

import (
	"bytes"
	_ "embed"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
)

const (
	// 仅对 multipart 请求做检测，避免影响非上传流量
	multipartPrefix = "multipart/form-data"
	// 限制解析的 body 大小，防止大文件拖垮内存
	maxBodyCheckSize = 32 << 20 // 32MB
	// part 数量上限，防止恶意请求海量 part 拖垮解析
	maxMultipartParts = 1000
	// 单次 filename 长度上限，超长视为异常
	maxFilenameLen = 2048
)

// 拦截页：与 static/waf/intercept.html 一致，仅去掉追踪信息/联系支持（不计入数据库）
//go:embed block.html
var blockPageHTML string

// ---------------- HostAPI ----------------
// 仅注入 getClientIP 即可（本插件不写 ACL，但需要可信代理感知的客户端 IP 写日志）；
// SetHostAPI 签名与其它插件保持一致，便于主进程统一注入。
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

// truncate 截断字符串用于日志展示，避免超长 filename 撑爆事件总线
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(trunc)"
}

// isDangerousFilename 检测 filename 是否包含空字节或路径穿越等危险字符（纯字符串判断，无正则，保证性能）
// 第二个返回值给出命中的具体原因，供日志记录区分攻击类型。
func isDangerousFilename(filename string) (bool, string) {
	if filename == "" {
		return false, ""
	}
	// 实际空字节
	if strings.Contains(filename, "\x00") {
		return true, "null_byte"
	}
	// URL 编码空字节
	if strings.Contains(filename, "%00") {
		return true, "url_encoded_null"
	}
	// 转义形式 \x00
	if strings.Contains(filename, `\x00`) {
		return true, "escaped_null"
	}
	// Windows 路径穿越
	if strings.Contains(filename, "..\\") {
		return true, "path_traversal_win"
	}
	// Unix 路径穿越
	if strings.Contains(filename, "../") {
		return true, "path_traversal_unix"
	}
	// URL 编码的 .. (%2e%2e) 路径穿越
	if strings.Contains(filename, "%2e%2e") {
		return true, "path_traversal_encoded"
	}
	return false, ""
}

// getRawFilenameFromPart 从 Part 的 Content-Disposition 头解析出原始 filename（未经 filepath.Base）
func getRawFilenameFromPart(part *multipart.Part) string {
	disp := part.Header.Get("Content-Disposition")
	if disp == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(disp)
	if err != nil {
		return ""
	}
	// 优先 filename* (RFC 5987)，再 filename
	if v := params["filename*"]; v != "" {
		// 格式多为 UTF-8''encoded 或 filename*0=...
		return v
	}
	return params["filename"]
}

// Handler 仅在 multipart 请求时解析并检查所有 part 的 filename，发现危险则 403
func Handler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	ct := r.Header.Get("Content-Type")
	if ct == "" || !strings.HasPrefix(strings.TrimSpace(ct), multipartPrefix) {
		return r, false
	}
	// 已知 body 超过限制时跳过检查，避免读取大 body 影响性能，且无法正确恢复 body
	if r.ContentLength > maxBodyCheckSize {
		return r, false
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyCheckSize))
	if err != nil {
		return r, false
	}
	// 后续链路需要读 body，必须恢复
	r.Body = io.NopCloser(bytes.NewReader(body))

	_, params, err := mime.ParseMediaType(ct)
	if err != nil || params["boundary"] == "" {
		return r, false
	}
	boundary := params["boundary"]
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	partCount := 0
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		partCount++
		if partCount > maxMultipartParts {
			ip := clientIPFromRequest(r)
			logEvt("block", "too_many_parts", ip, map[string]any{
				"path":  r.URL.Path,
				"host":  r.Host,
				"limit": maxMultipartParts,
			})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Blocked-By", "filename-validator")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(blockPageHTML))
			return nil, true
		}
		// Go 的 part.FileName() 会做 filepath.Base()，会丢掉 "../" 等路径，必须用原始 filename 检测
		rawFilename := getRawFilenameFromPart(part)
		if rawFilename == "" {
			rawFilename = part.FileName()
		}
		if len(rawFilename) > maxFilenameLen {
			ip := clientIPFromRequest(r)
			logEvt("block", "overlong_filename", ip, map[string]any{
				"path":     r.URL.Path,
				"host":     r.Host,
				"filename": truncate(rawFilename, 64),
				"length":   len(rawFilename),
				"limit":    maxFilenameLen,
			})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Blocked-By", "filename-validator")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(blockPageHTML))
			return nil, true
		}
		if hit, reason := isDangerousFilename(rawFilename); hit {
			ip := clientIPFromRequest(r)
			logEvt("block", "dangerous_filename", ip, map[string]any{
				"path":     r.URL.Path,
				"host":     r.Host,
				"filename": truncate(rawFilename, 256),
				"reason":   reason,
			})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Blocked-By", "filename-validator")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(blockPageHTML))
			return nil, true
		}
		// 只检查 filename，不消费 part 体，避免大文件占用
		io.Copy(io.Discard, part)
	}
	return r, false
}

// Init 插件入口：返回 name, order, enabled, handler
func Init() (string, int, bool, func(http.ResponseWriter, *http.Request) (*http.Request, bool)) {
	return "filename-validator", 10, true, Handler
}
