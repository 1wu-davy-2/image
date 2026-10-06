package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

/* ---------- 假图床 + 假中转站 ---------- */

// 生产代码里中转地址只认 uuapi.*，图片地址不许指向内网，所以测试里没法直接用
// 127.0.0.1 那个地址。办法是编两个域名（relay.test / img.test），再把拨号器改成
// 「不管拨谁，都拨到 httptest 这个监听上」——生产代码一行都不用为测试开洞。
const (
	fakeRelayHost = "relay.test"
	fakeImageHost = "img.test"
)

// 一张最小 PNG 的头，够魔数嗅探认出来。
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)

type upstreamCall struct {
	Path        string
	ContentType string
	Fields      map[string]string
	Files       map[string][]byte
	JSON        map[string]any
}

func (c upstreamCall) fileNames() []string {
	names := make([]string, 0, len(c.Files))
	for name := range c.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// prompt 在两种形状里的位置不一样：multipart 是字段，URL 形状在正文顶层。
func (c upstreamCall) prompt() string {
	if value := firstString(c.JSON["prompt"]); value != "" {
		return value
	}
	return c.Fields["prompt"]
}

// imageURLs 把 JSON 形状里的图片地址按 images / mask 取出来，好跟文件形状对照着断言。
func (c upstreamCall) imageURLs() []string {
	out := []string{}
	for _, item := range arr(c.JSON, "images") {
		if entry, ok := item.(map[string]any); ok {
			out = append(out, firstString(entry["image_url"]))
		}
	}
	if mask, ok := c.JSON["mask"].(map[string]any); ok {
		out = append(out, firstString(mask["image_url"]))
	}
	return out
}

type fakeWorld struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []upstreamCall
	// imagesFail 让生图接口回 400，用来逼 nano 走对话兜底。
	imagesFail bool
}

func newFakeWorld(t *testing.T) *fakeWorld {
	t.Helper()
	world := &fakeWorld{}
	world.server = httptest.NewTLSServer(http.HandlerFunc(world.serve))
	t.Cleanup(world.server.Close)

	original := upstream
	upstream = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, network, world.server.Listener.Addr().String())
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	allowedHosts[fakeRelayHost] = true
	t.Cleanup(func() {
		upstream = original
		delete(allowedHosts, fakeRelayHost)
	})
	return world
}

func (w *fakeWorld) serve(rw http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/ref.png", "/mask.png", "/out.png":
		rw.Header().Set("Content-Type", "image/png")
		rw.Write(pngBytes)
		return
	case "/page.html":
		// 常见的坑：用户复制的是网页地址，不是图片地址。
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(rw, `<html><body><img src="/ref.png"></body></html>`)
		return
	case "/no-extension":
		// 对象存储常见：真图，但 content-type 是八竿子打不着的 octet-stream。
		// 只能靠魔数认。（不显式设的话 net/http 会自己嗅探成 image/png，测不到这段。）
		rw.Header().Set("Content-Type", "application/octet-stream")
		rw.Write(pngBytes)
		return
	case "/missing.png":
		http.NotFound(rw, r)
		return
	}

	call := upstreamCall{
		Path:        r.URL.Path,
		ContentType: r.Header.Get("Content-Type"),
		Fields:      map[string]string{},
		Files:       map[string][]byte{},
	}
	mediaType, params, _ := mime.ParseMediaType(call.ContentType)
	if strings.HasPrefix(mediaType, "multipart/") {
		reader := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			body, _ := io.ReadAll(part)
			if part.FileName() != "" {
				call.Files[part.FormName()] = body
			} else {
				call.Fields[part.FormName()] = string(body)
			}
		}
	} else {
		_ = json.NewDecoder(r.Body).Decode(&call.JSON)
	}

	w.mu.Lock()
	w.calls = append(w.calls, call)
	imagesFail := w.imagesFail
	w.mu.Unlock()

	if strings.Contains(r.URL.Path, ":generateContent") {
		rw.Header().Set("Content-Type", "application/json")
		out := base64.StdEncoding.EncodeToString(pngBytes)
		io.WriteString(rw, `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"`+out+`"}}]}}]}`)
		return
	}
	if r.URL.Path == "/v1/chat/completions" {
		rw.Header().Set("Content-Type", "application/json")
		io.WriteString(rw, `{"choices":[{"message":{"content":"![img](https://`+fakeImageHost+`/out.png)"}}]}`)
		return
	}
	if imagesFail {
		rw.WriteHeader(400)
		io.WriteString(rw, `{"error":{"message":"images endpoint not supported"}}`)
		return
	}

	// 异步接口一律 404，让代码退到同步接口——同步那次就是用户实际会被发出去的请求。
	if strings.HasSuffix(r.URL.Path, "/async") {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	io.WriteString(rw, `{"data":[{"url":"https://`+fakeImageHost+`/out.png"}]}`)
}

