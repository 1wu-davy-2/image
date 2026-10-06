package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func floatPtr(value float64) *float64 { return &value }

func TestRelayBalanceSumsEnabledKeys(t *testing.T) {
	store := newTestStore(t)
	yes := true

	seedKey(t, store, "one", "gpt", floatPtr(10.5), &yes, true)
	seedKey(t, store, "two", "nano", floatPtr(1.75), &yes, true)

	balance, ok := store.relayBalance()
	if !ok {
		t.Fatal("有两把带余额的 Key，该算得出来")
	}
	if balance.Total != 12.25 {
		t.Fatalf("合计该是 12.25，实际 %v", balance.Total)
	}
	if balance.Keys != 2 {
		t.Fatalf("该算 2 把，实际 %d", balance.Keys)
	}
	if balance.Unit != "USD" {
		t.Fatalf("单位该是 USD，实际 %q", balance.Unit)
	}
}

// 停用的 Key 不参与挑 Key，钱也不该算进去——不然顶栏那个数比实际能用的多。
func TestRelayBalanceSkipsDisabledAndUnknown(t *testing.T) {
	store := newTestStore(t)
	yes := true

	seedKey(t, store, "live", "gpt", floatPtr(4), &yes, true)
	seedKey(t, store, "off", "gpt", floatPtr(100), &yes, false)
	// 还没查过余额的 Key：Balance 是 nil。
	seedKey(t, store, "unqueried", "gpt", nil, nil, true)

	balance, ok := store.relayBalance()
	if !ok {
		t.Fatal("该算得出来")
	}
	if balance.Total != 4 {
		t.Fatalf("只该算启用且有余额的那把，实际 %v", balance.Total)
	}
	if balance.Keys != 1 {
		t.Fatalf("该算 1 把，实际 %d", balance.Keys)
	}
}

func TestRelayBalanceEmptyWhenNothingKnown(t *testing.T) {
	store := newTestStore(t)
	if _, ok := store.relayBalance(); ok {
		t.Fatal("一把 Key 都没有时不该报出一个数")
	}

	seedKey(t, store, "unqueried", "gpt", nil, nil, true)
	if _, ok := store.relayBalance(); ok {
		t.Fatal("余额都没查过时不该报出一个数")
	}
}

// 单位不一样时只合计先遇到的那个单位：USD 和 CNY 直接相加得出的是个假数。
func TestRelayBalanceIgnoresMismatchedUnit(t *testing.T) {
	store := newTestStore(t)
	yes := true

	seedKey(t, store, "usd", "gpt", floatPtr(3), &yes, true)
	second := seedKey(t, store, "cny", "nano", floatPtr(70), &yes, true)
	// seedKey 一律写 USD，这把改成 CNY 造出单位不一致的情况。
	if _, err := store.setKeyBalance(second.ID, floatPtr(70), "CNY", &yes, ""); err != nil {
		t.Fatalf("写余额失败：%v", err)
	}

	balance, ok := store.relayBalance()
	if !ok {
		t.Fatal("该算得出来")
	}
	if balance.Total != 3 || balance.Keys != 1 || balance.Unit != "USD" {
		t.Fatalf("只该算 USD 那把，实际 %+v", balance)
	}
}

/* ---------- 给非管理员看的那份 ---------- */

// 账上快没钱的时候照实说，不然生图开始报错还以为是坏了。
func TestBlurBalanceKeepsLowBalancesHonest(t *testing.T) {
	for _, total := range []float64{0, 0.5, 9.99} {
		real := RelayBalance{Total: total, Unit: "USD", Keys: 1}
		got := blurBalance(real, "u1")
		if got.Total != total || got.Blurred {
			t.Fatalf("%v 低于下限，该原样给，实际 %+v", total, got)
		}
	}
}

func TestBlurBalanceInflatesHighBalances(t *testing.T) {
	real := RelayBalance{Total: 47.5, Unit: "USD", Keys: 2}
	got := blurBalance(real, "u1")

	if !got.Blurred {
		t.Fatal("高于下限该标成模糊过")
	}
	if got.Total < real.Total*5 || got.Total > real.Total*10 {
		t.Fatalf("该落在真实的 5-10 倍之间，实际 %v（真实 %v）", got.Total, real.Total)
	}
	if got.Unit != "USD" || got.Keys != 2 {
		t.Fatalf("单位和把数该原样带过来，实际 %+v", got)
	}
	// 传进来的那份不能被改动，调用方可能还要用。
	if real.Total != 47.5 || real.Blurred {
		t.Fatalf("不该动传进来的那份：%+v", real)
	}
}

