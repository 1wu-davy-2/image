package main

import (
	"net/http/httptest"
	"testing"
)

func recharge(t *testing.T, store *Store, userID string, quota int) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE users SET quota = ? WHERE id = ?`, quota, userID); err != nil {
		t.Fatalf("充值失败：%v", err)
	}
}

// 1 额度 × 1.5 倍 = 1.5，额度是整数，向上取整成 2。
// 不取整的话倍率等于没有，向下取整又会少扣。
func TestReserveForTypeAppliesMultiplier(t *testing.T) {
	store := newTestStore(t)
	yes := true
	seedKey(t, store, "gpt-key", "gpt", floatPtr(10), &yes, true)
	user := newTestUser(t, store, "buyer")
	recharge(t, store, user.ID, 100)

	if _, err := store.saveModelType("gpt", "gpt-image-2.5", 1.5); err != nil {
		t.Fatalf("配类型失败：%v", err)
	}

	key, item, cost, quota, err := store.reserveForType(user.ID, "gpt", 1)
	if err != nil {
		t.Fatalf("扣额度失败：%v", err)
	}
	if key.ModelType != "gpt" {
		t.Fatalf("该挑 gpt 类型的 Key，实际 %q", key.ModelType)
	}
	if item.DefaultModel != "gpt-image-2.5" {
		t.Fatalf("该把默认模型带回来：%+v", item)
	}
	if cost != 2 {
		t.Fatalf("1 × 1.5 该向上取整成 2，实际 %d", cost)
	}
	if quota != 98 {
		t.Fatalf("扣完该剩 98，实际 %d", quota)
	}
}

func TestReserveForTypeOnlyUsesItsOwnType(t *testing.T) {
	store := newTestStore(t)
	yes := true
	seedKey(t, store, "gemini-key", "nano", floatPtr(10), &yes, true)
	user := newTestUser(t, store, "buyer")
	recharge(t, store, user.ID, 100)

	if _, _, _, _, err := store.reserveForType(user.ID, "gpt", 1); err == nil {
		t.Fatal("gpt 类型下面没挂 Key，该报错")
	}
	// 挑不到 Key 就不能扣钱。
	after, err := store.userByID(user.ID)
	if err != nil {
		t.Fatalf("读用户失败：%v", err)
	}
	if after.Quota != 100 {
		t.Fatalf("挑不到 Key 时不该扣额度，实际剩 %d", after.Quota)
	}
}

// 默认模型配错了（这个类型的 Key 根本没这个模型）时，要挑真有的那把，
// 而不是按余额大小随便挑——挑错了用户收到的是看不懂的上游报错。
func TestPickKeyForTypePrefersKeyThatHasTheModel(t *testing.T) {
	store := newTestStore(t)
	yes := true
	rich := seedKey(t, store, "rich", "gpt", floatPtr(100), &yes, true)
	poor := seedKey(t, store, "poor", "gpt", floatPtr(1), &yes, true)
	if _, err := store.setKeyModels(rich.ID, []ModelInfo{{ID: "gpt-image-1"}}, ""); err != nil {
		t.Fatalf("写模型失败：%v", err)
	}
	if _, err := store.setKeyModels(poor.ID, []ModelInfo{{ID: "gpt-image-2.5"}}, ""); err != nil {
		t.Fatalf("写模型失败：%v", err)
	}

	key, err := store.pickKeyForType("gpt", "gpt-image-2.5")
	if err != nil {
		t.Fatalf("挑 Key 失败：%v", err)
	}
	if key.ID != poor.ID {
		t.Fatalf("该挑有 gpt-image-2.5 的那把（余额小的），实际挑了 %q", key.Name)
	}
}

// 还没拉过模型列表的 Key 是「不知道支不支持」，不能当成「不支持」排除掉。
func TestPickKeyForTypeKeepsUnsyncedKeys(t *testing.T) {
	store := newTestStore(t)
	yes := true
	fresh := seedKey(t, store, "fresh", "gpt", floatPtr(5), &yes, true)

	key, err := store.pickKeyForType("gpt", "gpt-image-2.5")
	if err != nil {
		t.Fatalf("挑 Key 失败：%v", err)
	}
	if key.ID != fresh.ID {
		t.Fatalf("没拉过模型列表的 Key 也该能用，实际挑了 %q", key.Name)
	}
}

// 挂了 Key 但都没这个模型时还是要兜底挑一把：模型列表可能是旧的，
// 硬排除会让用户直接生不了图。
func TestPickKeyForTypeFallsBackToMissing(t *testing.T) {
	store := newTestStore(t)
	yes := true
	key := seedKey(t, store, "only", "gpt", floatPtr(5), &yes, true)
	if _, err := store.setKeyModels(key.ID, []ModelInfo{{ID: "别的模型"}}, ""); err != nil {
		t.Fatalf("写模型失败：%v", err)
	}
	if _, err := store.pickKeyForType("gpt", "gpt-image-2.5"); err != nil {
		t.Fatalf("该兜底挑一把出来，实际报错：%v", err)
	}
}

func TestSaveModelTypeValidates(t *testing.T) {
	store := newTestStore(t)
	for _, bad := range []struct {
		name    string
		model   string
		factor  float64
		message string
	}{
		{"gpt", "gpt-image-2.5", 0, "倍率 0"},
		{"gpt", "gpt-image-2.5", 1000, "倍率 1000"},
		{"gpt", "带中文的模型名", 1, "模型名不合法"},
		{"不存在的类型", "x", 1, "不认识的类型"},
	} {
		if _, err := store.saveModelType(bad.name, bad.model, bad.factor); err == nil {
			t.Fatalf("%s 该被拒绝", bad.message)
		}
	}

	item, err := store.saveModelType("grok", "grok-2-image", 2.5)
	if err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	if item.Multiplier != 2.5 || item.DefaultModel != "grok-2-image" || item.Label != "GROK" {
		t.Fatalf("保存结果不对：%+v", item)
	}
	// 存过的要能读回来，没存过的用默认值。
	list, err := store.modelTypes()
	if err != nil {
		t.Fatalf("读类型失败：%v", err)
	}
	if len(list) != 3 {
		t.Fatalf("永远是三行，实际 %d 行", len(list))
	}
	if list[0].Type != "gpt" || list[2].Type != "grok" {
		t.Fatalf("顺序该固定，实际 %+v", list)
	}
	if list[2].Multiplier != 2.5 {
		t.Fatalf("grok 的倍率该读回 2.5，实际 %v", list[2].Multiplier)
	}
}

// 老库的 Key 只有调用方式。迁移时按对应关系猜一个类型——不猜的话这些 Key
// 一把都挑不出来，用户直接生不了图。
func TestModelTypeMigrationGuessesFromProtocol(t *testing.T) {
	store := newTestStore(t)
	yes := true
	gptKey := seedKey(t, store, "g", "gpt", floatPtr(1), &yes, true)
	nanoKey := seedKey(t, store, "n", "nano", floatPtr(1), &yes, true)
	officialKey := seedKey(t, store, "o", "gemini-official", floatPtr(1), &yes, true)

	// 模拟老库：这些 Key 建的时候还没有生图类型这一栏。
	if _, err := store.db.Exec(`UPDATE keys SET model_type = ''`); err != nil {
		t.Fatalf("清类型失败：%v", err)
	}
	if err := store.migrateModelTypes(); err != nil {
		t.Fatalf("迁移失败：%v", err)
	}

	for _, want := range []struct {
		id   string
		name string
		typ  string
	}{
		{gptKey.ID, "gpt", "gpt"},
		{nanoKey.ID, "nano", "gemini"},
		{officialKey.ID, "gemini-official", "gemini"},
	} {
		got, err := store.keyByID(want.id)
		if err != nil {
			t.Fatalf("读 Key 失败：%v", err)
		}
		if got.ModelType != want.typ {
			t.Fatalf("%s 该猜成 %s 类型，实际 %q", want.name, want.typ, got.ModelType)
		}
	}
}

// 老的调用方不会带 modelType，这时候按调用方式猜一个，别把 Key 挡在门外。
func TestSaveKeyGuessesModelTypeWhenMissing(t *testing.T) {
	store := newTestStore(t)
	key, err := store.saveKey(KeyInput{
		Name: "old-client", Protocol: "nano", BaseURL: "https://uuapi.io/v1",
		APIKey: "sk-old", Enabled: true,
	})
	if err != nil {
		t.Fatalf("没带类型也该存得进去：%v", err)
	}
	if key.ModelType != "gemini" {
		t.Fatalf("nano 该猜成 gemini，实际 %q", key.ModelType)
	}

	// 但填了个不认识的值要打回，不能悄悄换成猜的——那是调用方写错了，得让它知道。
	if _, err := store.saveKey(KeyInput{
		Name: "bad", Protocol: "gpt", ModelType: "banana",
		BaseURL: "https://uuapi.io/v1", APIKey: "sk-bad", Enabled: true,
	}); err == nil {
		t.Fatal("不认识的类型该被打回")
	}
}

// 用户端只该看到挂了 Key 的类型，还得知道那个类型下面有没有这个默认模型。
func TestGenerationTypesEndpoint(t *testing.T) {
	store := newTestStore(t)
	yes := true
	gptKey := seedKey(t, store, "g", "gpt", floatPtr(5), &yes, true)
	seedKey(t, store, "n", "nano", floatPtr(5), &yes, true)
	if _, err := store.setKeyModels(gptKey.ID, []ModelInfo{{ID: "gpt-image-2.5"}}, ""); err != nil {
		t.Fatalf("写模型失败：%v", err)
	}
	if _, err := store.saveModelType("gpt", "gpt-image-2.5", 1.5); err != nil {
		t.Fatalf("配类型失败：%v", err)
	}

	user := newTestUser(t, store, "viewer")
	_, token, err := store.issueUserSession(user.ID)
	if err != nil {
		t.Fatalf("发会话失败：%v", err)
	}

	payload := getJSON(t, store, "/api/types", token)
	types, _ := payload["types"].([]any)
	if len(types) != 2 {
		t.Fatalf("挂了 gpt 和 gemini 两把 Key，该列两行（grok 没 Key 不列），实际 %+v", types)
	}
	first, _ := types[0].(map[string]any)
	if first["type"] != "gpt" || first["label"] != "GPT" {
		t.Fatalf("第一行该是 GPT，实际 %+v", first)
	}
	if first["multiplier"] != 1.5 || first["defaultModel"] != "gpt-image-2.5" {
		t.Fatalf("倍率和默认模型该带上：%+v", first)
	}
	if first["usable"] != true || first["modelKnown"] != true {
		t.Fatalf("Key 有余额、模型列表里也有这个模型，该标成可用：%+v", first)
	}

	// 默认模型不在 Key 的模型列表里时要标出来，用户端才能提示。
	if _, err := store.saveModelType("gpt", "gpt-image-9", 1.5); err != nil {
		t.Fatalf("配类型失败：%v", err)
	}
	payload = getJSON(t, store, "/api/types", token)
	types, _ = payload["types"].([]any)
	first, _ = types[0].(map[string]any)
	if first["modelKnown"] != false {
		t.Fatalf("默认模型不在 Key 的列表里，该标成没拉到：%+v", first)
	}
}

// 没登录不该看到这些。
func TestGenerationTypesNeedsLogin(t *testing.T) {
	store := newTestStore(t)
	srv := &server{store: store}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/types", nil))
	if rec.Code != 401 {
		t.Fatalf("没登录该回 401，实际 %d", rec.Code)
	}
}
