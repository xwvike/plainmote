package main

import (
	"fmt"
	"strings"
)

// The command line speaks English, or Chinese when the locale asks for it.
// Each message is {English, Chinese}.
var messages = map[string][2]string{
	"usage": {`plainmote – view, edit and upload PlainMote resources

Usage:
  plainmote login [--read-only] [--no-browser]
                                    sign this device in (confirm in a browser)
  plainmote logout                  revoke and forget this device's sign-in
  plainmote whoami                  show the account and server in use
  plainmote ls [keyword]            list resources
  plainmote cat <resource>          print a resource's content
  plainmote edit <resource>         edit in $VISUAL/$EDITOR; closing it uploads a new version
  plainmote push <file|-> [--to <resource>] [--name <name>] [--filename <file>]
                                    upload a new resource, or a new version of one
  plainmote share <file|-> [--ttl 10m|1h|1d|7d|30d] [--filename <file>]
                                    make a quick share and print its link
  plainmote server [<url>]          show or set the server
  plainmote config language zh|en|auto
                                    set the language of these messages
  plainmote config editor <command> set the editor edit opens; plainmote config shows all
  plainmote version                 show this command line's version

A <resource> is an id, an id prefix of at least six characters, or an exact
name or filename. Every command takes --server <url>. The first edit asks
which editor to use unless $VISUAL or $EDITOR is set.
`, `plainmote – 查看、编辑和上传 PlainMote 资源

用法：
  plainmote login [--read-only] [--no-browser]
                                    登录这台设备（在浏览器中确认）
  plainmote logout                  撤销并删除这台设备上的登录
  plainmote whoami                  显示当前使用的账号和服务器
  plainmote ls [关键词]             列出资源
  plainmote cat <资源>              输出资源内容
  plainmote edit <资源>             用 $VISUAL/$EDITOR 编辑，关闭后上传为新版本
  plainmote push <文件|-> [--to <资源>] [--name <名称>] [--filename <文件名>]
                                    上传为新资源，或作为已有资源的新版本
  plainmote share <文件|-> [--ttl 10m|1h|1d|7d|30d] [--filename <文件名>]
                                    创建快速分享并输出链接
  plainmote server [<地址>]         查看或设置服务器
  plainmote config language zh|en|auto
                                    设置提示语言
  plainmote config editor <命令>    设置编辑时使用的编辑器；plainmote config 查看全部设置
  plainmote version                 显示命令行版本

<资源> 可以是 ID、至少 6 位的 ID 前缀，或完整的名称、文件名。
所有命令都可以加 --server <地址>。未设置 $VISUAL 或 $EDITOR 时，
第一次编辑会让你选择编辑器。
`},
	"unknown_command":       {"unknown command %q; run plainmote help", "未知命令 %q，运行 plainmote help 查看用法"},
	"needs_argument":        {"%s needs %s", "%s 需要参数：%s"},
	"not_signed_in":         {"not signed in to %s; run plainmote login", "尚未登录 %s，请运行 plainmote login"},
	"signin_expired":        {"the sign-in has expired or was revoked; run plainmote login", "登录已过期或已被撤销，请运行 plainmote login"},
	"bad_server":            {"the server must be an https:// address (http:// only for localhost): %s", "服务器地址必须以 https:// 开头（仅 localhost 可用 http://）：%s"},
	"server_is":             {"Server: %s", "服务器：%s"},
	"server_set":            {"Server set to %s", "服务器已设置为 %s"},
	"login_open":            {"Open this address in a browser, on this device or any other, and enter the code:", "在浏览器中打开以下地址（本机或任意设备均可），并输入验证码："},
	"login_code":            {"Code", "验证码"},
	"login_press_enter":     {"Press Enter to open it in your browser. Waiting for confirmation…", "按 Enter 在浏览器中打开这个地址。等待确认…"},
	"login_opened":          {"Opened in your browser.", "已在浏览器中打开。"},
	"login_open_failed":     {"Could not open a browser; open the address above by hand.", "无法自动打开浏览器，请手动打开上面的地址。"},
	"login_valid":           {"The code is valid for %d minutes.", "验证码 %d 分钟内有效。"},
	"login_waiting":         {"Waiting for confirmation…", "等待确认…"},
	"login_denied":          {"The sign-in was denied in the browser.", "已在浏览器中拒绝此次登录。"},
	"login_expired":         {"The code expired before it was confirmed; run plainmote login again.", "验证码在确认前已过期，请重新运行 plainmote login。"},
	"login_done":            {"✓ Signed in as %s (%s, expires %s)", "✓ 已登录为 %s（%s，%s 到期）"},
	"login_saved":           {"Saved to %s", "凭据已保存到 %s"},
	"scope_write":           {"read & write", "读写"},
	"scope_read":            {"read only", "只读"},
	"logout_done":           {"Signed out; the sign-in was revoked.", "已退出登录，令牌已撤销。"},
	"logout_none":           {"Not signed in to %s.", "尚未登录 %s。"},
	"whoami":                {"%s on %s (%s, expires %s)", "%s @ %s（%s，%s 到期）"},
	"version_mismatch":      {"Note: the server runs %s and this command line is %s; reinstall with: curl -fsSL %s/cli | sh", "提示：服务器版本为 %s，本机命令行为 %s；可运行 curl -fsSL %s/cli | sh 更新。"},
	"not_found":             {"no resource matches %q", "找不到资源 %q"},
	"ambiguous":             {"%q matches more than one resource; use the id:", "%q 匹配到多个资源，请改用 ID："},
	"ls_empty":              {"No resources.", "没有资源。"},
	"ls_header":             {"NAME\tFILE\tSIZE\tVER\tUPDATED\tID", "名称\t文件名\t大小\t版本\t更新时间\tID"},
	"untitled":              {"(untitled)", "（未命名）"},
	"remote_mark":           {"(reference)", "（引用）"},
	"cat_binary":            {"%q is not text; redirect it to a file: plainmote cat %s > file (or pass --force)", "%q 不是文本内容，请重定向到文件：plainmote cat %s > 文件（或加 --force）"},
	"is_reference":          {"%q points at a remote address and has no stored content", "%q 引用的是远程地址，没有可编辑的内容"},
	"quick_share_read_only": {"%q is a quick share and read only; keep it as a resource on its page first", "%q 是快速分享，为只读；请先在其页面中转为资源"},
	"share_created":         {"Shared on %s, until %s; it is in your resources until then.", "已在 %s 分享，有效期至 %s；到期前可在我的资源中查看。"},
	"quick_mark":            {"quick share", "快速分享"},
	"not_editable":          {"%q is not text and cannot be edited; use plainmote push to replace it", "%q 不是文本内容，无法编辑；可用 plainmote push 替换"},
	"encrypted":             {"%q is end-to-end encrypted and cannot be edited here", "%q 经过端到端加密，无法在这里编辑"},
	"editor_pick":           {"Choose the editor to edit with (it is remembered; change it with plainmote config editor):", "选择用来编辑的编辑器（会记住，之后可用 plainmote config editor 修改）："},
	"editor_number":         {"Number [1-%d, Enter for 1]: ", "编号 [1-%d，回车选 1]："},
	"editor_saved":          {"Using %s from now on.", "以后将使用 %s。"},
	"config_saved":          {"Saved.", "已保存。"},
	"edit_downloaded":       {"Downloaded %[2]s (v%[3]d, %[4]s) from %[1]s, opening it with %[5]s…", "已从 %s 下载 %s（v%d，%s），正在用 %s 打开…"},
	"edit_close_hint":       {"Close the tab when you are done to upload it.", "编辑完成后关闭标签页即可上传。"},
	"edit_unchanged":        {"No changes; nothing uploaded.", "没有修改，未上传。"},
	"edit_saved":            {"✓ Saved as v%d on %s", "✓ 已保存为 v%d（%s）"},
	"edit_trimmed":          {"To make room, %d of the oldest earlier versions were removed.", "为腾出空间，已清除 %d 个最旧的历史版本。"},
	"edit_editor_failed":    {"the editor exited with an error: %v", "编辑器异常退出：%v"},
	"edit_kept":             {"Your changes are kept in %s", "你的修改保留在 %s"},
	"edit_kept_push":        {"Upload them later with: plainmote push %s --to %s", "稍后可以这样上传：plainmote push %s --to %s"},
	"conflict":              {"✗ Not saved: this resource was saved elsewhere as v%d while you were editing.", "✗ 未保存：编辑期间这个资源已在别处更新为 v%d。"},
	"conflict_diff":         {"v%d → your changes:", "v%d → 你的修改："},
	"conflict_too_big":      {"(the difference is too large to show)", "（差异太大，不在这里显示）"},
	"conflict_more":         {"… %d more lines", "… 另有 %d 行"},
	"conflict_prompt":       {"[o] save over v%d   [r] edit again on v%d   [k] keep the file for later  (o/r/k) ", "[o] 覆盖为 v%d   [r] 在 v%d 上重新编辑   [k] 保留文件稍后处理  (o/r/k) "},
	"conflict_reedit":       {"Opening v%d, with your changes beside it as %s…", "正在打开 v%d，并附上你的修改作为参考（%s）…"},
	"push_created":          {"✓ Created %s on %s  %s", "✓ 已新建 %s（%s）  %s"},
	"push_saved":            {"✓ Saved as v%d on %s", "✓ 已保存为 v%d（%s）"},
	"push_same":             {"No change on %[2]s: the content is already v%[1]d.", "内容没有变化，仍为 v%d（%s）。"},
	"push_needs_name":       {"reading standard input needs --name or --filename", "从标准输入读取时需要 --name 或 --filename"},
	"read_only":             {"this sign-in is read-only; run plainmote login to sign in with write access", "当前登录为只读，请运行 plainmote login 重新以读写权限登录"},
	"interrupted":           {"Interrupted.", "已中断。"},
	"server_error":          {"the server answered %d: %s", "服务器返回 %d：%s"},
	"request_failed":        {"could not reach %s: %v", "无法连接 %s：%v"},
	"error_prefix":          {"plainmote: ", "plainmote："},
}

// lang is 0 for English and 1 for Chinese.
var lang = 0

func detectLanguage(getenv func(string) string) int {
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if value := getenv(name); value != "" {
			if strings.HasPrefix(strings.ToLower(value), "zh") {
				return 1
			}
			return 0
		}
	}
	return 0
}

func msg(key string, args ...any) string {
	text, ok := messages[key]
	if !ok {
		return key
	}
	if len(args) == 0 {
		return text[lang]
	}
	return fmt.Sprintf(text[lang], args...)
}
