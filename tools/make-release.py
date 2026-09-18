#!/usr/bin/env python3
"""本地打包 work2api 发布包。

用法（仓库根目录下）：
    python tools/make-release.py

产出：
    dist/release/work2api-<日期>-<短rev>[-dirty]-windows-amd64.zip
    dist/release/work2api-<日期>-<短rev>[-dirty]-windows-amd64/    （同内容的解包目录）

包内布局：
    bin/work2api.exe            服务本体
    bin/work2api-tray.exe       托盘（唯一入口）
    config.example.json         配置样例
    README.md
    start-work2api.vbs          开机自启（**按自身所在目录定位**，不写死路径）
    BUILD.txt
    SHA256SUMS

设计取舍（都是踩过之后定的，别随手改）
--------------------------------------
1. **exe 必须放在包内的 bin/ 子目录，不能直接摊在包根。**
   托盘的 servicePaths() 规则是「工作目录 = exe 所在目录的上一级」——
   这条规则在 dev 树里是对的（exe 在 dist/，上一级就是仓库根，config.json
   和 auths/ 都在那儿）。发布包早先让 exe 直接躺在包根，于是"上一级"跑到了
   **包外面**：实测解到 dist/release/_pkgtest/ 后，config.json 被写到了
   _pkgtest/config.json —— 包外；而 serve.log 又在包内，两者不一致。
   现在让包遵守和 dev 树相同的约定：exe 下沉一层，包根即工作目录。
   脚本末尾有断言盯着这条（包根不得出现任何 .exe）。

2. **包里那个 vbs 是现生成的，不是从 tools/ 复制的。**
   tools/start-work2api.vbs 里写死了本机的绝对路径（E:\\AI\\...\\dist），
   那是给这台机器的「启动」文件夹用的部署件；原样打进包，在任何别的机器上
   都是废的，而且指向的还是旧的扁平布局。这里现生成一份按 ScriptFullName
   自定位的版本。

3. **打到 dist/release/，不覆盖 dist/work2api.exe。**
   后者常被正在跑的服务占用；就地重编只会留下 .exe~ 备份，跑着的进程仍是旧镜像 ——
   于是"构建成功"和"服务是新版"会脱钩，最容易得出假结论。

4. **注入 -X main.version，同时保留默认开启的 VCS 戳。**
   版本号给人看，vcs.revision 给机器核；两个都要，别二选一。
   `go version -m <exe>` 能读回构建时的 commit，这是可复核的溯源，不是自述。

5. **用 Python zipfile 而不是 zip/7z。**
   本机没有 `zip`；7z 有，但还要一并算 SHA256，来回调外部命令不如一个脚本做完。

6. **校验和文件写进包里，并且脚本自己复算一遍。**
   写完就验，避免"生成了校验和但校验和本身是错的"这种低级事故。

7. **-trimpath 必须开。** 否则二进制里会带上本机绝对路径（泄露目录结构，也破坏可复现性）。

这个脚本只做打包，不做部署。
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

# exe 在包内的子目录。见文件头 1：改了它，托盘算出的工作目录就错了。
BIN_SUBDIR = "bin"

# (包路径, 产物名, 是否 GUI 子系统)
TARGETS = [
    ("./cmd/work2api", "work2api.exe", False),
    ("./cmd/work2api-tray", "work2api-tray.exe", True),
]

# 一并进包的附带文件（vbs 是现生成的，不在这里）
EXTRAS = ["config.example.json", "README.md"]

# 包内开机自启脚本。自定位：以 ScriptFullName 所在目录为包根，找 bin\work2api-tray.exe。
#
# 为什么不直接复制 tools/start-work2api.vbs：那份写死了本机绝对路径。
#
# 可执行行必须**全 ASCII** —— WScript 默认按 ANSI 读 .vbs，UTF-8 的中文会变乱码。
# （虽然乱码只出现在字符串里不影响解析，但这类文件一旦有非 ASCII，编码就成了隐患。）
# 另外这份文件必须 **CRLF**：裸 LF 下 WScript 是静默失效，不报错也不启动。
VBS = (
    "' work2api —— 开机静默启动托盘（按本文件自身所在目录定位，不写死任何路径）\r\n"
    "'\r\n"
    "' 开机自启用**快捷方式**，不要直接复制本文件：\r\n"
    "'   右键本文件 -> 创建快捷方式 -> 把快捷方式拖进「启动」文件夹\r\n"
    "'   （Win+R 输入 shell:startup 可打开该文件夹）\r\n"
    "'\r\n"
    "' 直接复制进来的话，本文件的\"所在目录\"就变成「启动」文件夹了，\r\n"
    "' 那里没有 bin\\work2api-tray.exe —— 下面会弹框报错，而不是静默不启动。\r\n"
    "'\r\n"
    "' 托盘有单实例互斥体，重复启动会自己退出，所以这里不用查进程。\r\n"
    "Option Explicit\r\n"
    "\r\n"
    "Dim fso, sh, here, tray\r\n"
    "Set fso = CreateObject(\"Scripting.FileSystemObject\")\r\n"
    "Set sh  = CreateObject(\"WScript.Shell\")\r\n"
    "here = fso.GetParentFolderName(WScript.ScriptFullName)\r\n"
    "tray = fso.BuildPath(fso.BuildPath(here, \"bin\"), \"work2api-tray.exe\")\r\n"
    "\r\n"
    "If Not fso.FileExists(tray) Then\r\n"
    "  MsgBox \"Cannot find:\" & vbCrLf & tray & vbCrLf & vbCrLf & _\r\n"
    "         \"Put a SHORTCUT to this file in the Startup folder \" & _\r\n"
    "         \"(right-click -> Create shortcut). Do not copy this file there.\", _\r\n"
    "         16, \"work2api\"\r\n"
    "  WScript.Quit 1\r\n"
    "End If\r\n"
    "\r\n"
    "sh.CurrentDirectory = here\r\n"
    "sh.Run \"\"\"\" & tray & \"\"\"\", 0, False\r\n"
)


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


def rel_files(stage):
    """返回 (包内相对路径[正斜杠], 绝对路径) 列表，排除 SHA256SUMS 自身。"""
    out = []
    for root, _dirs, files in os.walk(stage):
        for f in files:
            p = os.path.join(root, f)
            rel = os.path.relpath(p, stage).replace(os.sep, "/")
            if rel == "SHA256SUMS":
                continue
            out.append((rel, p))
    return sorted(out)


def main():
    rev = run(["git", "rev-parse", "--short=7", "HEAD"])
    rev_full = run(["git", "rev-parse", "HEAD"])
    dirty = run(["git", "status", "--porcelain"])
    if dirty:
        print("警告：工作区不干净，产物会标注 -dirty（该包不可作为正式发布）")
    ver = "%s-%s%s" % (time.strftime("%Y%m%d"), rev, "-dirty" if dirty else "")
    name = "work2api-%s-windows-amd64" % ver
    stage = os.path.join(OUT_DIR, name)
    bin_dir = os.path.join(stage, BIN_SUBDIR)

    if os.path.isdir(stage):
        shutil.rmtree(stage)
    os.makedirs(bin_dir, exist_ok=True)

    gover = run(["go", "version"])

    for pkg, out, gui in TARGETS:
        print("构建 %s ..." % out)
        ld = "-s -w -X main.version=" + ver + (" -H=windowsgui" if gui else "")
        run(
            ["go", "build", "-trimpath", "-ldflags", ld, "-o", os.path.join(bin_dir, out), pkg],
            env={"CGO_ENABLED": "0", "GOOS": "windows", "GOARCH": "amd64"},
        )

    for rel in EXTRAS:
        shutil.copy2(os.path.join(ROOT, rel), stage)

    # 编码用 UTF-8（无 BOM）—— 与仓库里那份一致：中文只出现在注释里，
    # 可执行行全 ASCII。这个约束下面会逐行验，不靠"应该没问题"。
    with open(os.path.join(stage, "start-work2api.vbs"), "w",
              encoding="utf-8", newline="") as f:
        f.write(VBS)

    build_txt = """work2api 本地构建包
