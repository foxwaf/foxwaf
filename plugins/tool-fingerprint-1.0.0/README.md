# tool-fingerprint

安全扫描器 / 渗透工具指纹识别插件，把"拿工具来扫"的行为在第一个请求就拦下。

## 工作原理

前置钩子在 before_waf 位置做三重识别（任一命中即 403 + 立即写 ACL 封 1 小时）：

### 1. User-Agent 特征（40+）

`sqlmap / nmap / nuclei / nikto / wpscan / wfuzz / ffuf / dirb / gobuster / feroxbuster / masscan / zmap / Acunetix / Nessus / Netsparker / AppScan / Qualys / WebInspect / burpsuite / OWASP ZAP / Arachni / w3af / skipfish / WhatWeb / httpx / katana / subfinder / amass / dnsrecon / dalfox / XSStrike / commix / Havij / SQLNinja / Metasploit` 等。

### 2. 特征请求头

- `X-Scanner`、`X-Acunetix-*`、`Acunetix-Aspect-*`
- Nessus / AppScan 的内部调试头
- Nuclei 的 `X-Template`

### 3. URL / body 路径特征

- `{{BaseURL}}`、`{{interactsh-url}}`、`{{randstr}}` 等 Nuclei/Burp 占位符
- `/.git/ /.env /.aws/credentials /wp-config.php.bak` 等敏感文件探测（量大时）
- **OOB 回连域名**：`*.oastify.com / *.interactsh.*/*.dnslog.cn / *.burpcollaborator.net / *.canarytokens.com / *.requestbin.* / *.ngrok.io`（在任意 header、URL、body 里出现即视为 OOB 回连测试）

## 策略

- 识别为高置信度工具 → **立即拉黑 1 小时**（不需要累计）。
- 同 IP 60 秒内重复识别做本地去重，避免日志刷屏。
- 白名单内网 IP 不参与识别。

## 应用场景

- 对外生产环境默认开启，扫描器流量直接归零。
- 配合 scan-guard：tool-fingerprint 杀"已知工具"，scan-guard 杀"行为像扫描"。

## 配置常量

| 常量 | 默认 | 说明 |
|---|---|---|
| `aclBlockSec` | 3600 | 封禁 1 小时 |
| `localDedupSec` | 60 | 本地去重窗口 |
| `maxTrackedIPs` | 50000 | LRU 上限 |

## 性能

- 正常浏览器 UA 命中第一条 `strings.Contains` 失败即返回，约 300–600 ns。
- header / path 扫描只取前几项 header、路径前 256 字节，避免极端情况拖慢。

## 注意

- 请先在测试环境验证：**你自己跑的安全扫描（例如 CI 的 ZAP baseline）会被拦**。把 CI IP 加入 WAF 白名单再开启。
- 特征列表在 `toolUASigs / toolHeaderSigs / toolPathSigs / oobDomains`，可自行补充。
- 识别误伤通常是自定义的小工具 UA 用了 `python-requests/*` 这类泛用标识 —— 这类默认不在列表里，需要你显式加。
