#!/usr/bin/env python3
"""本地打包 work2api 的 **Linux** 发布包（只含服务本体，不含托盘）。

用法（仓库根目录下，Windows / Linux 均可执行）：
    python tools/make-release-linux.py

产出：
    dist/release/work2api-<日期>-<短rev>[-dirty]-linux-amd64.tar.gz
    dist/release/work2api-<日期>-<短rev>[-dirty]-linux-amd64/    （同内容的解包目录）

包内布局：
    bin/work2api                服务本体（0755，无扩展名）
    config.example.json         配置样例
    README.md
    BUILD.txt
    SHA256SUMS

设计取舍（都是踩过之后定的，别随手改）
--------------------------------------
1. **Linux 包只有服务，没有托盘 —— 这是能力差异，不是遗漏。**
   cmd/work2api-tray/ 下**每个文件**都带 `//go:build windows`，整包用的是
   Win32 API（GDI 画图标、CreateProcessW 拉服务、进程名判重）。给它加
   `GOOS=linux` 只会得到「build constraints exclude all Go files」。
   因此这里 TARGETS 只有 work2api 一个，不要"顺手补上托盘的交叉编译"。

2. **产出 tar.gz 而不是 zip。** 两个理由，都不是审美：
   - zip 不保留可执行位。Linux 上解压出来是个 0644 的文件，必须手动
     `chmod +x` 才能跑 —— 而用户往往在 `./bin/work2api serve` 报
     "Permission denied" 之后才想起来。tar.gz 能带 mode。
   - tar.gz 是 Linux 发布物的通行格式，`tar xzf` 是三秒的本能动作。

3. **SHA256SUMS 与 BUILD.txt 必须是 LF，绝不能是 CRLF。**
   Windows 版脚本用 `newline="\\r\\n"` 写这两个文件（Windows 上好看），
   Linux 版照抄就会出事：GNU `sha256sum -c SHA256SUMS` 会把行尾的 \\r
   当成文件名的一部分，于是报「找不到文件 SHA256SUMS: bin/work2api<CR>」——
   校验失败但不是因为文件被改过。**这是个会让人怀疑包被篡改的假警报。**

4. **自检不能靠「跑一下二进制」—— 本机是 Windows，跑不了 ELF。**
   Windows 版脚本的版本自检是直接执行 exe 读它自报的版本号；Linux 包在
   打包机上执行不了，所以换成两条**静态可验**的证据：
   - 版本号字符串出现在二进制字节里（`-X main.version=` 会把它写进 .rodata）
   - `go version -m <bin>` 读回的 vcs.revision 等于当次 git rev
   这两条是实测的字节证据，不是"应该没问题"。**但要说清边界**：这只证明
   二进制里带了正确的版本与 commit，**不证明它能在 Linux 上跑起来** ——
   后者需要一台 Linux 机器（见文件末的「未验证项」）。

5. **EXTRAS 里的 README.md 是 Windows 视角写的。**
   它讲托盘、讲 .vbs 开机自启 —— 那些内容与本包无关。照放是因为它是项目
   唯一的文档，但 BUILD.txt 里必须写明这一点，否则拿到 Linux 包的人会去找
   一个不存在的托盘程序。

6. **-trimpath 必须开。** 否则二进制里会带上打包机的绝对路径（泄露目录结构，
   也破坏可复现性）。

这个脚本只做打包，不做部署。部署在 Linux 上该用 systemd（见 BUILD.txt）。
"""
import gzip
import hashlib
import os
import shutil
import subprocess
import sys
import tarfile
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT_DIR = os.path.join(ROOT, "dist", "release")

# exe/bin 在包内的子目录。**改了它，服务的数据根判定就会错**：
# config.DataRoot 的规则是「exe 所在目录的上一级」，包内 bin/work2api 的上一级
# 正是包根，于是 config.json / auths / data 都落在包根 —— 与 Windows 包同一约定。
BIN_SUBDIR = "bin"

# (包路径, 产物名)。只有服务本体 —— 见文件头 1。
TARGETS = [("./cmd/work2api", "work2api")]

# 一并进包的附带文件
EXTRAS = ["config.example.json", "README.md"]