// 同一个用户同一天必须是同一个数：刷新一下就变，看着像坏了，
// 反而更让人盯着这个数看。
func TestBlurBalanceIsStablePerUser(t *testing.T) {
	real := RelayBalance{Total: 100, Unit: "USD", Keys: 1}
	first := blurBalance(real, "user-a")
	for i := 0; i < 20; i++ {
		if again := blurBalance(real, "user-a"); again.Total != first.Total {
			t.Fatalf("同一用户该摇出同一个数：%v vs %v", first.Total, again.Total)
		}
	}

	// 换个用户不该全都撞到同一个倍数上。种子是定死的，这个断言不会时好时坏。
	seen := map[float64]bool{}
	for _, id := range []string{"u1", "u2", "u3", "u4", "u5", "u6", "u7", "u8", "u9", "u10", "u11", "u12"} {
		seen[blurBalance(real, id).Total] = true
	}
	if len(seen) < 2 {
		t.Fatalf("12 个用户该摇出不止一个数，实际只有 %v", seen)
	}
}

/* ---------- /api/me 的可见性 ---------- */

func getJSON(t *testing.T, store *Store, path, token string) map[string]any {
	t.Helper()
	srv := &server{store: store}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: userCookieName, Value: token})
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%s 该回 200，实际 %d：%s", path, rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解不开响应：%v", err)
	}
	return payload
}

func getMe(t *testing.T, store *Store, token string) map[string]any {
	t.Helper()
	return getJSON(t, store, "/api/me", token)
}

// 管理员看真账，别人看模糊过的。真数字不能下发给非管理员——它在网络响应里
// 躺着的话，翻一下 devtools 就看见了，模糊就没意义了。
func TestMeBalanceAdminSeesRealOthersSeeBlurred(t *testing.T) {
	store := newTestStore(t)
	yes := true
	seedKey(t, store, "one", "gpt", floatPtr(50), &yes, true)

	plain := newTestUser(t, store, "plainuser")
	_, token, err := store.issueUserSession(plain.ID)
	if err != nil {
		t.Fatalf("发会话失败：%v", err)
	}

	raw, ok := getMe(t, store, token)["relayBalance"].(map[string]any)
	if !ok {
		t.Fatalf("非管理员该拿到一份模糊过的余额，实际没这个字段")
	}
	if raw["blurred"] != true {
		t.Fatalf("非管理员拿到的该标成模糊过：%+v", raw)
	}
	total, _ := raw["total"].(float64)
	if total < 250 || total > 500 {
		t.Fatalf("该是真实的 5-10 倍（250-500），实际 %v", total)
	}

	if _, err := store.db.Exec(`UPDATE users SET is_admin = 1 WHERE id = ?`, plain.ID); err != nil {
		t.Fatalf("改成管理员失败：%v", err)
	}
	raw, ok = getMe(t, store, token)["relayBalance"].(map[string]any)
	if !ok {
		t.Fatalf("管理员该拿到余额")
	}
	if raw["total"] != 50.0 || raw["unit"] != "USD" {
		t.Fatalf("管理员该看到真账：%+v", raw)
	}
	if raw["blurred"] == true {
		t.Fatalf("真账不该标成模糊过：%+v", raw)
	}
}

// 余额低的时候非管理员也看真的——这份没被模糊，前端就不该缀「充足」。
func TestMeLowBalanceIsHonestForPlainUsers(t *testing.T) {
	store := newTestStore(t)
	yes := true
	seedKey(t, store, "one", "gpt", floatPtr(5), &yes, true)

	plain := newTestUser(t, store, "plainuser")
	_, token, err := store.issueUserSession(plain.ID)
	if err != nil {
		t.Fatalf("发会话失败：%v", err)
	}
	raw, ok := getMe(t, store, token)["relayBalance"].(map[string]any)
	if !ok {
		t.Fatalf("该拿到余额，实际没这个字段")
	}
	if raw["total"] != 5.0 {
		t.Fatalf("低于下限该照实给 5，实际 %v", raw["total"])
	}
	if raw["blurred"] == true {
		t.Fatalf("照实给的那份不该标成模糊过：%+v", raw)
	}
}

// 未登录时 user 是 null，别在这里顺手把余额漏出去。
func TestMeWithoutSessionHasNoBalance(t *testing.T) {
	store := newTestStore(t)
	yes := true
	seedKey(t, store, "one", "gpt", floatPtr(8), &yes, true)

	payload := getMe(t, store, "")
	if payload["user"] != nil {
		t.Fatalf("没会话时 user 该是 null，实际 %+v", payload["user"])
	}
	if _, ok := payload["relayBalance"]; ok {
		t.Fatal("没登录不该拿到中转站余额")
	}
}