========================================
版本标识 : {ver}
git rev  : {rev_full}
构建时间 : {now}
工作区   : {dirty}
Go 工具链: {gover}
目标平台 : windows/amd64  (CGO_ENABLED=0, -trimpath)

包内布局
--------
bin\\work2api.exe        反代服务本体
bin\\work2api-tray.exe   系统托盘（唯一入口，会自己拉起服务）
config.example.json    配置样例
README.md
start-work2api.vbs     开机自启（给它的**快捷方式**放进「启动」文件夹）
BUILD.txt / SHA256SUMS

用法
----
1) 把整个目录放到任意路径（config.json / auths / data 都是相对**工作目录**的，
   托盘会把工作目录设成包根目录 —— 所以 exe 在 bin\\ 里，别把 exe 单独挪出去）
2) 首次运行自动生成 config.json 于**包根目录**（随机 api_key 写回该文件）
3) 起托盘（推荐）：bin\\work2api-tray.exe
   —— 它会拉起服务，并在包根目录生成 config.json / auths / data
      日志：bin\\tray.log、bin\\serve.log
4) 只起服务（手动）：在**包根目录**下执行  bin\\work2api.exe serve
   （必须在包根执行：config.json / auths 都按工作目录解析）
5) 看版本：bin\\work2api.exe version
6) 登录账号：bin\\work2api.exe login -kind <workbuddy|trae> -region <cn|global>
7) 开机自启：右键 start-work2api.vbs -> 创建快捷方式，
   把**快捷方式**拖进「启动」文件夹（Win+R 输入 shell:startup）。
   不要直接复制该 vbs —— 它按自身所在目录定位 bin\\work2api-tray.exe。

