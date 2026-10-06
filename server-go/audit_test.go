package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

/* ---------- 一个最小的 HTTP 客户端，带着会话 Cookie 走完整路由 ---------- */

type apiClient struct {
	t       *testing.T
	handler http.Handler
	cookie  *http.Cookie
}

func (c *apiClient) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("拼不出请求体：%v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)

	// 登出会把 Cookie 清成空值，这里跟着把会话丢掉。
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Value == "" {
			c.cookie = nil
			continue
		}
		c.cookie = cookie
	}
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	return rec.Code, payload
}

func (c *apiClient) mustDo(method, path string, body any) map[string]any {
	c.t.Helper()
	code, payload := c.do(method, path, body)
	if code != 200 {
		c.t.Fatalf("%s %s 该回 200，实际 %d：%v", method, path, code, payload)
	}
	return payload
}

/* ---------- 用例 ---------- */

// 走真实 HTTP 接口把图生图跑一遍：注册 → 签到 → 带参考图和蒙版生图。
// 用的是假中转站，所以不碰真的上游、不花钱。
func TestGenerateWithReferenceAndMaskEndToEnd(t *testing.T) {
	world := newFakeWorld(t)
	store := newTestStore(t)
	seedRelayKey(t, store)

	client := &apiClient{t: t, handler: (&server{store: store}).routes()}

	client.mustDo("POST", "/api/auth/register", map[string]any{
		"username": "painter", "password": "abcd1234",
		"phone": "13800138000", "email": "painter@example.com",
	})

	// 新用户额度是 0，先签到换点额度出来。
	checkin := client.mustDo("POST", "/api/checkin", nil)
	if amount, _ := checkin["amount"].(float64); amount <= 0 {
		t.Fatalf("签到该给额度，实际 %v", checkin["amount"])
	}

	client.mustDo("POST", "/api/generate", map[string]any{
		"type":    "gpt",
		"model":   "gpt-image-2.5",
		"prompt":  "把背景换成雪山",
		"mode":    "edit",
		"size":    "1024x1024",
		"quality": "medium",
		"image":   pngField("参考图.png"),
		"mask":    pngField("蒙版.png"),
	})

	// 上游那次同步请求里，参考图和蒙版都该是实打实的文件。
	call := world.find(t, "/v1/images/edits")
	if got := strings.Join(call.fileNames(), ","); got != "image,mask" {
		t.Fatalf("上游收到的文件字段 = %q，想要 image,mask", got)
	}
	if string(call.Files["image"]) != string(pngBytes) || string(call.Files["mask"]) != string(pngBytes) {
		t.Fatal("参考图或蒙版的字节不对")
	}
	if call.prompt() != "把背景换成雪山" {
		t.Fatalf("提示词没带过去：%q", call.prompt())
	}
}

// 审计要把这几件事都记下来，并且记清楚是谁干的。
func TestAuditLogRecordsUserActions(t *testing.T) {
	world := newFakeWorld(t)
	store := newTestStore(t)
	seedRelayKey(t, store)

	client := &apiClient{t: t, handler: (&server{store: store}).routes()}
	client.mustDo("POST", "/api/auth/register", map[string]any{
		"username": "painter", "password": "abcd1234",
		"phone": "13800138000", "email": "painter@example.com",
	})
	client.mustDo("POST", "/api/checkin", nil)
	client.mustDo("POST", "/api/generate", map[string]any{
		"type": "gpt", "model": "gpt-image-2.5", "prompt": "把背景换成雪山",
		"mode": "edit", "size": "1024x1024", "quality": "medium",
		"image": pngField("参考图.png"), "mask": pngField("蒙版.png"),
	})
	client.mustDo("POST", "/api/auth/logout", nil)

	// 先确认这次生图真的发出去了，免得审计测的是「失败也被记成成功」。
	if got := strings.Join(world.find(t, "/v1/images/edits").fileNames(), ","); got != "image,mask" {
		t.Fatalf("上游该收到 image,mask，实际 %q", got)
	}

	logs, total, err := store.auditLogs("", 50, 0)
	if err != nil {
		t.Fatalf("读审计失败：%v", err)
	}
	if total != len(logs) {
		t.Fatalf("total 和条数对不上：%d vs %d", total, len(logs))
	}

	byAction := map[string]AuditEntry{}
	for _, item := range logs {
		byAction[item.Action] = item
	}
	for _, action := range []string{"注册", "签到", "生图", "登出"} {
		if _, ok := byAction[action]; !ok {
			t.Fatalf("审计里少了「%s」，实际记了：%v", action, actionNames(logs))
		}
	}

	// 每一条都得能看出是谁干的、从哪儿来的。
	for _, item := range logs {
		if item.ActorName != "painter" {
			t.Errorf("「%s」的操作者该是 painter，实际 %q", item.Action, item.ActorName)
		}
		if item.IP == "" {
			t.Errorf("「%s」没记下来访地址", item.Action)
		}
		if item.CreatedAt == "" {
			t.Errorf("「%s」没记时间", item.Action)
		}
	}

	// 生图这条要说清楚是图生图还是文生图、带没带蒙版——翻日志时这是关键信息。
	detail := byAction["生图"].Detail
	if !strings.Contains(detail, "图生图") || !strings.Contains(detail, "蒙版") {
		t.Errorf("生图的详情该写明图生图+蒙版，实际 %q", detail)
	}
}

