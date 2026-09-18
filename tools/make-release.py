#!/usr/bin/env python3
"""本地打包 work2api 发布包。

用法（仓库根目录下）：
    python tools/make-release.py

产出：
    dist/release/work2api-<日期>-<短rev>[-dirty]-windows-amd64.zip
    dist/release/work2api-<日期>-<短rev>[-dirty]-windows-amd64/    （同内容的解包目录）

包内：work2api.exe / work2api-tray.exe / config.example.json / README.md /
      start-work2api.vbs / BUILD.txt / SHA256SUMS

设计取舍（都是踩过之后定的，别随手改）
--------------------------------------
1. **打到 dist/release/，不覆盖 dist/work2api.exe。**
   后者常被正在跑的服务占用；就地重编只会留下 .exe~ 备份，跑着的进程仍是旧镜像 ——
   于是"构建成功"和"服务是新版"会脱钩，最容易得出假结论。

2. **注入 -X main.version，同时保留默认开启的 VCS 戳。**
   版本号给人看，vcs.revision 给机器核；两个都要，别二选一。
   `go version -m <exe>` 能读回构建时的 commit，这是可复核的溯源，不是自述。

3. **用 Python zipfile 而不是 zip/7z。**
   本机没有 `zip`；7z 有，但还要一并算 SHA256，来回调外部命令不如一个脚本做完。

4. **校验和文件写进包里，并且脚本自己复算一遍。**
   写完就验，避免"生成了校验和但校验和本身是错的"这种低级事故。

5. **-trimpath 必须开。** 否则二进制里会带上本机绝对路径（泄露目录结构，也破坏可复现性）。

这个脚本只做打包，不做部署。部署走 tools/start-work2api.vbs（开机自启 → 托盘 → 服务）。
"""
import hashlib
import os
import shutil
import subprocess
import sys
import time
import zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT_DIR = os.path.join(ROOT, "dist", "release")

# (包路径, 产物名, 是否 GUI 子系统)
TARGETS = [
    ("./cmd/work2api", "work2api.exe", False),
    ("./cmd/work2api-tray", "work2api-tray.exe", True),
]

# 一并进包的附带文件
EXTRAS = ["config.example.json", "README.md", os.path.join("tools", "start-work2api.vbs")]


def run(cmd, env=None, check=True):
    e = os.environ.copy()
    e.update(env or {})
    p = subprocess.run(cmd, cwd=ROOT, env=e, capture_output=True, text=True)
    if check and p.returncode != 0:
        sys.exit("命令失败：%s\n%s%s" % (" ".join(cmd), p.stdout, p.stderr))
    return (p.stdout or "").strip()


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def main():
    rev = run(["git", "rev-parse", "--short=7", "HEAD"])
    rev_full = run(["git", "rev-parse", "HEAD"])
    dirty = run(["git", "status", "--porcelain"])
    if dirty:
        print("警告：工作区不干净，产物会标注 -dirty（该包不可作为正式发布）")
    ver = "%s-%s%s" % (time.strftime("%Y%m%d"), rev, "-dirty" if dirty else "")
    name = "work2api-%s-windows-amd64" % ver
    stage = os.path.join(OUT_DIR, name)

    if os.path.isdir(stage):
        shutil.rmtree(stage)
    os.makedirs(stage, exist_ok=True)

    gover = run(["go", "version"])

    for pkg, out, gui in TARGETS:
        print("构建 %s ..." % out)
        ld = "-s -w -X main.version=" + ver + (" -H=windowsgui" if gui else "")
        run(
            ["go", "build", "-trimpath", "-ldflags", ld, "-o", os.path.join(stage, out), pkg],
            env={"CGO_ENABLED": "0", "GOOS": "windows", "GOARCH": "amd64"},
        )

    for rel in EXTRAS:
        shutil.copy2(os.path.join(ROOT, rel), stage)

    build_txt = """work2api 本地构建包
========================================
版本标识 : {ver}
git rev  : {rev_full}
构建时间 : {now}
工作区   : {dirty}
Go 工具链: {gover}
目标平台 : windows/amd64  (CGO_ENABLED=0, -trimpath)

用法
----
1) 把本目录放到任意路径（config.json / auths / data 都是相对**工作目录**的，
   所以工作目录必须是本目录）
2) 首次运行自动生成 config.json（随机 api_key 写回该文件）
3) 启动服务：work2api.exe serve
4) 看版本  ：work2api.exe version
5) 登录账号：work2api.exe login -kind <workbuddy|trae> -region <cn|global>
6) 开机自启：把 start-work2api.vbs 复制到
   %APPDATA%\\Microsoft\\Windows\\Start Menu\\Programs\\Startup\\
   （只起托盘，托盘自己拉起服务；该文件必须是 CRLF 行尾，压缩包内已保证）

溯源与校验
----------
go version -m work2api.exe            # 读回 vcs.revision / vcs.modified
work2api.exe version                  # 打印版本标识
certutil -hashfile <zip> SHA256       # Windows 上校验压缩包
""".format(
        ver=ver,
        rev_full=rev_full,
        now=time.strftime("%Y-%m-%d %H:%M:%S %z"),
        dirty="有未提交改动（非正式发布）" if dirty else "干净",
        gover=gover,
    )
    with open(os.path.join(stage, "BUILD.txt"), "w", encoding="utf-8", newline="\r\n") as f:
        f.write(build_txt)

    # 校验和：先写 SHA256SUMS，再逐行复算一遍确认它自己没错
    names = sorted(n for n in os.listdir(stage) if os.path.isfile(os.path.join(stage, n)))
    sums_path = os.path.join(stage, "SHA256SUMS")
    with open(sums_path, "w", encoding="ascii", newline="\r\n") as f:
        for n in names:
            f.write("%s  %s\n" % (sha256(os.path.join(stage, n)), n))

    for ln in open(sums_path, encoding="ascii"):
        want, n = ln.strip().split("  ", 1)
        if sha256(os.path.join(stage, n)) != want:
            sys.exit("校验和不匹配：%s" % n)

    zip_path = os.path.join(OUT_DIR, name + ".zip")
    if os.path.exists(zip_path):
        os.remove(zip_path)
    with zipfile.ZipFile(zip_path, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        for n in sorted(os.listdir(stage)):
            z.write(os.path.join(stage, n), name + "/" + n)

    # 自检 1：压缩包可读
    bad = zipfile.ZipFile(zip_path).testzip()
    if bad is not None:
        sys.exit("压缩包损坏：%s" % bad)

    # 自检 2：版本号真的注进去了（不注入时是 dev，必须报出来）
    printed = run([os.path.join(stage, "work2api.exe"), "version"])
    if ver not in printed:
        sys.exit("版本注入失败，exe 自报：%r（期望包含 %s）" % (printed, ver))

    print()
    print("=== 包内容 ===")
    for n in sorted(os.listdir(stage)):
        print("  %-24s %10d 字节" % (n, os.path.getsize(os.path.join(stage, n))))
    print()
    print("=== 自检 ===")
    print("  exe 自报版本 : %s" % printed.splitlines()[0])
    print("  压缩包完整性 : OK")
    print("  校验和复算   : 全部匹配")
    print()
    print("=== 产物 ===")
    print("  %s" % zip_path)
    print("  大小   = %d 字节" % os.path.getsize(zip_path))
    print("  sha256 = %s" % sha256(zip_path))


if __name__ == "__main__":
    main()
