package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultUserAgent   = "darkroom/1.0 (local image studio)"
	upstreamTimeout    = 120 * time.Second
	pollDeadline       = 180 * time.Second
	imageLimit         = 20 * 1024 * 1024
	imageFetchTimeout  = 60 * time.Second
	batchOutputLimit   = 200
	batchItemOutputMax = 4
	batchProtocol      = "gemini-batch"
)

// 中转地址白名单。RELAY_HOSTS 可以覆盖它（逗号分隔），自建中转或者本地起桩测试时用；
// 放开这个等于允许把请求发到任意主机，除非你清楚在做什么，否则别动。
var allowedHosts = loadAllowedHosts()

func loadAllowedHosts() map[string]bool {
	defaults := []string{"uuapi.io", "uuapi.net", "uuapi.shop", "uuapi.cc"}
	raw := strings.TrimSpace(os.Getenv("RELAY_HOSTS"))
	hosts := defaults
	if raw != "" {
		hosts = nil
		for _, part := range strings.Split(raw, ",") {
			if host := strings.TrimSpace(part); host != "" {
				hosts = append(hosts, host)
			}
		}
	}
	out := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		out[host] = true
	}
	return out
}

var protocols = map[string]bool{
	"gpt":             true,
	"nano":            true,
	"gemini-official": true,
	batchProtocol:     true,
}

// 不上跟随重定向，跟 Node 版一样自己判断，免得被带到别处。
// RELAY_CA_FILE 可以指定一份额外的 CA 证书，用来信任自签的中转站；默认不加载。
var upstream = newUpstreamClient()

func newUpstreamClient() *http.Client {
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if caFile := strings.TrimSpace(os.Getenv("RELAY_CA_FILE")); caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			log.Fatalf("读取 RELAY_CA_FILE 失败：%v", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			log.Fatalf("RELAY_CA_FILE 里没有可用的证书：%s", caFile)
		}
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	}
	return client
}

type credential struct {
	APIKey    string
	BaseURL   string
	UserAgent string
}

type imageFile struct {
	Mime   string
	Buffer []byte
	Name   string
}

type imageResult struct {
	URL  string `json:"url,omitempty"`
	B64  string `json:"b64,omitempty"`
	Mime string `json:"mime,omitempty"`
}

type generateResult struct {
	Channel string        `json:"channel"`
	TaskID  string        `json:"taskId"`
	Images  []imageResult `json:"images"`
}

/* ---------- 地址与头部 ---------- */

func parseRelayURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return nil, fail(400, "中转地址不是合法的 URL")
	}
	if parsed.Scheme != "https" {
		return nil, fail(400, "中转地址只允许 https")
	}
	if !allowedHosts[parsed.Hostname()] {
		return nil, fail(400, "中转地址只允许 uuapi.io、uuapi.net、uuapi.shop、uuapi.cc")
	}
	return parsed, nil
}

// openaiBase 把各种写法都归一到 {origin}/v1。
func openaiBase(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "https://uuapi.io/v1"
	}
	parsed, err := parseRelayURL(raw)
	if err != nil {
		return "", err
	}
	path := strings.TrimRight(parsed.Path, "/")
	path = strings.TrimSuffix(path, "/v1beta")
	if !strings.HasSuffix(path, "/v1") {
		path += "/v1"
	}
	return parsed.Scheme + "://" + parsed.Host + path, nil
}

func relayOrigin(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "https://uuapi.io"
	}
	parsed, err := parseRelayURL(raw)
	if err != nil {
		return "", err
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// authHeaders 带上 Key 自己的 User-Agent：中转站的外接策略会挡默认的 Go agent。
func authHeaders(cred credential) map[string]string {
	agent := strings.TrimSpace(cred.UserAgent)
	if agent == "" {
		agent = defaultUserAgent
	}
	return map[string]string{
		"Authorization": "Bearer " + cred.APIKey,
		"User-Agent":    agent,
	}
}

/* ---------- 发请求 ---------- */

type upstreamResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r upstreamResponse) payload() map[string]any {
	if len(r.Body) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(r.Body, &out); err != nil {
		return map[string]any{"message": truncate(string(r.Body), 500)}
	}
	return out
}

func callUpstream(ctx context.Context, method, target string, headers map[string]string, body io.Reader) (upstreamResponse, error) {
	return callUpstreamLimit(ctx, method, target, headers, body, 0)
}

// errTooBig 表示正文超过了 limit。调用方自己决定怎么跟用户说。
var errTooBig = errors.New("响应体超过上限")