# 包内每个条目的权限。显式写死而不是靠 os.walk + 源文件 mode：
# **Windows 上源文件的 mode 是 0666，没有可执行位这个概念**，
# 照抄到 tar 里就是一个解压后跑不起来的包。见文件头 2。
MODE_EXEC = 0o755
MODE_FILE = 0o644
MODE_DIR = 0o755


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
    name = "work2api-%s-linux-amd64" % ver
    stage = os.path.join(OUT_DIR, name)
    bin_dir = os.path.join(stage, BIN_SUBDIR)

    if os.path.isdir(stage):
        shutil.rmtree(stage)
    os.makedirs(bin_dir, exist_ok=True)

    gover = run(["go", "version"])

    for pkg, out in TARGETS:
        print("构建 %s ..." % out)
        run(
            ["go", "build", "-trimpath", "-ldflags", "-s -w -X main.version=" + ver,
             "-o", os.path.join(bin_dir, out), pkg],
            # CGO_ENABLED=0 → 静态链接。Linux 包尤其吃这条：动态链接的二进制
            # 绑 glibc 版本，换台机器就可能 GLIBC_2.xx not found。
            env={"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "amd64"},
        )

    for rel in EXTRAS:
        shutil.copy2(os.path.join(ROOT, rel), stage)

    build_txt = """work2api Linux 构建包（仅服务本体）
========================================
版本标识 : {ver}
git rev  : {rev_full}
构建时间 : {now}
工作区   : {dirty}
Go 工具链: {gover}
目标平台 : linux/amd64  (CGO_ENABLED=0 静态链接, -trimpath)

包内布局
--------
bin/work2api            反代服务本体（CLI + 内嵌面板）
config.example.json     配置样例
README.md               项目文档（**Windows 视角**，其中托盘 / .vbs 部分与本包无关）
BUILD.txt / SHA256SUMS

本包**不含托盘**：托盘程序（work2api-tray）是纯 Win32 实现，源码全部带
//go:build windows，不存在 Linux 版本。Linux 上由 systemd 负责拉起服务。

用法
----
1) 解压到任意目录（下面记作 <包根>）：
     tar xzf work2api-{ver}-linux-amd64.tar.gz

2) 试跑（前台，Ctrl-C 退出）：
     cd <包根> && ./bin/work2api serve
   服务监听 127.0.0.1:7865，面板在 http://127.0.0.1:7865
   首次运行会在**当前工作目录**生成 config.json（随机 api_key 写回该文件）。

   注意 command 用 ./bin/work2api 而不是 work2api，且**在包根执行**。
   数据根由程序自己校正（config.DataRoot：「cwd 像数据根就用 cwd，否则用
   exe 所在目录的上一级」），所以从包根或从 bin/ 里启动都会把 config.json /
   auths / data 落到**包根**。切换时 stderr 会打一行「工作目录 <路径>（数据根）」。

3) 看版本：
     ./bin/work2api version
   复核溯源（不执行二进制也能读）：
     go version -m ./bin/work2api

4) 登录账号（交互式，需要浏览器完成授权）：
     ./bin/work2api login -kind <workbuddy|trae> -region <cn|global>

5) 开机自启（systemd user 服务，不需要 root）：
     mkdir -p ~/.config/systemd/user
     cat > ~/.config/systemd/user/work2api.service <<'EOF'
     [Unit]
     Description=work2api 反代服务
     After=network-online.target

     [Service]
     Type=simple
     WorkingDirectory=<包根绝对路径>
     ExecStart=<包根绝对路径>/bin/work2api serve
     Restart=on-failure
     RestartSec=3

     [Install]
     WantedBy=default.target
     EOF
     systemctl --user daemon-reload
     systemctl --user enable --now work2api
     systemctl --user status work2api
     journalctl --user -u work2api -f        # 看日志
     loginctl enable-linger $USER            # 让服务在未登录时也常驻

   把上面两处 <包根绝对路径> 换成实际路径。WorkingDirectory 必须填 ——
   config.json / auths / data 都是相对它解析的。

   用 systemd 而不是 nohup：nohup 起的进程在注销后会被 SIGHUP 收走，
   而 journalctl 能看到程序自己打的结构化日志。

校验
----
    sha256sum -c SHA256SUMS     # 在包根执行，逐文件复算
    sha256sum <tar.gz>          # 与发布页给出的值对比

关于「本包在别的机器上是否真的能跑」
------------------------------------
本包由 Windows 打包机交叉编译产出。构建阶段已验证：目标平台 linux/amd64、
CGO_ENABLED=0 静态链接（不依赖 glibc 版本）、无 Windows 专属依赖
（internal/ 与 web/ 下无 //go:build 约束）。
**未验证**：在真实 Linux 上启动、监听端口、面板渲染、以及实际反代一次请求。
拿到包后请按上面第 2 步先跑一次 `serve` 确认能起来。
""".format(
        ver=ver,
        rev_full=rev_full,
        now=time.strftime("%Y-%m-%d %H:%M:%S %z"),
        dirty="有未提交改动（非正式发布）" if dirty else "干净",
        gover=gover,
    )
    # newline="\n" —— 见文件头 3：这里是 Linux 包，行尾必须是 LF。
    with open(os.path.join(stage, "BUILD.txt"), "w", encoding="utf-8", newline="\n") as f:
        f.write(build_txt)

    # ── 布局断言 ────────────────────────────────────────────────────────
    # 包根出现可执行文件说明落点被改回了扁平布局，服务算出的数据根会跑到包外
    # （Windows 版踩过这个：config.json 写到解压目录的上一级）。静默错，当场拦。
    stray = [f for f in os.listdir(stage)
             if os.path.isfile(os.path.join(stage, f)) and f not in EXTRAS
             and f not in ("BUILD.txt", "SHA256SUMS")]
    if stray:
        sys.exit("包根出现了预期外的文件（%s）—— 可执行文件必须在 %s/ 下，"
                 "否则数据根会跑到包外" % (", ".join(stray), BIN_SUBDIR))
    for _pkg, out in TARGETS:
        if not os.path.isfile(os.path.join(bin_dir, out)):
            sys.exit("缺少 %s/%s" % (BIN_SUBDIR, out))

    # 校验和：先写 SHA256SUMS，再逐行复算一遍确认它自己没错。
    # 文件列表**显式列出**而不是 os.walk —— 包内布局只有这 5 个条目，
    # 写死能防止将来往 stage 里多放东西（例如凭证）而没人发现。
    entries = [
        ("bin/" + TARGETS[0][1], os.path.join(bin_dir, TARGETS[0][1]), MODE_EXEC),
        ("config.example.json", os.path.join(stage, "config.example.json"), MODE_FILE),
        ("README.md", os.path.join(stage, "README.md"), MODE_FILE),
        ("BUILD.txt", os.path.join(stage, "BUILD.txt"), MODE_FILE),
    ]
    sums_path = os.path.join(stage, "SHA256SUMS")
    with open(sums_path, "w", encoding="ascii", newline="\n") as f:
        for rel, p, _m in entries:
            f.write("%s  %s\n" % (sha256(p), rel))
    entries.append(("SHA256SUMS", sums_path, MODE_FILE))

    for rel, p, _m in entries[:-1]:
        want = None
        for ln in open(sums_path, encoding="ascii", newline=""):
            if ln.rstrip("\n").endswith("  " + rel):
                want = ln.split("  ", 1)[0]
        if want is None:
            sys.exit("SHA256SUMS 里没有 %s" % rel)
        if sha256(p) != want:
            sys.exit("校验和不匹配：%s" % rel)

    # 行尾断言 —— SHA256SUMS 与 BUILD.txt 必须是纯 LF，见文件头 3。
    # 这是**实测**而非约定：CRLF 会让 Linux 上的 sha256sum -c 报「找不到文件」。
    for fn in ("SHA256SUMS", "BUILD.txt"):
        raw = open(os.path.join(stage, fn), "rb").read()
        crlf = raw.count(b"\r\n")
        lone = raw.count(b"\n") - crlf
        if crlf or raw.count(b"\r") != crlf:
            sys.exit("%s 含 CR 字节（CRLF=%d）—— Linux 包必须纯 LF，"
                     "否则 sha256sum -c 会报找不到文件" % (fn, crlf))
        if not lone:
            sys.exit("%s 没有换行符" % fn)

    # 版本自检：本机跑不了 ELF，改用两条静态证据（见文件头 4）。
    bin_path = os.path.join(bin_dir, TARGETS[0][1])
    blob = open(bin_path, "rb").read()
    if ver.encode() not in blob:
        sys.exit("版本注入失败：二进制里搜不到 %r（-X main.version= 没生效？）" % ver)
    if blob[:4] != b"\x7fELF":
        sys.exit("%s 不是 ELF 文件（前 4 字节 %r）—— GOOS 是不是没设成 linux？"
                 % (TARGETS[0][1], blob[:4]))
    # ELF machine 字段：0x3E = x86-64。防止 GOARCH 被改错却看不出来。
    if len(blob) < 20 or int.from_bytes(blob[18:20], "little") != 0x3E:
        sys.exit("%s 不是 x86-64 的 ELF" % TARGETS[0][1])

    buildinfo = run(["go", "version", "-m", bin_path])
    if rev_full not in buildinfo:
        sys.exit("溯源失败：go version -m 里读不到 commit %s\n%s" % (rev_full, buildinfo))
    # 静态链接判据：CGO_ENABLED=0 的产物 buildinfo 里应带 CGO_ENABLED=0。
    # 这条比「有没有 PT_INTERP」更直接 —— 后者要解析 ELF 程序头。
    if "CGO_ENABLED=0" not in buildinfo:
        sys.exit("构建信息里没有 CGO_ENABLED=0，静态链接的前提不成立\n%s" % buildinfo)

    # ── 打 tar.gz ───────────────────────────────────────────────────────
    tar_path = os.path.join(OUT_DIR, name + ".tar.gz")
    if os.path.exists(tar_path):
        os.remove(tar_path)

    def tarinfo(arcname, mode):
        ti = tarfile.TarInfo(arcname)
        ti.mode = mode
        ti.uid = 0
        ti.gid = 0
        ti.uname = "root"
        ti.gname = "root"
        ti.mtime = int(time.time())
        return ti

    with open(tar_path, "wb") as raw:
        # gzip 的 mtime 归零 —— 让同一份内容的包字节稳定（gzip 默认写当前时间）。
        with gzip.GzipFile(fileobj=raw, mode="wb", compresslevel=9, mtime=0) as gz:
            with tarfile.open(fileobj=gz, mode="w") as tf:
                ti = tarinfo(name + "/" + BIN_SUBDIR, MODE_DIR)
                ti.type = tarfile.DIRTYPE
                tf.addfile(ti)
                for rel, p, mode in entries:
                    ti = tarinfo(name + "/" + rel, mode)
                    ti.size = os.path.getsize(p)
                    with open(p, "rb") as f:
                        tf.addfile(ti, f)

    # 自检 1：压缩包可读
    with tarfile.open(tar_path, "r:gz") as tf:
        members = tf.getnames()
    want_members = [name + "/" + BIN_SUBDIR] + [name + "/" + r for r, _p, _m in entries]
    if sorted(members) != sorted(want_members):
        sys.exit("tar 成员不对：\n  实际 %s\n  期望 %s" % (sorted(members), sorted(want_members)))

    # 自检 2：解出来逐字节比对，并确认可执行位真的带上了。
    # 「打进去了」和「解出来是对的」是两件事 —— 权限位正是这里最容易丢。
    chk = os.path.join(OUT_DIR, "_lintest")
    if os.path.isdir(chk):
        shutil.rmtree(chk)
    os.makedirs(chk)
    with tarfile.open(tar_path, "r:gz") as tf:
        # filter="tar" 保留权限位；新版 Python 的默认值会告警，显式指定消掉。
        tf.extractall(chk, filter="tar")
    extracted = os.path.join(chk, name, BIN_SUBDIR, TARGETS[0][1])
    if open(extracted, "rb").read() != blob:
        sys.exit("解压后的二进制与打包前不一致")
    if tarfile.open(tar_path, "r:gz").getmember(
            name + "/" + BIN_SUBDIR + "/" + TARGETS[0][1]).mode != MODE_EXEC:
        sys.exit("%s 在包内的权限不是 0755 —— 解压后无法执行" % TARGETS[0][1])
    # 顺带验一次 sha256sum 能否直接吃下这个文件（行尾真的没 CR）
    sums_in_pkg = open(os.path.join(chk, name, "SHA256SUMS"), "rb").read()
    if b"\r" in sums_in_pkg:
        sys.exit("解压出的 SHA256SUMS 含 CR")
    shutil.rmtree(chk)

    print()
    print("=== 包内容 ===")
    for rel in want_members:
        p = os.path.join(stage, rel.replace(name + "/", "", 1))
        if os.path.isdir(p):
            print("  %-26s <dir>" % rel)
        else:
            print("  %-26s %10d 字节" % (rel, os.path.getsize(p)))
    print()
    print("=== 自检 ===")
    print("  目标平台     : linux/amd64（ELF 头校验通过，x86-64）")
    print("  版本注入     : 二进制字节里含 %s" % ver)
    print("  溯源         : go version -m → vcs.revision=%s" % rev)
    print("  静态链接     : CGO_ENABLED=0（不绑 glibc 版本）")
    print("  权限位       : bin/%s = 0755（解压即可执行）" % TARGETS[0][1])
    print("  行尾         : SHA256SUMS / BUILD.txt 纯 LF（sha256sum -c 可用）")
    print("  压缩包完整性 : 成员齐全，解压后逐字节一致")
    print("  校验和复算   : %d 项全部匹配" % (len(entries) - 1))
    print()
    print("=== 产物 ===")
    print("  %s" % tar_path)
    print("  大小   = %d 字节" % os.path.getsize(tar_path))
    print("  sha256 = %s" % sha256(tar_path))
    print()
    print("提醒：以上都是**静态证据**（编译、字节、权限、溯源）。")
    print("      本机是 Windows，跑不了 ELF —— 「能在 Linux 上启动」尚未验证。")


if __name__ == "__main__":
    main()
