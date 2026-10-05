// 系统设置：账号信息、改中文名、改密码、退出。
// 外壳在 shell.js，这里只管 main 里的东西。

(() => {

const api = window.Darkroom.api;

const displayName = document.querySelector("#displayName");
const profileStatus = document.querySelector("#profileStatus");
const passwordStatus = document.querySelector("#passwordStatus");

function setStatus(node, message, isError) {
  node.textContent = message || "";
  node.classList.toggle("error", Boolean(isError));
}

function render(session) {
  const user = session.user;
  document.querySelector("#accountName").textContent = user.username;
  document.querySelector("#accountEmail").textContent = user.email || "—（管理端账号）";
  document.querySelector("#accountPhone").textContent = user.phone || "—";
  document.querySelector("#accountCreated").textContent = window.Darkroom.formatDate(user.createdAt) || "—";
  displayName.value = user.displayName === "管理端" ? "" : user.displayName || "";
}

document.querySelector("#profileForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  setStatus(profileStatus, "");
  try {
    const response = await fetch(api("/api/me"), {
      credentials: "include",
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ displayName: displayName.value.trim() }),
    });
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.ok) throw new Error(data.error || "存不下");
    setStatus(profileStatus, "已保存。", false);
    render(await window.Darkroom.refreshMe());
  } catch (error) {
    setStatus(profileStatus, error.message, true);
  }
});

document.querySelector("#passwordForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  setStatus(passwordStatus, "");
  try {
    const response = await fetch(api("/api/me/password"), {
      credentials: "include",
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        oldPassword: document.querySelector("#oldPassword").value,
        newPassword: document.querySelector("#newPassword").value,
      }),
    });
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.ok) throw new Error(data.error || "改不了");
    document.querySelector("#oldPassword").value = "";
    document.querySelector("#newPassword").value = "";
    setStatus(passwordStatus, "密码已修改。", false);
  } catch (error) {
    setStatus(passwordStatus, error.message, true);
  }
});

document.querySelector("#logoutBtn").addEventListener("click", async () => {
  await fetch(api("/api/auth/logout"), { method: "POST", credentials: "include" });
  window.location.replace("./login");
});

window.Darkroom.ready.then((session) => {
  if (!session) return;
  render(session);
});

})();
