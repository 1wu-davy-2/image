// 登录页：登录 / 注册，成功就进工作台。
//
// 登录态是 HttpOnly Cookie，JS 读不到，所以「已经登录了吗」只能问后端 /api/me
// ——它未登录时回 user: null。见 config.js 里的 DARKROOM_API。
//
// 登录收名称或邮箱（后端两个字段都查），注册收名称、中文名、密码、手机号、邮箱。

(() => {

const API_BASE = String(window.DARKROOM_API || "").replace(/\/+$/, "");
function api(path) {
  return `${API_BASE}${path}`;
}

const loginForm = document.querySelector("#loginForm");
const registerForm = document.querySelector("#registerForm");
const status = document.querySelector("#status");

function setStatus(message, isError) {
  status.textContent = message || "";
  status.classList.toggle("error", Boolean(isError));
}

function showMode(mode) {
  loginForm.classList.toggle("hidden", mode !== "login");
  registerForm.classList.toggle("hidden", mode !== "register");
  setStatus("");
}

for (const input of document.querySelectorAll('input[name="authMode"]')) {
  input.addEventListener("change", () => showMode(input.value));
}

async function post(path, body) {
  const buttons = document.querySelectorAll("form button");
  buttons.forEach((button) => { button.disabled = true; });
  setStatus("");
  try {
    const response = await fetch(api(path), {
      credentials: "include",
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.ok) throw new Error(data.error || "进不去");
    window.location.replace("./studio");
  } catch (error) {
    setStatus(error.message, true);
    buttons.forEach((button) => { button.disabled = false; });
  }
}

const value = (selector) => document.querySelector(selector).value.trim();

loginForm.addEventListener("submit", (event) => {
  event.preventDefault();
  post("/api/auth/login", {
    account: value("#loginAccount"),
    password: document.querySelector("#loginPassword").value,
  });
});

registerForm.addEventListener("submit", (event) => {
  event.preventDefault();
  post("/api/auth/register", {
    username: value("#regUsername"),
    displayName: value("#regDisplayName"),
    password: document.querySelector("#regPassword").value,
    phone: value("#regPhone"),
    email: value("#regEmail"),
  });
});

// 已经登录就不用再进这一页。探不通（后端没起）就照常显示表单。
fetch(api("/api/me"), { credentials: "include" })
  .then((response) => response.json())
  .then((data) => {
    if (data && data.user) window.location.replace("./studio");
  })
  .catch(() => {});

})();
