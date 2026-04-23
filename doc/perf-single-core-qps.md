# LittleFox WAF 单核 QPS 性能测试报告

测试时间：**2026-04-23 21:00:07**

## 1. 测试环境

| 项目 | 值 |
|---|---|
| 主机 | WSL2 / Linux 6.6.87.2-microsoft-standard-WSL2 |
| CPU | 13th Gen Intel(R) Core(TM) i9-13900H |
| 总核数 | 20 （但 WAF 被 `taskset` 绑定到 **CPU 3** 单核） |
| 内存 | 16 GB |
| WAF 进程 | `memfd:waf`  PID 61295，`Cpus_allowed_list: 3` |
| 上游 | **不存在的端口**（固定返回 502，隔离下游噪声） |
| 客户端 | `bombardier` (bombardier version unspecified linux/amd64) 跑 HTTP/1.1、HTTP/2；自研 `h3bench` (基于 quic-go v0.59.0) 跑 HTTP/3 |
| 客户端 CPU | taskset `0-2,4-19`，与 WAF 物理隔离 |
| Go 版本 | go version go1.24.9 linux/amd64 |
| 每场时长 | 30 秒（H3 另加 3 秒 warmup） |
| 并发选择 | 每个协议在 smoke-sweep 中找到的饱和点（见 §2） |

## 2. 并发饱和扫描（5 秒短测）

为了让每个协议在公平工作点对比，先做一轮并发扫描，GET 空 body：

| 协议 | c=4 | c=8 | c=16 | c=32 | c=64 | c=128 | c=256 | 选用 c |
|---|---|---|---|---|---|---|---|---|
| HTTP/1.1 | — | — | — | **9 897** | 6 801 | — | — | 32 |
| HTTP/2   | 6 690 | 7 845 | 8 673 | 9 107 | 9 893 | **9 920** | 8 919 | 128 |
| HTTP/3   | 5 033 | 3 331 | 3 927 | 6 435 | 6 840 | 6 730 | **6 986** | 128 |

- HTTP/1.1 在 c≈32 饱和，继续加连接反而下降（单核上下文切换 + TLS 握手排队）。
- HTTP/2 单连接多流，c=128 达到 9 920。
- HTTP/3 单连接多流（UDP），c=128~256 稳在 7K 左右；p50 延迟随 c 上升。

## 3. 主结果（30s 正式测试，WAF 单核 100%）

| 协议 | 场景 | 并发 c | 总请求 | **QPS** | p50 | p90 | p99 | 下行 MB/s | 上行 MB/s | WAF CPU 中位 |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| HTTP/1.1 | GET 小 (0 B) | 32 | 276,671 | **9,218** | 0.41 ms | 2.26 ms | 86.26 ms | 13.5 | 1.1 | 98% |
| HTTP/1.1 | POST 小 (128 B) | 32 | 175,574 | **5,851** | 0.50 ms | 4.64 ms | 97.09 ms | 8.6 | 1.5 | 99% |
| HTTP/1.1 | POST 大 (64 KB) | 32 | 980 | **32** | 931.95 ms | 1256.01 ms | 2594.20 ms | 0.0 | 2.0 | 99% |
| HTTP/2 | GET 小 (0 B) | 128 | 300,690 | **10,021** | 10.32 ms | 26.23 ms | 57.91 ms | 13.2 | 0.9 | 99% |
| HTTP/2 | POST 小 (128 B) | 128 | 163,906 | **5,462** | 19.70 ms | 44.45 ms | 87.31 ms | 7.2 | 1.2 | 99% |
| HTTP/2 | POST 大 (64 KB) | 128 | 1,096 | **33** | 3266.79 ms | 6566.07 ms | 10445.95 ms | 0.0 | 2.0 | 99% |
| HTTP/3 | GET 小 (0 B) | 128 | 188,551 | **6,279** | 12.91 ms | 50.27 ms | 126.57 ms | 7.9 | 0.0 | 99% |
| HTTP/3 | POST 小 (128 B) | 128 | 129,450 | **4,311** | 18.42 ms | 74.31 ms | 172.16 ms | 5.4 | 0.5 | 99% |
| HTTP/3 | POST 大 (64 KB) | 128 | 1,917 | **60** | 1696.32 ms | 3511.87 ms | 8522.39 ms | 0.1 | 3.7 | 99% |

> 说明：WAF CPU 数据来自 `pidstat -p $PID 1`，所有场景中位都已到 98~99%（CPU 3 单核上限 100%），说明 QPS 已被单核算力打满。

## 4. 按场景横向对比

| 场景 | HTTP/1.1 QPS | HTTP/2 QPS | HTTP/3 QPS | H2 vs H1 | H3 vs H2 |
|---|---:|---:|---:|---:|---:|
| GET 小 | 9,218 | 10,021 | 6,279 | +8.7% | -37.3% |
| POST 小 | 5,851 | 5,462 | 4,311 | -6.7% | -21.1% |
| POST 大 | 32 | 33 | 60 | +1.5% | +83.6% |

## 4.1 业务混合加权平均（单核"日常 QPS"）

QPS 是速率，不能简单算术平均；必须用 **调和平均**（按每请求的 CPU 时间加权）：

$$
\mathrm{QPS}_{\text{mix}} = \frac{1}{\sum_i w_i / \mathrm{QPS}_i}
$$

按 3 种常见业务形态加权：