func (w *fakeWorld) all() []upstreamCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]upstreamCall(nil), w.calls...)
}

// find 按路径找那一次请求。生图成功后服务端会另起一个 goroutine 去把成品图拉回来，
// 那也会记一笔，所以不能只看最后一次。
func (w *fakeWorld) find(t *testing.T, path string) upstreamCall {
	t.Helper()
	for _, call := range w.all() {
		if call.Path == path {
			return call
		}
	}
	t.Fatalf("上游没收到 %s", path)
	return upstreamCall{}
}

func (w *fakeWorld) last(t *testing.T) upstreamCall {
	t.Helper()
	calls := w.all()
	if len(calls) == 0 {
		t.Fatal("上游一次都没被调到")
	}
	return calls[len(calls)-1]
}

func (w *fakeWorld) reset() {
	w.mu.Lock()
	w.calls = nil
	w.mu.Unlock()
}

/* ---------- 用例 ---------- */

func testCredential() credential {
	return credential{APIKey: "sk-test", BaseURL: "https://" + fakeRelayHost + "/v1", UserAgent: "darkroom-test"}
}

func editBody() map[string]any {
	return map[string]any{
		"model":   "gpt-image-2.5",
		"prompt":  "把背景换成雪山",
		"mode":    "edit",
		"size":    "1024x1024",
		"quality": "medium",
	}
}

func pngField(name string) map[string]any {
	return map[string]any{
		"mime": "image/png",
		"name": name,
		"data": base64.StdEncoding.EncodeToString(pngBytes),
	}
}

// runEdit 走一遍 buildSpec + generate，返回上游最后收到的那次请求。
func runEdit(t *testing.T, world *fakeWorld, body map[string]any, protocol string) (upstreamCall, error) {
	t.Helper()
	world.reset()
	spec, err := buildSpec(context.Background(), body, protocol)
	if err != nil {
		return upstreamCall{}, err
	}
	if _, err := generate(context.Background(), testCredential(), protocol, spec); err != nil {
		return upstreamCall{}, err
	}
	return world.last(t), nil
}

