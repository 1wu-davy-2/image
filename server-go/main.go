package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	bodyLimit       = 32 * 1024 * 1024
	userCookieName  = "darkroom_user"
	adminCookieName = "darkroom_admin"
)

type httpError struct {
	status int
	msg    string
	quota  *int
}

func (e *httpError) Error() string { return e.msg }

// fail 不做格式化：上游返回的错误文案里可能有 %，格式化会把它们弄坏。
func fail(status int, message string) *httpError {
	return &httpError{status: status, msg: message}
}

type server struct {
	store   *Store
	webDir  string
	handler http.Handler
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "6670"
	}
	dataPath := os.Getenv("DATA_FILE")
	if dataPath == "" {
		dataPath = filepath.Join("..", "data", "darkroom.db")
	}

	store, err := openStore(dataPath)
	if err != nil {
		log.Fatalf("打开数据库失败：%v", err)
	}
	if err := store.ensureAdmin(); err != nil {
		log.Fatalf("初始化管理员失败：%v", err)
	}

	webDir := os.Getenv("WEB_DIR")
	if webDir == "" {
		webDir = filepath.Join("..", "web")
	}
	if info, err := os.Stat(webDir); err != nil || !info.IsDir() {
		log.Fatalf("找不到前端目录 %s —— 请在 server-go/ 目录下运行，或用 WEB_DIR 指定", webDir)
	}
	srv := &server{store: store, webDir: webDir}
	srv.handler = srv.routes()

	httpServer := &http.Server{
		Addr:              "127.0.0.1:" + port,
		Handler:           srv.handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		fmt.Printf("暗房（Go）已启动 http://127.0.0.1:%s\n", port)
		fmt.Printf("管理端 http://127.0.0.1:%s/admin\n", port)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("监听失败：%v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpServer.Shutdown(ctx)
}

/* ---------- 路由 ---------- */

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /api/auth/register", s.wrap(s.register))
	mux.HandleFunc("POST /api/auth/login", s.wrap(s.login))
	mux.HandleFunc("POST /api/auth/logout", s.wrap(s.logout))
	mux.HandleFunc("GET /api/me", s.wrap(s.me))
	mux.HandleFunc("PATCH /api/me", s.wrap(s.updateMe))
	mux.HandleFunc("POST /api/checkin", s.wrap(s.checkin))
	mux.HandleFunc("GET /api/checkins", s.wrap(s.checkinList))
	mux.HandleFunc("GET /api/models", s.wrap(s.availableModels))
	mux.HandleFunc("POST /api/me/password", s.wrap(s.changeMyPassword))
	mux.HandleFunc("POST /api/generate", s.wrap(s.generateHandler))

	mux.HandleFunc("POST /api/batches", s.wrap(s.batchSubmit))
	mux.HandleFunc("GET /api/batches", s.wrap(s.batchList))
	mux.HandleFunc("GET /api/batches/models", s.wrap(s.batchRead("", "/models")))
	mux.HandleFunc("GET /api/batches/{id}", s.wrap(s.batchRead("/{id}", "")))
	mux.HandleFunc("DELETE /api/batches/{id}", s.wrap(s.batchDelete("")))
	mux.HandleFunc("GET /api/batches/{id}/items", s.wrap(s.batchRead("/{id}", "/items")))
	mux.HandleFunc("POST /api/batches/{id}/cancel", s.wrap(s.batchCancel))
	mux.HandleFunc("DELETE /api/batches/{id}/outputs", s.wrap(s.batchDelete("/outputs")))
	mux.HandleFunc("GET /api/batches/{id}/download", s.wrap(s.batchRaw("/download")))
	mux.HandleFunc("GET /api/batches/{id}/items/{customID}/content", s.wrap(s.batchItemContent))

	// 作品集（自己的）与公开作品。
	mux.HandleFunc("GET /api/generations", s.wrap(s.generationList))
	mux.HandleFunc("GET /api/generations/{id}", s.wrap(s.generationDetail))
	mux.HandleFunc("DELETE /api/generations/{id}", s.wrap(s.generationDelete))
	mux.HandleFunc("PATCH /api/generations/{id}", s.wrap(s.generationPatch))
	mux.HandleFunc("GET /api/generations/{id}/images/{position}", s.wrap(s.generationImageRaw))
	mux.HandleFunc("GET /api/works", s.wrap(s.publicWorkList))

	mux.HandleFunc("POST /api/admin/login", s.wrap(s.adminLogin))
	mux.HandleFunc("POST /api/admin/logout", s.wrap(s.adminLogout))
	mux.HandleFunc("GET /api/admin/state", s.wrap(s.adminState))
	mux.HandleFunc("PUT /api/admin/settings", s.wrap(s.adminSettings))
	mux.HandleFunc("POST /api/admin/password", s.wrap(s.adminPassword))
	mux.HandleFunc("POST /api/admin/keys", s.wrap(s.adminSaveKey))
	mux.HandleFunc("DELETE /api/admin/keys/{id}", s.wrap(s.adminDeleteKey))
	mux.HandleFunc("POST /api/admin/keys/{id}/balance", s.wrap(s.adminRefreshBalance))
	mux.HandleFunc("POST /api/admin/keys/{id}/models", s.wrap(s.adminRefreshModels))
	mux.HandleFunc("PATCH /api/admin/users/{id}", s.wrap(s.adminPatchUser))
	mux.HandleFunc("DELETE /api/admin/users/{id}", s.wrap(s.adminDeleteUser))
	mux.HandleFunc("POST /api/admin/users/{id}/password", s.wrap(s.adminResetUserPassword))

	// 其余路径当静态文件；不匹配的方法走这里也会被挡掉。
	mux.HandleFunc("/", s.wrap(s.static))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("panic: %v", recovered)
				writeJSON(w, 500, map[string]any{"ok": false, "error": "服务器内部错误"})
			}
		}()

		// 前端在 6664、后端在 6670，属于跨源。登录态是 HttpOnly Cookie，
		// 所以这里必须回具体来源 + Allow-Credentials，不能用 *。
		origin := r.Header.Get("Origin")
		allowed := origin != "" && corsOrigins[origin]
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Add("Vary", "Origin")
		}
		// 预检请求不带 Cookie，也不能要求登录，直接放行。
		if r.Method == http.MethodOptions {
			if allowed {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		mux.ServeHTTP(w, r)
	})
}

