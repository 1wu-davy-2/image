package main

import (
	"os"
	"testing"
)

// 用代码真正走的那条语句，往真表里写一张接近上限的图，再读回来比对。
func TestBigBlobThroughStore(t *testing.T) {
	if os.Getenv("IMAGE_DB_PASSWORD") == "" { t.Skip() }
	store, err := openMariaDB(DBConfig{
		Host: "101.43.75.72", Port: "3306", Name: "image",
		User: "image", Password: os.Getenv("IMAGE_DB_PASSWORD"),
	})
	if err != nil { t.Fatal(err) }
	defer store.db.Close()
	t.Logf("blobLimit = %dMB", store.blobLimit()/1024/1024)

	genID, imgID := "blobcheck_gen", "blobcheck_img"
	store.db.Exec(`DELETE FROM generation_images WHERE id = ?`, imgID)
	store.db.Exec(`DELETE FROM generations WHERE id = ?`, genID)
	defer func() {
		store.db.Exec(`DELETE FROM generation_images WHERE id = ?`, imgID)
		store.db.Exec(`DELETE FROM generations WHERE id = ?`, genID)
	}()
	if _, err := store.db.Exec(`INSERT INTO generations (id, user_id, prompt, protocol, model, created_at)
		VALUES (?, 'x', '大图测试', 'gpt', 'm', '2026-01-01T00:00:00Z')`, genID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO generation_images (id, generation_id, position, mime)
		VALUES (?, ?, 0, 'image/png')`, imgID, genID); err != nil {
		t.Fatal(err)
	}

	// 比上限小 1MB，走的就是 fillGenerationImages 里那条 UPDATE。
	size := store.blobLimit() - 1024*1024
	data := make([]byte, size)
	for i := range data { data[i] = byte(i % 251) }
	if _, err := store.db.Exec(`UPDATE generation_images SET bytes = ?, mime = ? WHERE id = ?`,
		data, "image/png", imgID); err != nil {
		t.Fatalf("写 %dMB 失败：%v", size/1024/1024, err)
	}
	t.Logf("写 %dMB：成功", size/1024/1024)

	// 读回来比对，确认没被截断。
	var got []byte
	if err := store.db.QueryRow(`SELECT bytes FROM generation_images WHERE id = ?`, imgID).Scan(&got); err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	if len(got) != size {
		t.Fatalf("读回来是 %d 字节，写进去是 %d 字节", len(got), size)
	}
	for i := range got {
		if got[i] != byte(i%251) { t.Fatalf("第 %d 字节对不上", i) }
	}
	t.Logf("读回 %dMB：逐字节一致", len(got)/1024/1024)
}
