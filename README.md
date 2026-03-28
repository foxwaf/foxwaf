<p align="center">
  <br>
  <img width="240" src="https://img.shields.io/badge/%F0%9F%A6%8A_FoxWAF-ff6600?style=for-the-badge&labelColor=1a1a2e" alt="FoxWAF"/>
  <br><br>
  <strong>轻量级高性能 Web 应用防火墙</strong>
  <br>
  <sub>Lightweight High-Performance Web Application Firewall</sub>
  <br><br>
  <a href="#-快速开始"><img src="https://img.shields.io/badge/快速开始-30s_部署-28a745?style=flat-square" alt="Quick Start"/></a>
  <img src="https://img.shields.io/badge/Go-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go"/>
  <img src="https://img.shields.io/badge/Docker-2496ED?style=flat-square&logo=docker&logoColor=white" alt="Docker"/>
  <img src="https://img.shields.io/badge/License-Apache_2.0-blue?style=flat-square" alt="License"/>
  <img src="https://img.shields.io/badge/Linux-x86__64%20|%20ARM64-FCC624?style=flat-square&logo=linux&logoColor=black" alt="Linux"/>
</p>

---

## 📖 概述

> **⚠️ 本项目目前处于开发测试阶段，服务端功能将逐步开放，敬请关注。**

FoxWAF 是一款基于 Go 构建的 Web 应用防火墙，以**单文件、零依赖**的设计理念，为中小型网站提供企业级的安全防护能力。内置可视化管理面板，支持 Docker 一键部署，适用于反向代理、API 网关等场景。

**主要解决的问题：**
- SQL 注入、XSS、路径遍历等 OWASP Top 10 威胁
- CC / DDoS 应用层攻击
- 恶意爬虫与自动化扫描工具
- 敏感信息泄露

---

## ✨ 特性一览

<table>
<tr>
<td width="50%">

**🛡️ 安全防护**
- Aho-Corasick 多模式匹配引擎
- 480+ 内置安全规则（OWASP 覆盖）
- CC 攻击智能防护（JS Challenge）
- 反爬虫 / 反自动化工具检测
- User-Agent 黑名单过滤
- 出站内容审计（敏感信息防泄露）

</td>
<td width="50%">

**⚡ 高性能架构**
- Go 原生高并发处理
- 256 分片无锁计数器
- AC 自动机毫秒级规则匹配
- 静态资源智能缓存
- 请求体按需读取，零冗余 I/O
- Body 缓冲池复用（sync.Pool）

</td>
</tr>
<tr>
<td>

**🔧 运维友好**
- 单文件部署，无外部依赖
- Docker / 裸机双模式支持
- 可视化管理面板（实时监控）
- 多镜像源自动更新 + MD5 校验
- 一键备份恢复（含 Docker 镜像）
- SSL/TLS 证书在线管理

</td>
<td>

**🌐 代理能力**
- HTTP / HTTPS 反向代理
- WebSocket 全双工代理
- 负载均衡（轮询 / 加权）
- 上游健康检查 + 故障摘除
- SNI 动态证书加载
- 自定义 Header / 路由规则

</td>
</tr>
</table>

---

## 🚀 快速开始

### 方式一：一键安装（推荐）

```bash
curl -fsSL http://<服务端地址>/install.sh | bash -s -- --docker
```

> 安装脚本会自动检测环境、下载镜像、配置并启动服务。

### 方式二：手动 Docker 部署

```bash
# 1. 从 Release 下载镜像
curl -L -o foxwaf-image.tar.gz "<Release 下载地址>/foxwaf-image.tar.gz"

# 2. 导入镜像
docker load -i foxwaf-image.tar.gz

# 3. 创建配置目录
mkdir -p /data/foxwaf && cd /data/foxwaf

# 4. 下载 docker-compose.yaml 和默认配置
# 5. 启动
docker compose up -d
```

### 方式三：裸机部署

```bash
# 1. 下载 waf 主程序和 source.enc
# 2. 放入同一目录，创建 conf.yaml
# 3. 启动
chmod +x waf && ./waf
```

### 安装选项

| 参数 | 说明 | 默认值 |
|:---|:---|:---|
| `--docker` | Docker 容器模式 | 自动检测 |
| `--bare` | 裸机模式 | - |
| `--mirror NAME` | 首选镜像源 (github/gitcode/gitee/gitlab) | gitcode |
| `--version VER` | 指定版本号 | 最新版 |
| `--dir PATH` | 安装目录 | /data/foxwaf |
| `--no-start` | 安装后不自动启动 | - |

---

## 🎛️ 管理命令

安装完成后，使用 `foxwaf` 命令管理服务：

### 服务控制

```bash
foxwaf start          # 启动
foxwaf stop           # 停止
foxwaf restart        # 重启
foxwaf status         # 查看状态（版本、模式、运行情况）
foxwaf logs           # 实时查看日志
```

### 数据维护

