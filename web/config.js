// 前端与后端唯一的接头处。
//
// 前端跑在 16666（node web-server.js），后端跑在 18888（Go），两者不同源。
// 这里按当前页面的主机名拼出后端地址，这样用 127.0.0.1 或 localhost 打开都对得上
// —— 两者在浏览器眼里是不同站点，写死其中一个会让另一个的 Cookie 发不出去。
//
// 想换后端端口就改下面的 18888；想改成前后端同源（比如由后端直接托管 web/），
// 把整个表达式换成空串即可：
//
//   window.DARKROOM_API = "";
//
// 跨源时后端必须回 Access-Control-Allow-Origin（不能是 *）和
// Access-Control-Allow-Credentials，因为登录态是 HttpOnly Cookie。
window.DARKROOM_API = `${location.protocol}//${location.hostname}:18888`;
