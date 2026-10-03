// 前端与后端唯一的接头处。
//
// 留空表示跟页面同源 —— 由 server/server.js 同时提供静态文件和 /api。
// 把前端挂到别的后端（比如以后用 Go 重写的那套）时，改成那个后端的地址：
//
//   window.DARKROOM_API = "http://127.0.0.1:8080";
//
// 跨源的话后端要允许携带 Cookie（CORS 的 Access-Control-Allow-Credentials），
// 因为登录态是 HttpOnly Cookie。
window.DARKROOM_API = "";
