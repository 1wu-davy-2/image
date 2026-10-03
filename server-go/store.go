package main

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	defaultCheckinQuota  = 5
	defaultGenerateCost  = 1
)

// 上海没有夏令时，用固定偏移就够，省得依赖 tzdata。
var shanghai = time.FixedZone("CST", 8*3600)

func todayShanghai() string { return time.Now().In(shanghai).Format("2006-01-02") }

type User struct {
	ID              string `json:"id"`
	Username        string `json:"username"`
	Quota           int    `json:"quota"`
	Disabled        bool   `json:"disabled"`
	LastCheckinDate string `json:"lastCheckinDate"`
	CheckedInToday  bool   `json:"checkedInToday"`
	CreatedAt       string `json:"createdAt"`
}

type Key struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Protocol         string   `json:"protocol"`
	BaseURL          string   `json:"baseUrl"`
	APIKey           string   `json:"apiKey"`
	UserAgent        string   `json:"userAgent"`
	Balance          *float64 `json:"balance"`
	BalanceUnit      string   `json:"balanceUnit"`
	BalanceValid     *bool    `json:"balanceValid"`
	BalanceUpdatedAt string   `json:"balanceUpdatedAt"`
	BalanceError     string   `json:"balanceError"`
	Enabled          bool     `json:"enabled"`
	Note             string   `json:"note"`
	LastUsedAt       int64    `json:"lastUsedAt"`
}

type Settings struct {
	CheckinQuota int `json:"checkinQuota"`
	GenerateCost int `json:"generateCost"`
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
  checkin_quota  INTEGER NOT NULL,
  generate_cost  INTEGER NOT NULL
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
  salt              BLOB NOT NULL,
  hash              BLOB NOT NULL,
  quota             INTEGER NOT NULL DEFAULT 0,
  disabled          INTEGER NOT NULL DEFAULT 0,
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
  enabled            INTEGER NOT NULL DEFAULT 1,
  note               TEXT NOT NULL DEFAULT '',
  last_used_at       INTEGER NOT NULL DEFAULT 0,
  created_at         TEXT NOT NULL
);
INSERT OR IGNORE INTO settings (id, checkin_quota, generate_cost) VALUES (1, 5, 1);
`)
	return err
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
	err := s.db.QueryRow(`SELECT checkin_quota, generate_cost FROM settings WHERE id = 1`).
		Scan(&out.CheckinQuota, &out.GenerateCost)
	return out, err
}

func (s *Store) updateSettings(checkin, cost int) (Settings, error) {
	if checkin < 0 || checkin > 1000 {
		return Settings{}, fail(400, "签到额度需要是 0 到 1000 的整数")
	}
	if cost < 0 || cost > 1000 {
		return Settings{}, fail(400, "每次消耗需要是 0 到 1000 的整数")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`UPDATE settings SET checkin_quota = ?, generate_cost = ? WHERE id = 1`, checkin, cost); err != nil {
		return Settings{}, err
	}
	return Settings{CheckinQuota: checkin, GenerateCost: cost}, nil
}

/* ---------- 用户 ---------- */

func validUsername(name string) bool {
	if len([]rune(name)) < 2 || len([]rune(name)) > 20 {
		return false
	}
	for _, r := range name {
		if r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127 {
			continue
		}
		return false
	}
	return true
}

func validPassword(password string) bool {
	n := len([]rune(password))
	return n >= 6 && n <= 72
}

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var user User
	var disabled int
	var quota sql.NullInt64
	err := row.Scan(&user.ID, &user.Username, &quota, &disabled, &user.LastCheckinDate, &user.CreatedAt)
	if err != nil {
		return user, err
	}
	user.Quota = int(quota.Int64)
	user.Disabled = disabled != 0
	user.CheckedInToday = user.LastCheckinDate == todayShanghai()
	return user, nil
}

const userColumns = `id, username, quota, disabled, last_checkin_date, created_at`

func (s *Store) register(username, password string) (User, string, error) {
	name := strings.TrimSpace(username)
	if !validUsername(name) {
		return User{}, "", fail(400, "用户名需要 2 到 20 位，可用字母、数字、下划线")
	}
	if !validPassword(password) {
		return User{}, "", fail(400, "密码需要 6 到 72 位")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var existing int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE username_lower = ?`, strings.ToLower(name)).Scan(&existing); err != nil {
		return User{}, "", err
	}
	if existing > 0 {
		return User{}, "", fail(409, "这个用户名已经注册过")
	}

	salt, hash := hashPassword(password)
	id := newID()
	if _, err := s.db.Exec(`INSERT INTO users (id, username, username_lower, salt, hash, quota, disabled, last_checkin_date, created_at)
		VALUES (?, ?, ?, ?, ?, 0, 0, '', ?)`, id, name, strings.ToLower(name), salt, hash, s.now()); err != nil {
		return User{}, "", err
	}
	token, err := s.createSession("user", id)
	if err != nil {
		return User{}, "", err
	}
	user, err := s.userByID(id)
	return user, token, err
}

func (s *Store) login(username, password string) (User, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var id string
	var disabled int
	var salt, hash []byte
	err := s.db.QueryRow(`SELECT id, disabled, salt, hash FROM users WHERE username_lower = ?`, strings.ToLower(strings.TrimSpace(username))).
		Scan(&id, &disabled, &salt, &hash)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !verifyPassword(password, salt, hash)) {
		return User{}, "", fail(401, "用户名或密码不对")
	}
	if err != nil {
		return User{}, "", err
	}
	if disabled != 0 {
		return User{}, "", fail(403, "这个账号已被停用")
	}
	token, err := s.createSession("user", id)
	if err != nil {
		return User{}, "", err
	}
	user, err := s.userByID(id)
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
	if _, err := s.db.Exec(`UPDATE users SET quota = quota + ?, last_checkin_date = ? WHERE id = ?`,
		settings.CheckinQuota, today, userID); err != nil {
		return 0, User{}, err
	}
	updated, err := s.userByID(userID)
	return settings.CheckinQuota, updated, err
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
	_, err = s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)
	return err
}

/* ---------- Key ---------- */

const keyColumns = `id, name, protocol, base_url, api_key, user_agent, balance, balance_unit,
	balance_valid, balance_updated_at, balance_error, enabled, note, last_used_at`

func scanKey(row interface{ Scan(...any) error }) (Key, error) {
	var key Key
	var balance sql.NullFloat64
	var valid sql.NullBool
	var enabled int
	err := row.Scan(&key.ID, &key.Name, &key.Protocol, &key.BaseURL, &key.APIKey, &key.UserAgent,
		&balance, &key.BalanceUnit, &valid, &key.BalanceUpdatedAt, &key.BalanceError, &enabled, &key.Note, &key.LastUsedAt)
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