// CORS_ORIGINS 覆盖放行名单（逗号分隔）。默认只放行本地前端那个端口。
var corsOrigins = loadCORSOrigins()

func loadCORSOrigins() map[string]bool {
	raw := strings.TrimSpace(os.Getenv("CORS_ORIGINS"))
	origins := []string{"http://127.0.0.1:6664", "http://localhost:6664"}
	if raw != "" {
		origins = nil
		for _, part := range strings.Split(raw, ",") {
			if origin := strings.TrimSpace(part); origin != "" {
				origins = append(origins, origin)
			}
		}
	}
	out := make(map[string]bool, len(origins))
	for _, origin := range origins {
		out[origin] = true
	}
	return out
}

type handlerFunc func(http.ResponseWriter, *http.Request) error

func (s *server) wrap(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			var typed *httpError
			if errors.As(err, &typed) {
				if typed.quota != nil {
					writeJSON(w, typed.status, map[string]any{"ok": false, "error": typed.msg, "quota": *typed.quota})
					return
				}
				writeJSON(w, typed.status, map[string]any{"ok": false, "error": typed.msg})
				return
			}
			log.Printf("内部错误：%v", err)
			writeJSON(w, 500, map[string]any{"ok": false, "error": "服务器内部错误"})
		}
	}
}

/* ---------- 收发 ---------- */

func writeJSON(w http.ResponseWriter, status int, body any, cookies ...*http.Cookie) {
	raw, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "服务器内部错误", 500)
		return
	}
	for _, cookie := range cookies {
		http.SetCookie(w, cookie)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	w.Write(raw)
}

func readJSON(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, bodyLimit))
	if err != nil {
		return nil, fail(413, "请求体过大")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fail(400, "请求不是 JSON")
	}
	return out, nil
}

func sessionCookie(name, token string) *http.Cookie {
	cookie := &http.Cookie{
		Name:     name,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if token == "" {
		cookie.MaxAge = -1
	} else {
		cookie.MaxAge = 1209600
	}
	return cookie
}

func cookieValue(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (s *server) requireUser(r *http.Request) (*User, error) {
	user, err := s.store.sessionUser(cookieValue(r, userCookieName))
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, fail(401, "请先登录")
	}
	return user, nil
}

func (s *server) requireAdmin(r *http.Request) error {
	ok, err := s.store.sessionAdmin(cookieValue(r, adminCookieName))
	if err != nil {
		return err
	}
	if !ok {
		return fail(401, "请先登录管理端")
	}
	return nil
}

/* ---------- 静态文件 ---------- */

func (s *server) static(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return fail(405, "不支持的方法")
	}
	requested := "/" + strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	switch requested {
	case "/", "//":
		requested = "/index.html"
	case "/admin", "/admin/":
		requested = "/admin.html"
	}
	full := filepath.Join(s.webDir, filepath.FromSlash(requested))
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		return fail(404, "找不到页面")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, full)
	return nil
}

/* ---------- 参数校验 ---------- */

var (
	modelRe    = regexp.MustCompile(`^[\w.+-]{1,120}$`)
	customIDRe = regexp.MustCompile(`^[\w.-]{1,64}$`)
	batchIDRe  = regexp.MustCompile(`^[\w.-]{1,120}$`)
	sizeRe     = regexp.MustCompile(`^(\d{3,4})x(\d{3,4})$`)
)

var (
	ratios = map[string]bool{"1:1": true, "3:2": true, "2:3": true, "4:3": true, "3:4": true, "16:9": true, "9:16": true}
	tiers  = map[string]bool{"1K": true, "2K": true, "4K": true}
)

var studioSizes = map[string]string{
	"1:1|1K":  "1024x1024",
	"4:3|1K":  "1024x768",
	"3:4|1K":  "768x1024",
	"16:9|1K": "1024x576",
	"1:1|2K":  "2048x2048",
	"4:3|2K":  "2048x1536",
	"3:4|2K":  "1536x2048",
	"16:9|2K": "2048x1152",
	"1:1|4K":  "4096x4096",
	"4:3|4K":  "4096x3072",
	"3:4|4K":  "3072x4096",
	"16:9|4K": "3840x2160",
}