// callUpstreamLimit 比 callUpstream 多一个正文上限。用户给的图片地址可能指向一个
// 几 G 的文件，读完了再判大小就晚了——所以边读边掐，超了就停。
// limit 为 0 表示不限。
func callUpstreamLimit(ctx context.Context, method, target string, headers map[string]string, body io.Reader, limit int64) (upstreamResponse, error) {
	reqCtx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, target, body)
	if err != nil {
		return upstreamResponse{}, fail(502, "连接中转站失败")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := upstream.Do(req)
	if err != nil {
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			return upstreamResponse{}, fail(504, "上游超时")
		}
		return upstreamResponse{}, fail(502, "连接中转站失败")
	}
	defer res.Body.Close()
	reader := io.Reader(res.Body)
	if limit > 0 {
		reader = io.LimitReader(res.Body, limit+1)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return upstreamResponse{}, fail(502, "读取上游响应失败")
	}
	if limit > 0 && int64(len(raw)) > limit {
		return upstreamResponse{Status: res.StatusCode, Header: res.Header}, errTooBig
	}
	return upstreamResponse{Status: res.StatusCode, Header: res.Header, Body: raw}, nil
}

func callJSON(ctx context.Context, method, target string, cred credential, payload any) (upstreamResponse, error) {
	headers := authHeaders(cred)
	headers["Content-Type"] = "application/json"
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return upstreamResponse{}, err
		}
		body = bytes.NewReader(raw)
	}
	return callUpstream(ctx, method, target, headers, body)
}

/* ---------- 错误信息 ---------- */

func errorMessage(payload map[string]any, status int) string {
	return upstreamHint(rawErrorMessage(payload, status))
}

// upstreamHint 给中转站的英文原文补一句中文，说清下一步该干什么。
// 光把原文透出去，用户看到 "is not supported by any configured account" 也不知道是
// 该换模型还是该换 Key。
func upstreamHint(message string) string {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "is not supported by any configured account"),
		strings.Contains(lower, "无可用渠道"), strings.Contains(lower, "没有可用渠道"):
		return message + "（这个模型不在你中转站账号的分组里：换一个模型，或到中转站后台确认这把 Key 所属的分组支持哪些模型。）"
	case strings.Contains(lower, "insufficient") && strings.Contains(lower, "quota"):
		return message + "（中转站账号余额不够了，去管理端刷新一下余额看看。）"
	}
	return message
}

func rawErrorMessage(payload map[string]any, status int) string {
	if payload != nil {
		switch nested := payload["error"].(type) {
		case string:
			if strings.TrimSpace(nested) != "" {
				return strings.TrimSpace(nested)
			}
		case map[string]any:
			if message, ok := nested["message"].(string); ok && strings.TrimSpace(message) != "" {
				return strings.TrimSpace(message)
			}
		}
		if message, ok := payload["message"].(string); ok && strings.TrimSpace(message) != "" {
			return strings.TrimSpace(message)
		}
		if feedback := obj(payload, "promptFeedback"); feedback != nil {
			if reason, ok := feedback["blockReason"].(string); ok && reason != "" {
				return "内容被拦截：" + reason
			}
		}
	}
	return fmt.Sprintf("上游返回 HTTP %d", status)
}

/* ---------- 取值小工具 ---------- */

func obj(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

func arr(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].([]any); ok {
		return v
	}
	return nil
}

