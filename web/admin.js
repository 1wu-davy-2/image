const PROTOCOL_LABEL = {
  gpt: "GPT",
  nano: "香蕉",
  "gemini-official": "官方直连",
  "gemini-batch": "批量",
};

// 所有请求都从这里过，方便整体指向另一个后端。见 config.js。
const API_BASE = String(window.DARKROOM_API || "").replace(/\/+$/, "");

const loginView = document.querySelector("#loginView");
const appView = document.querySelector("#appView");
const loginStatus = document.querySelector("#loginStatus");
const settingsStatus = document.querySelector("#settingsStatus");
const keyStatus = document.querySelector("#keyStatus");
const userStatus = document.querySelector("#userStatus");
const keyRows = document.querySelector("#keyRows");
const userRows = document.querySelector("#userRows");

let state = { keys: [], users: [], settings: { checkinQuota: 5, generateCost: 1 } };

function setStatus(el, message, isError) {
  el.textContent = message || "";
  el.classList.toggle("error", Boolean(isError));
}

async function api(path, options = {}) {
  const response = await fetch(`${API_BASE}${path}`, {
    ...options,
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok || data.ok === false) throw new Error(data.error || `请求失败（${response.status}）`);
  return data;
}

function mask(secret) {
  const value = String(secret || "");
  if (value.length <= 4) return "••••";
  return `••••${value.slice(-4)}`;
}

function balanceText(key) {
  if (typeof key.balance === "number") return `${key.balance} ${key.balanceUnit || "USD"}`;
  return "未查询";
}

function fillForm(key) {
  document.querySelector("#keyId").value = key ? key.id : "";
  document.querySelector("#keyName").value = key ? key.name : "";
  document.querySelector("#keyProtocol").value = key ? key.protocol : "gpt";
  document.querySelector("#keyBase").value = key ? key.baseUrl : "https://uuapi.io/v1";
  document.querySelector("#keySecret").value = "";
  document.querySelector("#keyAgent").value = key ? key.userAgent || "" : "";
  document.querySelector("#keyBalance").value = key && typeof key.balance === "number" ? String(key.balance) : "";
  document.querySelector("#keyEnabled").checked = key ? key.enabled !== false : true;
  document.querySelector("#keyNote").value = key ? key.note || "" : "";
  document.querySelector("#saveKey").textContent = key ? "保存修改" : "保存 Key";
}

function render() {
  document.querySelector("#checkinQuota").value = state.settings.checkinQuota;
  document.querySelector("#generateCost").value = state.generateCost ?? state.settings.generateCost;
  keyRows.replaceChildren();
  if (!state.keys.length) {
    const row = document.createElement("tr");
    row.innerHTML = "<td colspan='5'>还没有 Key。</td>";
    keyRows.append(row);
  }
  state.keys.forEach((key) => {
    const row = document.createElement("tr");
    const name = document.createElement("td");
    name.textContent = key.name;
    const sub = document.createElement("div");
    sub.className = "sub";
    sub.textContent = key.userAgent ? `UA: ${key.userAgent}` : "UA: 默认";
    name.append(sub);
    const kind = document.createElement("td");
    kind.textContent = PROTOCOL_LABEL[key.protocol] || key.protocol;
    const money = document.createElement("td");
    money.className = "money";
    money.textContent = balanceText(key);
    if (key.balanceValid === false) {
      const invalid = document.createElement("div");
      invalid.className = "bad-text";
      invalid.textContent = "Key 已失效（is_active 为 false）";
      money.append(invalid);
    }
    if (key.balanceError) {
      const error = document.createElement("div");
      error.className = "bad-text";
      error.textContent = key.balanceError;
      money.append(error);
    }
    const enabled = document.createElement("td");
    const badge = document.createElement("span");
    badge.className = key.enabled ? "badge" : "badge off";
    badge.textContent = key.enabled ? "启用" : "停用";
    enabled.append(badge);
    const actions = document.createElement("td");
    actions.className = "row-actions";
    const edit = document.createElement("button");
    edit.type = "button";
    edit.className = "small";
    edit.textContent = "编辑";
    edit.addEventListener("click", () => {
      fillForm(key);
      document.querySelector("#keyName").focus();
    });
    const refresh = document.createElement("button");
    refresh.type = "button";
    refresh.className = "small";
    refresh.textContent = "刷新余额";
    refresh.addEventListener("click", () => refreshBalance(key.id, refresh));
    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "small";
    remove.textContent = "删除";
    remove.addEventListener("click", () => removeKey(key));
    actions.append(edit, refresh, remove);
    row.append(name, kind, money, enabled, actions);
    keyRows.append(row);
  });

  userRows.replaceChildren();
  if (!state.users.length) {
    const row = document.createElement("tr");
    row.innerHTML = "<td colspan='4'>还没有用户。</td>";
    userRows.append(row);
  }
  state.users.forEach((user) => {
    const row = document.createElement("tr");
    const name = document.createElement("td");
    name.textContent = user.username;
    const amount = document.createElement("td");
    const input = document.createElement("input");
    input.className = "user-quota";
    input.type = "number";
    input.min = "0";
    input.step = "1";
    input.value = String(user.quota);
    amount.append(input);
    const checkin = document.createElement("td");
    checkin.textContent = user.checkedInToday ? `${user.lastCheckinDate} · 今日已签` : (user.lastCheckinDate || "从未");
    const stateCell = document.createElement("td");
    const badge = document.createElement("span");
    badge.className = user.disabled ? "badge off" : "badge";
    badge.textContent = user.disabled ? "已停用" : "正常";
    stateCell.append(badge);
    const actions = document.createElement("td");
    actions.className = "row-actions";
    const save = document.createElement("button");
    save.type = "button";
    save.className = "small";
    save.textContent = "保存额度";
    save.addEventListener("click", () => saveQuota(user, input, save));
    const reset = document.createElement("button");
    reset.type = "button";
    reset.className = "small";
    reset.textContent = "重置密码";
    reset.addEventListener("click", () => resetPassword(user));
    const toggle = document.createElement("button");
    toggle.type = "button";
    toggle.className = "small";
    toggle.textContent = user.disabled ? "启用" : "停用";
    toggle.addEventListener("click", () => toggleDisabled(user, toggle));
    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "small danger";
    remove.textContent = "删除";
    remove.addEventListener("click", () => removeUser(user));
    actions.append(save, reset, toggle, remove);
    row.append(name, amount, checkin, stateCell, actions);
    userRows.append(row);
  });
}

async function loadState() {
  state = await api("/api/admin/state");
  loginView.classList.add("hidden");
  appView.classList.remove("hidden");
  render();
}

async function refreshBalance(id, button) {
  button.disabled = true;
  setStatus(keyStatus, "正在向中转站查询余额…");
  try {
    await api(`/api/admin/keys/${encodeURIComponent(id)}/balance`, { method: "POST", body: "{}" });
    await loadState();
    setStatus(keyStatus, "余额已更新。");
  } catch (error) {
    await loadState().catch(() => {});
    setStatus(keyStatus, error.message, true);
  } finally {
    button.disabled = false;
  }
}

async function removeKey(key) {
  if (!window.confirm(`删除「${key.name}」？`)) return;
  await api(`/api/admin/keys/${encodeURIComponent(key.id)}`, { method: "DELETE" });
  if (document.querySelector("#keyId").value === key.id) fillForm(null);
  await loadState();
  setStatus(keyStatus, "已删除。");
}

async function saveQuota(user, input, button) {
  button.disabled = true;
  try {
    await api(`/api/admin/users/${encodeURIComponent(user.id)}`, {
      method: "PATCH",
      body: JSON.stringify({ quota: Number(input.value) }),
    });
    await loadState();
    setStatus(userStatus, `已更新 ${user.username} 的额度。`);
  } catch (error) {
    setStatus(userStatus, error.message, true);
  } finally {
    button.disabled = false;
  }
}

async function resetPassword(user) {
  const password = window.prompt(`给「${user.username}」设置新密码（6 到 72 位）：`);
  if (password === null) return;
  try {
    await api(`/api/admin/users/${encodeURIComponent(user.id)}/password`, {
      method: "POST",
      body: JSON.stringify({ password }),
    });
    setStatus(userStatus, `${user.username} 的密码已重置，之前的登录都失效了。`);
  } catch (error) {
    setStatus(userStatus, error.message, true);
  }
}

async function toggleDisabled(user, button) {
  button.disabled = true;
  try {
    await api(`/api/admin/users/${encodeURIComponent(user.id)}`, {
      method: "PATCH",
      body: JSON.stringify({ disabled: !user.disabled }),
    });
    await loadState();
    setStatus(userStatus, user.disabled ? `已启用 ${user.username}。` : `已停用 ${user.username}，他的登录都失效了。`);
  } catch (error) {
    setStatus(userStatus, error.message, true);
  } finally {
    button.disabled = false;
  }
}

async function removeUser(user) {
  if (!window.confirm(`删除用户「${user.username}」？他的额度记录会一起消失。`)) return;
  try {
    await api(`/api/admin/users/${encodeURIComponent(user.id)}`, { method: "DELETE" });
    await loadState();
    setStatus(userStatus, `已删除 ${user.username}。`);
  } catch (error) {
    setStatus(userStatus, error.message, true);
  }
}

document.querySelector("#loginView").addEventListener("submit", async (event) => {
  event.preventDefault();
  setStatus(loginStatus, "正在进入…");
  try {
    await api("/api/admin/login", {
      method: "POST",
      body: JSON.stringify({
        username: document.querySelector("#adminUser").value.trim(),
        password: document.querySelector("#adminPassword").value,
      }),
    });
    document.querySelector("#adminPassword").value = "";
    await loadState();
  } catch (error) {
    setStatus(loginStatus, error.message, true);
  }
});

document.querySelector("#logoutBtn").addEventListener("click", async () => {
  await api("/api/admin/logout", { method: "POST", body: "{}" });
  appView.classList.add("hidden");
  loginView.classList.remove("hidden");
});

document.querySelector("#saveSettings").addEventListener("click", async () => {
  try {
    const data = await api("/api/admin/settings", {
      method: "PUT",
      body: JSON.stringify({
        checkinQuota: Number(document.querySelector("#checkinQuota").value),
        generateCost: Number(document.querySelector("#generateCost").value),
      }),
    });
    state.settings = data.settings;
    setStatus(settingsStatus, "规则已保存。");
  } catch (error) {
    setStatus(settingsStatus, error.message, true);
  }
});

document.querySelector("#passwordForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    await api("/api/admin/password", {
      method: "POST",
      body: JSON.stringify({
        oldPassword: document.querySelector("#oldPassword").value,
        newPassword: document.querySelector("#newPassword").value,
      }),
    });
    document.querySelector("#oldPassword").value = "";
    document.querySelector("#newPassword").value = "";
    setStatus(settingsStatus, "管理密码已修改。");
  } catch (error) {
    setStatus(settingsStatus, error.message, true);
  }
});

