# FoxWAF 插件开发指南

> 本文面向 **开发者**，系统讲解 FoxWAF 插件的设计原则、编写方式、构建流程、打包格式与导入方法。
> 如果你只想**使用**插件（启用/禁用/删除），请直接看管理面板的 "插件管理" 菜单。

---

## 1. 插件能做什么

FoxWAF 插件是一个 **Go `plugin` 构建的 `.so` 动态库**，在 WAF 进程运行时热加载，可在以下位置介入请求：

| 位置 (`position`) | 触发时机 | 典型用途 |
|---|---|---|
| `front` | 最前置，**ACL 之前** | 改写请求、拒绝国家/ASN、预计数 |
| `before_cc` | ACL 之后，**CC 检查之前** | 对已通过 ACL 的流量做业务限流 |
| `before_waf` | CC 之后，**WAF 规则引擎之前**（默认） | UA 校验、文件名校验、扫描探测 |
| `before_origin` | WAF 通过后，**回源之前** | 重写 URL / 注入 Header |
| `after` | 回源完成后（响应回写前的末端） | 日志增强、响应改写（需谨慎） |

除了 "请求前" 钩子，插件还可导出一个 **响应后钩子 `AfterResponse`**，在上游响应回写完毕后调用，仅拿到请求 + 状态码 + 响应头（不含 body），可用于状态码统计、行为分析类场景（例如内置的 `scan-guard` 就是通过它检测 404 爆破）。

---

## 2. 插件目录结构

一个插件包就是一个目录，**目录名必须是 `<name>-<version>`**（例如 `myplugin-1.0.0`）：

```
myplugin-1.0.0/
├── source/
│   ├── main.go       # 插件源码（主进程用 go build -buildmode=plugin 编译）
│   ├── block.html    # （可选）拦截页，用 //go:embed 嵌入
│   └── ...           # 其它仅 plugin 用的 .go 文件
├── version.json      # 元数据（version / features）
├── README.md         # 对用户的说明（面板会展示）
└── plugin.so         # 编译产物（由构建流程生成，也可预置）
```

> **强约束**：
> - `version.json` 的 `version` 字段必须与目录名里的版本号完全一致
> - `Init()` 返回的 `name` 必须与目录名里的前缀完全一致
> - 编译产物文件名必须为 `plugin.so`

### 2.1 `version.json`

```json
{
  "version": "1.0.0",
  "features": [
    "一行功能说明 1",
    "一行功能说明 2"
  ]
}
```

### 2.2 `plugins/plugin.yaml`（全局插件配置）

`/app/plugins/plugin.yaml` 控制每个插件的加载顺序、启用状态、挂载位置：

```yaml
plugins:
    myplugin:
        order: 10             # 同位置下的执行顺序，数字越小越早
        enabled: true         # 是否启用
        position: before_waf  # 见上表
```

---

## 3. 编写插件源码

### 3.1 最小可运行示例

```go
// myplugin-1.0.0/source/main.go
package main

import "net/http"

// 必须导出：Init 返回 (name, order, enabled, handler)
func Init() (string, int, bool, func(http.ResponseWriter, *http.Request) (*http.Request, bool)) {
    return "myplugin", 10, true, Handler
}

// 必须导出：Handler 是请求前钩子
// 返回 (newReq, stop)：
//   - stop=false: 继续走后续链路，newReq 可为原 r 或改写后的
//   - stop=true:  直接终止请求（通常 Handler 自己已写回响应）
func Handler(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
    if r.Header.Get("X-Fake") == "1" {
        w.WriteHeader(http.StatusForbidden)
        w.Write([]byte("blocked by myplugin"))
        return nil, true
    }
    return r, false
}
```

### 3.2 可选：响应后钩子 `AfterResponse`

仅在需要"响应完成后做事"（如状态码统计、行为分析）时导出：