func parseProtocol(value any) (string, error) {
	protocol := strings.TrimSpace(firstString(value))
	if protocol == "" {
		protocol = "gpt"
	}
	if !protocols[protocol] {
		return "", fail(400, "请选择调用方式")
	}
	return protocol, nil
}

func parseModel(value any) (string, error) {
	model := strings.TrimSpace(firstString(value))
	model = strings.TrimSuffix(strings.TrimPrefix(model, "models/"), ":generateContent")
	if !modelRe.MatchString(model) {
		return "", fail(400, "模型名不合法")
	}
	return model, nil
}

func requirePrompt(value any) (string, error) {
	prompt := strings.TrimSpace(firstString(value))
	if prompt == "" {
		return "", fail(400, "请填写画面描述")
	}
	if len([]rune(prompt)) > 4000 {
		return "", fail(400, "画面描述请少于 4000 字")
	}
	return prompt, nil
}

func parseRatio(value any) (string, error) {
	ratio := firstString(value)
	if ratio == "" {
		ratio = "1:1"
	}
	if !ratios[ratio] {
		return "", fail(400, "画幅不支持")
	}
	return ratio, nil
}

func parseTier(value any) (string, error) {
	tier := firstString(value)
	if tier == "" {
		tier = "1K"
	}
	if !tiers[tier] {
		return "", fail(400, "分辨率只支持 1K、2K、4K")
	}
	return tier, nil
}

func parseQuality(value any) (string, error) {
	quality := firstString(value)
	if quality == "" {
		quality = "medium"
	}
	if quality != "auto" && quality != "medium" && quality != "high" {
		return "", fail(400, "质量只支持 auto、medium、high")
	}
	return quality, nil
}

func parseSize(value any) (string, error) {
	match := sizeRe.FindStringSubmatch(strings.TrimSpace(firstString(value)))
	if match == nil {
		return "", fail(400, "尺寸格式应为 宽x高，例如 1024x1024")
	}
	width, _ := strconv.Atoi(match[1])
	height, _ := strconv.Atoi(match[2])
	if width < 256 || height < 256 || width > 8192 || height > 8192 {
		return "", fail(400, "宽和高都要在 256 到 8192 之间")
	}
	return fmt.Sprintf("%dx%d", width, height), nil
}

func pixelsFor(ratio, tier string) string {
	if known, ok := studioSizes[ratio+"|"+tier]; ok {
		return known
	}
	parts := strings.SplitN(ratio, ":", 2)
	if len(parts) != 2 {
		parts = []string{"1", "1"}
	}
	a, err := strconv.Atoi(parts[0])
	if err != nil || a < 1 {
		a = 1
	}
	b, err := strconv.Atoi(parts[1])
	if err != nil || b < 1 {
		b = 1
	}
	long := 1024
	if tier == "4K" {
		long = 4096
	} else if tier == "2K" {
		long = 2048
	}
	if a >= b {
		return fmt.Sprintf("%dx%d", long, max(256, long*b/a))
	}
	return fmt.Sprintf("%dx%d", max(256, long*a/b), long)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func parseImageField(input map[string]any, key, label string) (*imageFile, error) {
	raw := obj(input, key)
	if raw == nil {
		return nil, nil
	}
	data := firstString(raw["data"])
	if data == "" {
		return nil, nil
	}
	return decodeImage(firstString(raw["mime"]), data, firstString(raw["name"]), label)
}

/* ---------- 用户端 ---------- */

func (s *server) register(w http.ResponseWriter, r *http.Request) error {
	body, err := readJSON(w, r)
	if err != nil {
		return err
	}
	user, token, err := s.store.register(RegisterInput{
		Username:    firstString(body["username"]),
		DisplayName: firstString(body["displayName"]),
		Password:    firstString(body["password"]),
		Phone:       firstString(body["phone"]),
		Email:       firstString(body["email"]),
	})
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "user": user}, sessionCookie(userCookieName, token))
	return nil
}

func (s *server) login(w http.ResponseWriter, r *http.Request) error {
	body, err := readJSON(w, r)
	if err != nil {
		return err
	}
	// account 既收名称也收邮箱。
	user, token, err := s.store.login(firstString(body["account"]), firstString(body["password"]))
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "user": user}, sessionCookie(userCookieName, token))
	return nil
}

// 目前只有中文名可改；名称、手机号、邮箱定了就不让动。
func (s *server) updateMe(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	body, err := readJSON(w, r)
	if err != nil {
		return err
	}
	updated, err := s.store.setUserDisplayName(user.ID, firstString(body["displayName"]))
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "user": updated})
	return nil
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) error {
	if err := s.store.logout(cookieValue(r, userCookieName)); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true}, sessionCookie(userCookieName, ""))
	return nil
}

