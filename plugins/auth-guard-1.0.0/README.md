# auth-guard

登录暴力破解 / 撞库 / 认证探测防御插件。

## 工作原理

1. **响应后钩子 `AfterResponse`**：只对已知认证端点计数，零业务侵入。
2. **认证端点列表**（前缀匹配）：
   - 通用：`/login`、`/signin`、`/sign-in`、`/auth`、`/oauth`、`/sso`、`/account/login`
   - CMS：`/wp-login.php`、`/wp-admin`、`/administrator`、`/admin/login`、`/user/login`
   - 框架：`/api/login`、`/api/auth`、`/api/v*/login`、`/api/token`
   - 数据库/中间件：`/phpmyadmin`、`/adminer.php`
3. **失败状态码**：`400 / 401 / 403 / 404 / 422 / 429`（404 也计入，因为爬字典时大量探测不存在的 admin 路径也是典型撞库行为）。
4. **滑窗算法**：10 个 1 秒环形桶，**10 秒内累计 10 次失败** → 写 ACL 封 1 小时。
5. **拉黑生效**：依赖中央 ACL，再次访问任意路径直接 `dropConnection`（状态码 000）或 403。

## 应用场景

- 任何对外暴露登录页的站点（WordPress / Joomla / Drupal / 自研后台）。
- 基于 Token 的 API 网关，防止批量尝试账密。
- `/wp-login.php` 这类典型攻击热点即使**你不用 WordPress** 也会被海量字典扫描，开启本插件可大幅降低日志噪音。

## 配置常量

| 常量 | 默认 | 说明 |
|---|---|---|
| `windowSec` | 10 | 滑动窗口秒数 |
| `threshold` | 10 | 窗口内失败阈值 |
| `blockSec` | 3600 | ACL 封禁时长 |
| `maxTrackedIPs` | 50000 | LRU 上限 |

## 性能

- Handler 阶段：零开销（仅注册 AfterResponse）。
- AfterResponse：path 前缀不匹配立即 return；命中时仅 atomic 自增，无锁无分配，< 100 ns。

## 注意

- 正常用户输错密码 10 秒内 10 次几乎不可能，误伤率极低；如有特殊业务（自动化测试账号）请加白名单。
- `authEndpoints` 仅在源码里维护；新增自定义登录路径后需重编译。
- 与 CC / 频率限制模块互补：auth-guard 定向盯认证端点，不处理泛洪攻击。
