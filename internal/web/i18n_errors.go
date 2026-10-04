package web

import (
	"fmt"
	"regexp"
	"strings"
)

var localizedErrorCodes = map[string]string{
	"服务暂时无法完成该操作，请稍后重试。":                                    "error_service_unavailable",
	"加密内容格式无法识别":                                            "e2ee_bad_envelope",
	"内容不能为空":                                                "error_content_empty",
	"内容必须是有效的 UTF-8 文本":                                     "error_content_utf8",
	"resource content must not be empty":                    "error_resource_empty",
	"content is too large":                                  "error_content_too_large",
	"uploaded file is too large":                            "error_uploaded_too_large",
	"文件名必须是有效的 UTF-8 文本":                                    "error_filename_utf8",
	"文件名不能是 . 或 ..":                                         "error_filename_dots",
	"文件名不能包含路径分隔符":                                          "error_filename_separator",
	"文件名不能包含控制字符":                                           "error_filename_control",
	"文件名不能包含文字方向控制符":                                        "error_filename_bidi",
	"编辑器内容不是有效的 Unicode 文本":                                 "error_editor_unicode",
	"请先填写远程地址。":                                             "error_remote_required",
	"使用次数无法识别":                                              "error_share_uses",
	"此资源已被下架，不能创建分享链接":                                      "error_taken_down",
	"此资源已被下架，不能另存为新资源":                                      "error_copy_taken_down",
	"存活时长无法识别":                                              "error_share_lifetime",
	"自定义时长无法识别，单位 s / m / h / d，例如 90m":                     "error_share_custom_lifetime",
	"存活时长不能为负":                                              "error_lifetime_negative",
	"存活时长最长一年，如需更久请选择「永不过期」":                                "error_lifetime_year",
	"使用次数不能为负":                                              "error_uses_negative",
	"快速分享为只读，转为资源后才能修改":                                     "quick_share_read_only",
	"端到端加密的快速分享暂不能转为资源":                                     "quick_share_encrypted_keep",
	"此账号已设置主密码":                                             "keyring_exists",
	"主密码已在其他设备上修改，请刷新页面后重试":                                 "keyring_changed",
	"不支持的密钥派生参数":                                            "keyring_bad_format",
	"密钥格式无法识别":                                              "keyring_bad_format",
	"不支持的自动锁定时长":                                            "keyring_bad_lock",
	"临时分享已失效":                                               "error_paste_expired",
	"临时分享已失效，无法保存":                                          "error_paste_expired_save",
	"临时分享已失效，无法保存。":                                         "error_paste_expired_save",
	"upstream URL must be HTTP(S) without user information": "error_upstream_url",
	"local upstream hosts are not allowed":                  "error_upstream_local",
	"non-public upstream addresses are not allowed":         "error_upstream_local",
	"upstream resolved to a non-public address":             "error_upstream_local",
	"upstream host resolves to a non-public address":        "error_upstream_local",
}

var localizedErrorPatterns = []struct {
	pattern *regexp.Regexp
	key     string
}{
	{regexp.MustCompile(`^内容最大 (.+)$`), "error_content_max"},
	{regexp.MustCompile(`^文件名最长 ([0-9]+) 个字符$`), "error_filename_length"},
	{regexp.MustCompile(`^不支持的文本编码 (.+)$`), "error_encoding_unsupported"},
	{regexp.MustCompile(`^内容含有 (.+) 无法表示的字符(?::.*)?$`), "error_unrepresentable"},
	{regexp.MustCompile(`^存储空间不足：已用 (.+)，上限 (.+)，本次需要 (.+)。请先删除或缩减已有资源。$`), "error_quota_storage"},
	{regexp.MustCompile(`^资源数量已达上限：已有 ([0-9]+) 个，上限 ([0-9]+) 个。请先删除不再需要的资源。$`), "error_quota_resources"},
	{regexp.MustCompile(`^上游内容超过 ([0-9]+) MiB 上限$`), "error_upstream_too_large"},
}

// localizePageError translates the validation errors that can be returned to
// a form. Unknown text is preserved: it may have come from an OAuth provider
// or another boundary where replacing the detail with a generic message would
// make the failure harder to diagnose.
func localizePageError(locale, message string) string {
	if message == "" {
		return ""
	}
	const retainedSuffix = "（该内容无法在页面中保留，请重新选择文件）"
	if strings.HasSuffix(message, retainedSuffix) {
		base := strings.TrimSuffix(message, retainedSuffix)
		return localizePageError(locale, base) + translate(locale, "error_content_not_retained")
	}
	if code := localizedErrorCodes[message]; code != "" {
		return translate(locale, code)
	}
	if strings.HasPrefix(message, "parse remote link:") || strings.HasPrefix(message, "parse upstream URL:") {
		return translate(locale, "error_upstream_url")
	}
	for _, candidate := range localizedErrorPatterns {
		match := candidate.pattern.FindStringSubmatch(message)
		if match == nil {
			continue
		}
		arguments := make([]any, len(match)-1)
		for index := range arguments {
			arguments[index] = match[index+1]
		}
		return fmt.Sprintf(translate(locale, candidate.key), arguments...)
	}
	return message
}