```go
// AfterResponse 只拿 header，零拷贝，严禁在这里做 I/O 阻塞
func AfterResponse(r *http.Request, statusCode int, respHeaders http.Header) {
    if statusCode >= 400 && statusCode < 500 {
        // 做点轻量计数或投递到 channel 异步处理
    }
}
```

### 3.3 可选：`SetHostAPI`（v1.1+ 提供）

插件可以访问主进程提供的部分能力，前提是导出 **`SetHostAPI`** 符号：

```go
var (
    hostAddACLBlock   func(ip, source, desc string, expireUnix int64) error
    hostIsWhitelisted func(ip string) bool
    hostGetClientIP   func(r *http.Request) string
)

// 由主进程在加载时调用；未导出时主进程静默跳过（向下兼容）
func SetHostAPI(
    addACLBlock func(ip, source, desc string, expireUnix int64) error,
    isWhitelisted func(ip string) bool,
    getClientIP func(r *http.Request) string,
) {
    hostAddACLBlock = addACLBlock
    hostIsWhitelisted = isWhitelisted
    hostGetClientIP = getClientIP
}
```

| HostAPI | 作用 |
|---|---|
| `addACLBlock(ip, source, desc, expireUnix)` | 把 IP 以 `global+ip+block` 写入中央 ACL，带 TTL。白名单命中自动跳过；同 `(source, ip)` 幂等刷新；自动条目总数硬上限 5000 |
| `isWhitelisted(ip)` | 查询 ACL 是否存在匹配该 IP 的 `action=allow` 规则 |
| `getClientIP(r)` | 用主进程可信代理规则解析客户端 IP，与 WAF/ACL/CC **完全一致**，避免 X-Real-IP 被伪造 |

> **强烈建议**：任何涉及"对 IP 判断"的插件都用 `getClientIP` 取 IP，不要自己读 header。

### 3.4 签名规则速查

| 符号 | 签名 | 必需 |
|---|---|---|
| `Init` | `func() (string, int, bool, func(http.ResponseWriter, *http.Request) (*http.Request, bool))` | ✅ |
| `Handler` | `func(http.ResponseWriter, *http.Request) (*http.Request, bool)` | ✅（`Init` 返回值里已经引用了它）|
| `AfterResponse` | `func(*http.Request, int, http.Header)` | ❌ |
| `SetHostAPI` | `func(func(string,string,string,int64) error, func(string) bool, func(*http.Request) string)` | ❌ |

### 3.5 性能与安全红线

- **Handler 是热路径**：每个请求都会过一遍。不要做 `fmt.Sprintf` / 正则复杂匹配 / 锁争用 / 磁盘 I/O；能用 `atomic` 就用 `atomic`，能用 `sync.Map` 就别用 `map+mutex`
- **AfterResponse 零拷贝**：只能用 header 与 status，**严禁读 body**（FoxWAF 不会传 body，避免 N 倍内存压力）
- **不要 `panic`**：主进程对插件调用包了 `recover()`，但你仍应在逻辑里自行兜底
- **禁止副作用型符号**：主进程会拒绝导出 `init`/`main`/可疑全局变量操作（见 `checkForbiddenSymbols`）
- **Go 版本必须匹配**：Go plugin 要求 **构建插件的 Go 版本与 WAF 主进程完全一致**。否则 `plugin.Open` 会报版本不一致
- **架构必须匹配**：linux/amd64 或 linux/arm64，不能跨平台编译

---

## 4. 构建插件

### 4.1 推荐方式：通过 FoxWAF 面板

面板的 "插件市场 / 导入插件" 支持上传 **zip 源码包**，主进程会：

1. 解压到临时目录
2. 校验目录结构与 `version.json`
3. 用**主进程当前的 Go 工具链**（默认 Go 1.24.9）执行 `go build -buildmode=plugin -o plugin.so main.go`
4. 失败则在响应里返回编译错误；成功则把插件写入 `/app/plugins/<name>-<ver>/` 并热加载

你无需本地搭建 Go 环境，上传 zip 就行。

### 4.2 本地构建（高级）

若你想预先产出 `plugin.so` 再打包，请用与 WAF 一致的 Go 工具链：