// 登不上的也要记，连着试很多次是要看得出来的。
func TestAuditLogRecordsFailedLogin(t *testing.T) {
	store := newTestStore(t)
	newTestUser(t, store, "alice")
	client := &apiClient{t: t, handler: (&server{store: store}).routes()}

	if code, _ := client.do("POST", "/api/auth/login", map[string]any{"account": "alice", "password": "wrong-one"}); code != 401 {
		t.Fatalf("密码错该回 401，实际 %d", code)
	}
	logs, _, err := store.auditLogs("登录失败", 10, 0)
	if err != nil {
		t.Fatalf("读审计失败：%v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("该记下 1 条登录失败，实际 %d 条", len(logs))
	}
	if logs[0].Target != "alice" {
		t.Fatalf("该记下试的是哪个账号，实际 %q", logs[0].Target)
	}
}

// 管理端的操作也要记，而且要能跟用户端的操作区分开。
func TestAuditLogRecordsAdminActions(t *testing.T) {
	store := newTestStore(t)
	seedKey(t, store, "老号", "gpt", floatPtr(5), nil, true)

	token, err := store.createSession("admin", "")
	if err != nil {
		t.Fatalf("发管理端会话失败：%v", err)
	}
	admin := &apiClient{t: t, handler: (&server{store: store}).routes(), cookie: &http.Cookie{Name: adminCookieName, Value: token}}

	admin.mustDo("PUT", "/api/admin/settings", map[string]any{"checkinMin": 3, "checkinMax": 9, "generateCost": 2})
	admin.mustDo("PUT", "/api/admin/types/gpt", map[string]any{"defaultModel": "gpt-image-2.5", "multiplier": 1.5})

	logs, _, err := store.auditLogs("", 50, 0)
	if err != nil {
		t.Fatalf("读审计失败：%v", err)
	}
	seen := map[string]bool{}
	for _, item := range logs {
		if item.ActorKind != "admin" {
			t.Errorf("「%s」该记成管理端操作，实际 %q", item.Action, item.ActorKind)
		}
		seen[item.Action] = true
	}
	for _, action := range []string{"改签到规则", "改生图类型"} {
		if !seen[action] {
			t.Fatalf("审计里少了「%s」，实际记了：%v", action, actionNames(logs))
		}
	}
}

// 审计接口只给管理端看。
func TestAdminAuditNeedsAdmin(t *testing.T) {
	store := newTestStore(t)
	user := newTestUser(t, store, "alice")
	_, token, err := store.issueUserSession(user.ID)
	if err != nil {
		t.Fatalf("发会话失败：%v", err)
	}

	client := &apiClient{t: t, handler: (&server{store: store}).routes(), cookie: &http.Cookie{Name: userCookieName, Value: token}}
	if code, _ := client.do("GET", "/api/admin/audit", nil); code != 401 {
		t.Fatalf("普通用户翻审计该回 401，实际 %d", code)
	}

	anonymous := &apiClient{t: t, handler: (&server{store: store}).routes()}
	if code, _ := anonymous.do("GET", "/api/admin/audit", nil); code != 401 {
		t.Fatalf("没登录翻审计该回 401，实际 %d", code)
	}
}

// 筛选和分页。
func TestAuditLogFilterAndPaging(t *testing.T) {
	store := newTestStore(t)
	for i := 0; i < 5; i++ {
		if err := store.audit(AuditEntry{ActorKind: "user", ActorName: "alice", Action: "签到", Detail: "第 " + string(rune('A'+i)) + " 次"}); err != nil {
			t.Fatalf("写审计失败：%v", err)
		}
	}
	if err := store.audit(AuditEntry{ActorKind: "user", ActorName: "alice", Action: "登出"}); err != nil {
		t.Fatalf("写审计失败：%v", err)
	}

	only, total, err := store.auditLogs("签到", 10, 0)
	if err != nil || total != 5 || len(only) != 5 {
		t.Fatalf("筛「签到」该有 5 条：total=%d len=%d err=%v", total, len(only), err)
	}
	page, total, err := store.auditLogs("", 2, 0)
	if err != nil || total != 6 || len(page) != 2 {
		t.Fatalf("不筛该是 6 条里翻 2 条：total=%d len=%d err=%v", total, len(page), err)
	}
	// 倒序：最新的那条在最前面。
	if page[0].Action != "登出" {
		t.Fatalf("该按时间倒序，第一条是 %q", page[0].Action)
	}

	actions, err := store.auditActions()
	if err != nil || len(actions) != 2 {
		t.Fatalf("该有两个动作可选，实际 %v err=%v", actions, err)
	}
}

/* ---------- 小工具 ---------- */

func actionNames(logs []AuditEntry) []string {
	out := make([]string, 0, len(logs))
	for _, item := range logs {
		out = append(out, item.Action)
	}
	return out
}

// seedRelayKey 铺一把指向假中转站的 Key，让挑 Key 挑得中它。
func seedRelayKey(t *testing.T, store *Store) Key {
	t.Helper()
	yes := true
	key, err := store.saveKey(KeyInput{
		Name: "测试号", Protocol: "gpt", ModelType: "gpt",
		BaseURL: "https://" + fakeRelayHost + "/v1",
		APIKey:  "sk-test-endtoend", Enabled: true,
	})
	if err != nil {
		t.Fatalf("建 Key 失败：%v", err)
	}
	if _, err := store.setKeyBalance(key.ID, floatPtr(10), "USD", &yes, ""); err != nil {
		t.Fatalf("写余额失败：%v", err)
	}
	if _, err := store.setKeyModels(key.ID, []ModelInfo{{ID: "gpt-image-2.5"}}, ""); err != nil {
		t.Fatalf("写模型失败：%v", err)
	}
	return key
}
