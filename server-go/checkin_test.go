package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestCheckinRange(t *testing.T) {
	store := newTestStore(t)
	user := newTestUser(t, store, "alice")

	if _, err := store.updateSettings(2, 5, 1); err != nil {
		t.Fatalf("存规则失败：%v", err)
	}

	// 每次签到都要落在区间里，额度按同样的数加上去。
	seen := map[int]bool{}
	expected := user.Quota
	for i := 0; i < 60; i++ {
		amount, updated, err := store.checkin(user.ID)
		if err != nil {
			t.Fatalf("第 %d 次签到失败：%v", i, err)
		}
		if amount < 2 || amount > 5 {
			t.Fatalf("签到额度 %d 掉出了 2-5", amount)
		}
		seen[amount] = true
		expected += amount
		if updated.Quota != expected {
			t.Fatalf("第 %d 次之后额度应该是 %d，实际 %d", i, expected, updated.Quota)
		}
		// 签过之后把日期往回拨，好让下一次还能签。
		if _, err := store.db.Exec(`UPDATE users SET last_checkin_date = '' WHERE id = ?`, user.ID); err != nil {
			t.Fatalf("重置签到日期失败：%v", err)
		}
	}
	// 60 次都抽不到第二个值，说明根本没随机。
	if len(seen) < 2 {
		t.Fatalf("区间里应该随机出多个值，实际只有 %v", seen)
	}
}

func TestCheckinFixedWhenMinEqualsMax(t *testing.T) {
	store := newTestStore(t)
	user := newTestUser(t, store, "alice")

	if _, err := store.updateSettings(4, 4, 1); err != nil {
		t.Fatalf("存规则失败：%v", err)
	}
	for i := 0; i < 5; i++ {
		amount, _, err := store.checkin(user.ID)
		if err != nil {
			t.Fatalf("签到失败：%v", err)
		}
		if amount != 4 {
			t.Fatalf("min = max 时应该固定给 4，实际 %d", amount)
		}
		if _, err := store.db.Exec(`UPDATE users SET last_checkin_date = '' WHERE id = ?`, user.ID); err != nil {
			t.Fatalf("重置失败：%v", err)
		}
	}
}

func TestCheckinRangeValidation(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.updateSettings(5, 2, 1); err == nil {
		t.Fatal("最小值大于最大值应该报错")
	}
	if _, err := store.updateSettings(-1, 5, 1); err == nil {
		t.Fatal("负数应该报错")
	}
	if _, err := store.updateSettings(0, 1001, 1); err == nil {
		t.Fatal("超过 1000 应该报错")
	}
	// 0-0 是合法的（签到不给额度）。
	if _, err := store.updateSettings(0, 0, 1); err != nil {
		t.Fatalf("0-0 应该合法：%v", err)
	}
}

func TestCheckinRecords(t *testing.T) {
	store := newTestStore(t)
	alice := newTestUser(t, store, "alice")
	bob := newTestUser(t, store, "bob")

	if _, err := store.updateSettings(3, 3, 1); err != nil {
		t.Fatalf("存规则失败：%v", err)
	}
	if _, _, err := store.checkin(alice.ID); err != nil {
		t.Fatalf("签到失败：%v", err)
	}

	items, total, err := store.checkinsByUser(alice.ID, 10, 0)
	if err != nil {
		t.Fatalf("列记录失败：%v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("alice 应该有 1 条记录：total=%d len=%d", total, len(items))
	}
	if items[0].Amount != 3 || items[0].Day != todayShanghai() {
		t.Fatalf("记录内容不对：%+v", items[0])
	}

	// 别人的记录看不到。
	if _, total, _ := store.checkinsByUser(bob.ID, 10, 0); total != 0 {
		t.Fatalf("bob 不该看到 alice 的记录，实际 %d 条", total)
	}

	// 分页：直接塞 25 条，看每页切得对不对。
	for i := 0; i < 25; i++ {
		if _, err := store.db.Exec(
			`INSERT INTO checkins (id, user_id, day, amount, created_at) VALUES (?, ?, ?, ?, ?)`,
			newID(), alice.ID, "2026-01-01", i, "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("塞记录失败：%v", err)
		}
	}
	first, total, err := store.checkinsByUser(alice.ID, 10, 0)
	if err != nil || total != 26 || len(first) != 10 {
		t.Fatalf("第一页不对：total=%d len=%d err=%v", total, len(first), err)
	}
	last, _, err := store.checkinsByUser(alice.ID, 10, 20)
	if err != nil || len(last) != 6 {
		t.Fatalf("最后一页应该是 6 条：len=%d err=%v", len(last), err)
	}
	if first[0].ID == last[0].ID {
		t.Fatal("两页不该重叠")
	}
}

// 删用户要连签到记录一起删，别留下指不到人的孤儿行。
func TestDeleteUserRemovesCheckins(t *testing.T) {
	store := newTestStore(t)
	user := newTestUser(t, store, "alice")
	if _, _, err := store.checkin(user.ID); err != nil {
		t.Fatalf("签到失败：%v", err)
	}
	if err := store.deleteUser(user.ID); err != nil {
		t.Fatalf("删用户失败：%v", err)
	}
	var left int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM checkins WHERE user_id = ?`, user.ID).Scan(&left); err != nil {
		t.Fatalf("查记录失败：%v", err)
	}
	if left != 0 {
		t.Fatalf("删用户之后还剩 %d 条签到记录", left)
	}
}

// 老库的固定额度要能搬成 min = max 的区间，并把旧列删掉。
func TestCheckinRangeMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("建老库失败：%v", err)
	}
	if _, err := db.Exec(`
CREATE TABLE settings (
  id             INTEGER PRIMARY KEY CHECK (id = 1),
  checkin_quota  INTEGER NOT NULL,
  generate_cost  INTEGER NOT NULL
);
INSERT INTO settings (id, checkin_quota, generate_cost) VALUES (1, 8, 3);`); err != nil {
		t.Fatalf("写老库失败：%v", err)
	}
	db.Close()

	store, err := openStore(path)
	if err != nil {
		t.Fatalf("迁移失败：%v", err)
	}
	defer store.db.Close()

	settings, err := store.settings()
	if err != nil {
		t.Fatalf("读规则失败：%v", err)
	}
	if settings.CheckinMin != 8 || settings.CheckinMax != 8 {
		t.Fatalf("固定额度 8 应该搬成 8-8，实际 %d-%d", settings.CheckinMin, settings.CheckinMax)
	}
	if settings.GenerateCost != 3 {
		t.Fatalf("每次消耗应保持 3，实际 %d", settings.GenerateCost)
	}
	// 旧列该没了，不然新库老库结构不一致。
	columns, err := store.tableColumns("settings")
	if err != nil {
		t.Fatalf("读表结构失败：%v", err)
	}
	if columns["checkin_quota"] {
		t.Fatal("迁移之后不该还留着 checkin_quota")
	}
	// 再开一次不该出问题（迁移是幂等的）。
	store.db.Close()
	again, err := openStore(path)
	if err != nil {
		t.Fatalf("二次打开失败：%v", err)
	}
	defer again.db.Close()
	if s, _ := again.settings(); s.CheckinMin != 8 || s.CheckinMax != 8 {
		t.Fatalf("二次打开把规则改了：%+v", s)
	}
}