func (s *server) me(w http.ResponseWriter, r *http.Request) error {
	user, err := s.store.sessionUser(cookieValue(r, userCookieName))
	if err != nil {
		return err
	}
	settings, err := s.store.settings()
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{
		"ok":           true,
		"user":         user,
		"checkinMin":   settings.CheckinMin,
		"checkinMax":   settings.CheckinMax,
		"generateCost": settings.GenerateCost,
	})
	return nil
}

func (s *server) checkin(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	amount, updated, err := s.store.checkin(user.ID)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "amount": amount, "user": updated})
	return nil
}

// checkinList 列自己的签到记录，倒序，分页。
func (s *server) checkinList(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	limit, offset := pageParams(r)
	items, total, err := s.store.checkinsByUser(user.ID, limit, offset)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "items": items, "total": total})
	return nil
}

func (s *server) changeMyPassword(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	body, err := readJSON(w, r)
	if err != nil {
		return err
	}
	if err := s.store.changeUserPassword(user.ID, firstString(body["oldPassword"]), firstString(body["newPassword"])); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true})
	return nil
}

func (s *server) generateHandler(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	body, err := readJSON(w, r)
	if err != nil {
		return err
	}
	protocol, err := parseProtocol(body["protocol"])
	if err != nil {
		return err
	}
	if protocol == batchProtocol {
		return fail(400, "批量生图请用 /api/batches 提交")
	}
	spec, err := buildSpec(r.Context(), body, protocol)
	if err != nil {
		return err
	}

	key, cost, quota, err := s.store.reserveGeneration(user.ID, protocol, 1)
	if err != nil {
		return err
	}
	cred := credential{APIKey: key.APIKey, BaseURL: key.BaseURL, UserAgent: key.UserAgent}
	result, genErr := generate(r.Context(), cred, protocol, spec)
	if genErr != nil {
		refunded := s.store.refund(user.ID, cost)
		var typed *httpError
		if errors.As(genErr, &typed) {
			typed.quota = &refunded
			return typed
		}
		return genErr
	}
	log.Printf("[generate] %s protocol=%s model=%s images=%d", result.Channel, protocol, spec.Model, len(result.Images))

	// 落库，作品集和公开作品都靠它。存不下也不影响这次生成的结果。
	if record, err := s.store.saveGeneration(GenerationInput{
		UserID:    user.ID,
		Prompt:    spec.Prompt,
		Protocol:  protocol,
		Model:     spec.Model,
		SizeLabel: specSizeLabel(spec),
		Channel:   result.Channel,
		TaskID:    result.TaskID,
		Images:    collectStoredImages(result.Images),
	}); err != nil {
		log.Printf("[gallery] 保存生成记录失败：%v", err)
	} else {
		// 只有链接的图放到后台去拉，别让用户等下载。
		go s.store.fillGenerationImages(record.ID)
	}

	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"channel": result.Channel,
		"taskId":  result.TaskID,
		"images":  result.Images,
		"quota":   quota,
	})
	return nil
}

func specSizeLabel(spec generateSpec) string {
	parts := []string{}
	for _, part := range []string{spec.Size, spec.Quality, spec.AspectRatio, spec.ImageSize} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, " · ")
}

// collectStoredImages 把上游回来的图整理成待落库的形状：base64 的直接带上字节，
// 只有链接的先留链接，由 fillGenerationImages 在后台补拉。
func collectStoredImages(images []imageResult) []StoredImage {
	out := make([]StoredImage, 0, len(images))
	for _, image := range images {
		switch {
		case image.B64 != "":
			data, err := base64.StdEncoding.DecodeString(image.B64)
			if err != nil {
				continue
			}
			mime := image.Mime
			if mime == "" {
				mime = sniffMime(image.B64)
			}
			out = append(out, StoredImage{Mime: mime, Data: data})
		case image.URL != "":
			out = append(out, StoredImage{Mime: image.Mime, URL: image.URL})
		}
	}
	return out
}

/* ---------- 作品集与公开作品 ---------- */

const (
	pageLimitDefault = 24
	pageLimitMax     = 100
)

func pageParams(r *http.Request) (int, int) {
	limit := pageLimitDefault
	if value, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && value > 0 {
		limit = value
		if limit > pageLimitMax {
			limit = pageLimitMax
		}
	}
	offset := 0
	if value, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && value > 0 {
		offset = value
	}
	return limit, offset
}

func (s *server) generationList(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	limit, offset := pageParams(r)
	list, total, err := s.store.generationsByUser(user.ID, limit, offset)
	if err != nil {
		return err
	}
	for i := range list {
		list[i].Mine = true
	}
	writeJSON(w, 200, map[string]any{"ok": true, "items": list, "total": total})
	return nil
}

// 公开作品不登录也能看；登录了就顺手标出哪些是自己的。
func (s *server) publicWorkList(w http.ResponseWriter, r *http.Request) error {
	user, err := s.store.sessionUser(cookieValue(r, userCookieName))
	if err != nil {
		return err
	}
	limit, offset := pageParams(r)
	list, total, err := s.store.publicGenerations(limit, offset)
	if err != nil {
		return err
	}
	for i := range list {
		list[i].Mine = user != nil && list[i].userID == user.ID
	}
	writeJSON(w, 200, map[string]any{"ok": true, "items": list, "total": total})
	return nil
}