func firstNumber(values ...any) (float64, bool) {
	for _, value := range values {
		switch n := value.(type) {
		case float64:
			return n, true
		case string:
			if strings.TrimSpace(n) == "" {
				continue
			}
			if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

func firstString(values ...any) string {
	for _, value := range values {
		if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

/* ---------- 从各种响应形状里抠图片 ---------- */

func sniffMime(b64 string) string {
	switch {
	case strings.HasPrefix(b64, "/9j/"):
		return "image/jpeg"
	case strings.HasPrefix(b64, "UklGR"):
		return "image/webp"
	case strings.HasPrefix(b64, "R0lGOD"):
		return "image/gif"
	default:
		return "image/png"
	}
}

type imageCollector struct {
	images []imageResult
	seen   map[string]bool
}

func newCollector() *imageCollector {
	return &imageCollector{seen: map[string]bool{}}
}

func (c *imageCollector) push(image imageResult) {
	key := image.URL
	if key == "" {
		key = image.B64
	}
	if key == "" || c.seen[key] {
		return
	}
	c.seen[key] = true
	c.images = append(c.images, image)
}

var (
	dataURLRe = regexp.MustCompile(`(?i)data:image/([a-z0-9.+-]+);base64,([a-z0-9+/=\s]+)`)
	linkRe    = regexp.MustCompile(`https://[^\s)"']+`)
	imageExt  = regexp.MustCompile(`(?i)\.(png|jpe?g|webp|gif)(\?|$)`)
)

func (c *imageCollector) addStrings(chunk string) {
	for _, match := range dataURLRe.FindAllStringSubmatch(chunk, -1) {
		c.push(imageResult{B64: strings.Join(strings.Fields(match[2]), ""), Mime: "image/" + strings.ToLower(match[1])})
	}
	for _, link := range linkRe.FindAllString(chunk, -1) {
		if imageExt.MatchString(link) || strings.Contains(strings.ToLower(link), "image") {
			c.push(imageResult{URL: link})
		}
	}
}

func collectImages(payload map[string]any) []imageResult {
	c := newCollector()
	if u := firstString(payload["image_url"]); u != "" {
		c.push(imageResult{URL: u})
	}
	buckets := []any{payload["data"], obj(payload, "result")["data"], payload["result"]}
	for _, bucket := range buckets {
		var list []any
		switch typed := bucket.(type) {
		case []any:
			list = typed
		case map[string]any:
			list = arr(typed, "data")
		}
		for _, item := range list {
			switch typed := item.(type) {
			case string:
				if strings.HasPrefix(typed, "https://") {
					c.push(imageResult{URL: typed})
				}
			case map[string]any:
				if u := firstString(typed["url"]); u != "" {
					c.push(imageResult{URL: u})
				} else if b64 := firstString(typed["b64_json"]); b64 != "" {
					c.push(imageResult{B64: strings.Join(strings.Fields(b64), ""), Mime: sniffMime(b64)})
				}
			}
		}
	}
	return c.images
}

func collectChatImages(payload map[string]any) []imageResult {
	c := newCollector()
	choices := arr(payload, "choices")
	if len(choices) == 0 {
		return c.images
	}
	message := obj(choices[0].(map[string]any), "message")
	var chunks []string
	switch content := message["content"].(type) {
	case string:
		chunks = append(chunks, content)
	case []any:
		for _, part := range content {
			typed, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := typed["text"].(string); ok {
				chunks = append(chunks, text)
			}
			if imageURL := obj(typed, "image_url"); imageURL != nil {
				if u, ok := imageURL["url"].(string); ok {
					chunks = append(chunks, u)
				}
			}
			if u, ok := typed["url"].(string); ok {
				chunks = append(chunks, u)
			}
			if b64, ok := typed["b64_json"].(string); ok && b64 != "" {
				c.push(imageResult{B64: strings.Join(strings.Fields(b64), ""), Mime: sniffMime(b64)})
			}
		}
	}
	for _, item := range arr(message, "images") {
		switch typed := item.(type) {
		case string:
			chunks = append(chunks, typed)
		case map[string]any:
			if imageURL := obj(typed, "image_url"); imageURL != nil {
				if u, ok := imageURL["url"].(string); ok {
					chunks = append(chunks, u)
				}
			}
			if u, ok := typed["url"].(string); ok {
				chunks = append(chunks, u)
			}
		}
	}
	for _, chunk := range chunks {
		c.addStrings(chunk)
	}
	return c.images
}

func chatText(payload map[string]any) string {
	choices := arr(payload, "choices")
	if len(choices) == 0 {
		return ""
	}
	message := obj(choices[0].(map[string]any), "message")
	text := ""
	switch content := message["content"].(type) {
	case string:
		text = content
	case []any:
		for _, part := range content {
			if typed, ok := part.(map[string]any); ok {
				if s, ok := typed["text"].(string); ok {
					text += s
				}
			}
		}
	}
	text = dataURLRe.ReplaceAllString(text, "")
	return truncate(strings.TrimSpace(text), 300)
}

func collectGeminiImages(payload map[string]any) []imageResult {
	c := newCollector()
	for _, candidate := range arr(payload, "candidates") {
		typed, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		parts := arr(obj(typed, "content"), "parts")
		for _, part := range parts {
			partMap, ok := part.(map[string]any)
			if !ok {
				continue
			}
			inline := obj(partMap, "inlineData")
			if inline == nil {
				inline = obj(partMap, "inline_data")
			}
			if inline != nil {
				if data, ok := inline["data"].(string); ok && data != "" {
					mime := firstString(inline["mimeType"], inline["mime_type"])
					if mime == "" {
						mime = sniffMime(data)
					}
					c.push(imageResult{B64: strings.Join(strings.Fields(data), ""), Mime: mime})
				}
			}
			if text, ok := partMap["text"].(string); ok {
				c.addStrings(text)
			}
		}
	}
	return c.images
}

/* ---------- 余额 ---------- */

type balance struct {
	Amount float64
	Unit   string
	Valid  *bool
}

// parseBalance 和 cc-switch 导入中转站时用的 extractor 完全一致。
func parseBalance(payload map[string]any) (balance, bool) {
	if payload == nil {
		return balance{}, false
	}
	quota := obj(payload, "quota")
	amount, ok := firstNumber(payload["remaining"], quota["remaining"], payload["balance"])
	if !ok {
		return balance{}, false
	}
	unit := firstString(payload["unit"], quota["unit"])
	if unit == "" {
		unit = "USD"
	}
	out := balance{Amount: amount, Unit: unit}
	switch valid := payload["is_active"].(type) {
	case bool:
		out.Valid = &valid
	default:
		if valid, ok := payload["isValid"].(bool); ok {
			out.Valid = &valid
		}
	}
	return out, true
}

// fetchKeyModels 问中转站这把 Key 能用哪些模型。
//
// /v1/models 的正文各家写法不一，id 和 display_name 都可能缺，所以两个字段都认，
// 缺 id 的条目直接丢掉——没有 id 就没法拿去生图。
func fetchKeyModels(ctx context.Context, key Key) ([]ModelInfo, error) {
	origin, err := relayOrigin(key.BaseURL)
	if err != nil {
		return nil, err
	}
	res, err := callUpstream(ctx, http.MethodGet, origin+"/v1/models",
		authHeaders(credential{APIKey: key.APIKey, UserAgent: key.UserAgent}), nil)
	if err != nil {
		return nil, err
	}
	payload := res.payload()
	if res.Status < 200 || res.Status >= 300 {
		return nil, fail(res.Status, errorMessage(payload, res.Status))
	}
	raw := arr(payload, "data")
	if raw == nil {
		raw = arr(payload, "models")
	}
	out := []ModelInfo{}
	seen := map[string]bool{}
	for _, entry := range raw {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		id := firstString(item["id"], item["name"], item["model"])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := firstString(item["display_name"], item["displayName"])
		if name == "" {
			name = id
		}
		out = append(out, ModelInfo{ID: id, Name: name})
	}
	if len(out) == 0 {
		return nil, fail(502, "模型接口没有返回任何模型")
	}
	return out, nil
}

func fetchKeyBalance(ctx context.Context, key Key) (balance, error) {
	origin, err := relayOrigin(key.BaseURL)
	if err != nil {
		return balance{}, err
	}
	res, err := callUpstream(ctx, http.MethodGet, origin+"/v1/usage", authHeaders(credential{APIKey: key.APIKey, UserAgent: key.UserAgent}), nil)
	if err != nil {
		return balance{}, err
	}
	payload := res.payload()
	if res.Status < 200 || res.Status >= 300 {
		return balance{}, fail(res.Status, errorMessage(payload, res.Status))
	}
	picked, ok := parseBalance(payload)
	if !ok {
		return balance{}, fail(502, "余额接口没有返回数字")
	}
	return picked, nil
}

/* ---------- 组装请求体 ---------- */

func buildJSONBody(payload map[string]any) (io.Reader, string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	return bytes.NewReader(raw), "application/json", nil
}

func buildMultipartBody(payload map[string]any, file *imageFile, mask *imageFile) (io.Reader, string, error) {
	buf := &bytes.Buffer{}
	writer := multipart.NewWriter(buf)
	write := func(name, value string) error { return writer.WriteField(name, value) }
	if err := write("model", firstString(payload["model"])); err != nil {
		return nil, "", err
	}
	if err := write("prompt", firstString(payload["prompt"])); err != nil {
		return nil, "", err
	}
	if err := write("size", firstString(payload["size"])); err != nil {
		return nil, "", err
	}
	if quality := firstString(payload["quality"]); quality != "" {
		if err := write("quality", quality); err != nil {
			return nil, "", err
		}
	}
	if err := writeFilePart(writer, "image", file); err != nil {
		return nil, "", err
	}
	if mask != nil {
		if err := writeFilePart(writer, "mask", mask); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buf, writer.FormDataContentType(), nil
}

func writeFilePart(writer *multipart.Writer, field string, file *imageFile) error {
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, field, file.Name))
	header.Set("Content-Type", file.Mime)
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = part.Write(file.Buffer)
	return err
}

func buildEditedRequest(cred credential, payload map[string]any, file, mask *imageFile) (io.Reader, map[string]string, error) {
	headers := authHeaders(cred)
	body, contentType, err := buildJSONBody(payload)
	if file != nil {
		body, contentType, err = buildMultipartBody(payload, file, mask)
	}
	if err != nil {
		return nil, nil, err
	}
	headers["Content-Type"] = contentType
	return body, headers, nil
}

/* ---------- 生图 ---------- */

var (
	syncFallbackStatuses = map[int]bool{404: true, 405: true, 501: true}
	chatFallbackStatuses = map[int]bool{400: true, 404: true, 405: true, 422: true, 501: true}
	doneStatuses         = map[string]bool{"completed": true, "succeeded": true, "success": true}
	failedStatuses       = map[string]bool{"failed": true, "error": true, "cancelled": true, "canceled": true}
)

func taskIDOf(payload map[string]any, res upstreamResponse) string {
	if id := firstString(payload["task_id"]); id != "" {
		return id
	}
	location := res.Header.Get("Location")
	if location == "" {
		location = firstString(payload["poll_url"])
	}
	match := regexp.MustCompile(`images/tasks/([^/?#]+)`).FindStringSubmatch(location)
	if match == nil {
		return ""
	}
	decoded, err := url.PathUnescape(match[1])
	if err != nil {
		return match[1]
	}
	return decoded
}

// 参考图和蒙版一律是「已经拿到手的文件」：用户给网址的话，buildSpec 会先替上游取回来。
// 所以这里没有 ImageURL / MaskURL——上游不用自己取图，热链、防盗链、签名短链过期
// 这些坑就都跟这次请求无关了。
type generateSpec struct {
	Model       string
	Prompt      string
	Mode        string
	Size        string
	Quality     string
	AspectRatio string
	ImageSize   string
	File        *imageFile
	Mask        *imageFile
}

func generate(ctx context.Context, cred credential, protocol string, spec generateSpec) (generateResult, error) {
	switch protocol {
	case "gemini-official":
		return generateOfficial(ctx, cred, spec)
	case "nano":
		// spec.Size 在 buildSpec 里已经按画幅和分辨率算好了。
		base, err := openaiBase(cred.BaseURL)
		if err != nil {
			return generateResult{}, err
		}
		return generateOpenAI(ctx, base, cred, spec, "chat")
	default:
		base, err := openaiBase(cred.BaseURL)
		if err != nil {
			return generateResult{}, err
		}
		return generateOpenAI(ctx, base, cred, spec, "sync")
	}
}

func generateOpenAI(ctx context.Context, base string, cred credential, spec generateSpec, fallback string) (generateResult, error) {
	payload := map[string]any{
		"model":           spec.Model,
		"prompt":          spec.Prompt,
		"size":            spec.Size,
		"response_format": "url",
	}
	if spec.Quality != "" {
		payload["quality"] = spec.Quality
	}
	asyncPath, syncPath := "/images/generations/async", "/images/generations"
	if spec.Mode == "edit" {
		asyncPath, syncPath = "/images/edits/async", "/images/edits"
	}
	body, headers, err := buildEditedRequest(cred, payload, spec.File, spec.Mask)
	if err != nil {
		return generateResult{}, err
	}
	queued, err := callUpstream(ctx, http.MethodPost, base+asyncPath, headers, body)
	if err != nil {
		return generateResult{}, err
	}
	if queued.Status >= 300 && queued.Status < 400 {
		return generateResult{}, fail(502, "上游返回了重定向，已中止")
	}

	if fallback == "chat" && chatFallbackStatuses[queued.Status] {
		imagesError := errorMessage(queued.payload(), queued.Status)
		// 对话接口只收文字和参考图，带不了蒙版。这时候退过去，用户会拿到一张
		// 整张重画的图，还以为局部重绘生效了——不如当场说清楚。
		if spec.Mask != nil {
			return generateResult{}, fail(400, "这把 Key 的生图接口不收这次请求（"+imagesError+
				"），而对话接口带不了蒙版。去掉蒙版再试，或换一把支持生图接口的 Key。")
		}
		result, chatErr := generateViaChat(ctx, base, cred, spec)
		if chatErr == nil {
			return result, nil
		}
		if syncFallbackStatuses[queued.Status] {
			return generateResult{}, chatErr
		}
		message := "对话生图失败"
		if typed, ok := chatErr.(*httpError); ok {
			message = typed.msg
		}
		return generateResult{}, fail(statusOf(chatErr), message+"；生图接口："+imagesError)
	}
	if fallback == "sync" && syncFallbackStatuses[queued.Status] {
		return syncImages(ctx, base, cred, syncPath, payload, spec.File, spec.Mask)
	}

	queuedPayload := queued.payload()
	if (queued.Status < 200 || queued.Status >= 300) && queued.Status != 202 {
		return generateResult{}, fail(queued.Status, errorMessage(queuedPayload, queued.Status))
	}
	immediate := collectImages(queuedPayload)
	queuedStatus := strings.ToLower(firstString(queuedPayload["status"]))
	if failedStatuses[queuedStatus] {
		return generateResult{}, fail(502, errorMessage(queuedPayload, queued.Status))
	}
	taskID := taskIDOf(queuedPayload, queued)
	if len(immediate) > 0 && (doneStatuses[queuedStatus] || queuedStatus == "") {
		return generateResult{Channel: "async", TaskID: taskID, Images: immediate}, nil
	}
	if taskID == "" {
		message := errorMessage(queuedPayload, queued.Status)
		if message == fmt.Sprintf("上游返回 HTTP %d", queued.Status) {
			message = "异步接口没有返回任务编号"
		}
		return generateResult{}, fail(502, message)
	}
	polled, err := pollTask(ctx, base, cred, taskID)
	if err != nil {
		return generateResult{}, err
	}
	return generateResult{Channel: "async", TaskID: polled.TaskID, Images: polled.Images}, nil
}

func syncImages(ctx context.Context, base string, cred credential, path string, payload map[string]any, file, mask *imageFile) (generateResult, error) {
	body, headers, err := buildEditedRequest(cred, payload, file, mask)
	if err != nil {
		return generateResult{}, err
	}
	res, err := callUpstream(ctx, http.MethodPost, base+path, headers, body)
	if err != nil {
		return generateResult{}, err
	}
	parsed := res.payload()
	if res.Status < 200 || res.Status >= 300 {
		return generateResult{}, fail(res.Status, errorMessage(parsed, res.Status))
	}
	images := collectImages(parsed)
	if len(images) == 0 {
		return generateResult{}, fail(502, "同步接口没有返回图片")
	}
	return generateResult{Channel: "sync", Images: images}, nil
}

func generateViaChat(ctx context.Context, base string, cred credential, spec generateSpec) (generateResult, error) {
	content := []any{map[string]any{"type": "text", "text": spec.Prompt}}
	if spec.File != nil {
		encoded := base64.StdEncoding.EncodeToString(spec.File.Buffer)
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + spec.File.Mime + ";base64," + encoded},
		})
	}
	message := map[string]any{"role": "user", "content": spec.Prompt}
	if len(content) > 1 {
		message["content"] = content
	}
	payload := map[string]any{"model": spec.Model, "stream": false, "messages": []any{message}}
	if spec.Size != "" {
		payload["size"] = spec.Size
	}
	res, err := callJSON(ctx, http.MethodPost, base+"/chat/completions", cred, payload)
	if err != nil {
		return generateResult{}, err
	}
	parsed := res.payload()
	if res.Status < 200 || res.Status >= 300 {
		return generateResult{}, fail(res.Status, errorMessage(parsed, res.Status))
	}
	images := collectChatImages(parsed)
	if len(images) == 0 {
		message := chatText(parsed)
		if message == "" {
			message = "对话接口没有返回图片"
		}
		return generateResult{}, fail(502, message)
	}
	return generateResult{Channel: "chat", Images: images}, nil
}

func generateOfficial(ctx context.Context, cred credential, spec generateSpec) (generateResult, error) {
	origin, err := relayOrigin(cred.BaseURL)
	if err != nil {
		return generateResult{}, err
	}
	parts := []any{}
	if spec.File != nil {
		parts = append(parts, map[string]any{"inlineData": map[string]any{
			"mimeType": spec.File.Mime,
			"data":     base64.StdEncoding.EncodeToString(spec.File.Buffer),
		}})
	}
	parts = append(parts, map[string]any{"text": spec.Prompt})

	target := origin + "/v1beta/models/" + url.PathEscape(spec.Model) + ":generateContent"
	res, err := callJSON(ctx, http.MethodPost, target, cred, map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": parts}},
		"generationConfig": map[string]any{
			"responseModalities": []any{"TEXT", "IMAGE"},
			"imageConfig":        map[string]any{"aspectRatio": spec.AspectRatio, "imageSize": spec.ImageSize},
		},
	})
	if err != nil {
		return generateResult{}, err
	}
	if res.Status >= 300 && res.Status < 400 {
		return generateResult{}, fail(502, "上游返回了重定向，已中止")
	}
	parsed := res.payload()
	if res.Status < 200 || res.Status >= 300 {
		return generateResult{}, fail(res.Status, errorMessage(parsed, res.Status))
	}
	images := collectGeminiImages(parsed)
	if len(images) == 0 {
		candidates := arr(parsed, "candidates")
		reason := ""
		if len(candidates) > 0 {
			if typed, ok := candidates[0].(map[string]any); ok {
				reason = firstString(typed["finishReason"])
			}
		}
		if reason != "" {
			return generateResult{}, fail(502, "官方接口没有返回图片（"+reason+"）")
		}
		return generateResult{}, fail(502, "官方接口没有返回图片")
	}
	return generateResult{Channel: "gemini", Images: images}, nil
}