// 参考图和蒙版六种组合，最后都要在上游那儿变成「实打实的文件」：
// 给网址的那一半由我们取回来，上游不用自己取图。
func TestGPTEditReferenceAndMask(t *testing.T) {
	world := newFakeWorld(t)

	cases := []struct {
		name  string
		body  map[string]any
		files []string
	}{
		{
			name:  "参考图文件 + 蒙版文件",
			body:  map[string]any{"image": pngField("参考图.png"), "mask": pngField("蒙版.png")},
			files: []string{"image", "mask"},
		},
		{
			name:  "只给参考图文件",
			body:  map[string]any{"image": pngField("参考图.png")},
			files: []string{"image"},
		},
		{
			name:  "参考图 URL",
			body:  map[string]any{"imageUrl": "https://" + fakeImageHost + "/ref.png"},
			files: []string{"image"},
		},
		{
			name: "参考图 URL + 蒙版 URL",
			body: map[string]any{
				"imageUrl": "https://" + fakeImageHost + "/ref.png",
				"maskUrl":  "https://" + fakeImageHost + "/mask.png",
			},
			files: []string{"image", "mask"},
		},
		{
			name:  "参考图文件 + 蒙版 URL",
			body:  map[string]any{"image": pngField("参考图.png"), "maskUrl": "https://" + fakeImageHost + "/mask.png"},
			files: []string{"image", "mask"},
		},
		{
			name:  "参考图 URL + 蒙版文件",
			body:  map[string]any{"imageUrl": "https://" + fakeImageHost + "/ref.png", "mask": pngField("蒙版.png")},
			files: []string{"image", "mask"},
		},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			body := editBody()
			for key, value := range item.body {
				body[key] = value
			}
			call, err := runEdit(t, world, body, "gpt")
			if err != nil {
				t.Fatalf("生图失败：%v", err)
			}
			if got := call.fileNames(); strings.Join(got, ",") != strings.Join(item.files, ",") {
				t.Errorf("上游收到的文件字段 = %v，想要 %v（正文 %s）", got, item.files, call.ContentType)
			}
			// 网址不该再转给上游——上游自己取图正是要避开的坑。
			if got := call.imageURLs(); len(got) != 0 {
				t.Errorf("图片地址不该出现在上游正文里：%v", got)
			}
			if string(call.Files["image"]) != string(pngBytes) {
				t.Errorf("上游收到的参考图不是原字节：%d 字节", len(call.Files["image"]))
			}
			if len(call.Files["mask"]) > 0 && string(call.Files["mask"]) != string(pngBytes) {
				t.Errorf("上游收到的蒙版不是原字节：%d 字节", len(call.Files["mask"]))
			}
			if call.prompt() != "把背景换成雪山" {
				t.Errorf("prompt 没带过去：%q", call.prompt())
			}
		})
	}
}

// 参考图 / 蒙版给的是网址时，先自己取一遍，确认真是张图再往上游发。
// 网页地址、404、非图片，都要当场说清楚，别把上游那句看不懂的错转给用户。
func TestEditRejectsUnfetchableURL(t *testing.T) {
	world := newFakeWorld(t)

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{
			name: "参考图指向网页",
			body: map[string]any{"imageUrl": "https://" + fakeImageHost + "/page.html"},
			want: "不是图片",
		},
		{
			name: "参考图 404",
			body: map[string]any{"imageUrl": "https://" + fakeImageHost + "/missing.png"},
			want: "取不到",
		},
		{
			name: "蒙版指向网页",
			body: map[string]any{
				"image":   pngField("参考图.png"),
				"maskUrl": "https://" + fakeImageHost + "/page.html",
			},
			want: "蒙版",
		},
		{
			name: "蒙版 404",
			body: map[string]any{
				"imageUrl": "https://" + fakeImageHost + "/ref.png",
				"maskUrl":  "https://" + fakeImageHost + "/missing.png",
			},
			want: "蒙版",
		},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			body := editBody()
			for key, value := range item.body {
				body[key] = value
			}
			_, err := runEdit(t, world, body, "gpt")
			if err == nil {
				t.Fatalf("应该报错，实际发到上游去了：%s", world.last(t).Path)
			}
			if !strings.Contains(err.Error(), item.want) {
				t.Errorf("错误信息 = %q，想要含 %q", err.Error(), item.want)
			}
		})
	}
}

// 图床把 content-type 标成 octet-stream 时不能一棍子打死，得看真字节是不是图片。
func TestEditAcceptsImageWithoutContentType(t *testing.T) {
	world := newFakeWorld(t)
	body := editBody()
	// 参考图 URL + 蒙版文件 → 参考图必须被下下来，正好验下载那一段。
	body["imageUrl"] = "https://" + fakeImageHost + "/no-extension"
	body["mask"] = pngField("蒙版.png")

	call, err := runEdit(t, world, body, "gpt")
	if err != nil {
		t.Fatalf("真图片不该被拒：%v", err)
	}
	if len(call.Files["image"]) == 0 {
		t.Errorf("参考图没下下来，上游收到的是 %v", call.imageURLs())
	}
}