func (s *server) generationDetail(w http.ResponseWriter, r *http.Request) error {
	user, err := s.store.sessionUser(cookieValue(r, userCookieName))
	if err != nil {
		return err
	}
	item, err := s.store.generationByID(r.PathValue("id"))
	if err != nil {
		return err
	}
	mine := user != nil && item.userID == user.ID
	if !item.IsPublic && !mine {
		// 不区分「不存在」和「不是你的」，免得能拿来探别人有哪些作品。
		return fail(404, "找不到这条作品")
	}
	item.Mine = mine
	writeJSON(w, 200, map[string]any{"ok": true, "item": item})
	return nil
}

func (s *server) generationDelete(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if err := s.store.deleteGeneration(r.PathValue("id"), user.ID); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true})
	return nil
}

func (s *server) generationPatch(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	body, err := readJSON(w, r)
	if err != nil {
		return err
	}
	isPublic, ok := body["isPublic"].(bool)
	if !ok {
		return fail(400, "isPublic 需要是 true 或 false")
	}
	item, err := s.store.setGenerationPublic(r.PathValue("id"), user.ID, isPublic)
	if err != nil {
		return err
	}
	item.Mine = true
	writeJSON(w, 200, map[string]any{"ok": true, "item": item})
	return nil
}

func (s *server) generationImageRaw(w http.ResponseWriter, r *http.Request) error {
	user, err := s.store.sessionUser(cookieValue(r, userCookieName))
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	item, err := s.store.generationByID(id)
	if err != nil {
		return err
	}
	if !item.IsPublic && (user == nil || item.userID != user.ID) {
		return fail(404, "找不到这张图")
	}
	position, err := strconv.Atoi(r.PathValue("position"))
	if err != nil {
		return fail(400, "图片序号不对")
	}
	mime, data, sourceURL, err := s.store.generationImage(id, position)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		// 还没拉回来，或者拉失败。把人送回上游链接，总比空框强。
		if sourceURL == "" {
			return fail(404, "这张图没有内容")
		}
		http.Redirect(w, r, sourceURL, http.StatusFound)
		return nil
	}
	if mime == "" {
		mime = "image/png"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(200)
	_, err = w.Write(data)
	return err
}

func buildSpec(ctx context.Context, body map[string]any, protocol string) (generateSpec, error) {
	model, err := parseModel(body["model"])
	if err != nil {
		return generateSpec{}, err
	}
	prompt, err := requirePrompt(body["prompt"])
	if err != nil {
		return generateSpec{}, err
	}
	mode := "generate"
	if firstString(body["mode"]) == "edit" {
		mode = "edit"
	}
	spec := generateSpec{Model: model, Prompt: prompt, Mode: mode}

	file, err := parseImageField(body, "image", "参考图")
	if err != nil {
		return generateSpec{}, err
	}
	spec.File = file
	imageURL := firstString(body["imageUrl"])
	if mode == "edit" {
		if spec.File != nil {
			// 文件优先
		} else if imageURL != "" {
			if spec.ImageURL, err = assertPublicImageURL(imageURL); err != nil {
				return generateSpec{}, err
			}
		} else {
			return generateSpec{}, fail(400, "图生图需要上传参考图，或填写 https 图片地址")
		}
	}

	mask, err := parseImageField(body, "mask", "蒙版")
	if err != nil {
		return generateSpec{}, err
	}
	maskURL := firstString(body["maskUrl"])
	if mask != nil || maskURL != "" {
		if mode != "edit" {
			return generateSpec{}, fail(400, "蒙版只在图生图时可用")
		}
		if protocol == "gemini-official" {
			return generateSpec{}, fail(400, "Gemini 官方直连不支持蒙版，请改用 GPT 或香蕉生图")
		}
		spec.Mask = mask
		if mask == nil {
			if spec.MaskURL, err = assertPublicImageURL(maskURL); err != nil {
				return generateSpec{}, err
			}
		}
		// 上游只有「两个文件」或「两个 URL」两种形状，混着来时把 URL 那一半下下来，
		// 统一走 multipart，免得蒙版被静默丢掉。
		if spec.Mask != nil && spec.ImageURL != "" {
			downloaded, err := downloadReference(ctx, spec.ImageURL)
			if err != nil {
				return generateSpec{}, err
			}
			spec.File = downloaded
			spec.ImageURL = ""
		} else if spec.MaskURL != "" && spec.File != nil {
			downloaded, err := downloadReference(ctx, spec.MaskURL)
			if err != nil {
				return generateSpec{}, err
			}
			spec.Mask = downloaded
			spec.MaskURL = ""
		}
	}

	if protocol == "gemini-official" {
		if spec.AspectRatio, err = parseRatio(body["aspectRatio"]); err != nil {
			return generateSpec{}, err
		}
		if spec.ImageSize, err = parseTier(body["imageSize"]); err != nil {
			return generateSpec{}, err
		}
		return spec, nil
	}
	if protocol == "nano" {
		if spec.AspectRatio, err = parseRatio(body["aspectRatio"]); err != nil {
			return generateSpec{}, err
		}
		if spec.ImageSize, err = parseTier(body["imageSize"]); err != nil {
			return generateSpec{}, err
		}
		spec.Size = pixelsFor(spec.AspectRatio, spec.ImageSize)
		return spec, nil
	}
	if spec.Size, err = parseSize(body["size"]); err != nil {
		return generateSpec{}, err
	}
	if spec.Quality, err = parseQuality(body["quality"]); err != nil {
		return generateSpec{}, err
	}
	return spec, nil
}