```bash
foxwaf update         # 检查并应用更新（自动从镜像源下载）
foxwaf export         # 备份（配置、数据库、证书、Docker 镜像）
foxwaf import <file>  # 从备份恢复
foxwaf uninstall      # 卸载（数据保留）
foxwaf version        # 当前版本号
```

### `foxwaf status` 输出示例

```
  FoxWAF

  版本    beta1.0
  模式    Docker
  状态    ✓ 运行中
  详情    a1b2c3d4  2 hours ago
  目录    /data/foxwaf
  面板    http://localhost:8088/foxadmin
```

---

## 💾 备份与恢复

防止服务器迁移或 Docker 重装导致数据丢失：

```bash
# 导出完整备份（包含配置、数据库、证书、插件、Docker 镜像）
foxwaf export
# → 备份文件: /data/foxwaf/backup/foxwaf-20260328_120000.tar.gz (53M)

# 迁移到新服务器后恢复
foxwaf import /data/foxwaf/backup/foxwaf-20260328_120000.tar.gz
```

**备份包含内容：**

| 项目 | 说明 |
|:---|:---|
| `conf.yaml` | WAF 配置 |
| `*.db` | 攻击日志、站点配置、证书等数据库 |
| `data/` | 运行时数据 |
| `certificates/` | SSL/TLS 证书文件 |
| `plugins/` | 插件 |
| `docker-image.tar` | Docker 镜像（Docker 模式） |

---

## 🔄 更新机制

FoxWAF 采用**多镜像源智能更新**架构：

```
检查更新 → 服务端返回版本 + 镜像源列表
              ↓
   按优先级尝试: GitCode → GitHub → Gitee → GitLab
              ↓（全部失败）
         服务端直链回退
              ↓
      MD5 校验 → 原子替换 → 自动回滚
```

- **Docker 模式**：下载新版镜像 → 导入 → 重启容器
- **裸机模式**：下载 waf + source.enc → 校验 → 替换 → 重启进程
- 校验失败自动回滚到旧版本

---

## 🏗️ 架构

```
                        ┌─────────────────────────────┐
                        │         FoxWAF              │
  Client ──── HTTP ────▶│                             │──── Proxy ────▶ Upstream
  Request     HTTPS     │  ┌────────┐  ┌──────────┐  │                 Servers
                        │  │AC 引擎 │  │ 管理面板 │  │
                        │  │CC 防护 │  │ 实时监控 │  │◀── Admin
                        │  │反爬虫  │  │ 规则管理 │  │
                        │  │SSL/TLS │  │ 证书管理 │  │
                        │  │负载均衡│  │ 攻击日志 │  │
                        │  │缓存加速│  │ 流量统计 │  │
                        │  └────────┘  └──────────┘  │
                        └─────────────────────────────┘
```

### 请求处理流程

```
请求进入 → IP 黑白名单 → CC 频率检测 → User-Agent 检查
    → 反爬虫验证 → WAF 规则匹配 (AC 自动机)
    → [插件链] → 缓存检查 → 反向代理
    → 响应内容审计 → 返回客户端
```

---

## ⚙️ 配置

默认配置文件 `conf.yaml` 位于安装目录：

```yaml
Server:
    Addr: 0.0.0.0
    Port: 8088              # 管理面板端口
    HTTPS: false            # 管理面板 HTTPS
Database:
    DBName: waf.db          # SQLite 数据库文件名
secureentry: foxadmin       # 管理面板路径
username: fox               # 管理员用户名
password: fox               # 管理员密码（请修改）
Update:
    CheckIntervalMinutes: 10
```

> ⚠️ **安装后请立即修改默认密码**

更多配置（站点管理、规则调整、CC 防护参数等）通过管理面板操作。

---

## 📋 系统要求

| 项目 | 最低要求 | 推荐配置 |
|:---|:---|:---|
| 操作系统 | Linux (x86_64 / ARM64) | Debian 11+ / Ubuntu 20.04+ |
| 内存 | 256 MB | 512 MB+ |
| 磁盘 | 200 MB | 1 GB+ |
| Docker | 20.10+（Docker 模式） | 24.0+ |
| 网络端口 | 80, 443 | - |

---

## 📁 Release 文件说明

每个版本发布包含以下文件：

| 文件 | 说明 |
|:---|:---|
| `waf` | WAF 主程序（Linux 二进制） |
| `waf.md5` | 主程序 MD5 校验 |
| `source.enc` | 加密资源文件（规则、静态资源） |
| `source.enc.md5` | 资源文件 MD5 校验 |
| `foxwaf-image.tar.gz` | Docker 镜像（docker load 导入） |
| `foxwaf-image.tar.gz.md5` | 镜像 MD5 校验 |
| `docker-compose.yaml` | Docker Compose 配置 |
| `install.sh` | 安装脚本 |

---

## 📄 许可证

本项目基于 [Apache License 2.0](LICENSE) 开源。

---

<p align="center">
  <sub>Copyright © 2026 FoxWAF · All rights reserved</sub>
</p>
