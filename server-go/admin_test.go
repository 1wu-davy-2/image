package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// adminRouteRe 从 main.go 里抠出管理端的路由注册。
var adminRouteRe = regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) (/api/admin[^"]*)"`)

var pathParamRe = regexp.MustCompile(`\{[^}]+\}`)

// adminRoutesInSource 直接读源码，而不是在测试里维护一份手抄的清单——
// 手抄的清单会漂：新加一条路由忘了抄进来，测试照样绿。
func adminRoutesInSource(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读不到 main.go：%v", err)
	}
	out := []string{}
	for _, match := range adminRouteRe.FindAllStringSubmatch(string(source), -1) {
		out = append(out, match[1]+" "+match[2])
	}
	sort.Strings(out)
	return out
}

// 管理端的每一条路由都要挡住没登录的人，以及「拿着用户端会话、哪怕带着 is_admin 标志」的人。
//
// 管理端的授权走的是独立的管理端会话 Cookie（role=admin），不是用户表里的 is_admin。
// 这个区分很关键：admin 表的账号能登用户端（登录后 user.isAdmin 为真），
// 但那只影响界面显示，不该让那个会话能调管理接口。
func TestAdminRoutesRejectNonAdmins(t *testing.T) {
	routes := adminRoutesInSource(t)
	if len(routes) < 10 {
		t.Fatalf("只从源码里抠到 %d 条管理端路由，正则多半失效了", len(routes))
	}

	// 这两条本来就该匿名可用：登录没法要求先登录，登出只是清个 Cookie。
	anonymousOK := map[string]bool{
		"POST /api/admin/login":  true,
		"POST /api/admin/logout": true,
	}

	store := newTestStore(t)
	handler := (&server{store: store}).routes()

	plain := newTestUser(t, store, "plain")
	_, plainToken, err := store.issueUserSession(plain.ID)
	if err != nil {
		t.Fatalf("发会话失败：%v", err)
	}

	// 把普通用户标成 is_admin，但只给它用户端会话。这正是要防的那条越权路。
	adminish := newTestUser(t, store, "adminish")
	if _, err := store.db.Exec(`UPDATE users SET is_admin = 1 WHERE id = ?`, adminish.ID); err != nil {
		t.Fatalf("打管理员标志失败：%v", err)
	}
	_, adminishToken, err := store.issueUserSession(adminish.ID)
	if err != nil {
		t.Fatalf("发会话失败：%v", err)
	}

	sessions := []struct {
		name   string
		cookie *http.Cookie
	}{
		{"没登录", nil},
		{"普通用户", &http.Cookie{Name: userCookieName, Value: plainToken}},
		{"带 is_admin 标志的用户", &http.Cookie{Name: userCookieName, Value: adminishToken}},
	}

	for _, route := range routes {
		if anonymousOK[route] {
			continue
		}
		parts := strings.SplitN(route, " ", 2)
		path := pathParamRe.ReplaceAllString(parts[1], "test-id")
		for _, session := range sessions {
			req := httptest.NewRequest(parts[0], path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			if session.cookie != nil {
				req.AddCookie(session.cookie)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != 401 {
				t.Errorf("%s（%s）该回 401，实际 %d：%s", route, session.name, rec.Code, rec.Body.String())
			}
		}
	}
}

// 反过来：拿着真正的管理端会话要能进去。不然上面那条「全都 401」可能只是因为
// 路由根本没接上，测了个寂寞。
func TestAdminRoutesAllowRealAdminSession(t *testing.T) {
	store := newTestStore(t)
	token, err := store.createSession("admin", "")
	if err != nil {
		t.Fatalf("发管理端会话失败：%v", err)
	}
	handler := (&server{store: store}).routes()
	cookie := &http.Cookie{Name: adminCookieName, Value: token}

	for _, route := range []string{"GET /api/admin/state", "GET /api/admin/audit"} {
		parts := strings.SplitN(route, " ", 2)
		req := httptest.NewRequest(parts[0], parts[1], nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("%s 带着管理端会话该回 200，实际 %d：%s", route, rec.Code, rec.Body.String())
		}
	}
}

// 用户端的会话不该被管理端的接口认成管理端，反过来也一样。
func TestSessionsDoNotCrossOver(t *testing.T) {
	store := newTestStore(t)
	user := newTestUser(t, store, "alice")
	_, userToken, err := store.issueUserSession(user.ID)
	if err != nil {
		t.Fatalf("发会话失败：%v", err)
	}
	adminToken, err := store.createSession("admin", "")
	if err != nil {
		t.Fatalf("发会话失败：%v", err)
	}

	if ok, err := store.sessionAdmin(userToken); err != nil || ok {
		t.Fatalf("用户端会话不该通过管理端校验：ok=%v err=%v", ok, err)
	}
	if user, err := store.sessionUser(adminToken); err != nil || user != nil {
		t.Fatalf("管理端会话不该被当成用户端会话：user=%+v err=%v", user, err)
	}
}

// 用户端自己那些需要登录的接口，匿名一律 401。
func TestUserRoutesNeedLogin(t *testing.T) {
	store := newTestStore(t)
	handler := (&server{store: store}).routes()

	// GET /api/me 不在名单里：它故意让匿名也能拿到 200 + user:null，
	// 页面靠它决定画登录态还是未登录态，见接口文档。
	routes := []string{
		"PATCH /api/me", "POST /api/me/password",
		"POST /api/checkin", "GET /api/checkins",
		"POST /api/generate", "GET /api/generations",
		"POST /api/batches", "GET /api/batches", "GET /api/batches/models",
	}
	for _, route := range routes {
		parts := strings.SplitN(route, " ", 2)
		req := httptest.NewRequest(parts[0], parts[1], strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("%s 匿名该回 401，实际 %d：%s", route, rec.Code, rec.Body.String())
		}
	}
}