溯源与校验
----------
go version -m bin\\work2api.exe       # 读回 vcs.revision / vcs.modified
bin\\work2api.exe version             # 打印版本标识
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

    # ── 布局断言 ────────────────────────────────────────────────────────
    # 包根出现 .exe 就说明 TARGETS 的落点被改回了扁平布局，托盘算出的工作目录
    # 会跑到包外面（见文件头 1）。这是条静默错，必须在打包时就拦住。
    stray = [f for f in os.listdir(stage) if f.lower().endswith(".exe")]
    if stray:
        sys.exit("包根不该出现 exe（%s）—— exe 必须在 %s\\ 下，否则工作目录会跑到包外"
                 % (", ".join(stray), BIN_SUBDIR))
    for _pkg, out, _gui in TARGETS:
        if not os.path.isfile(os.path.join(bin_dir, out)):
            sys.exit("缺少 %s\\%s" % (BIN_SUBDIR, out))

    # 校验和：先写 SHA256SUMS，再逐行复算一遍确认它自己没错
    files = rel_files(stage)
    sums_path = os.path.join(stage, "SHA256SUMS")
    with open(sums_path, "w", encoding="ascii", newline="\r\n") as f:
        for rel, p in files:
            f.write("%s  %s\n" % (sha256(p), rel))

    for ln in open(sums_path, encoding="ascii"):
        want, rel = ln.strip().split("  ", 1)
        if sha256(os.path.join(stage, rel.replace("/", os.sep))) != want:
            sys.exit("校验和不匹配：%s" % rel)

    # vbs 自检：行尾必须纯 CRLF（裸 LF 下 WScript 静默失效），
    # 且**可执行行**（非空、非注释）必须全 ASCII —— WScript 按 ANSI 读文件，
    # UTF-8 的中文落到可执行行上就是乱码。注释里的中文没关系。
    vb = open(os.path.join(stage, "start-work2api.vbs"), "rb").read()
    crlf = vb.count(b"\r\n")
    lone = vb.count(b"\n") - crlf
    if lone or not crlf:
        sys.exit("start-work2api.vbs 行尾不对：CRLF=%d 裸LF=%d（必须全 CRLF）" % (crlf, lone))
    bad_lines = []
    for i, ln in enumerate(vb.split(b"\r\n"), 1):
        s = ln.strip()
        if not s or s.startswith(b"'"):
            continue
        if any(b > 127 for b in ln):
            bad_lines.append(i)
    if bad_lines:
        sys.exit("start-work2api.vbs 第 %s 行是可执行行却含非 ASCII 字节（会乱码）"
                 % bad_lines)

    zip_path = os.path.join(OUT_DIR, name + ".zip")
    if os.path.exists(zip_path):
        os.remove(zip_path)
    with zipfile.ZipFile(zip_path, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        for rel, p in rel_files(stage):
            z.write(p, name + "/" + rel)

    # 自检 1：压缩包可读
    bad = zipfile.ZipFile(zip_path).testzip()
    if bad is not None:
        sys.exit("压缩包损坏：%s" % bad)

    # 自检 2：版本号真的注进去了（不注入时是 dev，必须报出来）
    printed = run([os.path.join(bin_dir, "work2api.exe"), "version"])
    if ver not in printed:
        sys.exit("版本注入失败，exe 自报：%r（期望包含 %s）" % (printed, ver))

    print()
    print("=== 包内容 ===")
    for rel, p in rel_files(stage):
        print("  %-26s %10d 字节" % (rel, os.path.getsize(p)))
    print()
    print("=== 自检 ===")
    print("  exe 自报版本 : %s" % printed.splitlines()[0])
    print("  包内布局     : exe 均在 %s\\ 下（包根无 exe）" % BIN_SUBDIR)
    print("  vbs          : %d CRLF / 0 裸 LF / 可执行行全 ASCII" % crlf)
    print("  压缩包完整性 : OK")
    print("  校验和复算   : %d 项全部匹配" % len(files))
    print()
    print("=== 产物 ===")
    print("  %s" % zip_path)
    print("  大小   = %d 字节" % os.path.getsize(zip_path))
    print("  sha256 = %s" % sha256(zip_path))


if __name__ == "__main__":
    main()
