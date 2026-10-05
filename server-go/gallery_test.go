package main

import (
	"bytes"
	"path/filepath"
	"testing"
)

// 一个最小的合法 PNG（1x1），用来验证图片存取。
var testPNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
	0x42, 0x60, 0x82,
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := openStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("建库失败：%v", err)
	}
	t.Cleanup(func() { store.db.Close() })
	return store
}

func newTestUser(t *testing.T, store *Store, name string) User {
	t.Helper()
	user, _, err := store.register(RegisterInput{
		Username: name,
		Password: "abcd1234",
		Phone:    "13800138000",
		Email:    name + "@example.com",
	})
	if err != nil {
		t.Fatalf("建用户失败：%v", err)
	}
	return user
}

func seedGeneration(t *testing.T, store *Store, userID, prompt string) Generation {
	t.Helper()
	item, err := store.saveGeneration(GenerationInput{
		UserID:    userID,
		Prompt:    prompt,
		Protocol:  "gpt",
		Model:     "gpt-image-2.5-flare",
		SizeLabel: "1024x1024 · high",
		Channel:   "sync",
		Images: []StoredImage{
			{Mime: "image/png", Data: testPNG},
			{Mime: "image/png", URL: "https://example.com/two.png"},
		},
	})
	if err != nil {
		t.Fatalf("存作品失败：%v", err)
	}
	return item
}

func TestGallerySaveAndRead(t *testing.T) {
	store := newTestStore(t)
	user := newTestUser(t, store, "alice")

	item := seedGeneration(t, store, user.ID, "冬夜里的灯塔")
	if item.ID == "" {
		t.Fatal("没有生成 id")
	}
	if len(item.Images) != 2 {
		t.Fatalf("图片数应为 2，实际 %d", len(item.Images))
	}
	if item.Images[0].Position != 0 || item.Images[1].Position != 1 {
		t.Fatalf("图片序号不对：%+v", item.Images)
	}
	if item.IsPublic {
		t.Fatal("新作品默认不该是公开的")
	}
	if item.Username != "alice" {
		t.Fatalf("用户名应为 alice，实际 %q", item.Username)
	}

	// 存进去的字节要能原样取回来。
	mime, data, _, err := store.generationImage(item.ID, 0)
	if err != nil {
		t.Fatalf("取图失败：%v", err)
	}
	if mime != "image/png" || !bytes.Equal(data, testPNG) {
		t.Fatalf("取回的图对不上：mime=%q len=%d", mime, len(data))
	}
	// 第二张只留了链接，字节应该是空的。
	_, data, sourceURL, err := store.generationImage(item.ID, 1)
	if err != nil {
		t.Fatalf("取第二张失败：%v", err)
	}
	if len(data) != 0 || sourceURL != "https://example.com/two.png" {
		t.Fatalf("第二张应只有链接：len=%d url=%q", len(data), sourceURL)
	}
	if _, _, _, err := store.generationImage(item.ID, 9); err == nil {
		t.Fatal("取不存在的序号应该报错")
	}
}

func TestGalleryVisibility(t *testing.T) {
	store := newTestStore(t)
	alice := newTestUser(t, store, "alice")
	bob := newTestUser(t, store, "bob")

	mine := seedGeneration(t, store, alice.ID, "alice 的图")
	seedGeneration(t, store, bob.ID, "bob 的图")

	// 各看各的。
	list, total, err := store.generationsByUser(alice.ID, 24, 0)
	if err != nil {
		t.Fatalf("列作品失败：%v", err)
	}
	if total != 1 || len(list) != 1 || list[0].Prompt != "alice 的图" {
		t.Fatalf("alice 应该只看到自己那条：total=%d list=%+v", total, list)
	}

	// 没公开时公开列表是空的。
	if _, total, err = store.publicGenerations(24, 0); err != nil || total != 0 {
		t.Fatalf("公开列表应为空：total=%d err=%v", total, err)
	}

	// 公开之后才出现。
	if _, err := store.setGenerationPublic(mine.ID, alice.ID, true); err != nil {
		t.Fatalf("公开失败：%v", err)
	}
	public, total, err := store.publicGenerations(24, 0)
	if err != nil || total != 1 {
		t.Fatalf("公开列表应有 1 条：total=%d err=%v", total, err)
	}
	if public[0].Username != "alice" {
		t.Fatalf("公开作品应带作者名，实际 %q", public[0].Username)
	}

	// 别人不能替 alice 取消公开。
	if _, err := store.setGenerationPublic(mine.ID, bob.ID, false); err == nil {
		t.Fatal("bob 不该能改 alice 的作品")
	}
	// 也不能删。
	if err := store.deleteGeneration(mine.ID, bob.ID); err == nil {
		t.Fatal("bob 不该能删 alice 的作品")
	}
	// 自己可以。
	if err := store.deleteGeneration(mine.ID, alice.ID); err != nil {
		t.Fatalf("alice 删自己的应该成功：%v", err)
	}
	if _, total, _ = store.publicGenerations(24, 0); total != 0 {
		t.Fatalf("删完公开列表应为空，实际 %d", total)
	}
	if _, err := store.generationByID(mine.ID); err == nil {
		t.Fatal("删掉的作品不该还能查到")
	}
}

