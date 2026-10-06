package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	// 签到额度要在区间里随机，用 math/rand/v2；crypto/rand 留给 newID 和加盐。
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/scrypt"
	_ "modernc.org/sqlite"
)

const (
	defaultAdminUser     = "admin"
	defaultAdminPassword = "admin@123"
	sessionTTL           = 14 * 24 * time.Hour
	defaultGenerateCost  = 1
)

// 上海没有夏令时，用固定偏移就够，省得依赖 tzdata。
var shanghai = time.FixedZone("CST", 8*3600)

func todayShanghai() string { return time.Now().In(shanghai).Format("2006-01-02") }

type User struct {
	ID              string `json:"id"`
	Username        string `json:"username"`
	DisplayName     string `json:"displayName"`
	Phone           string `json:"phone"`
	Email           string `json:"email"`
	Quota           int    `json:"quota"`
	Disabled        bool   `json:"disabled"`
	IsAdmin         bool   `json:"isAdmin"`
	LastCheckinDate string `json:"lastCheckinDate"`
	CheckedInToday  bool   `json:"checkedInToday"`
	CreatedAt       string `json:"createdAt"`
}

// ModelInfo 是中转站 /v1/models 里的一条。名字常常和 id 一样，但有的会更好看
// （gpt-image-2.5-flare 的 display_name 是 "GPT Image 2.5 Flare"），所以两个都留。
type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Key struct {
	ID               string      `json:"id"`
	Name             string      `json:"name"`
	Protocol         string      `json:"protocol"`
	ModelType        string      `json:"modelType"`
	BaseURL          string      `json:"baseUrl"`
	APIKey           string      `json:"apiKey"`
	UserAgent        string      `json:"userAgent"`
	Balance          *float64    `json:"balance"`
	BalanceUnit      string      `json:"balanceUnit"`
	BalanceValid     *bool       `json:"balanceValid"`
	BalanceUpdatedAt string      `json:"balanceUpdatedAt"`
	BalanceError     string      `json:"balanceError"`
	Models           []ModelInfo `json:"models"`
	ModelsUpdatedAt  string      `json:"modelsUpdatedAt"`
	ModelsError      string      `json:"modelsError"`
	Enabled          bool        `json:"enabled"`
	Note             string      `json:"note"`
	LastUsedAt       int64       `json:"lastUsedAt"`
}

// AvailableModel 是汇总给用户端的一条：这个模型有几把 Key 能用。
type AvailableModel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Keys      int    `json:"keys"`
	Available bool   `json:"available"`
}

// ModelType 是给用户看的生图类型：GPT / GEMINI / GROK。
// 用户只挑这个，具体模型走 DefaultModel；Multiplier 是扣额度的倍率
// （稳定版贵一点，比如 1.5 倍）。
type ModelType struct {
	Type         string  `json:"type"`
	Label        string  `json:"label"`
	DefaultModel string  `json:"defaultModel"`
	Multiplier   float64 `json:"multiplier"`
}

// 三个类型是固定的，管理端只能改默认模型和倍率，不能增删。
var modelTypeList = []ModelType{
	{Type: "gpt", Label: "GPT", DefaultModel: "gpt-image-2.5"},
	{Type: "gemini", Label: "GEMINI", DefaultModel: "gemini-2.5-flash-image"},
	{Type: "grok", Label: "GROK", DefaultModel: "grok-2-image"},
}

func modelTypeLabel(name string) string {
	for _, item := range modelTypeList {
		if item.Type == name {
			return item.Label
		}
	}
	return ""
}

func validModelType(name string) bool { return modelTypeLabel(name) != "" }

// RelayBalance 是中转站那边一共还剩多少钱，给顶栏显示用。
// Keys 是真正报了余额的把数——没查过余额的 Key 不算在里面，所以这个数
// 只是「已知的那几把」的合计，不是账户全貌。
//
// Blurred 为真表示 Total 是编出来的（见 blurBalance），不是真账。
type RelayBalance struct {
	Total   float64 `json:"total"`
	Unit    string  `json:"unit"`
	Keys    int     `json:"keys"`
	Blurred bool    `json:"blurred"`
}

// 余额低于这个数就照实说：账上快没钱的时候得让人知道，不然生图开始报错
// 还以为是坏了。高于它就只透个「还够用」的印象。
const balanceBlurFloor = 10.0

// blurBalance 把真实余额换成给非管理员看的那份。
//
// 倍数按「用户 + 当天」定死，不是每次请求摇一次：同一个页面刷新两下数字就变，
// 看着像坏了，反而更让人盯着这个数看。
func blurBalance(balance RelayBalance, userID string) RelayBalance {
	if balance.Total < balanceBlurFloor {
		return balance
	}
	seed := fnv.New64a()
	seed.Write([]byte(userID))
	seed.Write([]byte(todayShanghai()))
	// 第二个种子随便给个常数：这里只要结果稳定，不需要密码学上的随机。
	rng := mrand.New(mrand.NewPCG(seed.Sum64(), 0x9e3779b97f4a7c15))
	factor := 5 + rng.IntN(6) // 5..10

	out := balance
	out.Total = math.Round(balance.Total*float64(factor)*100) / 100
	out.Blurred = true
	return out
}

// Settings 里签到额度是个闭区间：每次签到在 [CheckinMin, CheckinMax] 里随机。
// 两者相等就是固定额度。
type Settings struct {
	CheckinMin   int `json:"checkinMin"`
	CheckinMax   int `json:"checkinMax"`
	GenerateCost int `json:"generateCost"`
}

// Checkin 是一条签到记录。
type Checkin struct {
	ID        string `json:"id"`
	Day       string `json:"day"`
	Amount    int    `json:"amount"`
	CreatedAt string `json:"createdAt"`
}

type AdminState struct {
	Settings    Settings           `json:"settings"`
	Keys        []Key              `json:"keys"`
	Users       []User             `json:"users"`
	ModelTypes  []ModelType        `json:"modelTypes"`
	ModelRates  map[string]float64 `json:"modelRates"`
	CheckinDate string             `json:"checkinDate"`
}

type Store struct {
	db *sql.DB
	// SQLite 一次只允许一个写事务，用互斥锁把「读-改-写」串起来。
	mu sync.Mutex
}

func openStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS settings (
  id             INTEGER PRIMARY KEY CHECK (id = 1),
  checkin_min    INTEGER NOT NULL DEFAULT 5,
  checkin_max    INTEGER NOT NULL DEFAULT 5,
  generate_cost  INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS admin (
  id       INTEGER PRIMARY KEY CHECK (id = 1),
  username TEXT NOT NULL,
  salt     BLOB NOT NULL,
  hash     BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS users (
  id                TEXT PRIMARY KEY,
  username          TEXT NOT NULL,
  username_lower    TEXT NOT NULL UNIQUE,
  display_name      TEXT NOT NULL DEFAULT '',
  phone             TEXT NOT NULL DEFAULT '',
  email             TEXT NOT NULL DEFAULT '',
  email_lower       TEXT NOT NULL DEFAULT '',
  salt              BLOB NOT NULL,
  hash              BLOB NOT NULL,
  quota             INTEGER NOT NULL DEFAULT 0,
  disabled          INTEGER NOT NULL DEFAULT 0,
  is_admin          INTEGER NOT NULL DEFAULT 0,
  last_checkin_date TEXT NOT NULL DEFAULT '',
  created_at        TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  token      TEXT PRIMARY KEY,
  role       TEXT NOT NULL,
  user_id    TEXT NOT NULL DEFAULT '',
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user_id);
CREATE TABLE IF NOT EXISTS keys (
  id                 TEXT PRIMARY KEY,
  name               TEXT NOT NULL,
  protocol           TEXT NOT NULL,
  base_url           TEXT NOT NULL,
  api_key            TEXT NOT NULL,
  user_agent         TEXT NOT NULL DEFAULT '',
  balance            REAL,
  balance_unit       TEXT NOT NULL DEFAULT 'USD',
  balance_valid      INTEGER,
  balance_updated_at TEXT NOT NULL DEFAULT '',
  balance_error      TEXT NOT NULL DEFAULT '',
  models             TEXT NOT NULL DEFAULT '',
  models_updated_at  TEXT NOT NULL DEFAULT '',
  models_error       TEXT NOT NULL DEFAULT '',
  enabled            INTEGER NOT NULL DEFAULT 1,
  note               TEXT NOT NULL DEFAULT '',
  last_used_at       INTEGER NOT NULL DEFAULT 0,
  created_at         TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS generations (
  id          TEXT PRIMARY KEY,
  user_id     TEXT NOT NULL,
  prompt      TEXT NOT NULL,
  protocol    TEXT NOT NULL,
  model       TEXT NOT NULL,
  size_label  TEXT NOT NULL DEFAULT '',
  channel     TEXT NOT NULL DEFAULT '',
  task_id     TEXT NOT NULL DEFAULT '',
  is_public   INTEGER NOT NULL DEFAULT 0,
  created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS generations_user ON generations(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS generations_public ON generations(is_public, created_at DESC);
CREATE TABLE IF NOT EXISTS generation_images (
  id            TEXT PRIMARY KEY,
  generation_id TEXT NOT NULL,
  position      INTEGER NOT NULL,
  mime          TEXT NOT NULL DEFAULT '',
  source_url    TEXT NOT NULL DEFAULT '',
  bytes         BLOB
);
CREATE INDEX IF NOT EXISTS generation_images_gen ON generation_images(generation_id, position);
CREATE TABLE IF NOT EXISTS checkins (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL,
  day        TEXT NOT NULL,
  amount     INTEGER NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS checkins_user ON checkins(user_id, created_at DESC);
CREATE TABLE IF NOT EXISTS model_types (
  type          TEXT PRIMARY KEY,
  default_model TEXT NOT NULL DEFAULT '',
  multiplier    REAL NOT NULL DEFAULT 1,
  updated_at    TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS model_rates (
  model_id   TEXT PRIMARY KEY,
  multiplier REAL NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS audit_logs (
  id         TEXT PRIMARY KEY,
  actor_kind TEXT NOT NULL DEFAULT 'user',
  actor_id   TEXT NOT NULL DEFAULT '',
  actor_name TEXT NOT NULL DEFAULT '',
  action     TEXT NOT NULL,
  target     TEXT NOT NULL DEFAULT '',
  detail     TEXT NOT NULL DEFAULT '',
  ip         TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_logs_created ON audit_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS audit_logs_action ON audit_logs(action, created_at DESC);
INSERT OR IGNORE INTO settings (id) VALUES (1);
`)
	if err != nil {
		return err
	}
	// 老库补列：SQLite 没有 ADD COLUMN IF NOT EXISTS，先查 PRAGMA 再补。
	if err := s.addMissingColumns("users", [][2]string{
		{"display_name", "TEXT NOT NULL DEFAULT ''"},
		{"phone", "TEXT NOT NULL DEFAULT ''"},
		{"email", "TEXT NOT NULL DEFAULT ''"},
		{"email_lower", "TEXT NOT NULL DEFAULT ''"},
		{"is_admin", "INTEGER NOT NULL DEFAULT 0"},
	}); err != nil {
		return err
	}
	if err := s.addMissingColumns("keys", [][2]string{
		{"models", "TEXT NOT NULL DEFAULT ''"},
		{"models_updated_at", "TEXT NOT NULL DEFAULT ''"},
		{"models_error", "TEXT NOT NULL DEFAULT ''"},
		{"model_type", "TEXT NOT NULL DEFAULT ''"},
	}); err != nil {
		return err
	}
	// 邮箱要能当登录名，所以唯一。老库补列留下的空串不参与唯一，故用条件索引。
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS users_email ON users(email_lower) WHERE email_lower <> ''`); err != nil {
		return err
	}
	if err := s.migrateCheckinRange(); err != nil {
		return err
	}
	return s.migrateModelTypes()
}

// migrateModelTypes 给生图类型表铺好三行，并把老 Key 按调用方式猜一个类型。
//
// 生图类型（GPT / GEMINI / GROK）是给用户看的分组，调用方式还是管请求形状，
// 两者并存。老库的 Key 只有调用方式，这里按最接近的对应关系猜一次；
// 猜错了管理端改一下就行，不猜的话这些 Key 会挑不出来，用户直接生不了图。
func (s *Store) migrateModelTypes() error {
	for _, item := range modelTypeList {
		if _, err := s.db.Exec(
			`INSERT OR IGNORE INTO model_types (type, default_model, multiplier) VALUES (?, ?, 1)`,
			item.Type, item.DefaultModel,
		); err != nil {
			return err
		}
	}
	for protocol := range protocols {
		if _, err := s.db.Exec(
			`UPDATE keys SET model_type = ? WHERE model_type = '' AND protocol = ?`,
			guessModelType(protocol), protocol,
		); err != nil {
			return err
		}
	}
	return nil
}

// guessModelType 按调用方式猜一个生图类型。迁移老 Key 时用，管理端新建 Key
// 没填类型时也用它兜底——宁可猜一个让人去改，也别让这把 Key 直接挑不出来。
func guessModelType(protocol string) string {
	if protocol == "gpt" {
		return "gpt"
	}
	return "gemini"
}

// migrateCheckinRange 把老的固定签到额度搬成区间。
//
// settings 表早先只有 checkin_quota 一列，是个固定值；现在改成 checkin_min /
// checkin_max，签到时在区间里随机。搬完就把旧列删掉，让新旧库的结构一致。
func (s *Store) migrateCheckinRange() error {
	if err := s.addMissingColumns("settings", [][2]string{
		{"checkin_min", "INTEGER NOT NULL DEFAULT 5"},
		{"checkin_max", "INTEGER NOT NULL DEFAULT 5"},
	}); err != nil {
		return err
	}
	columns, err := s.tableColumns("settings")
	if err != nil {
		return err
	}
	if !columns["checkin_quota"] {
		return nil
	}
	// 老的固定值就是 min = max 的退化区间。
	if _, err := s.db.Exec(
		`UPDATE settings SET checkin_min = checkin_quota, checkin_max = checkin_quota`); err != nil {
		return err
	}
	_, err = s.db.Exec(`ALTER TABLE settings DROP COLUMN checkin_quota`)
	return err
}

// tableColumns 列出表上现有的列名。
func (s *Store) tableColumns(table string) (map[string]bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	existing := map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, ctype      string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		existing[name] = true
	}
	return existing, rows.Err()
}

// addMissingColumns 给已存在的表补上后加的列。
func (s *Store) addMissingColumns(table string, columns [][2]string) error {
	existing, err := s.tableColumns(table)
	if err != nil {
		return err
	}
	for _, column := range columns {
		if existing[column[0]] {
			continue
		}
		if _, err := s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column[0] + ` ` + column[1]); err != nil {
			return err
		}
	}
	return nil
}

/* ---------- 小工具 ---------- */

func newID() string {
	buf := make([]byte, 8)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

func newToken() string {
	buf := make([]byte, 24)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

func hashPassword(password string) (salt, hash []byte) {
	salt = make([]byte, 16)
	rand.Read(salt)
	hash, _ = scrypt.Key([]byte(password), salt, 16384, 8, 1, 32)
	return salt, hash
}

func verifyPassword(password string, salt, expected []byte) bool {
	actual, err := scrypt.Key([]byte(password), salt, 16384, 8, 1, 32)
	if err != nil || len(actual) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func (s *Store) now() string { return time.Now().UTC().Format(time.RFC3339) }

/* ---------- 启动时准备管理员 ---------- */

func (s *Store) ensureAdmin() error {
	if err := s.ensureAdminRow(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 管理端账号在用户表里可能已经有一行（登过用户端就会有），补上管理员标志。
	// 不能靠「名字等于 admin」来判断：管理员没登过用户端时，这个名字是能被抢注的。
	var name string
	if err := s.db.QueryRow(`SELECT username FROM admin WHERE id = 1`).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	_, err := s.db.Exec(`UPDATE users SET is_admin = 1 WHERE username_lower = ?`,
		strings.ToLower(strings.TrimSpace(name)))
	return err
}

func (s *Store) ensureAdminRow() error {
	username := strings.TrimSpace(os.Getenv("ADMIN_USER"))
	if username == "" {
		username = defaultAdminUser
	}
	envPassword := os.Getenv("ADMIN_PASSWORD")

	s.mu.Lock()
	defer s.mu.Unlock()

	var currentUser string
	var salt, hash []byte
	err := s.db.QueryRow(`SELECT username, salt, hash FROM admin WHERE id = 1`).Scan(&currentUser, &salt, &hash)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// 显式给了 ADMIN_PASSWORD 就每次启动都对齐，忘记密码时可以靠它找回。
	if envPassword != "" {
		if len(envPassword) < 6 {
			return errors.New("ADMIN_PASSWORD 至少 6 位")
		}
		if exists && currentUser == username && verifyPassword(envPassword, salt, hash) {
			return nil
		}
		newSalt, newHash := hashPassword(envPassword)
		if _, err := s.db.Exec(`INSERT INTO admin (id, username, salt, hash) VALUES (1, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET username = excluded.username, salt = excluded.salt, hash = excluded.hash`,
			username, newSalt, newHash); err != nil {
			return err
		}
		fmt.Printf("管理端账号已按环境变量对齐：%s\n", username)
		return nil
	}

	if exists {
		if currentUser != username {
			if _, err := s.db.Exec(`UPDATE admin SET username = ? WHERE id = 1`, username); err != nil {
				return err
			}
		}
		return nil
	}

	newSalt, newHash := hashPassword(defaultAdminPassword)
	if _, err := s.db.Exec(`INSERT INTO admin (id, username, salt, hash) VALUES (1, ?, ?, ?)`,
		username, newSalt, newHash); err != nil {
		return err
	}
	fmt.Printf("管理端默认账号：%s / %s\n", username, defaultAdminPassword)
	fmt.Println("服务只监听 127.0.0.1。登录后请到管理端把密码改掉。")
	return nil
}

/* ---------- 设置 ---------- */

func (s *Store) settings() (Settings, error) {
	var out Settings
	err := s.db.QueryRow(`SELECT checkin_min, checkin_max, generate_cost FROM settings WHERE id = 1`).
		Scan(&out.CheckinMin, &out.CheckinMax, &out.GenerateCost)
	return out, err
}

func (s *Store) updateSettings(checkinMin, checkinMax, cost int) (Settings, error) {
	if checkinMin < 0 || checkinMin > 1000 || checkinMax < 0 || checkinMax > 1000 {
		return Settings{}, fail(400, "签到额度需要是 0 到 1000 的整数")
	}
	if checkinMin > checkinMax {
		return Settings{}, fail(400, "签到额度的最小值不能大于最大值")
	}
	if cost < 0 || cost > 1000 {
		return Settings{}, fail(400, "每次消耗需要是 0 到 1000 的整数")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`UPDATE settings SET checkin_min = ?, checkin_max = ?, generate_cost = ? WHERE id = 1`,
		checkinMin, checkinMax, cost); err != nil {
		return Settings{}, err
	}
	return Settings{CheckinMin: checkinMin, CheckinMax: checkinMax, GenerateCost: cost}, nil
}

/* ---------- 用户 ---------- */

// 名称只收英文字母和数字，所以按字节数就是字数。
func validUsername(name string) bool {
	if len(name) < 2 || len(name) > 20 {
		return false
	}
	for _, r := range name {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		return false
	}
	return true
}

func validPassword(password string) bool {
	n := len([]rune(password))
	return n >= 8 && n <= 72
}

// 中文名是可选的，填了就别太长。
func validDisplayName(name string) bool {
	return len([]rune(name)) <= 24
}

// 大陆手机号：1 开头、第二位 3-9、共 11 位。
func validPhone(phone string) bool {
	if len(phone) != 11 || phone[0] != '1' || phone[1] < '3' || phone[1] > '9' {
		return false
	}
	for _, r := range phone {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// 邮箱只校验基本形状：本地部分@域名，域名至少带一个点。
var emailPattern = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$`)

func validEmail(email string) bool {
	return len(email) <= 254 && emailPattern.MatchString(email)
}

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var user User
	var disabled, isAdmin int
	var quota sql.NullInt64
	err := row.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Phone, &user.Email,
		&quota, &disabled, &isAdmin, &user.LastCheckinDate, &user.CreatedAt)
	if err != nil {
		return user, err
	}
	user.Quota = int(quota.Int64)
	user.Disabled = disabled != 0
	user.IsAdmin = isAdmin != 0
	user.CheckedInToday = user.LastCheckinDate == todayShanghai()
	return user, nil
}

const userColumns = `id, username, display_name, phone, email, quota, disabled, is_admin, last_checkin_date, created_at`

type RegisterInput struct {
	Username    string
	DisplayName string
	Password    string
	Phone       string
	Email       string
}

func (s *Store) register(in RegisterInput) (User, string, error) {
	name := strings.TrimSpace(in.Username)
	displayName := strings.TrimSpace(in.DisplayName)
	phone := strings.TrimSpace(in.Phone)
	email := strings.TrimSpace(in.Email)

	if !validUsername(name) {
		return User{}, "", fail(400, "名称需要 2 到 20 位，只能用英文字母和数字")
	}
	if !validDisplayName(displayName) {
		return User{}, "", fail(400, "中文名最多 24 个字")
	}
	if !validPassword(in.Password) {
		return User{}, "", fail(400, "密码至少 8 位")
	}
	if !validPhone(phone) {
		return User{}, "", fail(400, "手机号要填 11 位的大陆号码")
	}
	if !validEmail(email) {
		return User{}, "", fail(400, "邮箱格式不对")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var existing int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE username_lower = ?`, strings.ToLower(name)).Scan(&existing); err != nil {
		return User{}, "", err
	}
	if existing > 0 {
		return User{}, "", fail(409, "这个名称已经注册过")
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE email_lower = ?`, strings.ToLower(email)).Scan(&existing); err != nil {
		return User{}, "", err
	}
	if existing > 0 {
		return User{}, "", fail(409, "这个邮箱已经注册过")
	}

	salt, hash := hashPassword(in.Password)
	id := newID()
	if _, err := s.db.Exec(`INSERT INTO users
		(id, username, username_lower, display_name, phone, email, email_lower, salt, hash, quota, disabled, last_checkin_date, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, '', ?)`,
		id, name, strings.ToLower(name), displayName, phone, email, strings.ToLower(email), salt, hash, s.now()); err != nil {
		return User{}, "", err
	}
	token, err := s.createSession("user", id)
	if err != nil {
		return User{}, "", err
	}
	user, err := s.userByID(id)
	return user, token, err
}

// account 可以是名称，也可以是邮箱。管理端的账号密码同样能登用户端。
func (s *Store) login(account, password string) (User, string, error) {
	key := strings.ToLower(strings.TrimSpace(account))
	if key == "" {
		return User{}, "", fail(401, "请填名称或邮箱")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var id string
	var disabled int
	var salt, hash []byte
	err := s.db.QueryRow(`SELECT id, disabled, salt, hash FROM users WHERE username_lower = ? OR email_lower = ?`, key, key).
		Scan(&id, &disabled, &salt, &hash)
	switch {
	case err == nil && verifyPassword(password, salt, hash):
		if disabled != 0 {
			return User{}, "", fail(403, "这个账号已被停用")
		}
		return s.issueUserSession(id)
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return User{}, "", err
	}
	// 用户表里没有这个人（或密码不对），再看是不是管理端账号。
	return s.loginAsAdmin(key, password)
}

// 管理端账号登用户端：第一次成功时补一条用户记录，之后就走普通用户那一套
// ——额度、签到、生图都需要 users 表里有行。
func (s *Store) loginAsAdmin(key, password string) (User, string, error) {
	var name string
	var salt, hash []byte
	err := s.db.QueryRow(`SELECT username, salt, hash FROM admin WHERE id = 1`).Scan(&name, &salt, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", fail(401, "名称或密码不对")
	}
	if err != nil {
		return User{}, "", err
	}
	if strings.ToLower(strings.TrimSpace(name)) != key || !verifyPassword(password, salt, hash) {
		return User{}, "", fail(401, "名称或密码不对")
	}

	user, err := s.ensureUserForAdmin(name, password)
	if err != nil {
		return User{}, "", err
	}
	if user.Disabled {
		return User{}, "", fail(403, "这个账号已被停用")
	}
	return s.issueUserSession(user.ID)
}

func (s *Store) ensureUserForAdmin(name, password string) (User, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM users WHERE username_lower = ?`, strings.ToLower(name)).Scan(&id)
	switch {
	case err == nil:
		// 已经有行了（比如自己先注册过同名账号），补上管理员标志。
		if _, err := s.db.Exec(`UPDATE users SET is_admin = 1 WHERE id = ?`, id); err != nil {
			return User{}, err
		}
		return s.userByID(id)
	case !errors.Is(err, sql.ErrNoRows):
		return User{}, err
	}

	salt, hash := hashPassword(password)
	id = newID()
	// 手机号和邮箱留空：这是内部补的行，不占用唯一邮箱，也走不了邮箱登录。
	if _, err := s.db.Exec(`INSERT INTO users
		(id, username, username_lower, display_name, phone, email, email_lower, salt, hash, quota, disabled, is_admin, last_checkin_date, created_at)
		VALUES (?, ?, ?, '管理端', '', '', '', ?, ?, 0, 0, 1, '', ?)`,
		id, name, strings.ToLower(name), salt, hash, s.now()); err != nil {
		return User{}, err
	}
	return s.userByID(id)
}

func (s *Store) issueUserSession(userID string) (User, string, error) {
	token, err := s.createSession("user", userID)
	if err != nil {
		return User{}, "", err
	}
	user, err := s.userByID(userID)
	return user, token, err
}

func (s *Store) loginAdmin(username, password string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var expected string
	var salt, hash []byte
	err := s.db.QueryRow(`SELECT username, salt, hash FROM admin WHERE id = 1`).Scan(&expected, &salt, &hash)
	if err != nil {
		return "", fail(401, "管理端还没初始化，重启一次服务")
	}
	// 账号错和密码错都走一遍哈希，免得从耗时上分辨出来。
	nameOK := strings.EqualFold(strings.TrimSpace(username), expected)
	passwordOK := verifyPassword(password, salt, hash)
	if !nameOK || !passwordOK {
		return "", fail(401, "管理账号或密码不对")
	}
	return s.createSession("admin", "")
}

func (s *Store) changeAdminPassword(oldPassword, newPassword string) error {
	if !validPassword(newPassword) {
		return fail(400, "密码需要 6 到 72 位")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var salt, hash []byte
	if err := s.db.QueryRow(`SELECT salt, hash FROM admin WHERE id = 1`).Scan(&salt, &hash); err != nil {
		return err
	}
	if !verifyPassword(oldPassword, salt, hash) {
		return fail(401, "原密码不对")
	}
	newSalt, newHash := hashPassword(newPassword)
	_, err := s.db.Exec(`UPDATE admin SET salt = ?, hash = ? WHERE id = 1`, newSalt, newHash)
	return err
}

func (s *Store) createSession(role, userID string) (string, error) {
	token := newToken()
	_, err := s.db.Exec(`INSERT INTO sessions (token, role, user_id, expires_at) VALUES (?, ?, ?, ?)`,
		token, role, userID, time.Now().Add(sessionTTL).UnixMilli())
	return token, err
}

func (s *Store) logout(token string) error {
	if token == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// sessionUser 未登录、会话过期、账号被停用都返回 nil。
func (s *Store) sessionUser(token string) (*User, error) {
	if token == "" {
		return nil, nil
	}
	var role, userID string
	var expires int64
	err := s.db.QueryRow(`SELECT role, user_id, expires_at FROM sessions WHERE token = ?`, token).Scan(&role, &userID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if role != "user" || expires <= time.Now().UnixMilli() {
		return nil, nil
	}
	var disabled int
	err = s.db.QueryRow(`SELECT disabled FROM users WHERE id = ?`, userID).Scan(&disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if disabled != 0 {
		return nil, nil
	}
	user, err := s.userByID(userID)
	return &user, err
}

func (s *Store) sessionAdmin(token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	var expires int64
	err := s.db.QueryRow(`SELECT expires_at FROM sessions WHERE token = ? AND role = 'admin'`, token).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return expires > time.Now().UnixMilli(), nil
}

func (s *Store) userByID(id string) (User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

func (s *Store) users() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userColumns + ` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, user)
	}
	return out, rows.Err()
}

func (s *Store) checkin(userID string) (int, User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, err := s.userByID(userID)
	if err != nil {
		return 0, User{}, fail(401, "请先登录")
	}
	today := todayShanghai()
	if user.LastCheckinDate == today {
		return 0, User{}, fail(400, "今天已经签过到了")
	}
	settings, err := s.settings()
	if err != nil {
		return 0, User{}, err
	}
	// 区间里随机；min = max 时就是这个固定值。
	amount := settings.CheckinMin
	if settings.CheckinMax > settings.CheckinMin {
		amount += mrand.IntN(settings.CheckinMax - settings.CheckinMin + 1)
	}
	if _, err := s.db.Exec(`UPDATE users SET quota = quota + ?, last_checkin_date = ? WHERE id = ?`,
		amount, today, userID); err != nil {
		return 0, User{}, err
	}
	if _, err := s.db.Exec(`INSERT INTO checkins (id, user_id, day, amount, created_at) VALUES (?, ?, ?, ?, ?)`,
		newID(), userID, today, amount, s.now()); err != nil {
		return 0, User{}, err
	}
	updated, err := s.userByID(userID)
	return amount, updated, err
}

// checkinsByUser 倒序列出某个用户的签到记录，带总数好分页。
func (s *Store) checkinsByUser(userID string, limit, offset int) ([]Checkin, int, error) {
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM checkins WHERE user_id = ?`, userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT id, day, amount, created_at FROM checkins
		WHERE user_id = ? ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, userID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Checkin{}
	for rows.Next() {
		var item Checkin
		if err := rows.Scan(&item.ID, &item.Day, &item.Amount, &item.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, item)
	}
	return out, total, rows.Err()
}

func (s *Store) changeUserPassword(userID, oldPassword, newPassword string) error {
	if !validPassword(newPassword) {
		return fail(400, "密码需要 6 到 72 位")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var salt, hash []byte
	if err := s.db.QueryRow(`SELECT salt, hash FROM users WHERE id = ?`, userID).Scan(&salt, &hash); err != nil {
		return fail(401, "请先登录")
	}
	if !verifyPassword(oldPassword, salt, hash) {
		return fail(401, "原密码不对")
	}
	newSalt, newHash := hashPassword(newPassword)
	_, err := s.db.Exec(`UPDATE users SET salt = ?, hash = ? WHERE id = ?`, newSalt, newHash, userID)
	return err
}

func (s *Store) setUserQuota(id string, quota int) (User, error) {
	if quota < 0 || quota > 1000000 {
		return User{}, fail(400, "额度需要是 0 到 1000000 的整数")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE users SET quota = ? WHERE id = ?`, quota, id)
	if err != nil {
		return User{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, fail(404, "找不到这个用户")
	}
	return s.userByID(id)
}

func (s *Store) setUserDisabled(id string, disabled bool) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := 0
	if disabled {
		value = 1
	}
	res, err := s.db.Exec(`UPDATE users SET disabled = ? WHERE id = ?`, value, id)
	if err != nil {
		return User{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, fail(404, "找不到这个用户")
	}
	if disabled {
		// 停用要立刻踢掉已有会话。
		if _, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
			return User{}, err
		}
	}
	return s.userByID(id)
}

func (s *Store) resetUserPassword(id, newPassword string) (User, error) {
	if !validPassword(newPassword) {
		return User{}, fail(400, "密码需要 6 到 72 位")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	salt, hash := hashPassword(newPassword)
	res, err := s.db.Exec(`UPDATE users SET salt = ?, hash = ? WHERE id = ?`, salt, hash, id)
	if err != nil {
		return User{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, fail(404, "找不到这个用户")
	}
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return User{}, err
	}
	return s.userByID(id)
}

func (s *Store) deleteUser(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fail(404, "找不到这个用户")
	}
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return err
	}
	// 签到记录跟着走，不然会留下指不到人的孤儿行。
	_, err = s.db.Exec(`DELETE FROM checkins WHERE user_id = ?`, id)
	return err
}

/* ---------- Key ---------- */

const keyColumns = `id, name, protocol, model_type, base_url, api_key, user_agent, balance, balance_unit,
	balance_valid, balance_updated_at, balance_error, models, models_updated_at, models_error,
	enabled, note, last_used_at`

func scanKey(row interface{ Scan(...any) error }) (Key, error) {
	var key Key
	var balance sql.NullFloat64
	var valid sql.NullBool
	var enabled int
	var models string
	err := row.Scan(&key.ID, &key.Name, &key.Protocol, &key.ModelType, &key.BaseURL, &key.APIKey, &key.UserAgent,
		&balance, &key.BalanceUnit, &valid, &key.BalanceUpdatedAt, &key.BalanceError,
		&models, &key.ModelsUpdatedAt, &key.ModelsError, &enabled, &key.Note, &key.LastUsedAt)
	if err != nil {
		return key, err
	}
	if balance.Valid {
		value := balance.Float64
		key.Balance = &value
	}
	if valid.Valid {
		value := valid.Bool
		key.BalanceValid = &value
	}
	key.Enabled = enabled != 0
	// 存坏了就当没拉过，别让一行坏数据把整个 Key 列表带崩。
	key.Models = []ModelInfo{}
	if models != "" {
		if err := json.Unmarshal([]byte(models), &key.Models); err != nil {
			key.Models = []ModelInfo{}
		}
	}
	return key, nil
}

func (s *Store) keys() ([]Key, error) {
	rows, err := s.db.Query(`SELECT ` + keyColumns + ` FROM keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		key, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

func (s *Store) keyByID(id string) (Key, error) {
	return scanKey(s.db.QueryRow(`SELECT `+keyColumns+` FROM keys WHERE id = ?`, id))
}

// relayBalance 把启用中的 Key 的余额加起来。停用的 Key 不算——它已经不参与挑 Key 了，
// 把钱算进去会让顶栏那个数比实际能用的多。单位不一致时只合计先遇到的那个单位：
// 把 USD 和 CNY 直接相加得出的是个假数，宁可不显示那么多把。
func (s *Store) relayBalance() (RelayBalance, bool) {
	list, err := s.keys()
	if err != nil {
		return RelayBalance{}, false
	}
	var out RelayBalance
	for _, key := range list {
		if !key.Enabled || key.Balance == nil {
			continue
		}
		unit := key.BalanceUnit
		if unit == "" {
			unit = "USD"
		}
		if out.Keys == 0 {
			out.Unit = unit
		} else if unit != out.Unit {
			continue
		}
		out.Total += *key.Balance
		out.Keys++
	}
	return out, out.Keys > 0
}

type KeyInput struct {
	ID        string
	Name      string
	Protocol  string
	ModelType string
	BaseURL   string
	APIKey    string
	UserAgent string
	Balance   *float64
	Enabled   bool
	Note      string
}

func (s *Store) saveKey(in KeyInput) (Key, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > 40 {
		return Key{}, fail(400, "请填写 40 字以内的名称")
	}
	// 没填就按调用方式猜一个。老的调用方（管理端页面还没更新时）不会带这个字段，
	// 直接打回的话那把 Key 就存不进去了。猜错了管理端改一下就是。
	if in.ModelType == "" {
		in.ModelType = guessModelType(in.Protocol)
	} else if !validModelType(in.ModelType) {
		return Key{}, fail(400, "不认识的生图类型")
	}
	note := []rune(strings.TrimSpace(in.Note))
	if len(note) > 200 {
		note = note[:200]
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if in.ID == "" {
		id := newID()
		if _, err := s.db.Exec(`INSERT INTO keys (id, name, protocol, model_type, base_url, api_key, user_agent, balance,
			balance_unit, balance_valid, balance_updated_at, balance_error, enabled, note, last_used_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'USD', NULL, '', '', ?, ?, 0, ?)`,
			id, name, in.Protocol, in.ModelType, in.BaseURL, in.APIKey, in.UserAgent, in.Balance, boolToInt(in.Enabled), string(note), s.now()); err != nil {
			return Key{}, err
		}
		if in.APIKey == "" {
			return Key{}, fail(400, "请填写 API Key")
		}
		return s.keyByID(id)
	}

	existing, err := s.keyByID(in.ID)
	if err != nil {
		return Key{}, fail(404, "找不到这把 Key")
	}
	apiKey := existing.APIKey
	if in.APIKey != "" {
		apiKey = in.APIKey
	}
	if apiKey == "" {
		return Key{}, fail(400, "请填写 API Key")
	}
	// 缓存下来的余额属于当时那把凭据，换了就不算数了。
	credentialsChanged := (in.APIKey != "" && in.APIKey != existing.APIKey) || (in.BaseURL != "" && in.BaseURL != existing.BaseURL)
	valid := existing.BalanceValid
	balanceError := existing.BalanceError
	if credentialsChanged {
		valid = nil
		balanceError = ""
	}
	if _, err := s.db.Exec(`UPDATE keys SET name = ?, protocol = ?, model_type = ?, base_url = ?, api_key = ?, user_agent = ?,
		balance = ?, balance_valid = ?, balance_error = ?, enabled = ?, note = ? WHERE id = ?`,
		name, in.Protocol, in.ModelType, in.BaseURL, apiKey, in.UserAgent, in.Balance, boolPtrToInt(valid), balanceError,
		boolToInt(in.Enabled), string(note), in.ID); err != nil {
		return Key{}, err
	}
	return s.keyByID(in.ID)
}

func (s *Store) deleteKey(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM keys WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fail(404, "找不到这把 Key")
	}
	return nil
}

func (s *Store) setKeyBalance(id string, balance *float64, unit string, valid *bool, errMessage string) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if balance != nil {
		if _, err := s.db.Exec(`UPDATE keys SET balance = ?, balance_unit = ?, balance_valid = ?,
			balance_updated_at = ?, balance_error = '' WHERE id = ?`,
			*balance, unit, boolPtrToInt(valid), s.now(), id); err != nil {
			return Key{}, err
		}
	} else if errMessage != "" {
		trimmed := errMessage
		if len([]rune(trimmed)) > 300 {
			trimmed = string([]rune(trimmed)[:300])
		}
		if _, err := s.db.Exec(`UPDATE keys SET balance_error = ? WHERE id = ?`, trimmed, id); err != nil {
			return Key{}, err
		}
	}
	return s.keyByID(id)
}

// setKeyModels 记下这把 Key 能用的模型。errMessage 非空表示这次拉取失败了。
func (s *Store) setKeyModels(id string, models []ModelInfo, errMessage string) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if errMessage != "" {
		trimmed := errMessage
		if len([]rune(trimmed)) > 300 {
			trimmed = string([]rune(trimmed)[:300])
		}
		if _, err := s.db.Exec(`UPDATE keys SET models_error = ? WHERE id = ?`, trimmed, id); err != nil {
			return Key{}, err
		}
		return s.keyByID(id)
	}
	encoded, err := json.Marshal(models)
	if err != nil {
		return Key{}, err
	}
	if _, err := s.db.Exec(`UPDATE keys SET models = ?, models_updated_at = ?, models_error = '' WHERE id = ?`,
		string(encoded), s.now(), id); err != nil {
		return Key{}, err
	}
	return s.keyByID(id)
}

// availableModels 把各把 Key 拉到的模型按调用方式汇总。
//
// 一条模型只要有一把「启用、有凭据、中转没说过失效、余额还大于 0」的 Key 挂着，
// 就算可用；否则列出来但标成不可用——直接藏掉的话，用户只会看到空下拉框，
// 不知道是没拉过模型还是 Key 出了问题。
func (s *Store) availableModels() (map[string][]AvailableModel, error) {
	all, err := s.keys()
	if err != nil {
		return nil, err
	}
	type slot struct {
		name      string
		keys      int
		available bool
	}
	byProtocol := map[string]map[string]*slot{}
	for _, key := range all {
		if !key.Enabled || key.APIKey == "" || len(key.Models) == 0 {
			continue
		}
		usable := key.BalanceValid == nil || *key.BalanceValid
		if usable && key.Balance != nil && *key.Balance <= 0 {
			usable = false
		}
		group := byProtocol[key.Protocol]
		if group == nil {
			group = map[string]*slot{}
			byProtocol[key.Protocol] = group
		}
		for _, model := range key.Models {
			item := group[model.ID]
			if item == nil {
				item = &slot{name: model.Name}
				group[model.ID] = item
			}
			item.keys++
			if usable {
				item.available = true
			}
		}
	}

	out := map[string][]AvailableModel{}
	for protocol, group := range byProtocol {
		list := []AvailableModel{}
		for id, item := range group {
			name := item.name
			if name == "" {
				name = id
			}
			list = append(list, AvailableModel{ID: id, Name: name, Keys: item.keys, Available: item.available})
		}
		// 可用的排前面，同组按 id 排，顺序稳定。
		sort.Slice(list, func(i, j int) bool {
			if list[i].Available != list[j].Available {
				return list[i].Available
			}
			return list[i].ID < list[j].ID
		})
		out[protocol] = list
	}
	return out, nil
}

// pickKey 挑一把能用的：启用、凭据齐全、中转没说过它失效。
// 优先余额大于 0 的，按余额从大到小、最近最少用；余额未知的排最后。
func (s *Store) pickKey(protocol string) (Key, error) {
	all, err := s.keys()
	if err != nil {
		return Key{}, err
	}
	var known, unknown []Key
	for _, key := range all {
		if !key.Enabled || key.Protocol != protocol || key.APIKey == "" {
			continue
		}
		if key.BalanceValid != nil && !*key.BalanceValid {
			continue
		}
		if key.Balance != nil && *key.Balance > 0 {
			known = append(known, key)
		} else if key.Balance == nil {
			unknown = append(unknown, key)
		}
	}
	byBalance := func(a, b Key) bool {
		if *a.Balance != *b.Balance {
			return *a.Balance > *b.Balance
		}
		return a.LastUsedAt < b.LastUsedAt
	}
	sortKeys(known, byBalance)
	sortKeys(unknown, func(a, b Key) bool { return a.LastUsedAt < b.LastUsedAt })
	if len(known) > 0 {
		return known[0], nil
	}
	if len(unknown) > 0 {
		return unknown[0], nil
	}
	return Key{}, fail(400, "没有可用的生图 Key。请在管理端添加，或确认剩余余额大于 0。")
}

// modelTypes 按固定顺序返回三个类型，没铺过的行按默认值补上，
// 保证管理端和用户端看到的永远是三行。
func (s *Store) modelTypes() ([]ModelType, error) {
	rows, err := s.db.Query(`SELECT type, default_model, multiplier FROM model_types`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stored := map[string]ModelType{}
	for rows.Next() {
		var item ModelType
		if err := rows.Scan(&item.Type, &item.DefaultModel, &item.Multiplier); err != nil {
			return nil, err
		}
		stored[item.Type] = item
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ModelType, 0, len(modelTypeList))
	for _, fallback := range modelTypeList {
		item := fallback
		if saved, ok := stored[fallback.Type]; ok {
			item.DefaultModel = saved.DefaultModel
			item.Multiplier = saved.Multiplier
		}
		if item.Multiplier <= 0 {
			item.Multiplier = 1
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *Store) modelType(name string) (ModelType, error) {
	if !validModelType(name) {
		return ModelType{}, fail(400, "不认识的生图类型")
	}
	list, err := s.modelTypes()
	if err != nil {
		return ModelType{}, err
	}
	for _, item := range list {
		if item.Type == name {
			return item, nil
		}
	}
	return ModelType{}, fail(400, "不认识的生图类型")
}

// modelRates 返回所有设过的模型倍率。没设过的模型不在里面，取的时候按 1.0 算。
func (s *Store) modelRates() (map[string]float64, error) {
	rows, err := s.db.Query(`SELECT model_id, multiplier FROM model_rates`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var id string
		var rate float64
		if err := rows.Scan(&id, &rate); err != nil {
			return nil, err
		}
		out[id] = rate
	}
	return out, rows.Err()
}

// modelMultiplier 取一个模型的倍率，没设过就是 1.0——1.0 是乘法里的单位元，
// 不设就等于「这个模型不加价」，类型那一层的倍率照旧生效。
func (s *Store) modelMultiplier(modelID string) (float64, error) {
	if modelID == "" {
		return 1, nil
	}
	var rate float64
	err := s.db.QueryRow(`SELECT multiplier FROM model_rates WHERE model_id = ?`, modelID).Scan(&rate)
	if errors.Is(err, sql.ErrNoRows) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	if rate <= 0 {
		return 1, nil
	}
	return rate, nil
}

func (s *Store) saveModelRate(modelID string, multiplier float64) (map[string]float64, error) {
	modelID = strings.TrimSpace(modelID)
	if !modelRe.MatchString(modelID) {
		return nil, fail(400, "模型名不合法")
	}
	if multiplier < 0.1 || multiplier > 100 {
		return nil, fail(400, "倍率请填 0.1 到 100 之间")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(
		`INSERT INTO model_rates (model_id, multiplier, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(model_id) DO UPDATE SET multiplier = excluded.multiplier, updated_at = excluded.updated_at`,
		modelID, multiplier, s.now(),
	); err != nil {
		return nil, err
	}
	return s.modelRates()
}

func (s *Store) saveModelType(name, defaultModel string, multiplier float64) (ModelType, error) {
	if !validModelType(name) {
		return ModelType{}, fail(400, "不认识的生图类型")
	}
	defaultModel = strings.TrimSpace(defaultModel)
	if defaultModel != "" && !modelRe.MatchString(defaultModel) {
		return ModelType{}, fail(400, "默认模型名不合法")
	}
	if multiplier < 0.1 || multiplier > 100 {
		return ModelType{}, fail(400, "倍率请填 0.1 到 100 之间")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(
		`UPDATE model_types SET default_model = ?, multiplier = ?, updated_at = ? WHERE type = ?`,
		defaultModel, multiplier, s.now(), name,
	); err != nil {
		return ModelType{}, err
	}
	return s.modelType(name)
}

// pickKeyForType 按生图类型挑 Key。类型下面可能挂着好几把不同调用方式的 Key，
// 挑中那把的 protocol 决定这次请求走什么形状。
//
// 分三档挑，档内还是按余额和最近最少用排：
//  1. 模型列表里确实有这个默认模型的
//  2. 还没拉过模型列表的（不知道支不支持，不能用「不知道」当理由排除掉）
//  3. 拉了列表但里面没有这个模型的
//
// 第三档留着不删是为了兜底：模型列表可能是旧的，硬排除会让用户直接生不了图。
// 但只要有前两档，就先不用它——中转站按分组给模型，配了 Key 不代表这把 Key 的
// 账号支持这个模型，挑错了用户收到的就是「not supported by any configured account」
// 这种看不懂的报错。
func (s *Store) pickKeyForType(modelType, defaultModel string) (Key, error) {
	all, err := s.keys()
	if err != nil {
		return Key{}, err
	}
	var has, unknown, missing []Key
	for _, key := range all {
		if !key.Enabled || key.ModelType != modelType || key.APIKey == "" {
			continue
		}
		if key.BalanceValid != nil && !*key.BalanceValid {
			continue
		}
		// 余额 0 的不要，未知的留着。
		if key.Balance != nil && *key.Balance <= 0 {
			continue
		}
		switch {
		case len(key.Models) == 0:
			unknown = append(unknown, key)
		case defaultModel != "" && keyHasModel(key, defaultModel):
			has = append(has, key)
		default:
			missing = append(missing, key)
		}
	}

	// 余额已知的优先，然后余额大的、最近没怎么用的。
	byBalance := func(a, b Key) bool {
		if (a.Balance == nil) != (b.Balance == nil) {
			return b.Balance == nil
		}
		if a.Balance != nil && *a.Balance != *b.Balance {
			return *a.Balance > *b.Balance
		}
		return a.LastUsedAt < b.LastUsedAt
	}
	for _, group := range [][]Key{has, unknown, missing} {
		sortKeys(group, byBalance)
		if len(group) > 0 {
			return group[0], nil
		}
	}
	label := modelTypeLabel(modelType)
	if label == "" {
		label = modelType
	}
	return Key{}, fail(400, "「"+label+"」下面没有可用的生图 Key。请在管理端把 Key 挂到这个类型上，或确认它的余额大于 0。")
}

func keyHasModel(key Key, model string) bool {
	for _, item := range key.Models {
		if item.ID == model {
			return true
		}
	}
	return false
}

// reserveGeneration 扣额度并锁定一把 Key。units 是消耗次数（批量按条目数算）。
func (s *Store) reserveGeneration(userID, protocol string, units int) (Key, int, int, error) {
	return s.reserve(userID, 1, units, func() (Key, error) { return s.pickKey(protocol) })
}

// reserveForType 按生图类型扣额度并锁定一把 Key，返回那把 Key（它的 protocol
// 决定请求形状）和这个类型的设置（默认模型、倍率）。
//
// wantModel 是这次实际要用的模型，用户手动选过就是它，没选就传空、用类型的默认模型。
// 挑 Key 时按这个模型找，不是按默认模型——用户特意挑了个别的模型，
// 挑到一把没这个模型的 Key 就白挑了。
//
// 倍率是两层相乘：类型的倍率 × 模型的倍率。模型没设过就是 1.0，等于不加价。
func (s *Store) reserveForType(userID, modelType, wantModel string, units int) (Key, ModelType, int, int, error) {
	item, err := s.modelType(modelType)
	if err != nil {
		return Key{}, ModelType{}, 0, 0, err
	}
	if wantModel == "" {
		wantModel = item.DefaultModel
	}
	rate, err := s.modelMultiplier(wantModel)
	if err != nil {
		return Key{}, ModelType{}, 0, 0, err
	}
	key, cost, quota, err := s.reserve(userID, item.Multiplier*rate, units, func() (Key, error) {
		return s.pickKeyForType(modelType, wantModel)
	})
	if err != nil {
		return Key{}, ModelType{}, 0, 0, err
	}
	return key, item, cost, quota, nil
}

// reserve 是扣额度那套动作的公共部分：查用户、算费用、挑 Key、扣。
//
// cost = 每次消耗 × 倍率 × 次数，向上取整。宁可多扣一点也不少扣——1.5 倍的
// 模型按 1 倍收，倍率就等于没有；而且额度是整数，不取整根本扣不动。
func (s *Store) reserve(userID string, multiplier float64, units int, pick func() (Key, error)) (Key, int, int, error) {
	if units < 1 {
		units = 1
	}
	if multiplier <= 0 {
		multiplier = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	user, err := s.userByID(userID)
	if err != nil {
		return Key{}, 0, 0, fail(401, "请先登录")
	}
	if user.Disabled {
		return Key{}, 0, 0, fail(403, "这个账号已被停用")
	}
	settings, err := s.settings()
	if err != nil {
		return Key{}, 0, 0, err
	}
	cost := int(math.Ceil(float64(settings.GenerateCost) * multiplier * float64(units)))
	if cost < 1 {
		cost = 1
	}
	if user.Quota < cost {
		hint := "额度不足，可以先签到领取。"
		if user.LastCheckinDate == todayShanghai() {
			hint = "额度不足，明天可以再签到。"
		}
		return Key{}, 0, 0, fail(402, hint)
	}
	key, err := pick()
	if err != nil {
		return Key{}, 0, 0, err
	}
	if _, err := s.db.Exec(`UPDATE users SET quota = quota - ? WHERE id = ?`, cost, userID); err != nil {
		return Key{}, 0, 0, err
	}
	if _, err := s.db.Exec(`UPDATE keys SET last_used_at = ? WHERE id = ?`, time.Now().UnixMilli(), key.ID); err != nil {
		return Key{}, 0, 0, err
	}
	return key, cost, user.Quota - cost, nil
}

// keyFor 只取一把 Key，不扣额度（批量任务的查询与下载用）。
func (s *Store) keyFor(protocol string) (Key, error) { return s.pickKey(protocol) }

func (s *Store) refund(userID string, cost int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`UPDATE users SET quota = quota + ? WHERE id = ?`, cost, userID); err != nil {
		return 0
	}
	user, err := s.userByID(userID)
	if err != nil {
		return 0
	}
	return user.Quota
}

func (s *Store) adminState() (AdminState, error) {
	settings, err := s.settings()
	if err != nil {
		return AdminState{}, err
	}
	keys, err := s.keys()
	if err != nil {
		return AdminState{}, err
	}
	users, err := s.users()
	if err != nil {
		return AdminState{}, err
	}
	types, err := s.modelTypes()
	if err != nil {
		return AdminState{}, err
	}
	rates, err := s.modelRates()
	if err != nil {
		return AdminState{}, err
	}
	return AdminState{
		Settings: settings, Keys: keys, Users: users,
		ModelTypes: types, ModelRates: rates, CheckinDate: todayShanghai(),
	}, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func boolPtrToInt(value *bool) any {
	if value == nil {
		return nil
	}
	return boolToInt(*value)
}

func sortKeys(list []Key, less func(a, b Key) bool) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && less(list[j], list[j-1]); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

/* ---------- 作品：生成记录与公开作品 ---------- */

type GenerationImage struct {
	Position int    `json:"position"`
	Mime     string `json:"mime"`
}

// Generation 是一条生成记录。图片本体不在这里，走
// /api/generations/{id}/images/{position} 单独取。
type Generation struct {
	ID          string            `json:"id"`
	Username    string            `json:"username"`
	DisplayName string            `json:"displayName"`
	Prompt      string            `json:"prompt"`
	Protocol    string            `json:"protocol"`
	Model       string            `json:"model"`
	SizeLabel   string            `json:"sizeLabel"`
	Channel     string            `json:"channel"`
	TaskID      string            `json:"taskId"`
	IsPublic    bool              `json:"isPublic"`
	Mine        bool              `json:"mine"`
	CreatedAt   string            `json:"createdAt"`
	Images      []GenerationImage `json:"images"`

	// 不给前端，只在算 Mine 和鉴权时用。
	userID string
}

// StoredImage 是准备落库的一张图。Data 为 nil 时只留 URL，
// 后台再补拉（见 fillGenerationImages）。
type StoredImage struct {
	Mime string
	Data []byte
	URL  string
}

type GenerationInput struct {
	UserID    string
	Prompt    string
	Protocol  string
	Model     string
	SizeLabel string
	Channel   string
	TaskID    string
	Images    []StoredImage
}

const generationFrom = `FROM generations g LEFT JOIN users u ON u.id = g.user_id`

const generationColumns = `g.id, g.prompt, g.protocol, g.model, g.size_label, g.channel,
	g.task_id, g.is_public, g.created_at, COALESCE(u.username, ''), COALESCE(u.display_name, ''), g.user_id`

func scanGeneration(row interface{ Scan(...any) error }) (Generation, error) {
	var out Generation
	var isPublic int
	if err := row.Scan(&out.ID, &out.Prompt, &out.Protocol, &out.Model, &out.SizeLabel, &out.Channel,
		&out.TaskID, &isPublic, &out.CreatedAt, &out.Username, &out.DisplayName, &out.userID); err != nil {
		return out, err
	}
	out.IsPublic = isPublic != 0
	out.Images = []GenerationImage{}
	return out, nil
}

// 图片单独一次查出来，省得列表里每条都挂一个子查询。
func (s *Store) attachImages(list []Generation) error {
	if len(list) == 0 {
		return nil
	}
	index := make(map[string]int, len(list))
	marks := make([]string, 0, len(list))
	args := make([]any, 0, len(list))
	for i, item := range list {
		index[item.ID] = i
		marks = append(marks, "?")
		args = append(args, item.ID)
	}
	rows, err := s.db.Query(`SELECT generation_id, position, mime FROM generation_images
		WHERE generation_id IN (`+strings.Join(marks, ",")+`) ORDER BY generation_id, position`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var image GenerationImage
		if err := rows.Scan(&id, &image.Position, &image.Mime); err != nil {
			return err
		}
		if at, ok := index[id]; ok {
			list[at].Images = append(list[at].Images, image)
		}
	}
	return rows.Err()
}

// generationLocked 调用方自己持锁。
func (s *Store) generationLocked(id string) (Generation, error) {
	out, err := scanGeneration(s.db.QueryRow(`SELECT `+generationColumns+` `+generationFrom+` WHERE g.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Generation{}, fail(404, "找不到这条作品")
	}
	if err != nil {
		return Generation{}, err
	}
	list := []Generation{out}
	if err := s.attachImages(list); err != nil {
		return Generation{}, err
	}
	return list[0], nil
}

func (s *Store) generationByID(id string) (Generation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generationLocked(id)
}

func (s *Store) saveGeneration(in GenerationInput) (Generation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := newID()
	// 新图默认公开：画坊那页本来就是给人看的，想藏起来的人自己去作品集点「取消公开」。
	if _, err := s.db.Exec(`INSERT INTO generations
		(id, user_id, prompt, protocol, model, size_label, channel, task_id, is_public, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		id, in.UserID, in.Prompt, in.Protocol, in.Model, in.SizeLabel, in.Channel, in.TaskID, s.now()); err != nil {
		return Generation{}, err
	}
	for position, image := range in.Images {
		if _, err := s.db.Exec(`INSERT INTO generation_images
			(id, generation_id, position, mime, source_url, bytes) VALUES (?, ?, ?, ?, ?, ?)`,
			newID(), id, position, image.Mime, image.URL, image.Data); err != nil {
			return Generation{}, err
		}
	}
	return s.generationLocked(id)
}

// generations 收一个 WHERE 片段，用户作品集和公开作品共用同一套分页。
func (s *Store) generations(where string, args []any, limit, offset int) ([]Generation, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) `+generationFrom+` `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	pageArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.db.Query(`SELECT `+generationColumns+` `+generationFrom+` `+where+
		` ORDER BY g.created_at DESC, g.id DESC LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	list := []Generation{}
	for rows.Next() {
		item, err := scanGeneration(rows)
		if err != nil {
			return nil, 0, err
		}
		list = append(list, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if err := s.attachImages(list); err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

func (s *Store) setUserDisplayName(userID, displayName string) (User, error) {
	name := strings.TrimSpace(displayName)
	if !validDisplayName(name) {
		return User{}, fail(400, "中文名最多 24 个字")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`UPDATE users SET display_name = ? WHERE id = ?`, name, userID); err != nil {
		return User{}, err
	}
	return s.userByID(userID)
}

func (s *Store) generationsByUser(userID string, limit, offset int) ([]Generation, int, error) {
	return s.generations(`WHERE g.user_id = ?`, []any{userID}, limit, offset)
}

func (s *Store) publicGenerations(limit, offset int) ([]Generation, int, error) {
	return s.generations(`WHERE g.is_public = 1`, nil, limit, offset)
}

func (s *Store) deleteGeneration(id, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM generations WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fail(404, "找不到这条作品")
	}
	if _, err := s.db.Exec(`DELETE FROM generation_images WHERE generation_id = ?`, id); err != nil {
		return err
	}
	return nil
}

func (s *Store) setGenerationPublic(id, userID string, isPublic bool) (Generation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE generations SET is_public = ? WHERE id = ? AND user_id = ?`,
		boolToInt(isPublic), id, userID)
	if err != nil {
		return Generation{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Generation{}, fail(404, "找不到这条作品")
	}
	return s.generationLocked(id)
}

func (s *Store) generationImage(id string, position int) (mime string, data []byte, sourceURL string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.db.QueryRow(`SELECT mime, bytes, source_url FROM generation_images
		WHERE generation_id = ? AND position = ?`, id, position).Scan(&mime, &data, &sourceURL)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, "", fail(404, "找不到这张图")
	}
	return mime, data, sourceURL, err
}

// fillGenerationImages 后台把只留了链接的图拉回来存好。
// 拉不动就留着链接，前端会退回直接用它。
func (s *Store) fillGenerationImages(generationID string) {
	type pending struct {
		id  string
		url string
	}
	s.mu.Lock()
	rows, err := s.db.Query(`SELECT id, source_url FROM generation_images
		WHERE generation_id = ? AND bytes IS NULL AND source_url <> ''`, generationID)
	if err != nil {
		s.mu.Unlock()
		return
	}
	list := []pending{}
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.id, &item.url); err == nil {
			list = append(list, item)
		}
	}
	rows.Close()
	s.mu.Unlock()

	if len(list) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), imageFetchTimeout)
	defer cancel()
	for _, item := range list {
		data, mime := fetchGeneratedImage(ctx, item.url)
		if len(data) == 0 {
			continue
		}
		s.mu.Lock()
		if mime != "" {
			s.db.Exec(`UPDATE generation_images SET bytes = ?, mime = ? WHERE id = ?`, data, mime, item.id)
		} else {
			s.db.Exec(`UPDATE generation_images SET bytes = ? WHERE id = ?`, data, item.id)
		}
		s.mu.Unlock()
	}
}

/* ---------- 审计日志 ---------- */

// AuditEntry 一条审计记录：谁、什么时候、对什么、做了什么。
// actor_kind 是 user 或 admin——管理端和用户端是两套会话，混在一起看不出是谁干的。
type AuditEntry struct {
	ID        string `json:"id"`
	ActorKind string `json:"actorKind"`
	ActorID   string `json:"actorId"`
	ActorName string `json:"actorName"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Detail    string `json:"detail"`
	IP        string `json:"ip"`
	CreatedAt string `json:"createdAt"`
}

// audit 落一条审计。写失败只返回错误让调用方记服务端日志，不往上抛——
// 审计是旁路，不能因为它自己写不进去就把已经成功的签到、生图搅黄。
func (s *Store) audit(entry AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO audit_logs
		(id, actor_kind, actor_id, actor_name, action, target, detail, ip, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newID(), entry.ActorKind, entry.ActorID, entry.ActorName,
		entry.Action, entry.Target, entry.Detail, entry.IP, s.now())
	return err
}

// auditLogs 按发生顺序倒序翻页。action 传空就是全部。
//
// 排序用 rowid 而不是 created_at：created_at 只到秒，同一秒里连着发生的几件事
// （登录完立刻改设置再登出）时间戳一模一样，按它排就乱套了——而审计要看的
// 恰恰是这个先后。rowid 是插入顺序，只增不减，正合适。
func (s *Store) auditLogs(action string, limit, offset int) ([]AuditEntry, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	where := ""
	args := []any{}
	if action != "" {
		where = " WHERE action = ?"
		args = append(args, action)
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	pageArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.db.Query(`SELECT id, actor_kind, actor_id, actor_name, action, target, detail, ip, created_at
		FROM audit_logs`+where+` ORDER BY rowid DESC LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var item AuditEntry
		if err := rows.Scan(&item.ID, &item.ActorKind, &item.ActorID, &item.ActorName,
			&item.Action, &item.Target, &item.Detail, &item.IP, &item.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, item)
	}
	return out, total, rows.Err()
}

// auditActions 已经出现过的动作，给筛选下拉用。写死的列表会跟写入点对不上，
// 这里直接从记录里取，多一个动作下拉里就多一项。
func (s *Store) auditActions() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT DISTINCT action FROM audit_logs ORDER BY action`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			return nil, err
		}
		out = append(out, action)
	}
	return out, rows.Err()
}

// adminName 管理端账号名。审计里管理员那一行要写清楚是谁在操作。
func (s *Store) adminName() string {
	var name string
	if err := s.db.QueryRow(`SELECT username FROM admin WHERE id = 1`).Scan(&name); err != nil {
		return "管理端"
	}
	if strings.TrimSpace(name) == "" {
		return "管理端"
	}
	return name
}