/* ---------- 批量生图 ---------- */

func batchSuffix(r *http.Request, idPart, tail string) (string, error) {
	suffix := ""
	if idPart != "" {
		id := r.PathValue("id")
		if !batchIDRe.MatchString(id) {
			return "", fail(400, "批量任务编号不合法")
		}
		suffix = "/" + id
	}
	return suffix + tail, nil
}

func (s *server) batchSubmit(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	body, err := readJSON(w, r)
	if err != nil {
		return err
	}
	payload, outputs, err := parseBatchBody(body)
	if err != nil {
		return err
	}

	key, cost, quota, err := s.store.reserveGeneration(user.ID, batchProtocol, outputs)
	if err != nil {
		return err
	}
	result, submitErr := batchJSON(r.Context(), key, http.MethodPost, "", payload)
	if submitErr != nil {
		// 任务没送到上游，额度退回去。
		refunded := s.store.refund(user.ID, cost)
		var typed *httpError
		if errors.As(submitErr, &typed) {
			typed.quota = &refunded
			return typed
		}
		return submitErr
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": result, "cost": cost, "outputs": outputs, "quota": quota})
	return nil
}

func parseBatchBody(body map[string]any) (map[string]any, int, error) {
	rawItems := arr(body, "items")
	if len(rawItems) == 0 {
		return nil, 0, fail(400, "批量任务至少要有一个条目")
	}
	if len(rawItems) > batchOutputLimit {
		return nil, 0, fail(400, fmt.Sprintf("单个批量任务最多 %d 个条目", batchOutputLimit))
	}
	items := []any{}
	outputs := 0
	for index, raw := range rawItems {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, 0, fail(400, fmt.Sprintf("第 %d 个条目不是对象", index+1))
		}
		customID := firstString(item["custom_id"])
		if customID == "" {
			customID = fmt.Sprintf("item_%d", index+1)
		}
		if !customIDRe.MatchString(customID) {
			return nil, 0, fail(400, fmt.Sprintf("第 %d 个条目的 custom_id 只能用字母、数字、下划线、点和短横线", index+1))
		}
		count := 1
		if value, ok := firstNumber(item["output_count"]); ok {
			count = int(value)
		}
		if count < 1 || count > batchItemOutputMax {
			return nil, 0, fail(400, fmt.Sprintf("第 %d 个条目的 output_count 需要是 1 到 %d 的整数", index+1, batchItemOutputMax))
		}
		prompt, err := requirePrompt(item["prompt"])
		if err != nil {
			return nil, 0, err
		}
		normalized := map[string]any{"custom_id": customID, "prompt": prompt, "output_count": count}
		if refs := arr(item, "reference_images"); len(refs) > 0 {
			if len(refs) > 8 {
				refs = refs[:8]
			}
			urls := []any{}
			for _, ref := range refs {
				checked, err := assertPublicImageURL(firstString(ref))
				if err != nil {
					return nil, 0, err
				}
				urls = append(urls, checked)
			}
			normalized["reference_images"] = urls
		}
		items = append(items, normalized)
		outputs += count
	}
	if outputs > batchOutputLimit {
		return nil, 0, fail(400, fmt.Sprintf("单个批量任务最多 %d 个输出，现在是 %d 个", batchOutputLimit, outputs))
	}

	model, err := parseModel(body["model"])
	if err != nil {
		return nil, 0, err
	}
	provider := firstString(body["provider"])
	if provider == "" {
		provider = "gemini_api"
	}
	payload := map[string]any{"model": model, "provider": truncate(provider, 40), "items": items}
	if imageSize := firstString(body["image_size"]); imageSize != "" {
		tier, err := parseTier(imageSize)
		if err != nil {
			return nil, 0, err
		}
		payload["image_size"] = tier
	}
	if mime := firstString(body["response_mime_type"]); mime != "" {
		if !allowedImageMimes[mime] {
			return nil, 0, fail(400, "response_mime_type 只支持 image/png、image/jpeg、image/webp")
		}
		payload["response_mime_type"] = mime
	}
	return payload, outputs, nil
}

func (s *server) batchList(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.requireUser(r); err != nil {
		return err
	}
	key, err := s.store.keyFor(batchProtocol)
	if err != nil {
		return err
	}
	result, err := batchJSON(r.Context(), key, http.MethodGet, r.URL.RawQuery, nil)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": result})
	return nil
}

func (s *server) batchRead(idPart, tail string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if _, err := s.requireUser(r); err != nil {
			return err
		}
		suffix, err := batchSuffix(r, idPart, tail)
		if err != nil {
			return err
		}
		key, err := s.store.keyFor(batchProtocol)
		if err != nil {
			return err
		}
		result, err := batchJSON(r.Context(), key, http.MethodGet, suffix, nil)
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"ok": true, "result": result})
		return nil
	}
}