func pollTask(ctx context.Context, base string, cred credential, taskID string) (generateResult, error) {
	deadline := time.Now().Add(pollDeadline)
	for time.Now().Before(deadline) {
		res, err := callUpstream(ctx, http.MethodGet, base+"/images/tasks/"+url.PathEscape(taskID), authHeaders(cred), nil)
		if err != nil {
			return generateResult{}, err
		}
		payload := res.payload()
		if res.Status >= 300 && res.Status < 400 {
			return generateResult{}, fail(502, "轮询被重定向，已中止")
		}
		if res.Status < 200 || res.Status >= 300 {
			return generateResult{}, fail(res.Status, errorMessage(payload, res.Status))
		}
		status := strings.ToLower(firstString(payload["status"]))
		if failedStatuses[status] {
			code := 502
			if raw, ok := firstNumber(payload["http_status"]); ok {
				code = int(raw)
			}
			return generateResult{}, fail(code, errorMessage(payload, code))
		}
		images := collectImages(payload)
		pending := status == "processing" || status == "pending" || status == "queued" || status == "running"
		if doneStatuses[status] || (len(images) > 0 && !pending) {
			if len(images) == 0 {
				return generateResult{}, fail(502, "任务已完成，但响应里没有图片")
			}
			return generateResult{Channel: "async", TaskID: taskID, Images: images}, nil
		}
		wait := 3 * time.Second
		if seconds, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && seconds > 0 {
			if seconds > 15 {
				seconds = 15
			}
			wait = time.Duration(seconds) * time.Second
		}
		select {
		case <-ctx.Done():
			return generateResult{}, ctx.Err()
		case <-time.After(wait):
		}
	}
	return generateResult{}, fail(504, "生图超时，任务仍在处理。可稍后用任务号到中转站查询")
}

