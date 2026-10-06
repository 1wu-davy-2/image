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
	Settings    Settings `json:"settings"`
	Keys        []Key    `json:"keys"`
	Users       []User   `json:"users"`
	CheckinDate string   `json:"checkinDate"`
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
	}); err != nil {
		return err
	}
	// 邮箱要能当登录名，所以唯一。老库补列留下的空串不参与唯一，故用条件索引。
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS users_email ON users(email_lower) WHERE email_lower <> ''`); err != nil {
		return err
	}
	return s.migrateCheckinRange()
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

const keyColumns = `id, name, protocol, base_url, api_key, user_agent, balance, balance_unit,
	balance_valid, balance_updated_at, balance_error, models, models_updated_at, models_error,
	enabled, note, last_used_at`

func scanKey(row interface{ Scan(...any) error }) (Key, error) {
	var key Key
	var balance sql.NullFloat64
	var valid sql.NullBool
	var enabled int
	var models string
	err := row.Scan(&key.ID, &key.Name, &key.Protocol, &key.BaseURL, &key.APIKey, &key.UserAgent,
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

type KeyInput struct {
	ID        string
	Name      string
	Protocol  string
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
	note := []rune(strings.TrimSpace(in.Note))
	if len(note) > 200 {
		note = note[:200]
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if in.ID == "" {
		id := newID()
		if _, err := s.db.Exec(`INSERT INTO keys (id, name, protocol, base_url, api_key, user_agent, balance,
			balance_unit, balance_valid, balance_updated_at, balance_error, enabled, note, last_used_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'USD', NULL, '', '', ?, ?, 0, ?)`,
			id, name, in.Protocol, in.BaseURL, in.APIKey, in.UserAgent, in.Balance, boolToInt(in.Enabled), string(note), s.now()); err != nil {
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
	if _, err := s.db.Exec(`UPDATE keys SET name = ?, protocol = ?, base_url = ?, api_key = ?, user_agent = ?,
		balance = ?, balance_valid = ?, balance_error = ?, enabled = ?, note = ? WHERE id = ?`,
		name, in.Protocol, in.BaseURL, apiKey, in.UserAgent, in.Balance, boolPtrToInt(valid), balanceError,
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

// reserveGeneration 扣额度并锁定一把 Key。units 是消耗次数（批量按条目数算）。
func (s *Store) reserveGeneration(userID, protocol string, units int) (Key, int, int, error) {
	if units < 1 {
		units = 1
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
	cost := settings.GenerateCost * units
	if user.Quota < cost {
		hint := "额度不足，可以先签到领取。"
		if user.LastCheckinDate == todayShanghai() {
			hint = "额度不足，明天可以再签到。"
		}
		return Key{}, 0, 0, fail(402, hint)
	}
	key, err := s.pickKey(protocol)
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
	return AdminState{Settings: settings, Keys: keys, Users: users, CheckinDate: todayShanghai()}, nil
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
	if _, err := s.db.Exec(`INSERT INTO generations
		(id, user_id, prompt, protocol, model, size_label, channel, task_id, is_public, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
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
