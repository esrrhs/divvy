package agent

import "strings"

// askRequiresConsent reports whether a model question is asking the user to
// approve a dangerous or external side effect. A timeout on such a question
// is never treated as consent.
func askRequiresConsent(question string) bool {
	q := strings.ToLower(strings.TrimSpace(question))
	if q == "" {
		return false
	}
	for _, phrase := range consentIntentPhrases {
		if strings.Contains(q, phrase) {
			return true
		}
	}
	if hasAny(q, dangerousActionPhrases) && hasAny(q, requestFramingPhrases) {
		return true
	}
	return false
}

// consentIntentPhrases are themselves a permission / approval request.
var consentIntentPhrases = []string{
	"approve", "approval", "permission", "authorize", "authorise",
	"please confirm", "need you to confirm", "confirm this", "confirm that",
	"is it ok", "is it okay", "ok to ", "okay to ",
	"批准", "同意", "确认执行", "是否允许", "是否同意",
	"可否把", "可否删除", "可否发送", "可否推送",
}

// dangerousActionPhrases name a destructive or external side effect.
var dangerousActionPhrases = []string{
	"delete ", "delete all", "remove all", "rm -rf", "rm -r ",
	"drop table", "drop database", "truncate ",
	"overwrite", "force push", "force-push", "git push", "push --force",
	"send email", "send this", "email this", "post this", "submit this",
	"http request", "webhook", "production", "prod env",
	"exfiltrat", "upload to", "publish to", "release to",
	"api key", "credential", "password",
	"删除", "清空", "覆盖", "强制推送", "推送到", "发送邮件", "发出去",
	"外发", "提交到生产", "生产环境", "泄露", "上传到",
}

// requestFramingPhrases mark the ask as requesting permission to act.
var requestFramingPhrases = []string{
	"can i ", "may i ", "should i ", "shall i ", "do you want me",
	"could i ", "would you like me", "let me ",
	"能不能", "要不要", "可以吗", "行吗", "好不好",
}

func hasAny(q string, phrases []string) bool {
	for _, p := range phrases {
		if strings.Contains(q, p) {
			return true
		}
	}
	return false
}

const (
	askTimeoutClarifyReply = "The user did not answer in time (timeout, not consent). For this clarification, make a reasonable assumption and continue; do not call ask again. Do not treat this as approval for any destructive, external, or privileged action."
	askNoUserConsentReply  = "No interactive user is available. This is not consent. Do not perform approval, destructive, or external-send actions; choose a safe alternative or call finish."
	askNoUserClarifyReply  = "No interactive user is available in this run. Make a reasonable assumption and continue; do not call ask again."
)
