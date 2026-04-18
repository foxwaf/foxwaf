# useragent-validator

用于在请求进入主 WAF 规则前进行 User-Agent 基础风控。

拦截条件包括：

- 缺失或仅空白的 User-Agent
- 超长 User-Agent
- 命中黑名单关键词（包含边界检测，减少误拦截）

命中后直接返回 403 拦截页，不进入后续处理链路。
