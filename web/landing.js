// 介绍页只有一个判断：登录了就直接进工作台，没登录先去登录页。
//
// 「开始创作」的 href 默认就是 ./studio，探到未登录才改成 ./login。
// 探不通（后端没起）就维持默认值，由工作台自己兜底跳登录页。

(() => {

const API_BASE = String(window.DARKROOM_API || "").replace(/\/+$/, "");
const startBtn = document.querySelector("#startBtn");

fetch(`${API_BASE}/api/me`, { credentials: "include" })
  .then((response) => response.json())
  .then((data) => {
    if (!data || !data.user) startBtn.href = "./login";
  })
  .catch(() => {});

})();
