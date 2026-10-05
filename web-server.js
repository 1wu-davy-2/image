// 只干一件事：把 web/ 当静态文件发出去。
//
// 它不碰任何数据、不代理任何接口，纯静态。后端在另一个端口上（默认 6670 的
// Go 服务），前端通过 web/config.js 里的 DARKROOM_API 指过去。
//
//   node web-server.js              # 127.0.0.1:6664
//   PORT=8080 node web-server.js    # 换端口

const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");

const PORT = Number(process.env.PORT) || 6664;
const HOST = process.env.HOST || "127.0.0.1";
const ROOT = path.resolve(__dirname, process.env.WEB_DIR || "web");

const MIME = {
  ".html": "text/html; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".json": "application/json; charset=utf-8",
  ".svg": "image/svg+xml",
  ".png": "image/png",
  ".jpg": "image/jpeg",
  ".jpeg": "image/jpeg",
  ".webp": "image/webp",
  ".ico": "image/x-icon",
  ".woff2": "font/woff2",
};

// 无扩展名的路径映射到具体页面，省得每个页面加一条判断。
const PAGES = {
  admin: "admin.html",
  studio: "studio.html",
  login: "login.html",
  create: "create.html",
  gallery: "gallery.html",
  works: "works.html",
  settings: "settings.html",
};

function resolveFile(urlPath) {
  const pathname = decodeURIComponent(urlPath.split("?")[0]);
  let relative = pathname === "/" ? "index.html" : pathname.replace(/^\/+/, "").replace(/\/$/, "");
  if (PAGES[relative]) relative = PAGES[relative];
  // path.resolve 会吃掉 ..，再确认结果仍在 web/ 里。
  const file = path.resolve(ROOT, relative);
  if (file !== ROOT && !file.startsWith(ROOT + path.sep)) return null;
  return file;
}

const server = http.createServer((req, res) => {
  if (req.method !== "GET" && req.method !== "HEAD") {
    res.writeHead(405, { "Content-Type": "application/json; charset=utf-8" });
    res.end(JSON.stringify({ ok: false, error: "不支持的方法" }));
    return;
  }

  const file = resolveFile(req.url);
  if (!file) {
    res.writeHead(404, { "Content-Type": "application/json; charset=utf-8" });
    res.end(JSON.stringify({ ok: false, error: "找不到页面" }));
    return;
  }

  fs.readFile(file, (error, data) => {
    if (error) {
      res.writeHead(error.code === "ENOENT" ? 404 : 500, { "Content-Type": "application/json; charset=utf-8" });
      res.end(JSON.stringify({ ok: false, error: "找不到页面" }));
      return;
    }
    res.writeHead(200, {
      "Content-Type": MIME[path.extname(file).toLowerCase()] || "application/octet-stream",
      "Content-Length": data.length,
      "Cache-Control": "no-store",
      "X-Content-Type-Options": "nosniff",
    });
    res.end(req.method === "HEAD" ? undefined : data);
  });
});

server.listen(PORT, HOST, () => {
  console.log(`前端已启动 http://${HOST}:${PORT}`);
  console.log(`管理端 http://${HOST}:${PORT}/admin`);
  console.log(`静态目录 ${ROOT}`);
});
