# TideSync 潮汐同步

定时自动同步局域网内**指定服务器**上**指定路径**下的所有文件。

一个可执行文件、两种角色：在有文件的机器上运行 `tidesync serve`（发布目录，只读），
在需要同步的机器上运行 `tidesync sync`（按计划拉取）。单个静态二进制，无任何运行时依赖。

```
┌──────────────────────────┐         HTTP(S)          ┌──────────────────────────┐
│  源服务器 tidesync serve │  ──────────────────────▶ │  目标机 tidesync sync    │
│  /srv/shared  (只读发布) │   清单 + 增量 + 校验      │  D:\Data 或 /srv/data    │
└──────────────────────────┘                          └──────────────────────────┘
        Windows / Linux / 树莓派 / NAS                     Windows / Linux / 树莓派
```

## 目录

- [特性](#特性)
- [支持的平台](#支持的平台)
- [快速开始](#快速开始)
- [定时同步](#定时同步)
- [安装为后台服务](#安装为后台服务)
- [命令行参考](#命令行参考)
- [配置文件](#配置文件)
- [工作原理](#工作原理)
- [安全建议](#安全建议)
- [故障排查](#故障排查)
- [从源码构建](#从源码构建)
- [项目结构](#项目结构)
- [验证情况](#验证情况)

## 特性

| 能力 | 说明 |
| --- | --- |
| 定时同步 | 守护进程按 `interval` 周期运行，或按 `schedule` 每天固定时刻运行；也可 `-once` 交给 cron / 计划任务 |
| 增量传输 | 只传变化的文件；用「大小 + 修改时间 + SHA-256」判断，重复运行几乎零开销 |
| 完整性校验 | 每个文件比对 SHA-256；校验失败自动重传，不会留下半个文件 |
| 原子写入 | 先写临时文件再改名替换，同步过程中读到的永远是完整文件（Windows 上同样成立） |
| 断点续传 | 中断后从断点继续（HTTP Range），已下载部分不会浪费 |
| 镜像模式 | `delete_extra` 打开后目标端与源端完全一致（含删除），默认关闭以免误删用户数据 |
| 冷启动加速 | 目标目录为空时自动用 tar.gz 打包流式传输，海量小文件在树莓派上也能快速完成 |
| 失败重试 | 网络错误指数退避重试；单个文件失败不中断整轮同步，退出码 `3` 便于监控告警 |
| 崩溃自愈 | 进程被强杀留下的半成品文件（`.tidesync-tmp-*`）会在后续运行中自动清理，不会越积越多 |
| 单实例保护 | 文件锁 + 心跳，避免上一轮还没跑完下一轮又开始；进程被强杀后锁会自动回收 |
| 安全 | 共享令牌（Bearer）、网段白名单、目录穿越防护、符号链接越界防护、可选 HTTPS |
| 可移植 | 只用 Go 标准库，`CGO_ENABLED=0` 静态编译，Windows / Linux / x86 / ARM 同一份源码 |
| 可观测 | 结构化日志、可选日志文件轮转、`check` 自检命令、`--dry-run` 预演 |

## 支持的平台

`dist/` 中已交叉编译好下列产物（静态链接，无 libc 依赖）：

| 目标 | 文件 | 典型设备 |
| --- | --- | --- |
| Linux x86-64 | `tidesync-<版本>-linux-amd64` | 普通 PC 服务器、虚拟机、群晖/威联通 x86 NAS |
| Linux ARM64 | `tidesync-<版本>-linux-arm64` | 树莓派 3/4/5（64 位系统）、ARM 服务器、香橙派 |
| Linux ARMv7 | `tidesync-<版本>-linux-armv7` | 树莓派 2/3/4（32 位系统）、多数 ARM 开发板 |
| Linux ARMv6 | `tidesync-<版本>-linux-armv6` | 树莓派 1 / Zero / Zero W |
| Linux i386 | `tidesync-<版本>-linux-386` | 老旧 32 位 x86 设备 |
| Windows x64 | `tidesync-<版本>-windows-amd64.exe` | Windows 10/11、Server 2016+ |
| Windows ARM64 | `tidesync-<版本>-windows-arm64.exe` | Surface Pro X、Windows on ARM |
| Windows 32 位 | `tidesync-<版本>-windows-386.exe` | 老旧 32 位 Windows |

预编译二进制可以直接从 [Releases](https://github.com/tbx709/TideSync/releases) 下载，每个平台一个文件，
不依赖 libc、也不需要安装 Go 或 Python：

```bash
# 树莓派 4（64 位系统）为例
curl -LO https://github.com/tbx709/TideSync/releases/download/v1.0.0/tidesync-1.0.0-linux-arm64
curl -LO https://github.com/tbx709/TideSync/releases/download/v1.0.0/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing      # Windows 用 certutil -hashfile <文件> SHA256
chmod +x tidesync-1.0.0-linux-arm64 && ./tidesync-1.0.0-linux-arm64 version
```

> 树莓派上直接用对应架构的二进制即可，无需安装 Go 或 Python：
> 直接拷贝或解压，`chmod +x tidesync`，然后 `./tidesync version` 验证。

## 快速开始

### 1. 源服务器（有文件的那台）

Linux：

```bash
# 生成一个令牌（也可自己指定）
TOKEN=$(./tidesync serve -gen-token)

# 发布 /srv/shared 到局域网
./tidesync serve --root /srv/shared --listen 0.0.0.0:8787 --token "$TOKEN"
```

Windows（PowerShell）：

```powershell
.\tidesync.exe serve --root "D:\Shared" --listen 0.0.0.0:8787 --token "你的令牌"
```

启动后会打印本机可供客户端使用的地址，例如 `http://192.168.1.10:8787`。

### 2. 目标机（需要同步的机器）

```bash
# 先自检：配置是否正确、能否连上、将要传多少数据
./tidesync check -config client.json

# 同步一次
./tidesync sync --server http://192.168.1.10:8787 --remote "" \
                --local /srv/data --token "$TOKEN" --once

# 常驻，每 5 分钟同步一次
./tidesync sync --server http://192.168.1.10:8787 --remote "" \
                --local /srv/data --token "$TOKEN" --interval 5m
```

`--remote` 是相对于源端 `root` 的子路径，`""` 表示整个目录。
生成带注释的配置文件：`./tidesync init client -out client.json`、
`./tidesync init server -out server.json`（Windows 上用 `-out C:\ProgramData\TideSync\client.json`）。

## 定时同步

三种方式，按场景挑一种：

1. **常驻守护进程**（推荐，跨平台一致）

   ```bash
   tidesync sync -config client.json          # 按 sync.interval 周期运行
   ```

   配置文件里也可以改成每天固定时刻：

   ```json
   "sync": { "schedule": ["02:30", "14:30"] }
   ```

2. **交给系统计划任务**（cron / Windows 任务计划程序），每次只跑一轮：

   ```bash
   tidesync sync -config client.json -once
   ```

   cron 示例（每 10 分钟）：`*/10 * * * * /usr/local/bin/tidesync sync -config /etc/tidesync/client.json -once`
   Windows 一条命令即可注册（管理员 PowerShell）：

   ```powershell
   tidesync.exe service install -config C:\ProgramData\TideSync\client.json -interval 10m
   ```

3. **安装为系统服务**（开机自启、崩溃自动重启）：见下一节。

`sync.jitter`（例如 `30s`）让多台目标机不要在同一秒同时冲击源服务器；
`sync.run_on_start` 控制守护进程启动时是否立即同步一次。

## 安装为后台服务

### Linux（systemd，含树莓派）

```bash
sudo install -m0755 dist/tidesync-*-linux-arm64 /usr/local/bin/tidesync
sudo mkdir -p /etc/tidesync
sudo ./tidesync init client -server 192.168.1.10:8787 -local /srv/data \
     -token "$TOKEN" -out /etc/tidesync/client.json
sudo /usr/local/bin/tidesync service install -config /etc/tidesync/client.json
sudo systemctl status tidesync          # 查看状态
journalctl -u tidesync -f               # 实时日志
```

`service install` 会写入 `/etc/systemd/system/tidesync.service` 并 `enable --now`。
非 root 用户会自动装成用户级服务（`systemctl --user`）。
不想用 systemd 也可以直接用 cron，或参考 `deploy/systemd/` 里的手工单元文件。

源端发布目录同理：`tidesync service install -role server -config /etc/tidesync/server.json`。

### Windows

**推荐：计划任务**（无需服务宿主，重启后自动运行）

```powershell
# 管理员 PowerShell
.\tidesync.exe service install -config C:\ProgramData\TideSync\client.json -interval 5m
.\tidesync.exe service status
```

等价的手工命令（脚本 `deploy\windows\install-service.ps1` 已封装并会检查管理员权限）：

```cmd
schtasks /Create /F /TN TideSync /SC MINUTE /MO 5 /RU SYSTEM /RL HIGHEST ^
  /TR "\"C:\TideSync\tidesync.exe\" sync -config \"C:\ProgramData\TideSync\client.json\" -once"
```

**备选：Windows 服务**

```powershell
.\tidesync.exe service install -config C:\ProgramData\TideSync\client.json -mode scm
```

这会用 `sc.exe` 注册一个常驻服务（内部走 Windows 服务控制管理器；若进程不是被服务管理器启动，
会自动退化到前台运行，因此 `tidesync.exe service run -config ...` 也可以直接在控制台调试）。

卸载：`.\tidesync.exe service uninstall`（或 `deploy\windows\uninstall-service.ps1`）。

## 命令行参考

```
tidesync <命令> [参数]

  serve     在源服务器上发布一个目录（只读 agent）
  sync      在目标机上执行同步（守护进程或 -once）
  check     校验配置文件、连通性与传输计划
  service   安装/卸载/查询后台服务（systemd、计划任务、Windows 服务）
  init      生成带注释的示例配置
  version   版本信息
```

常用参数：

| 命令 | 参数 | 说明 |
| --- | --- | --- |
| `serve` | `-root` | 要发布的目录 |
| | `-listen` | 监听地址，默认 `0.0.0.0:8787` |
| | `-token` | 客户端必须携带的共享令牌 |
| | `-gen-token` | 打印一个新的随机令牌 |
| | `-exclude` | 排除规则，可重复（如 `-exclude '*.tmp'`） |
| | `-allow-cidr` | 限制来源网段，可重复 |
| | `-hash-mode` | `auto` / `always` / `never` |
| | `-tls-cert` `-tls-key` | 启用 HTTPS |
| `sync` | `-server` | agent 地址，如 `http://192.168.1.10:8787` |
| | `-remote` | agent 根下的子路径，默认整个根 |
| | `-local` | 目标目录 |
| | `-interval` | 守护模式间隔，如 `5m`、`1h` |
| | `-schedule` | 固定时刻，如 `02:30,14:30` |
| | `-once` | 只跑一轮后退出（cron/计划任务用） |
| | `-delete-extra` | 镜像删除目标端多余文件 |
| | `-verify` | `sha256`（默认）/ `size-mtime` / `none` |
| | `-concurrency` | 并发下载数 |
| | `-exclude` | 目标端排除规则，可重复 |
| | `-max-file-size-mb` | 超过则跳过，0 为不限 |
| | `-bandwidth-limit-kbps` | 限速，0 为不限 |
| | `-dry-run` | 只报告将要做什么，不写磁盘 |
| | `-print-config` | 打印生效后的配置（排查用） |
| | `-log-level` `-log-file` | 日志级别与文件 |

任何命令都支持 `-config <文件>`，命令行参数会覆盖配置文件中的同名项。
退出码：`0` 成功，`1` 失败，`2` 用法错误，`3` 部分文件失败（可用于监控）。

## 配置文件

JSON 格式，允许 `//` 与 `/* */` 注释、尾随逗号，以及 `${环境变量}` 替换（推荐把令牌放环境变量，
避免明文写在文件里）。完整示例见 [`configs/server.example.json`](configs/server.example.json) 与
[`configs/client.example.json`](configs/client.example.json)。

### 服务端（agent）

| 键 | 默认值 | 说明 |
| --- | --- | --- |
| `listen` | `0.0.0.0:8787` | 监听地址 |
| `root` | 必填 | 被发布的目录 |
| `token` | 空 | 共享令牌；**为空时局域网内任何人都能读取发布目录** |
| `allow_cidrs` | 空 | 来源网段/IP 白名单 |
| `tls_cert` / `tls_key` | 空 | 同时设置则启用 HTTPS |
| `hash_mode` | `auto` | `auto` 仅对 ≤ `hash_max_size_mb` 的文件算摘要，`always` 全部算，`never` 不算 |
| `hash_cache` | `true` | 摘要缓存；`false` 时每次扫描都重读全部文件（最准确、最慢） |
| `hash_grace` | `2s` | 刚被修改过的文件总是重新计算摘要，避免同一时间戳刻度内的改动被漏掉 |
| `manifest_ttl` | `0s` | 清单复用时间；`0` 表示每次请求都重新扫描当前磁盘状态 |
| `follow_symlinks` | `false` | 是否发布符号链接指向的内容（越界的一律拒绝） |
| `exclude` | 空 | `.gitignore` 风格排除规则 |
| `state_dir` | 见下 | 摘要缓存位置（Linux root 为 `/var/lib/tidesync`） |
| `max_concurrent` | `4` | 并发传输上限 |
| `rate_limit_kbps` | `0` | 单连接限速，0 不限 |
| `archive_enabled` | `true` | 是否开放 tar.gz 打包接口 |
| `manifest_max_entries` | `200000` | 清单条数上限，超出则报错而不是同步半棵树 |

### 客户端

| 键 | 默认值 | 说明 |
| --- | --- | --- |
| `server.url` | 必填 | agent 地址（可省略 `http://`） |
| `server.token` | 空 | 共享令牌，支持 `${ENV}` |
| `server.remote_path` | `""` | agent 根下的子路径 |
| `server.timeout` | `30s` | 请求超时 |
| `local.dir` | 必填 | 目标目录（自动创建） |
| `local.delete_extra` | `false` | 镜像删除；**会删除目标端多余文件** |
| `local.preserve_mtime` | `true` | 保留源端修改时间（增量判断依赖它） |
| `local.preserve_perms` | `false` | 同步 Unix 权限位（Windows 忽略） |
| `local.create_dirs` | `true` | 重建源端目录（含空目录） |
| `local.create_symlinks` | `false` | 重建符号链接（Windows 需开发者模式） |
| `sync.interval` | `5m` | 守护模式间隔 |
| `sync.schedule` | 空 | 每天固定时刻数组，设置后忽略 `interval` |
| `sync.jitter` | `0s` | 每次运行的随机抖动 |
| `sync.concurrency` | `4` | 并发下载数 |
| `sync.retries` | `5` | 每个文件的失败重试次数 |
| `sync.retry_backoff` | `2s` | 首次重试等待，之后指数增长（上限 30s） |
| `sync.run_on_start` | `true` | 启动时立即同步一次 |
| `sync.full_scan_every` | `0` | 每隔多久强制做一次全量摘要比对 |
| `sync.bulk_threshold_mb` / `_files` | `512` / `2000` | 冷启动超过阈值时改用 tar.gz 打包传输 |
| `sync.max_file_size_mb` | `0` | 超过则跳过 |
| `sync.bandwidth_limit_kbps` | `0` | 下载限速 |
| `sync.stop_on_error` | `false` | 首个文件失败即中止整轮 |
| `verify` | `sha256` | `sha256` / `size-mtime` / `none` |
| `exclude` | 空 | 目标端排除规则 |
| `state_file` / `lock_file` | 见下 | 同步日志（journal）与锁文件位置 |
| `logging.*` | `info` | 级别、文件、单文件大小、保留个数、JSON 格式 |

状态文件默认位置：Linux root 为 `/var/lib/tidesync/`，普通用户为 `~/.local/state/tidesync/`，
Windows 为 `%ProgramData%\TideSync\`；若该目录不可写（容器、无家目录的服务账户等），
会自动退到 `<local.dir>/.tidesync/` 并在日志中说明（镜像模式不会删除这个目录）。

## 工作原理

1. 客户端向 agent 请求 `/api/v1/manifest`，得到整棵子树的清单（路径、类型、大小、修改时间、SHA-256）。
2. 与本地文件和 journal 比对，生成计划：需要下载的文件、需要创建的目录、目标端多余的文件。
3. 目标目录为空且文件数/体积超过阈值时，走 `/api/v1/archive` 的 tar.gz 流；否则按并发数逐个下载
   `/api/v1/file`（支持 Range 断点续传）。
4. 每个文件写入同目录的临时文件 → `fsync` → 校验 SHA-256 → 设置时间戳/权限 → 原子改名替换。
   因此任何时刻读到的目标文件都是完整的，断电或断网也不会留下半个文件。
5. 更新 journal（原子写入），记录本轮结果；`delete_extra` 打开时删除目标端多余文件并清理空目录。
   同一趟遍历还会清掉超过 24 小时的半成品文件（上次运行被强杀留下的），正在传输的文件不会被动到。

判断“是否需要下载”的顺序（`verify: sha256` 时）：

| 情况 | 处理 |
| --- | --- |
| 本地不存在 | 下载 |
| 大小不同 | 下载 |
| journal 记录与源端摘要一致且时间戳相同 | 跳过（最省，不读盘） |
| 时间戳相同但源端摘要与 journal 不同 | 说明源端内容变了，重新比对/下载 |
| 摘要相同、只有时间戳不同 | 跳过并校正本地时间戳 |
| 摘要不同 | 下载 |

已知边界：agent 的摘要缓存按「大小 + 修改时间」命中。若文件被改写后大小和修改时间都保持不变
（例如人为回拨时间戳），任何基于元数据的工具都可能漏判；此时把 agent 的 `hash_cache` 设为
`false`（或 `hash_mode: always`）即可做到每次扫描都是权威结果。刚被修改过的文件（`hash_grace` 内）
总是重新计算摘要，因此常见的“同秒内同长度改写”不会漏。

## 安全建议

- **一定设置 `token`**：不设令牌时局域网内任何人都能读取发布目录（agent 启动时会告警）。
- 用 `allow_cidrs` 把访问限制到你的网段；只发布确实需要的目录。
- 跨网段或不可信网络请用 `tls_cert` / `tls_key` 启用 HTTPS，客户端配 `ca_cert`。
- 目录穿越、URL 编码穿越、UNC/盘符路径、越界符号链接都会被拒绝（有对应测试）。
- agent 永远只读：没有任何上传、写入或执行接口。

### 放行防火墙端口

源服务器需要允许目标机访问 agent 端口（默认 8787/TCP）：

```powershell
# Windows（管理员 PowerShell）
netsh advfirewall firewall add rule name="TideSync agent" dir=in action=allow protocol=TCP localport=8787
# 更严格：只允许本网段
netsh advfirewall firewall add rule name="TideSync agent" dir=in action=allow protocol=TCP ^
  localport=8787 remoteip=192.168.1.0/24
```

```bash
# Linux: ufw
sudo ufw allow from 192.168.1.0/24 to any port 8787 proto tcp
# Linux: firewalld
sudo firewall-cmd --permanent --add-port=8787/tcp && sudo firewall-cmd --reload
# 树莓派常见的 iptables/nftables 同理
```

放行后可用 `curl http://<源服务器IP>:8787/healthz` 验证（返回 `ok` 即通）。

## 故障排查

先跑自检，它会逐项给出结论：

```bash
tidesync check -config client.json
```

```
TideSync check: client.json (client configuration)

  [OK  ] configuration    client config, http://192.168.1.10:8787 -> /srv/data (remote path "")
  [OK  ] destination      /srv/data is writable
  [OK  ] state directory  /var/lib/tidesync/state-4f9ccce348.json
  [OK  ] agent            reachable, 412 file(s), 35.7 MiB on the agent
  [OK  ] plan             12 to download (3.1 MiB), 400 already up to date, 0 local-only, 0 too large, 0 skipped
  [OK  ] free space       52.3 GiB free, 3.1 MiB needed

all checks passed
```

| 现象 | 原因与处理 |
| --- | --- |
| `agent ... is unreachable` | 源端没启动、防火墙拦截、地址/端口不对；源端 `curl http://IP:8787/healthz` 应返回 ok |
| `invalid or missing token` | 两端令牌不一致；Windows 上注意 `.json` 里的 `${ENV}` 是否已设置 |
| `another sync is already running` | 上一轮尚未结束；属正常保护。确认没有卡死进程后可删除锁文件 |
| 大量文件每次都重传 | 目标文件系统不支持保存修改时间（如某些 FAT/exFAT 挂载），改用 `verify: size-mtime` 或 `none` |
| 某个大文件一直失败 | 看日志里的具体错误；短视频/大文件可用 `-max-file-size-mb` 先跳过 |
| 日志太大 | 配置 `logging.max_size_mb` / `max_files`，自动轮转 |
| Windows 服务起不来 | 用计划任务模式（`-mode task`）；服务模式可用 `tidesync service run -config ...` 前台调试 |

日志级别调到 `debug` 可以看到每个文件的判定原因（`-log-level debug`）。

## 从源码构建

只需要 Go 1.22 以上，**没有任何第三方依赖**（纯标准库），可离线构建：

```bash
go build -o tidesync .                    # 当前平台
./scripts/build-all.sh                    # 交叉编译全部平台到 dist/
TARGETS="linux/arm64 windows/amd64" ./scripts/build-all.sh   # 只编指定平台
VERSION=1.0.0 ./scripts/build-all.sh      # 带版本号（会写进二进制）
./scripts/release.sh                      # 完整发布检查（格式、vet、测试、编译、产物核验）
./scripts/e2e-test.sh                     # 端到端验证（真实 agent + 真实客户端）
./scripts/arm-emulation-test.sh           # ARM 仿真验证（需要 QEMU 用户态模拟器）
go test ./...                             # 单元与集成测试
make help                                 # 常用目标
```

`arm-emulation-test.sh` 需要 `qemu-aarch64-static` / `qemu-arm-static`（Debian/Ubuntu 上无需 root 即可取得）：

```bash
curl -O http://mirrors.aliyun.com/ubuntu/pool/universe/q/qemu/qemu-user-static_8.2.2+ds-0ubuntu1_amd64.deb
dpkg-deb -x qemu-user-static_*.deb ./qemu
QEMU_DIR=./qemu/usr/bin ./scripts/arm-emulation-test.sh
```

交叉编译不需要额外工具链：`CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build`。

发布新版本（打 tag、建 Release、上传各平台二进制）：

```bash
VERSION=1.1.0 ./scripts/build-all.sh
git tag -a v1.1.0 -m "TideSync 1.1.0" && git push origin v1.1.0
GITHUB_TOKEN=<有 repo 权限的令牌> ./scripts/publish-release.sh 1.1.0 <你的用户名>/TideSync
```

## 项目结构

```
main.go                 命令分发、版本信息
cmd_serve.go            tidesync serve
cmd_sync.go             tidesync sync（守护进程与调度）
cmd_check.go            tidesync check（自检）
cmd_service.go          tidesync service
cmd_init.go             tidesync init（示例配置）
internal/proto/         协议类型、路径安全、排除规则匹配
internal/config/        配置解析（注释、时长、校验、默认值）
internal/server/        agent：扫描、摘要缓存、HTTP 接口、打包流
internal/client/        agent 客户端、同步引擎、journal、文件锁
internal/fsx/           原子写、目录清理、磁盘空间等跨平台文件工具
internal/logx/          分级日志与按大小轮转
internal/service/       systemd / Windows 计划任务 / Windows 服务
configs/                带注释的配置示例
deploy/                 systemd 单元、Windows 安装脚本
scripts/                构建、发布、端到端测试脚本
docs/PROTOCOL.md        HTTP 协议说明
```

## 验证情况

所有结论都可用仓库自带脚本复现，原始输出保存在
[`docs/verification/`](docs/verification/)：

```bash
go test ./...                      # 单元与集成测试
go test -race ./...                # 竞态检测（需要 cgo/gcc）
./scripts/release.sh               # gofmt + 6 平台 vet + 测试 + 交叉编译 + 产物检查
./scripts/e2e-test.sh              # 44 项端到端检查（真实 agent + 真实客户端）
./scripts/arm-emulation-test.sh    # 18 项 ARM 仿真检查（ARM64 与 ARMv7 全流程）
```

当前状态：`release.sh` 全部通过，`e2e-test.sh` 44/44 通过，`arm-emulation-test.sh` 18/18 通过。

`./scripts/e2e-test.sh` 覆盖 44 项检查，全部通过：

- 首次同步后目标树与源树逐字节一致（含中文文件名、空格文件名、2 MiB 二进制文件、空目录）
- 第二次运行 `downloaded=0`（增量生效）；源端改/增/删后立即被识别
- 本地文件被改坏或截断后能自动修复；修复后收敛（无重复传输）
- 镜像模式下删除多余文件并保留空目录，同时保留 journal 自身
- 冷启动走 tar.gz 打包通道且结果一致
- 未授权/错误令牌/目录穿越（含 URL 编码）被拒绝
- 源端停机时以非零退出码失败并给出明确原因；恢复后自动继续
- 崩溃遗留的锁会被自动回收；守护进程按间隔自动同步并能被 SIGTERM 优雅停止
- 单元与集成测试覆盖路径安全、排除规则、配置解析、原子写、日志轮转、重试、断点续传、
  协议版本不匹配、清单截断保护等，`go test ./...` 全绿（含 `-race`）

ARM 平台不止“编译通过”，还用 QEMU 用户态仿真**真实运行**了 ARM 版本：ARM64（树莓派 3/4/5 64 位系统）
与 ARMv7（树莓派 2/3/4 32 位系统）各跑完整流程——ARM 版 agent 发布目录、ARM 版客户端同步，
18 项检查全部通过（含 1 MiB 二进制文件、中文文件名、增量、排除规则）。x86-64 上原生运行 `linux/386`
与 `linux/amd64` 亦通过。

平台方面：8 个目标平台的二进制均已交叉编译产出并通过 `file` 检查（ELF x86-64 / ARM aarch64 /
ARM EABI5 / i386，PE32+ x86-64 / Aarch64 / PE32），全部为静态链接（无动态依赖），
六个平台的 `go vet` 均通过。源码不使用 cgo，也不依赖任何平台专有 API，因此在树莓派等 ARM 设备上
与 x86 上运行的是同一份逻辑。

Windows 与 systemd 的说明：Windows 可执行文件为交叉编译产物，已通过 `go vet` 与 `file` 检查
（PE32+ x86-64 / Aarch64），但本环境没有 Windows 或 Wine，无法实际执行；相关代码路径（`schtasks.exe`
计划任务、`advapi32.dll` 服务控制）都带有前台运行回退，即使服务管理器不可用也不会静默失败。
systemd 单元文件生成已验证，但容器内没有运行中的 systemd，`systemctl enable --now` 未能实测，
此时命令会明确打印需要手工执行的命令。