/* ---------- 参考图 ---------- */

// assertPublicImageURL 只管地址这一层：https、公网、不是本机。图到底取不取得到、
// 是不是真图片，得真发一次请求才知道，那在 downloadReference 里。
// label 是给用户看的名字（参考图 / 蒙版 / 图片），报错要指名道姓，
// 不然蒙版下载失败却提示「参考图」，用户会去改错的地方。
func assertPublicImageURL(raw, label string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return "", fail(400, label+" URL 不合法")
	}
	if parsed.Scheme != "https" {
		return "", fail(400, label+" URL 只允许 https")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return "", fail(400, label+" URL 不能指向本机")
	}
	if regexp.MustCompile(`^(127\.|10\.|192\.168\.|0\.0\.0\.0|169\.254\.)`).MatchString(host) ||
		regexp.MustCompile(`^172\.(1[6-9]|2\d|3[0-1])\.`).MatchString(host) {
		return "", fail(400, label+" URL 不能指向内网")
	}
	return parsed.String(), nil
}

var allowedImageMimes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true}

// sniffImageMime 认字节，不认响应头。对象存储经常把真图的 content-type 标成
// application/octet-stream，只看头会把好好的图拒掉。认不出来就返回空。
func sniffImageMime(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

func decodeImage(mime, data, name, label string) (*imageFile, error) {
	if !allowedImageMimes[mime] {
		return nil, fail(400, label+"只支持 PNG、JPEG、WebP")
	}
	cleaned := strings.Join(strings.Fields(data), "")
	if cleaned == "" {
		return nil, fail(400, label+"是空的")
	}
	buffer, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return nil, fail(400, label+"不是合法的 base64")
	}
	if len(buffer) == 0 || len(buffer) > imageLimit {
		return nil, fail(400, fmt.Sprintf("%s需小于 %dMB", label, imageLimit/1024/1024))
	}
	safe := regexp.MustCompile(`[^\w.-]+`).ReplaceAllString(name, "_")
	if safe == "" {
		safe = "reference.png"
	}
	return &imageFile{Mime: mime, Buffer: buffer, Name: truncate(safe, 80)}, nil
}

