<p align="center">
  <img src="https://img.shields.io/badge/FoxWAF-Web_Application_Firewall-ff6600?style=for-the-badge&logo=firefox&logoColor=white" alt="FoxWAF"/>
</p>

<p align="center">
  <a href="#快速安装"><img src="https://img.shields.io/badge/一键安装-30秒部署-success?style=flat-square" alt="Install"/></a>
  <img src="https://img.shields.io/badge/language-Go-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go"/>
  <img src="https://img.shields.io/badge/license-Apache_2.0-blue?style=flat-square" alt="License"/>
  <img src="https://img.shields.io/badge/platform-Linux_x86__64-lightgrey?style=flat-square" alt="Platform"/>
  <img src="https://img.shields.io/badge/docker-supported-2496ED?style=flat-square&logo=docker&logoColor=white" alt="Docker"/>
</p>

<p align="center">
  <b>轻量级 · 高性能 · 开箱即用</b>
</p>

---

## 简介

**FoxWAF** 是一款基于 Go 语言开发的轻量级高性能 Web 应用防火墙，单文件部署，零依赖。内置 Aho-Corasick 多模式匹配引擎、CC 攻击防护、反爬虫检测、响应内容审计、SSL/TLS 证书管理、负载均衡及静态资源缓存，提供可视化管理面板，支持一键安装与在线热更新。

FoxWAF is a lightweight, high-performance Web Application Firewall built in Go. Single-binary deployment, zero dependencies. Features include an Aho-Corasick multi-pattern matching engine, CC attack mitigation, anti-bot/crawler detection, response content inspection, SSL/TLS management, load balancing, and static resource caching — all managed through a built-in web dashboard with one-click install and live updates.

---

## 核心特性

| 模块 | 能力 |
|:---|:---|
| **规则引擎** | Aho-Corasick 多模式匹配 + 正则回退，毫秒级检测 |
| **CC 防护** | 256 分片计数器，滑动窗口限速，JS Challenge 验证 |
| **反爬虫** | 浏览器指纹检测，自动识别爬虫/扫描器/自动化工具 |
| **负载均衡** | 轮询 / 加权轮询，健康检查，故障自动摘除 |
| **SSL/TLS** | SNI 动态证书加载，在线证书管理，HTTPS 一键开启 |
| **静态缓存** | 智能静态资源缓存，自定义规则，显著降低源站压力 |
| **响应审计** | 出站内容检测，敏感信息泄露防护 |
| **WebSocket** | 完整 WebSocket 代理支持 |
| **管理面板** | 现代化 Web UI，实时流量监控，攻击日志可视化 |
| **插件系统** | Go Plugin 热加载，无需重编译即可扩展功能 |
| **在线更新** | 多镜像源智能下载，自动回退，零停机热更新 |

---

## 快速安装

### 一键安装（推荐）

```bash
curl -fsSL https://gitcode.com/kabubu/storage/raw/main/install.sh | bash
```

或指定镜像源：

```bash
curl -fsSL https://gitcode.com/kabubu/storage/raw/main/install.sh | bash -s -- --mirror gitcode
```

### Docker 安装

```bash
curl -fsSL https://gitcode.com/kabubu/storage/raw/main/install.sh | bash -s -- --docker
```

### 手动安装

1. 下载最新 Release 中的 `waf` 和 `source.enc`
2. 放入同一目录，创建 `conf.yaml` 配置文件
3. 赋予执行权限并运行：

```bash
chmod +x waf
./waf
```

---

## 管理命令

安装完成后，使用 `foxwaf` 命令管理服务：

```bash
foxwaf start        # 启动服务
foxwaf stop         # 停止服务
foxwaf restart      # 重启服务
foxwaf status       # 查看运行状态
foxwaf logs         # 查看实时日志
foxwaf update       # 检查并应用更新
foxwaf export       # 导出数据备份（含镜像）
foxwaf import       # 从备份恢复
foxwaf uninstall    # 卸载
foxwaf version      # 查看版本信息
```

---

## 数据备份与恢复

防止 Docker 重装或服务器迁移导致数据丢失：

```bash
# 导出备份（包含配置、数据库、证书、Docker 镜像）
foxwaf export

# 备份文件保存在 /data/foxwaf/backup/ 目录
# 迁移到新服务器后恢复：
foxwaf import /data/foxwaf/backup/foxwaf-backup-20260327.tar.gz
```

---

## 架构概览

```
                    ┌──────────────┐
    Client ──────▶  │   FoxWAF     │ ──────▶ Upstream
    Request         │              │         Servers
                    │  ┌────────┐  │
                    │  │AC 引擎 │  │
                    │  │CC 防护 │  │
                    │  │反爬虫  │  │
                    │  │SSL/TLS │  │
                    │  │负载均衡│  │
                    │  │静态缓存│  │
                    │  └────────┘  │
                    │              │
                    │  管理面板    │◀── Admin
                    └──────────────┘
```

---

## 更新机制

FoxWAF 采用多镜像源智能更新：

1. 服务端下发最新版本信息及镜像仓库地址
2. 客户端按优先级依次尝试从各镜像源下载
3. 所有镜像不可用时，自动回退到服务端直链
4. MD5 校验 + 原子替换，更新失败自动回滚

支持的镜像源：GitHub · GitCode · Gitee · GitLab

---

## 系统要求

- **操作系统**: Linux (x86_64)
- **内存**: ≥ 256MB（推荐 512MB+）
- **磁盘**: ≥ 200MB 可用空间
- **Docker**: 20.10+（Docker 安装模式）
- **网络**: 80/443 端口可用

---

## 许可证

本项目基于 [Apache License 2.0](LICENSE) 开源。

---

<p align="center">
  <sub>Copyright © 2026 FoxWAF. All rights reserved.</sub>
</p>