document.querySelector("#keyProtocol").addEventListener("change", () => {
  const protocol = document.querySelector("#keyProtocol").value;
  const base = document.querySelector("#keyBase");
  if (protocol === "gemini-official" && base.value.endsWith("/v1")) base.value = base.value.slice(0, -3);
  if (protocol !== "gemini-official" && /^https:\/\/[^/]+$/.test(base.value)) base.value = `${base.value}/v1`;
});

document.querySelectorAll("[data-origin]").forEach((button) => {
  button.addEventListener("click", () => {
    const origin = button.dataset.origin;
    const protocol = document.querySelector("#keyProtocol").value;
    document.querySelector("#keyBase").value = protocol === "gemini-official" ? origin : `${origin}/v1`;
  });
});

document.querySelectorAll("[data-agent]").forEach((button) => {
  button.addEventListener("click", () => {
    document.querySelector("#keyAgent").value = button.dataset.agent;
  });
});

document.querySelector("#resetKey").addEventListener("click", () => fillForm(null));

document.querySelector("#keyForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  const balanceRaw = document.querySelector("#keyBalance").value.trim();
  const payload = {
    id: document.querySelector("#keyId").value,
    name: document.querySelector("#keyName").value,
    protocol: document.querySelector("#keyProtocol").value,
    baseUrl: document.querySelector("#keyBase").value,
    apiKey: document.querySelector("#keySecret").value,
    userAgent: document.querySelector("#keyAgent").value,
    balance: balanceRaw === "" ? null : Number(balanceRaw),
    enabled: document.querySelector("#keyEnabled").checked,
    note: document.querySelector("#keyNote").value,
  };
  try {
    await api("/api/admin/keys", { method: "POST", body: JSON.stringify(payload) });
    fillForm(null);
    await loadState();
    setStatus(keyStatus, "Key 已保存。");
  } catch (error) {
    setStatus(keyStatus, error.message, true);
  }
});

loadState().catch(() => {
  appView.classList.add("hidden");
  loginView.classList.remove("hidden");
});