// downloadReference 把参考图 / 蒙版取回来，顺手验一遍再交给上游。
// 上游自己取图失败时只会丢一句很难懂的错，所以在这里先替它取：
// 状态码、字节大小、真字节的魔数，都过一遍，错也要错得说得清。
// label 是给用户看的名字，name 是发给上游的文件名（ASCII，免得 multipart 头里出中文）。
func downloadReference(ctx context.Context, raw, label, name string) (*imageFile, error) {
	current, err := assertPublicImageURL(raw, label)
	if err != nil {
		return nil, err
	}
	for hop := 0; hop < 3; hop++ {
		res, err := callUpstreamLimit(ctx, http.MethodGet, current, map[string]string{"User-Agent": defaultUserAgent}, nil, imageLimit)
		if errors.Is(err, errTooBig) {
			return nil, fail(400, fmt.Sprintf("%s需小于 %dMB", label, imageLimit/1024/1024))
		}
		if err != nil {
			return nil, err
		}
		if res.Status >= 300 && res.Status < 400 {
			location := res.Header.Get("Location")
			if location == "" {
				return nil, fail(400, label+"下载失败：对方返回了跳转却没给地址")
			}
			base, _ := url.Parse(current)
			next, _ := url.Parse(location)
			current, err = assertPublicImageURL(base.ResolveReference(next).String(), label)
			if err != nil {
				return nil, err
			}
			continue
		}
		if res.Status < 200 || res.Status >= 300 {
			return nil, fail(400, fmt.Sprintf("%s取不到：对方返回 HTTP %d", label, res.Status))
		}
		if len(res.Body) == 0 {
			return nil, fail(400, label+"是空的")
		}
		if len(res.Body) > imageLimit {
			return nil, fail(400, fmt.Sprintf("%s需小于 %dMB", label, imageLimit/1024/1024))
		}
		mime := sniffImageMime(res.Body)
		if mime == "" {
			// 最常见的坑：用户复制的是网页地址，不是图片地址。
			return nil, fail(400, fmt.Sprintf("%s不是图片（对方返回 %s），要图片直链", label, describeBody(res)))
		}
		return &imageFile{
			Mime:   mime,
			Buffer: res.Body,
			Name:   name + "." + strings.TrimPrefix(mime, "image/"),
		}, nil
	}
	return nil, fail(400, label+"跳转次数太多")
}

