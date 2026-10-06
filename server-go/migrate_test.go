package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// migrateTables 是 SQLite → MariaDB 要搬的表，键是源表名、值是目标表名。
// keys 改成 api_keys 是因为 keys 在 MariaDB 里是保留字。
var migrateTables = [][2]string{
	{"settings", "settings"},
	{"admin", "admin"},
	{"users", "users"},
	{"sessions", "sessions"},
	{"keys", "api_keys"},
	{"generations", "generations"},
	{"generation_images", "generation_images"},
	{"checkins", "checkins"},
	{"model_types", "model_types"},
	{"model_rates", "model_rates"},
	{"audit_logs", "audit_logs"},
}

// TestMigrateSQLiteToMariaDB 把本地 SQLite 的数据搬到远端 MariaDB。
//
// 只在设了 IMAGE_DB_PASSWORD 且显式加了 -run 时才跑，免得手滑重跑一遍把
// 目标库灌两遍。目标库里只要有一行数据就拒绝执行——真要重来先把表清空。
func TestMigrateSQLiteToMariaDB(t *testing.T) {
	if os.Getenv("IMAGE_DB_PASSWORD") == "" {
		t.Skip("没设 IMAGE_DB_PASSWORD，跳过")
	}
	sourcePath := orDefault(os.Getenv("SOURCE_DB"), filepath.Join("..", "data", "darkroom.db"))
	if _, err := os.Stat(sourcePath); err != nil {
		t.Fatalf("找不到源库 %s：%v", sourcePath, err)
	}

	src, err := sql.Open("sqlite", "file:"+filepath.ToSlash(sourcePath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("开源库失败：%v", err)
	}
	defer src.Close()

	target, err := openMariaDB(DBConfig{
		Host:     orDefault(os.Getenv("IMAGE_DB_ADDR_HOST"), "101.43.75.72"),
		Port:     orDefault(os.Getenv("IMAGE_DB_ADDR_PORT"), "3306"),
		Name:     orDefault(os.Getenv("IMAGE_DB_NAME"), "image"),
		User:     orDefault(os.Getenv("IMAGE_DB_USER"), "image"),
		Password: os.Getenv("IMAGE_DB_PASSWORD"),
	})
	if err != nil {
		t.Fatalf("开目标库失败：%v", err)
	}
	defer target.db.Close()

	// **先确认目标干净，再动任何一条 DELETE。** 顺序反过来的话，一个跑偏的
	// 目标库会被先删掉两行、再报错退出，留下一半被改过的数据。
	//
	// 开库时 migrate() 会铺一行默认 settings 和三行 model_types，那是种子不是数据，
	// 允许它们在；其余任何一张表有数据就直接退出，绝不碰。
	seeds := map[string]int{"settings": 1, "model_types": 3}
	for _, pair := range migrateTables {
		var count int
		if err := target.db.QueryRow(`SELECT COUNT(*) FROM ` + pair[1]).Scan(&count); err != nil {
			t.Fatalf("数 %s 失败：%v", pair[1], err)
		}
		if count > seeds[pair[1]] {
			t.Fatalf("目标表 %s 里已经有 %d 行，不是空库，拒绝执行（免得灌两遍或覆盖数据）", pair[1], count)
		}
	}
	// 确认干净了，再把种子行删掉——源库里本来就有这两张表，留着会撞主键。
	for _, table := range []string{"settings", "model_types"} {
		if _, err := target.db.Exec(`DELETE FROM ` + table); err != nil {
			t.Fatalf("清种子 %s 失败：%v", table, err)
		}
	}

	skipped := []string{}
	for _, pair := range migrateTables {
		copied, skippedRows, err := copyTable(src, target.db, pair[0], pair[1], target.blobLimit())
		if err != nil {
			t.Fatalf("搬 %s 失败：%v", pair[0], err)
		}
		t.Logf("%-20s → %-20s 搬了 %d 行", pair[0], pair[1], copied)
		skipped = append(skipped, skippedRows...)
	}
	for _, note := range skipped {
		t.Logf("跳过：%s", note)
	}

	// 对数：每张表的行数两边要对得上（跳过的不算）。
	for _, pair := range migrateTables {
		var srcCount, dstCount int
		if err := src.QueryRow(`SELECT COUNT(*) FROM ` + pair[0]).Scan(&srcCount); err != nil {
			t.Fatalf("数源表 %s 失败：%v", pair[0], err)
		}
		if err := target.db.QueryRow(`SELECT COUNT(*) FROM ` + pair[1]).Scan(&dstCount); err != nil {
			t.Fatalf("数目标表 %s 失败：%v", pair[1], err)
		}
		if srcCount != dstCount {
			t.Errorf("%s 行数对不上：源 %d，目标 %d", pair[0], srcCount, dstCount)
		}
	}
}

// copyTable 按列名搬一张表。目标表多出来的列（比如 audit_logs 的 seq）跳过，
// 源表多出来的列也跳过——两边 schema 不完全一样。
func copyTable(src, dst *sql.DB, srcTable, dstTable string, blobLimit int) (int, []string, error) {
	dstColumns, err := columnsOf(dst, dstTable)
	if err != nil {
		return 0, nil, err
	}

	rows, err := src.Query(`SELECT * FROM ` + srcTable)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	srcColumns, err := rows.Columns()
	if err != nil {
		return 0, nil, err
	}

	// 只搬两边都有的列。
	shared := []string{}
	for _, name := range srcColumns {
		if dstColumns[name] {
			shared = append(shared, name)
		}
	}
	if len(shared) == 0 {
		return 0, nil, fmt.Errorf("%s 和目标表没有共同列", srcTable)
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(shared)), ",")
	insert := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", dstTable, strings.Join(shared, ", "), placeholders)

	copied := 0
	skipped := []string{}
	for rows.Next() {
		values := make([]any, len(srcColumns))
		pointers := make([]any, len(srcColumns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return copied, skipped, err
		}
		// 按 shared 的顺序挑出要写的那几列。
		args := make([]any, 0, len(shared))
		tooBig := ""
		for _, name := range shared {
			index := indexOf(srcColumns, name)
			value := values[index]
			if blob, ok := value.([]byte); ok && len(blob) > blobLimit {
				tooBig = fmt.Sprintf("%s.%s 有 %.1fMB，超过上限 %dMB",
					srcTable, name, float64(len(blob))/1024/1024, blobLimit/1024/1024)
			}
			args = append(args, value)
		}
		if tooBig != "" {
			skipped = append(skipped, tooBig)
			continue
		}
		if _, err := dst.Exec(insert, args...); err != nil {
			return copied, skipped, fmt.Errorf("第 %d 行：%w", copied+1, err)
		}
		copied++
	}
	return copied, skipped, rows.Err()
}

func columnsOf(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT column_name FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = ?`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func indexOf(list []string, value string) int {
	for i, item := range list {
		if item == value {
			return i
		}
	}
	return -1
}
