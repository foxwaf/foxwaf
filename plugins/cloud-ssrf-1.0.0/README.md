# cloud-ssrf

SSRF（服务器端请求伪造）+ 云元数据防御插件，阻止攻击者通过服务端代理访问内网 / 云厂商 IMDS。

## 工作原理

1. **前置钩子 `Handler`**：扫描 URL 参数 + body（限 32 KB）里的：
   - **云元数据地址**：
     - AWS IMDS `169.254.169.254`
     - 阿里云 `100.100.100.200`
     - GCP `metadata.google.internal`、`metadata.goog`
     - Azure `169.254.169.254` + `Metadata: true` 头特征
     - DigitalOcean / Oracle / IBM Cloud 等
   - **危险 scheme**：`gopher:// / file:// / dict:// / ftp:// / ldap:// / tftp:// / jar:// / netdoc://`
   - **IP 混淆**：十进制（如 `2852039166` == `169.254.169.254`）、八进制、十六进制、`[::ffff:a9fe:a9fe]`、`0.0.0.0 / 127.x / 内网段直写` 等。
2. **策略**：
   - 单次命中立即 **403 + X-Blocked-By: cloud-ssrf**。
   - **30 秒内同 IP 命中 5 次** → 写 ACL 封禁 30 分钟。
3. **Host 归一化**：支持 `[v6]`、URL 编码、末尾点、端口号等各种绕过变形。

## 应用场景

- 任意接受 URL 输入的接口（图片代理、PDF 渲染、webhook 验证、短链跳转、RSS 订阅、富文本抓图…）。
- 云环境部署的后端，防止 IMDS 凭据被拖取。

## 配置常量

| 常量 | 默认 | 说明 |
|---|---|---|
| `maxBodyScan` | 32 KB | body 扫描上限 |
| `burstWindow` / `burstThresh` | 30s / 5 | 触发 ACL 封禁的滑窗 |
| `aclBlockSec` | 1800 | 封禁 30 分钟 |
| `maxTrackedIPs` | 50000 | LRU 上限 |

## 性能

- URL 扫描：一次字符串匹配 + 轻量 host 解析，约 500 ns–1 μs。
- body 仅扫文本/JSON；二进制类型（image/*、application/octet-stream）跳过。

## 注意

- 如果你**确实需要**业务请求云元数据（健康检查、资源盘点），请把调用源 IP 加入 WAF 全局白名单或部署到 `whitelistCIDRs`。
- `internalCIDRs` 默认包含 RFC1918 + link-local + loopback，不建议放宽。