func (s *server) batchDelete(tail string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if _, err := s.requireUser(r); err != nil {
			return err
		}
		suffix, err := batchSuffix(r, "/{id}", tail)
		if err != nil {
			return err
		}
		key, err := s.store.keyFor(batchProtocol)
		if err != nil {
			return err
		}
		result, err := batchJSON(r.Context(), key, http.MethodDelete, suffix, nil)
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"ok": true, "result": result})
		return nil
	}
}

func (s *server) batchCancel(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.requireUser(r); err != nil {
		return err
	}
	suffix, err := batchSuffix(r, "/{id}", "/cancel")
	if err != nil {
		return err
	}
	key, err := s.store.keyFor(batchProtocol)
	if err != nil {
		return err
	}
	result, err := batchJSON(r.Context(), key, http.MethodPost, suffix, map[string]any{})
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": result})
	return nil
}

// batchRaw / batchItemContent 要原样透传字节，不能当 JSON 读。
func (s *server) batchRaw(tail string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if _, err := s.requireUser(r); err != nil {
			return err
		}
		suffix, err := batchSuffix(r, "/{id}", tail)
		if err != nil {
			return err
		}
		return s.pipeBatch(w, r, suffix)
	}
}

func (s *server) batchItemContent(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.requireUser(r); err != nil {
		return err
	}
	customID := r.PathValue("customID")
	if !customIDRe.MatchString(customID) {
		return fail(400, "条目编号不合法")
	}
	id := r.PathValue("id")
	if !batchIDRe.MatchString(id) {
		return fail(400, "批量任务编号不合法")
	}
	return s.pipeBatch(w, r, "/"+id+"/items/"+customID+"/content")
}

func (s *server) pipeBatch(w http.ResponseWriter, r *http.Request, suffix string) error {
	key, err := s.store.keyFor(batchProtocol)
	if err != nil {
		return err
	}
	cred := credential{APIKey: key.APIKey, UserAgent: key.UserAgent}
	target, err := batchURL(key, suffix)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(r.Context(), upstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fail(502, "连接中转站失败")
	}
	for name, value := range authHeaders(cred) {
		req.Header.Set(name, value)
	}
	res, err := upstream.Do(req)
	if err != nil {
		return fail(502, "连接中转站失败")
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		payload := map[string]any{}
		json.Unmarshal(raw, &payload)
		return fail(res.StatusCode, errorMessage(payload, res.StatusCode))
	}
	contentType := res.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if length := res.Header.Get("Content-Length"); length != "" {
		w.Header().Set("Content-Length", length)
	}
	if disposition := res.Header.Get("Content-Disposition"); disposition != "" {
		w.Header().Set("Content-Disposition", disposition)
	}
	w.WriteHeader(res.StatusCode)
	io.Copy(w, res.Body)
	return nil
}

/* ---------- 管理端 ---------- */

func (s *server) adminLogin(w http.ResponseWriter, r *http.Request) error {
	body, err := readJSON(w, r)
	if err != nil {
		return err
	}
	token, err := s.store.loginAdmin(firstString(body["username"]), firstString(body["password"]))
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true}, sessionCookie(adminCookieName, token))
	return nil
}

func (s *server) adminLogout(w http.ResponseWriter, r *http.Request) error {
	if err := s.store.logout(cookieValue(r, adminCookieName)); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true}, sessionCookie(adminCookieName, ""))
	return nil
}

func (s *server) adminState(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireAdmin(r); err != nil {
		return err
	}
	state, err := s.store.adminState()
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{
		"ok":          true,
		"settings":    state.Settings,
		"keys":        state.Keys,
		"users":       state.Users,
		"checkinDate": state.CheckinDate,
	})
	return nil
}

func (s *server) adminSettings(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireAdmin(r); err != nil {
		return err
	}
	body, err := readJSON(w, r)
	if err != nil {
		return err
	}
	checkinMin, ok := firstNumber(body["checkinMin"])
	if !ok || checkinMin != float64(int(checkinMin)) {
		return fail(400, "签到额度需要是 0 到 1000 的整数")
	}
	checkinMax, ok := firstNumber(body["checkinMax"])
	if !ok || checkinMax != float64(int(checkinMax)) {
		return fail(400, "签到额度需要是 0 到 1000 的整数")
	}
	cost, ok := firstNumber(body["generateCost"])
	if !ok || cost != float64(int(cost)) {
		return fail(400, "每次消耗需要是 0 到 1000 的整数")
	}
	settings, err := s.store.updateSettings(int(checkinMin), int(checkinMax), int(cost))
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "settings": settings})
	return nil
}

func (s *server) adminPassword(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireAdmin(r); err != nil {
		return err
	}
	body, err := readJSON(w, r)
	if err != nil {
		return err
	}
	if err := s.store.changeAdminPassword(firstString(body["oldPassword"]), firstString(body["newPassword"])); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true})
	return nil
}