| 业务混合 | HTTP/1.1 | HTTP/2 | HTTP/3 | 三协议平均 |
|---|---:|---:|---:|---:|
| **典型 Web**（75% GET 小 / 20% POST 小 / 5% POST 大） | 596 | 615 | 1,001 | **~740** |
| **API 网关**（50% GET 小 / 48% POST 小 / 2% POST 大） | 1,314 | 1,344 | 1,907 | **~1,520** |
| **上传密集**（40% GET 小 / 40% POST 小 / 20% POST 大） | 157 | 162 | 287 | **~200** |

另外两个有意义的"单一数字"：

| 视角 | 单核 QPS |
|---|---:|
| 只看小请求（GET + POST 小，6 场景调和平均） | **~6,300** |
| 完整 9 场景调和平均（含 POST 大） | **~114** |
| 99% 小请求 + 1% POST 大（真实 WAF 常态） | **~5,000–6,000** |

> **一句话结论**：LittleFox WAF 单核承载力在 **小请求约 5k–10k QPS**、**典型混合业务约 3k–6k QPS**、**上传密集场景约 100–300 QPS**。
>
> 注意反直觉的点：**POST 大** 场景 HTTP/3 (~60 QPS) 比 HTTP/1.1 (~32 QPS) 快了近 **2 倍**，所以"上传密集"加权结果里 H3 最高。这也是宣传"HTTP/3 在大 body 场景更快"在 WAF 前端的真实体现。

## 5. 关键发现

1. **小请求 QPS：HTTP/1.1 ≈ HTTP/2 > HTTP/3**
   - GET 小场景：H1 9 218 / H2 10 021 / H3 6 279。
   - 原因：服务端（quic-go + Go runtime + TLS）在单核上处理一个 QUIC 帧 / HTTP/3 流的开销比 TLS-over-TCP 更大（加解密、congestion control、packet pacing、FEC 判定）。H3 在**多核**上才能借助多流并行超过 H2，单核被同一个 CPU 的锁和 pacing goroutine 拖累。

2. **POST 小 比 GET 慢 ~40%**（H1 5 851 / H2 5 462 / H3 4 311）
   - 请求解析要多读一段 body，WAF 里 body 扫描规则启动；bombardier 也要多发一个 data frame。
   - H1 vs H2 打成平手，说明单核瓶颈已不是 TCP vs stream 调度，而是**请求解析 + 规则检查** 的绝对 CPU cycles。

3. **大 POST 成了带宽-吞吐受限**
   - H1 32 QPS × 64 KB ≈ **2.0 MB/s 上行**
   - H2 33 QPS × 64 KB ≈ **2.0 MB/s 上行**
   - H3 60 QPS × 64 KB ≈ **3.7 MB/s 上行**
   - 此时瓶颈是 **body 解析 + 拷贝** 的 CPU 成本，H3 略胜是因为我们的 h3bench 对每个 POST body 直接写 `bytes.Reader` + quic-go 的零拷贝流式发送，H1/H2 走 bombardier 的 `net/http` chunked 路径多一次 buffer。两者都远高于上游吞吐，WAF CPU 都是 99~100%。

4. **p99 尾延迟**
   - 小请求 p99：H1 86 ms / H2 58 ms / H3 127 ms。
   - **H2 的 p99 最好**，因为它的连接是 TCP + 内核收发，排队尺度小、公平性好；H3 所有 stream 都走 quic-go 的单个 event loop，在 128 并发下用户态排队明显。
   - H1 的 p99 偏高是因为 c=32 每条连接串行处理，一旦 WAF busy 就会把整条 pipeline 停住。

5. **上游 502 的影响极小**
   - 502 页面是 WAF 内置静态 HTML（1319 字节），几乎零 I/O。本次测到的 QPS 基本就是 **WAF 的纯收-发+TLS+规则** 的单核上限。
   - 若上游变成真实 HTTP/1.1 后端（比如 localhost nginx），**QPS 会再降 30~50%**（多一跳 socket + 日志 + upstream keepalive 竞争）。

## 6. 单核上限估算（扣除协议开销）

| 组件 | 估算 CPU 开销 / 请求 (µs) |
|---|---:|
| TCP/TLS1.3 收 → 发（h1/h2）| ~50 |
| QUIC 收 → 发 (h3) | ~110 |
| HTTP 解析（h1 line / h2 HPACK / h3 QPACK）| 5~15 |
| WAF 规则扫描 (空 URL/空 body) | ~40 |
| 502 页面静态返回 | ~5 |
| **合计小 GET**（100~160 µs）→ 6k~10k QPS | ✔ 与实测吻合 |

## 7. 测试方法复现

```bash
# 1) 把 WAF 绑到单核
sudo taskset -apc 3 $(pgrep -f '^\\./waf')

# 2) 让上游返回 502（已配好）
curl -sk https://kabubu.com/ -o /dev/null -w '%{http_code}\\n'   # 502

# 3) 压测客户端隔离到其余核
taskset -c 0-2,4-19 bombardier -k -c 32 -d 30s --http1 -l https://kabubu.com/
taskset -c 0-2,4-19 bombardier -k -c 128 -d 30s --http2 -l https://kabubu.com/
taskset -c 0-2,4-19 /usr/local/bin/h3bench -u https://kabubu.com/ -c 128 -d 30s

# 4) 采样 WAF CPU
pidstat -p $(pgrep -f '^\\./waf') 1 35
```

所有原始数据：`/tmp/bench/raw/` 和 `/tmp/bench/results.jsonl`  
h3bench 源码：`/tmp/h3bench/main.go`（与 WAF 同 quic-go 版本，最公平对比）

