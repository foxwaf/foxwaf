package main

import (
	"bytes"
	_ "embed"
	"io"
	"mime"
	"mime/multipart"
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

// isDangerousFilename 检测 filename 是否包含空字节或路径穿越等危险字符（纯字符串判断，无正则，保证性能）
func isDangerousFilename(filename string) bool {
	if filename == "" {
		return false
	}
	// 实际空字节
	if strings.Contains(filename, "\x00") {
		return true
	}
	// URL 编码空字节
	if strings.Contains(filename, "%00") {
		return true
	}
	// 转义形式 \x00
	if strings.Contains(filename, `\x00`) {
		return true
	}
	// Windows 路径穿越
	if strings.Contains(filename, "..\\") {
		return true
	}
	// Unix 路径穿越
	if strings.Contains(filename, "../") {
		return true
	}
	// URL 编码的 .. (%2e%2e) 路径穿越
	if strings.Contains(filename, "%2e%2e") {
		return true
	}
	return false
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
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(blockPageHTML))
			return nil, true
		}
		if isDangerousFilename(rawFilename) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