func (s *server) adminSaveKey(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireAdmin(r); err != nil {
		return err
	}
	body, err := readJSON(w, r)
	if err != nil {
		return err
	}
	protocol, err := parseProtocol(body["protocol"])
	if err != nil {
		return err
	}
	baseURL := strings.TrimSpace(firstString(body["baseUrl"]))
	if protocol == "gemini-official" {
		if _, err := relayOrigin(orDefault(baseURL, "https://uuapi.io")); err != nil {
			return err
		}
	} else {
		if _, err := openaiBase(orDefault(baseURL, "https://uuapi.io/v1")); err != nil {
			return err
		}
	}
	if baseURL == "" {
		if protocol == "gemini-official" {
			baseURL = "https://uuapi.io"
		} else {
			baseURL = "https://uuapi.io/v1"
		}
	}
	apiKey := strings.TrimSpace(firstString(body["apiKey"]))
	if apiKey != "" && (len(apiKey) < 8 || len(apiKey) > 400) {
		return fail(400, "API Key 长度不对")
	}
	var balance *float64
	if raw, exists := body["balance"]; exists && raw != nil && raw != "" {
		value, ok := firstNumber(raw)
		if !ok {
			return fail(400, "余额需要是数字")
		}
		balance = &value
	}
	enabled := true
	if raw, ok := body["enabled"].(bool); ok {
		enabled = raw
	}
	key, err := s.store.saveKey(KeyInput{
		ID:        firstString(body["id"]),
		Name:      firstString(body["name"]),
		Protocol:  protocol,
		BaseURL:   baseURL,
		APIKey:    apiKey,
		UserAgent: truncate(strings.TrimSpace(firstString(body["userAgent"])), 200),
		Balance:   balance,
		Enabled:   enabled,
		Note:      firstString(body["note"]),
	})
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "key": key})
	return nil
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func (s *server) adminDeleteKey(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireAdmin(r); err != nil {
		return err
	}
	if err := s.store.deleteKey(r.PathValue("id")); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true})
	return nil
}

func (s *server) adminRefreshBalance(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireAdmin(r); err != nil {
		return err
	}
	id := r.PathValue("id")
	key, err := s.store.keyByID(id)
	if err != nil {
		return fail(404, "找不到这把 Key")
	}
	fresh, err := fetchKeyBalance(r.Context(), key)
	if err != nil {
		message := "刷新余额失败"
		var typed *httpError
		if errors.As(err, &typed) {
			message = typed.msg
		}
		if _, saveErr := s.store.setKeyBalance(id, nil, "", nil, message); saveErr != nil {
			log.Printf("记录余额错误失败：%v", saveErr)
		}
		return err
	}
	updated, err := s.store.setKeyBalance(id, &fresh.Amount, fresh.Unit, fresh.Valid, "")
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "key": updated})
	return nil
}

// adminRefreshModels 问中转站这把 Key 能用哪些模型，记下来给用户端的模型下拉用。
func (s *server) adminRefreshModels(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireAdmin(r); err != nil {
		return err
	}
	id := r.PathValue("id")
	key, err := s.store.keyByID(id)
	if err != nil {
		return fail(404, "找不到这把 Key")
	}
	models, err := fetchKeyModels(r.Context(), key)
	if err != nil {
		message := "拉取模型失败"
		var typed *httpError
		if errors.As(err, &typed) {
			message = typed.msg
		}
		if _, saveErr := s.store.setKeyModels(id, nil, message); saveErr != nil {
			log.Printf("记录模型错误失败：%v", saveErr)
		}
		return err
	}
	updated, err := s.store.setKeyModels(id, models, "")
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "key": updated})
	return nil
}

// availableModels 给用户端：按调用方式汇总所有 Key 能用的模型。
func (s *server) availableModels(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.requireUser(r); err != nil {
		return err
	}
	models, err := s.store.availableModels()
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "models": models})
	return nil
}

func (s *server) adminPatchUser(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireAdmin(r); err != nil {
		return err
	}
	body, err := readJSON(w, r)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	var user User
	changed := false
	if raw, exists := body["quota"]; exists {
		value, ok := firstNumber(raw)
		if !ok || value != float64(int(value)) {
			return fail(400, "额度需要是 0 到 1000000 的整数")
		}
		user, err = s.store.setUserQuota(id, int(value))
		if err != nil {
			return err
		}
		changed = true
	}
	if raw, exists := body["disabled"]; exists {
		disabled, ok := raw.(bool)
		if !ok {
			return fail(400, "disabled 需要是布尔值")
		}
		user, err = s.store.setUserDisabled(id, disabled)
		if err != nil {
			return err
		}
		changed = true
	}
	if !changed {
		return fail(400, "没有要修改的字段")
	}
	writeJSON(w, 200, map[string]any{"ok": true, "user": user})
	return nil
}

func (s *server) adminResetUserPassword(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireAdmin(r); err != nil {
		return err
	}
	body, err := readJSON(w, r)
	if err != nil {
		return err
	}
	user, err := s.store.resetUserPassword(r.PathValue("id"), firstString(body["password"]))
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "user": user})
	return nil
}

func (s *server) adminDeleteUser(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireAdmin(r); err != nil {
		return err
	}
	if err := s.store.deleteUser(r.PathValue("id")); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true})
	return nil
}