// describeBody 把「对方到底给了什么」写进报错里，用户好判断是复制错地址了还是图床抽风。
func describeBody(res upstreamResponse) string {
	kind := strings.ToLower(strings.TrimSpace(strings.Split(res.Header.Get("Content-Type"), ";")[0]))
	if kind == "" {
		kind = "没标类型"
	}
	if kind == "text/html" {
		return kind + "，像是网页不是图"
	}
	return kind
}

// fetchGeneratedImage 把上游给的成品图拉回来存进库，省得链接过期后作品集只剩空框。
// 拉不动（含地址不是公网、超限、上游 4xx）就返回空，调用方退回直接用原链接。
func fetchGeneratedImage(ctx context.Context, rawURL string) ([]byte, string) {
	target, err := assertPublicImageURL(rawURL, "图片")
	if err != nil {
		return nil, ""
	}
	res, err := callUpstreamLimit(ctx, http.MethodGet, target, map[string]string{"User-Agent": defaultUserAgent}, nil, imageLimit)
	if err != nil || res.Status < 200 || res.Status >= 300 {
		return nil, ""
	}
	if len(res.Body) == 0 {
		return nil, ""
	}
	mime := strings.ToLower(strings.TrimSpace(strings.Split(res.Header.Get("Content-Type"), ";")[0]))
	if !allowedImageMimes[mime] {
		mime = strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(res.Body), ";")[0]))
	}
	if !allowedImageMimes[mime] {
		mime = "image/png"
	}
	return res.Body, mime
}