```bash
cd myplugin-1.0.0/source
CGO_ENABLED=1 go build -buildmode=plugin -o ../plugin.so main.go
```

> ⚠️ **不要加 `-trimpath`** —— Go plugin 对路径元信息敏感，加了会 "plugin was built with a different version of package internal/goarch" 报错。

---

## 5. 打包与导入

### 5.1 zip 包格式

把 **插件目录** 直接打 zip：

```
myplugin-1.0.0.zip
└── myplugin-1.0.0/
    ├── source/main.go
    ├── version.json
    └── README.md
```

zip 内 **必须** 只有一个顶层目录，目录名必须等于 `<name>-<version>`。

### 5.2 两种导入方式

#### ① 面板上传

登录 → 插件管理 → "导入插件" → 上传 zip。

#### ② 通过 URL 下载导入（单文件 ≤ 50MB）

填写 `https://` 公网 zip 链接，面板会自动下载 → 编译 → 加载。

---

## 6. 实战示例：目录扫描防护插件 `scan-guard`

`scan-guard` 是内置插件，演示了 **`AfterResponse` + `SetHostAPI` + 中央 ACL** 的完整组合。核心思路：

1. `AfterResponse` 对每个 4xx 响应（400/401/403/404/405）做环形桶计数
2. 10 秒滑动窗口内同 IP 计数 ≥ 30 → 判定为扫描行为
3. 通过 `hostAddACLBlock()` 把 IP 写入全局 ACL，TTL 10 分钟
4. 后续请求由 **ACL 层**直接短路拦截（完全不走插件），解封只需在 ACL 管理页删除该条目

热路径做了极致优化：
- `buckets [10]atomic.Int32` 环形桶，全 atomic 无锁
- LRU 上限 10 万 IP，IPv6 按 `/64` 聚合防地址放大攻击
- 命中阈值后重置桶计数，30 秒本地去重窗口防止 ACL 写爆

源码位置：`/app/plugins/scan-guard-1.0.0/source/main.go`，可作为模板参考。

---

## 7. 调试与常见问题

| 现象 | 原因 | 解决 |
|---|---|---|
| `plugin was built with a different version of package ...` | Go 版本/`-trimpath` 不一致 | 用面板上传或确保本地 Go 版本与 WAF 主进程一致（`foxwaf version`） |
| `插件名称不匹配: Init=xxx, 目录名=yyy` | `Init` 返回值与目录名前缀不同 | 修改 `Init()` 第一个返回值 |
| `插件包 xxx 版本不一致` | `version.json` 的 `version` ≠ 目录名后缀 | 保持两边一致 |
| `插件名称已存在` | 同名插件已加载 | 先在面板卸载旧版本，再导入新的 |
| 插件能加载，但请求看不到效果 | `plugin.yaml` 中 `enabled: false` 或 `position` 选错 | 面板切换启用，或修改 position 再重载 |
| `AfterResponse` 不执行 | 请求被 ACL/CC/WAF 在前面拦了，根本没回源 | 这是正常的——"有响应" 才会触发 `AfterResponse` |

---

## 8. 约定与最佳实践

- **插件职责单一**：一个插件只解决一类问题，便于启用/禁用
- **绝不阻塞热路径**：需要 I/O 的工作放 goroutine + channel 异步处理
- **永远处理 nil header / 空 body**：不要假设客户端一定按规矩来
- **暴露的拦截行为最好可被 ACL 托管**：通过 `SetHostAPI` 把拉黑写到 ACL，用户能在面板统一管理、删除、观察
- **所有拦截页面尽量嵌入 `//go:embed`**：不要在运行期访问文件系统

---

## 9. 反馈与贡献

- Issue：在主仓库提交，带上 `plugin` 标签
- 示例插件 PR：请放在 `plugins/<name>-<ver>/` 下，附 `README.md` 与 `version.json`
- 高性能 / 安全相关讨论可先发到 QQ 群（见主 README）
