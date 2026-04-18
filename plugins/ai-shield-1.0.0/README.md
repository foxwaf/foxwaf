# ai-shield

AI 训练爬虫识别 + Prompt Injection 防御插件。

## 工作原理

1. **前置钩子 `Handler`（before_waf）**：
   - **AI 爬虫 UA**：匹配 `GPTBot / ClaudeBot / Claude-Web / anthropic-ai / ChatGPT-User / CCBot / Bytespider / Amazonbot / Google-Extended / PerplexityBot / Applebot-Extended / FacebookBot / ImagesiftBot / cohere-ai / Omgilibot / Diffbot` 等 20+ 主流 AI 训练爬虫，直接 403 + 拉黑 15 分钟。
   - **Prompt Injection**：扫描 URL（自动 `QueryUnescape`）和 body（限 32 KB）里的 `ignore previous instructions / disregard system prompt / you are now / jailbreak / DAN mode / act as / 忽略上文指令` 等 20+ 中英注入特征，命中即 403。
2. **HostAPI 集成**：AI 爬虫直接写入中央 ACL（`addACLBlock`）；Prompt Injection 同 IP 1 分钟内去重写 ACL。
3. **白名单**：`127/8、10/8、172.16/12、192.168/16、::1、fc00::/7、fe80::/10` 不触发。

## 应用场景

- 网站内容不希望被大模型训练集收录。
- 对外 LLM API 网关，防御用户绕过 system prompt。
- SEO 保护，区分搜索引擎爬虫和 AI 训练爬虫。

## 配置常量（`source/main.go`，修改后重编译）

| 常量 | 默认 | 说明 |
|---|---|---|
| `maxBodyScan` | 32 KB | body 扫描上限（避免大文件上传拖慢） |
| `aclBlockSec` | 900 | AI 爬虫拉黑秒数（15 分钟） |
| `promptDedupSec` | 60 | Prompt Injection 写 ACL 去重窗口 |
| `maxTrackedIPs` | 50000 | LRU 上限 |

## 性能

- 非 AI UA 命中时第一条 `strings.Contains` 失败即返回，约 300–500 ns。
- body 扫描仅在 POST/PUT/PATCH 且 Content-Type 为文本/JSON 时进行。

## 注意

- 若站点本身是 LLM 服务（上游就是 OpenAI 兼容 API），请检查自定义 system prompt 不要与 `promptInjectionSigs` 冲突。
- 特征列表位于源码顶部的 `aiBotSignatures` / `promptInjectionSigs`，按需自定。
