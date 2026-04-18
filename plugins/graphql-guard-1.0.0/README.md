# graphql-guard

GraphQL 端点滥用防御插件（Introspection / 深度爆炸 / Alias 爆炸 / 批量查询 DoS）。

## 工作原理

仅作用于 GraphQL 端点（默认 `/graphql`、`/api/graphql`、`/v1/graphql`、`/v2/graphql`、`/gql`，可改源码扩展），非 GQL 路径立即返回不耗 CPU。

### 检测项

| 检测 | 判据 | 说明 |
|---|---|---|
| **Introspection 查询** | query 含 `__schema` 或 `__type` | 生产环境通常应关闭 schema 反射 |
| **查询深度** | AST 嵌套层级 > 10 | 防御"嵌套递归查询"型 DoS |
| **Alias 爆炸** | alias 数 > 50 | 批量 alias 经典用于绕过限流 |
| **字段数** | 字段总数 > 500 | 单请求抓取过多资源 |
| **Batch 滥用** | 请求 body 是数组且长度异常 | 部分 GraphQL 服务允许 `[{query:...}, ...]` |

支持三种提交方式：POST JSON、POST `application/graphql`、GET `?query=...`。

### 策略

- 单次命中 → 403 + `X-Blocked-By: graphql-guard`。
- 60 秒内同 IP 命中 5 次 → 写 ACL 封 30 分钟。

## 应用场景

- 对外暴露 GraphQL API 的站点（Hasura / PostGraphile / Apollo / 自研）。
- Headless CMS（Strapi、Directus、Contentful 代理层）。

## 配置常量

| 常量 | 默认 | 说明 |
|---|---|---|
| `maxBodyScan` | 128 KB | GraphQL query 体扫描上限 |
| `maxDepth` | 10 | 允许的最大嵌套深度 |
| `maxAliases` | 50 | 允许的最大 alias 数 |
| `maxFields` | 500 | 允许的最大字段数 |
| `disallowIntrospect` | true | 是否禁用 introspection |
| `burstWindow` / `burstThresh` | 60s / 5 | ACL 封禁触发滑窗 |
| `aclBlockSec` | 1800 | 封禁时长 |

## 性能

- 非 GQL 端点：路径前缀判定后 return，< 50 ns。
- GQL 端点：轻量 AST 扫描（不做完整 parse），只统计字符 / 嵌套层数，约 10–50 μs（随 query 大小）。

## 注意

- 如果你的**开发/调试环境**需要 introspection（GraphQL Playground 依赖它），把 graphql-guard `enabled: false` 或在源码里把 `disallowIntrospect = false`。
- `gqlEndpoints` 是前缀匹配，比如把 `/api/` 全归为 GraphQL 会误伤 REST 接口，请精确到具体端点。
- 深度/字段阈值需要结合业务调，复杂关联查询（电商订单 + 明细 + 物流）可能超过 10 层。
