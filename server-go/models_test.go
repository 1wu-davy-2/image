package main

import "testing"

func seedKey(t *testing.T, store *Store, name, protocol string, balance *float64, valid *bool, enabled bool) Key {
	t.Helper()
	key, err := store.saveKey(KeyInput{
		Name: name, Protocol: protocol, BaseURL: "https://uuapi.io/v1",
		APIKey: "sk-test-" + name, Balance: balance, Enabled: enabled,
	})
	if err != nil {
		t.Fatalf("建 Key 失败：%v", err)
	}
	if _, err := store.setKeyBalance(key.ID, balance, "USD", valid, ""); err != nil {
		t.Fatalf("写余额失败：%v", err)
	}
	return key
}

func TestAvailableModelsAggregatesByProtocol(t *testing.T) {
	store := newTestStore(t)
	money := 10.0
	yes := true

	gptKey := seedKey(t, store, "gpt", "gpt", &money, &yes, true)
	nanoKey := seedKey(t, store, "nano", "nano", &money, &yes, true)

	if _, err := store.setKeyModels(gptKey.ID, []ModelInfo{
		{ID: "gpt-image-2.5", Name: "gpt-image-2.5"},
		{ID: "gpt-image-2.5-flare", Name: "GPT Image 2.5 Flare"},
	}, ""); err != nil {
		t.Fatalf("写模型失败：%v", err)
	}
	if _, err := store.setKeyModels(nanoKey.ID, []ModelInfo{{ID: "gemini-2.5-flash-image"}}, ""); err != nil {
		t.Fatalf("写模型失败：%v", err)
	}

	models, err := store.availableModels()
	if err != nil {
		t.Fatalf("汇总失败：%v", err)
	}
	if len(models["gpt"]) != 2 {
		t.Fatalf("gpt 应该有 2 个模型，实际 %+v", models["gpt"])
	}
	if len(models["nano"]) != 1 {
		t.Fatalf("nano 应该有 1 个模型，实际 %+v", models["nano"])
	}
	// 模型名带 display_name 的要用上。
	for _, item := range models["gpt"] {
		if item.ID == "gpt-image-2.5-flare" && item.Name != "GPT Image 2.5 Flare" {
			t.Fatalf("该用 display_name，实际 %q", item.Name)
		}
		if !item.Available || item.Keys != 1 {
			t.Fatalf("余额正常时该标成可用：%+v", item)
		}
	}
	// 没有 gemini-official 的 Key，这一组不该出现。
	if _, ok := models["gemini-official"]; ok {
		t.Fatal("没有对应 Key 的调用方式不该出现在结果里")
	}
}

func TestAvailableModelsAvailability(t *testing.T) {
	store := newTestStore(t)
	rich, broke := 10.0, 0.0
	yes, no := true, false

	// 停用的 Key 整把不算数。
	off := seedKey(t, store, "off", "gpt", &rich, &yes, false)
	if _, err := store.setKeyModels(off.ID, []ModelInfo{{ID: "only-on-disabled"}}, ""); err != nil {
		t.Fatalf("写模型失败：%v", err)
	}
	// 余额为 0 的 Key：模型要列出来，但标成不可用。
	empty := seedKey(t, store, "empty", "gpt", &broke, &yes, true)
	if _, err := store.setKeyModels(empty.ID, []ModelInfo{{ID: "no-money"}}, ""); err != nil {
		t.Fatalf("写模型失败：%v", err)
	}
	// 中转说过失效的 Key 同理。
	dead := seedKey(t, store, "dead", "gpt", &rich, &no, true)
	if _, err := store.setKeyModels(dead.ID, []ModelInfo{{ID: "invalid-key"}}, ""); err != nil {
		t.Fatalf("写模型失败：%v", err)
	}
	// 正常的 Key。
	good := seedKey(t, store, "good", "gpt", &rich, &yes, true)
	if _, err := store.setKeyModels(good.ID, []ModelInfo{{ID: "fine"}}, ""); err != nil {
		t.Fatalf("写模型失败：%v", err)
	}

	models, err := store.availableModels()
	if err != nil {
		t.Fatalf("汇总失败：%v", err)
	}
	got := map[string]bool{}
	for _, item := range models["gpt"] {
		got[item.ID] = item.Available
	}
	if _, ok := got["only-on-disabled"]; ok {
		t.Fatal("停用的 Key 上的模型不该出现")
	}
	if got["no-money"] {
		t.Fatal("余额为 0 的 Key 上的模型该标成不可用")
	}
	if got["invalid-key"] {
		t.Fatal("中转说失效的 Key 上的模型该标成不可用")
	}
	if !got["fine"] {
		t.Fatal("正常 Key 上的模型该是可用的")
	}
	// 可用的排前面。
	if models["gpt"][0].ID != "fine" {
		t.Fatalf("可用的该排最前，实际 %+v", models["gpt"])
	}
}

// 一把 Key 的模型没拉过时，不该影响别的 Key。
func TestAvailableModelsSkipsUnsyncedKeys(t *testing.T) {
	store := newTestStore(t)
	money := 5.0
	yes := true
	key := seedKey(t, store, "fresh", "gpt", &money, &yes, true)

	models, err := store.availableModels()
	if err != nil {
		t.Fatalf("汇总失败：%v", err)
	}
	if len(models["gpt"]) != 0 {
		t.Fatalf("还没拉过模型时该是空的，实际 %+v", models["gpt"])
	}

	// 拉取失败只记错误，不覆盖已有的模型列表。
	if _, err := store.setKeyModels(key.ID, []ModelInfo{{ID: "keep-me"}}, ""); err != nil {
		t.Fatalf("写模型失败：%v", err)
	}
	updated, err := store.setKeyModels(key.ID, nil, "中转站超时")
	if err != nil {
		t.Fatalf("记错误失败：%v", err)
	}
	if updated.ModelsError != "中转站超时" {
		t.Fatalf("该记下错误，实际 %q", updated.ModelsError)
	}
	if len(updated.Models) != 1 || updated.Models[0].ID != "keep-me" {
		t.Fatalf("失败不该清掉已有的模型，实际 %+v", updated.Models)
	}
}
