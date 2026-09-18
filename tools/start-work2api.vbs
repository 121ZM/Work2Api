' Work2Api —— 开机静默启动**托盘**（供「启动」文件夹开机自启使用）
'
' 部署：把本文件复制到
'   %APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\start_work2api.vbs
' 注意目标处文件名是 start_work2api.vbs（下划线），和本文件名的连字符不同 ——
' 别在 Startup 里留下两份，WScript 会把两份都执行。
' 登录时由 WScript 执行，窗口隐藏。
'
' 这里**只起托盘**这一个进程；服务（work2api.exe serve）由托盘自己拉起来。
' 以前这个脚本直接起服务，结果是托盘和服务两边谁都不知道对方存在 ——
' 托盘能把服务关掉却起不回来，服务没了只能重新登录才恢复。
'
' 幂等：托盘自带单实例互斥体，重复启动会自己退出，所以这里不用查进程。
Option Explicit

Dim sh
Set sh = CreateObject("WScript.Shell")
sh.CurrentDirectory = "E:\AI\Work2Api\dist"
sh.Run """E:\AI\Work2Api\dist\work2api-tray.exe""", 0, False