// Gemini 官方直连的参考图以前是它自己下，现在也统一在 buildSpec 里下好。
// 这条是防改完 GPT 那半边、忘了这半边。
func TestGeminiOfficialEditUsesDownloadedReference(t *testing.T) {
	world := newFakeWorld(t)
	body := editBody()
	body["imageUrl"] = "https://" + fakeImageHost + "/ref.png"
	body["aspectRatio"] = "1:1"
	body["imageSize"] = "2K"

	spec, err := buildSpec(context.Background(), body, "gemini-official")
	if err != nil {
		t.Fatalf("建规格失败：%v", err)
	}
	if spec.File == nil {
		t.Fatal("参考图应该已经取回来了")
	}
	world.reset()
	if _, err := generate(context.Background(), testCredential(), "gemini-official", spec); err != nil {
		t.Fatalf("生图失败：%v", err)
	}

	call := world.last(t)
	raw, _ := json.Marshal(call.JSON)
	want := base64.StdEncoding.EncodeToString(pngBytes)
	if !strings.Contains(string(raw), want) {
		t.Errorf("Gemini 正文里没有参考图的字节：%s", truncate(string(raw), 200))
	}
	if !strings.Contains(call.Path, ":generateContent") {
		t.Errorf("打错接口了：%s", call.Path)
	}
}

// 带蒙版时不许退到对话兜底。对话接口只收文字和参考图，退过去用户会拿到一张
// 整张重画的图，还以为局部重绘生效了——宁可不画，也不能悄悄画错。
func TestNanoWithMaskDoesNotFallBackToChat(t *testing.T) {
	world := newFakeWorld(t)
	world.imagesFail = true

	body := editBody()
	body["image"] = pngField("参考图.png")
	body["mask"] = pngField("蒙版.png")

	_, err := runEdit(t, world, body, "nano")
	if err == nil {
		t.Fatal("生图接口不收、对话接口又带不了蒙版，应该当场报错")
	}
	if !strings.Contains(err.Error(), "蒙版") {
		t.Errorf("错误信息得提到蒙版：%q", err.Error())
	}
	for _, call := range world.all() {
		if call.Path == "/v1/chat/completions" {
			t.Fatal("带蒙版还退了对话兜底")
		}
	}
}

// 不带蒙版时对话兜底照旧——这是香蕉出图的主要路径，别把这条路堵死。
func TestNanoWithoutMaskStillFallsBackToChat(t *testing.T) {
	world := newFakeWorld(t)
	world.imagesFail = true

	body := editBody()
	body["image"] = pngField("参考图.png")

	call, err := runEdit(t, world, body, "nano")
	if err != nil {
		t.Fatalf("不带蒙版该能出图：%v", err)
	}
	if call.Path != "/v1/chat/completions" {
		t.Fatalf("没走到对话兜底，最后打到 %s", call.Path)
	}
	raw, _ := json.Marshal(call.JSON)
	if !strings.Contains(string(raw), "data:image/png;base64,") {
		t.Errorf("对话兜底得把参考图带上：%s", truncate(string(raw), 200))
	}
}

// 蒙版自己单独出现、或者跑到非图生图 / Gemini 上，都得当场说清楚。
func TestGPTEditMaskNeedsReference(t *testing.T) {
	newFakeWorld(t)

	cases := []struct {
		name     string
		body     map[string]any
		protocol string
		want     string
	}{
		{
			name:     "只有蒙版没有参考图",
			body:     map[string]any{"mask": pngField("蒙版.png")},
			protocol: "gpt",
			want:     "图生图需要上传参考图",
		},
		{
			name:     "文生图却带了蒙版",
			body:     map[string]any{"mode": "generate", "mask": pngField("蒙版.png")},
			protocol: "gpt",
			want:     "蒙版只在图生图时可用",
		},
		{
			name:     "Gemini 官方直连带蒙版",
			body:     map[string]any{"image": pngField("参考图.png"), "mask": pngField("蒙版.png")},
			protocol: "gemini-official",
			want:     "Gemini 官方直连不支持蒙版",
		},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			body := editBody()
			for key, value := range item.body {
				body[key] = value
			}
			_, err := buildSpec(context.Background(), body, item.protocol)
			if err == nil {
				t.Fatalf("应该报错，实际放过去了")
			}
			if !strings.Contains(err.Error(), item.want) {
				t.Errorf("错误信息 = %q，想要含 %q", err.Error(), item.want)
			}
		})
	}
}
