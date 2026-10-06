package main

import (
	"strings"
	"testing"
)

func TestErrorMessagePrefersUpstreamText(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{"error 是字符串", map[string]any{"error": "boom"}, "boom"},
		{"error 是对象", map[string]any{"error": map[string]any{"message": "boom"}}, "boom"},
		{"顶层的 message", map[string]any{"message": "boom"}, "boom"},
		{"被拦截", map[string]any{"promptFeedback": map[string]any{"blockReason": "SAFETY"}}, "内容被拦截：SAFETY"},
	}
	for _, item := range cases {
		if got := rawErrorMessage(item.payload, 400); got != item.want {
			t.Errorf("%s：想要 %q，实际 %q", item.name, item.want, got)
		}
	}
	if got := rawErrorMessage(nil, 503); got != "上游返回 HTTP 503" {
		t.Errorf("没有正文时该报状态码，实际 %q", got)
	}
}

// 中转站的英文原文要补一句中文，说清下一步干什么。
func TestUpstreamHint(t *testing.T) {
	raw := `Model "gpt-image-1" is not supported by any configured account in this group`
	hinted := upstreamHint(raw)
	if !strings.HasPrefix(hinted, raw) {
		t.Fatalf("应该保留原文，实际 %q", hinted)
	}
	if !strings.Contains(hinted, "分组") {
		t.Fatalf("应该提示去查分组，实际 %q", hinted)
	}

	if got := upstreamHint("no available channel"); got != "no available channel" {
		t.Errorf("认不出来的错误应该原样返回，实际 %q", got)
	}
	if got := upstreamHint("Insufficient quota"); !strings.Contains(got, "余额") {
		t.Errorf("余额不足该有中文提示，实际 %q", got)
	}
}
