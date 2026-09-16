#!/usr/bin/env bash
# Publishes a GitHub release that carries the prebuilt binaries, so users can
# download the right file for their platform instead of installing Go.
#
#   GITHUB_TOKEN=ghp_xxx ./scripts/publish-release.sh 1.0.0 tbx709/TideSync
#
# The token needs the "repo" scope (classic) or "Contents: read and write"
# (fine grained). Binaries are taken from dist/, so run ./scripts/build-all.sh
# first. Behind a firewall, point curl at your proxy with the usual
# https_proxy / all_proxy environment variables.
set -euo pipefail

VERSION="${1:-1.0.0}"
REPO="${2:-tbx709/TideSync}"
TAG="v$VERSION"
TOKEN="${GITHUB_TOKEN:-}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT_DIR/dist"

if [[ -z "$TOKEN" ]]; then
  echo "GITHUB_TOKEN is not set. Create a token with the 'repo' scope:" >&2
  echo "  https://github.com/settings/tokens" >&2
  exit 2
fi

shopt -s nullglob
assets=("$DIST"/tidesync-"$VERSION"-* "$DIST"/SHA256SUMS "$DIST"/MANIFEST.txt)
if [[ ${#assets[@]} -eq 0 ]]; then
  echo "no binaries for version $VERSION in $DIST; run ./scripts/build-all.sh first" >&2
  exit 1
fi

api="https://api.github.com/repos/$REPO"
uploads="https://uploads.github.com/repos/$REPO"
auth=(-H "Authorization: Bearer $TOKEN" -H "Accept: application/vnd.github+json")
curl_common=(--silent --show-error --fail-with-body --retry 3 --retry-delay 2)

echo "repository : $REPO"
echo "tag        : $TAG"
echo "assets     : ${#assets[@]}"
echo

# The tag usually exists already (git push origin "v$VERSION"); the API creates
# it from the default branch when it does not.
payload=$(python3 - "$VERSION" "$TAG" "$REPO" <<'PY'
import json, sys
version, tag, repo = sys.argv[1], sys.argv[2], sys.argv[3]
body = f"""TideSync {version} —— 局域网内定时自动同步指定服务器上指定路径下的所有文件。

一个静态二进制、两种角色：

* `tidesync serve` 跑在有文件的那台服务器上，把指定目录只读发布到局域网
* `tidesync sync` 跑在需要同步的机器上，按计划拉取

## 下载

| 平台 | 文件 | 适用设备 |
| --- | --- | --- |
| Linux x86-64 | `tidesync-{version}-linux-amd64` | PC 服务器、x86 NAS、虚拟机 |
| Linux ARM64 | `tidesync-{version}-linux-arm64` | 树莓派 3/4/5（64 位系统）、ARM 服务器 |
| Linux ARMv7 | `tidesync-{version}-linux-armv7` | 树莓派 2/3/4（32 位系统） |
| Linux ARMv6 | `tidesync-{version}-linux-armv6` | 树莓派 1 / Zero / Zero W |
| Linux i386 | `tidesync-{version}-linux-386` | 老旧 32 位 x86 |
| Windows x64 | `tidesync-{version}-windows-amd64.exe` | Windows 10/11、Server |
| Windows ARM64 | `tidesync-{version}-windows-arm64.exe` | Windows on ARM |
| Windows 32 位 | `tidesync-{version}-windows-386.exe` | 老旧 32 位 Windows |

全部为静态链接（不依赖 libc），无需安装 Go、Python 或任何运行库。
校验下载：`sha256sum -c SHA256SUMS`（Windows 用 `certutil -hashfile <文件> SHA256`）。

## 快速开始

源服务器（有文件的那台）：

```bash
chmod +x tidesync-{version}-linux-arm64
./tidesync-{version}-linux-arm64 serve --root /srv/shared --listen 0.0.0.0:8787 --token 你的令牌
```

目标机（需要同步的机器）：

```bash
./tidesync-{version}-linux-amd64 sync --server http://192.168.1.10:8787 \\
    --remote "" --local /srv/data --token 你的令牌 --interval 5m
```

Windows（PowerShell）：

```powershell
.\\tidesync-{version}-windows-amd64.exe serve --root "D:\\Shared" --listen 0.0.0.0:8787 --token 你的令牌
.\\tidesync-{version}-windows-amd64.exe service install -config C:\\ProgramData\\TideSync\\client.json -interval 5m
```

完整说明（配置文件、systemd 与计划任务、镜像模式、安全、故障排查）见仓库
[README](https://github.com/{repo}/blob/main/README.md)，协议见
[docs/PROTOCOL.md](https://github.com/{repo}/blob/main/docs/PROTOCOL.md)。

## 验证

* `scripts/release.sh`：gofmt、6 个平台 `go vet`、全部测试、8 平台交叉编译与产物核验
* `scripts/e2e-test.sh`：44 项端到端检查（真实 agent + 真实客户端）
* `scripts/arm-emulation-test.sh`：18 项 ARM 仿真检查（ARM64 与 ARMv7 全流程）
* `go test -race ./...`：竞态检测通过

原始日志见 [docs/verification](https://github.com/{repo}/tree/main/docs/verification)。
"""
print(json.dumps({
    "tag_name": tag,
    "name": f"TideSync {version}",
    "body": body,
    "draft": False,
    "prerelease": False,
}))
PY
)

echo "creating the release..."
release=$(curl "${curl_common[@]}" -X POST "${auth[@]}" \
  -H "Content-Type: application/json" -d "$payload" "$api/releases")
release_id=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' <<< "$release")
html=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["html_url"])' <<< "$release")
echo "release #$release_id: $html"
echo

# GitHub rejects an asset that already exists, so clear previous uploads first.
existing=$(curl "${curl_common[@]}" "${auth[@]}" "$api/releases/$release_id/assets")
python3 -c 'import json,sys
for a in json.load(sys.stdin):
    print(a["id"], a["name"])' <<< "$existing" | while read -r id name; do
  echo "  removing previous asset $name"
  curl "${curl_common[@]}" -X DELETE "${auth[@]}" "$api/releases/assets/$id" >/dev/null
done

for file in "${assets[@]}"; do
  name="$(basename "$file")"
  size=$(stat -c %s "$file" 2>/dev/null || stat -f %z "$file")
  printf 'uploading %-38s %6s KiB ... ' "$name" "$((size / 1024))"
  curl "${curl_common[@]}" -X POST "${auth[@]}" \
    -H "Content-Type: application/octet-stream" \
    --data-binary "@$file" \
    "$uploads/releases/$release_id/assets?name=$name" >/dev/null
  echo ok
done

echo
echo "published: $html"
summary=$(curl "${curl_common[@]}" "${auth[@]}" "$api/releases/$release_id")
python3 - "$summary" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
print("tag        :", d["tag_name"])
print("assets     :", len(d["assets"]))
for a in sorted(d["assets"], key=lambda x: x["name"]):
    print("  %-40s %6.1f MiB  %s" % (a["name"], a["size"] / 1048576, a["browser_download_url"]))
PY
