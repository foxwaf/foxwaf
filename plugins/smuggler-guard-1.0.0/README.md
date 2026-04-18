# smuggler-guard

HTTP 请求走私（HTTP Request Smuggling）防御插件，纵深防御层。

## 工作原理

前置钩子扫描请求头异常组合，命中任一即 403 并纳入滑窗计数：

| 检测项 | 判据 |
|---|---|
| 双重 Content-Length | 同一请求出现 >1 个 CL 值 |
| TE + CL 共存 | 同时出现 Transfer-Encoding 和 Content-Length（经典 TE.CL / CL.TE 走私） |
| 混淆 TE | `Transfer-Encoding: xchunked / chunked, identity / ChUnKeD ` 等伪装变体 |
| 双重 Transfer-Encoding | 同时出现 `chunked` 和 `identity/gzip` 等 |
| 负 / 超大 Content-Length | CL < 0 或 CL > 1 GB |
| 异常 header 行 | Header name 含空格 / 冒号异位 / CR LF 夹带 |

**滑窗**：60s 内命中 3 次 → 写 ACL 封禁 30 分钟。

## 和 Go 标准库的关系（重要）

FoxWAF 基于 Go `net/http`，其 HTTP/1.1 解析器**在进入插件之前**就会：

- 拒绝双 CL、负 CL → `400 Bad Request`
- 拒绝混淆/双 TE → `501 Not Implemented`
- 对 "TE + CL" 自动**丢弃 CL、保留 chunked**，统一为标准 chunked 处理（不会走私）

**结论**：绝大多数走私载荷在 stdlib 层就被终结了；本插件作为**纵深防御**存在：

- 未来若引入 HTTP/2 直连、gRPC、裸 TCP 反代等非严格解析场景，本插件会成为主力拦截者。
- 现在主要提供**可见性**：把被 stdlib 拒绝之前/之后能观察到的异常写进 ACL + 事件日志，方便 SOC 溯源。

## 配置常量

| 常量 | 默认 | 说明 |
|---|---|---|
| `burstWindow` / `burstThresh` | 60s / 3 | 触发 ACL 封禁的滑窗 |
| `aclBlockSec` | 1800 | 封禁 30 分钟 |
| `maxCLValue` | 1 GB | CL 上限（未声明 chunked 时） |

## 性能

- 热路径仅扫 `r.Header` map，约 100–200 ns；非走私请求无分配无 GC 压力。

## 注意

- 与 `curl --data-raw` 等标准客户端不冲突；误伤率极低。
- 特征可能随着 HTTP/2 / HTTP/3 栈演进调整，留意版本更新。