/* ---------- 批量生图 ---------- */

func batchURL(key Key, suffix string) (string, error) {
	base, err := openaiBase(key.BaseURL)
	if err != nil {
		return "", err
	}
	return base + "/images/batches" + suffix, nil
}

func callBatch(ctx context.Context, key Key, method, suffix string, payload any) (upstreamResponse, error) {
	cred := credential{APIKey: key.APIKey, UserAgent: key.UserAgent}
	target, err := batchURL(key, suffix)
	if err != nil {
		return upstreamResponse{}, err
	}
	res, err := callJSON(ctx, method, target, cred, payload)
	if err != nil {
		return upstreamResponse{}, err
	}
	if res.Status >= 300 && res.Status < 400 {
		return upstreamResponse{}, fail(502, "上游返回了重定向，已中止")
	}
	return res, nil
}

func batchJSON(ctx context.Context, key Key, method, suffix string, payload any) (map[string]any, error) {
	res, err := callBatch(ctx, key, method, suffix, payload)
	if err != nil {
		return nil, err
	}
	parsed := res.payload()
	if res.Status < 200 || res.Status >= 300 {
		return nil, fail(res.Status, errorMessage(parsed, res.Status))
	}
	return parsed, nil
}

func statusOf(err error) int {
	var typed *httpError
	if errors.As(err, &typed) {
		return typed.status
	}
	return 502
}