func TestGalleryPaging(t *testing.T) {
	store := newTestStore(t)
	user := newTestUser(t, store, "alice")
	for i := 0; i < 5; i++ {
		seedGeneration(t, store, user.ID, "第 "+string(rune('A'+i))+" 张")
	}

	first, total, err := store.generationsByUser(user.ID, 2, 0)
	if err != nil || total != 5 || len(first) != 2 {
		t.Fatalf("第一页不对：total=%d len=%d err=%v", total, len(first), err)
	}
	second, _, err := store.generationsByUser(user.ID, 2, 2)
	if err != nil || len(second) != 2 {
		t.Fatalf("第二页不对：len=%d err=%v", len(second), err)
	}
	// 最新的排前面，两页不该重叠。
	if first[0].ID == second[0].ID {
		t.Fatal("分页结果重叠了")
	}
}

func TestAdminCanLogInAsUser(t *testing.T) {
	store := newTestStore(t)
	if err := store.ensureAdmin(); err != nil {
		t.Fatalf("建管理员失败：%v", err)
	}

	user, token, err := store.login(defaultAdminUser, defaultAdminPassword)
	if err != nil {
		t.Fatalf("管理端账号应能登用户端：%v", err)
	}
	if token == "" || user.Username != defaultAdminUser {
		t.Fatalf("登录结果不对：%+v", user)
	}
	if user.DisplayName != "管理端" {
		t.Fatalf("补的用户记录应标成管理端，实际 %q", user.DisplayName)
	}
	if !user.IsAdmin {
		t.Fatal("管理端账号登进来应该带 isAdmin")
	}

	// 普通用户不能带管理员标志，否则前端会给他显示「管理端」菜单。
	bob := newTestUser(t, store, "bob")
	if bob.IsAdmin {
		t.Fatal("普通用户不该是管理员")
	}

	// 再登一次不该又建一条。
	if _, _, err := store.login(defaultAdminUser, defaultAdminPassword); err != nil {
		t.Fatalf("第二次登录失败：%v", err)
	}
	list, err := store.users()
	if err != nil {
		t.Fatalf("列用户失败：%v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应该有两条用户记录，实际 %d", len(list))
	}

	// 密码不对仍然是 401。
	if _, _, err := store.login(defaultAdminUser, "wrong-password"); err == nil {
		t.Fatal("密码不对应该报错")
	}
}

// 老库补 is_admin 列时，已经存在的管理员用户行要能被打上标志。
func TestEnsureAdminMarksExistingUser(t *testing.T) {
	store := newTestStore(t)
	if err := store.ensureAdmin(); err != nil {
		t.Fatalf("建管理员失败：%v", err)
	}
	// 先按普通用户注册一个同名账号，模拟「管理员在用户端登过一次」的老状态。
	user := newTestUser(t, store, defaultAdminUser)
	if user.IsAdmin {
		t.Fatal("刚注册时不该是管理员")
	}
	// 重启时 ensureAdmin 应该把这一行补上标志。
	if err := store.ensureAdmin(); err != nil {
		t.Fatalf("第二次 ensureAdmin 失败：%v", err)
	}
	after, err := store.userByID(user.ID)
	if err != nil {
		t.Fatalf("查用户失败：%v", err)
	}
	if !after.IsAdmin {
		t.Fatal("ensureAdmin 应该给已有的同名用户行补上管理员标志")
	}
}
