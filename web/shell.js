// 工作台的外壳：顶栏右上角的用户菜单、左侧菜单栏、登录态。
//
// 每个工作台页面都引它，页面自己只管 <main> 里的内容。页面用
// <body data-nav="create"> 指明当前是哪一项，菜单栏据此高亮。
//
// 登录态是 HttpOnly Cookie，JS 读不到，所以只能问后端 /api/me。

// 包一层 IIFE：这些都是经典脚本，共享同一个全局作用域，不包的话
// 页面脚本里再写一个 const api 就会撞名报 SyntaxError。
(() => {

const API_BASE = String(window.DARKROOM_API || "").replace(/\/+$/, "");
function api(path) {
  return `${API_BASE}${path}`;
}

const NAV = [
  { key: "home", href: "./studio", label: "首页 · 签到" },
  { key: "works", href: "./works", label: "公开作品" },
  { key: "create", href: "./create", label: "创作" },
  { key: "gallery", href: "./gallery", label: "作品集" },
  { key: "settings", href: "./settings", label: "系统设置" },
];

let session = null;

function make(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function renderSidebar() {
  const box = document.querySelector("#sidebar");
  if (!box) return;
  const current = document.body.dataset.nav || "";

  box.replaceChildren();
  box.append(make("p", "eyebrow sidebar-title", "画坊"));

  const nav = make("nav", "sidebar-nav");
  for (const item of NAV) {
    const link = make("a", "sidebar-link");
    link.href = item.href;
    link.append(make("span", "", item.label));
    if (item.key === current) link.setAttribute("aria-current", "page");
    nav.append(link);
  }
  box.append(nav);

  const quota = make("div", "sidebar-quota");
  quota.append(make("p", "eyebrow", "额度"));
  quota.append(make("p", "sidebar-quota-number", "0"));
  quota.append(make("p", "hint", "每次生图消耗 1"));
  quota.id = "sidebarQuota";
  box.append(quota);

  // 管理端只有管理员看得见。第一次渲染时还没拿到登录态，refreshMe 之后再补一次。
  if (session && session.user.isAdmin) {
    box.append(make("div", "hairline"));
    const admin = make("a", "sidebar-link sidebar-link-quiet", "管理端");
    admin.href = "./admin";
    box.append(admin);
  }
}

function renderUserMenu() {
  const box = document.querySelector("#userMenu");
  if (!box || !session) return;
  const user = session.user;

  box.replaceChildren();
  const button = make("button", "user-trigger");
  button.type = "button";
  button.setAttribute("aria-haspopup", "true");
  button.setAttribute("aria-expanded", "false");
  button.append(make("span", "avatar", (user.username[0] || "?").toUpperCase()));
  button.append(make("span", "user-name", user.displayName || user.username));

  const panel = make("div", "user-panel hidden");
  const head = make("div", "user-panel-head");
  head.append(make("strong", "", user.username));
  head.append(make("small", "", user.email || "管理端账号"));
  panel.append(head);
  panel.append(make("div", "hairline"));

  const links = [
    ["./studio", "额度与签到"],
    ["./gallery", "我的作品集"],
    ["./settings", "系统设置"],
  ];
  if (user.isAdmin) links.push(["./admin", "管理端"]);
  for (const [href, label] of links) {
    const link = make("a", "user-panel-item", label);
    link.href = href;
    panel.append(link);
  }
  panel.append(make("div", "hairline"));

  const out = make("button", "user-panel-item danger", "退出登录");
  out.type = "button";
  out.addEventListener("click", async () => {
    await fetch(api("/api/auth/logout"), { method: "POST", credentials: "include" });
    window.location.replace("./login");
  });
  panel.append(out);

  button.addEventListener("click", (event) => {
    event.stopPropagation();
    const open = panel.classList.toggle("hidden");
    button.setAttribute("aria-expanded", String(!open));
  });
  document.addEventListener("click", () => {
    panel.classList.add("hidden");
    button.setAttribute("aria-expanded", "false");
  });

  box.append(button, panel);
}

function setQuota(value) {
  const node = document.querySelector("#sidebarQuota .sidebar-quota-number");
  if (node) node.textContent = String(value);
}

async function refreshMe() {
  const response = await fetch(api("/api/me"), { credentials: "include" });
  session = await response.json();
  if (!session || !session.user) {
    // 没会话（从没登录、过期、或被管理端踢了）就回登录页。
    window.location.replace("./login");
    return null;
  }
  // 管理端那一项要等登录态回来才知道显不显示，所以这里再渲染一次。
  renderSidebar();
  renderUserMenu();
  setQuota(session.user.quota);
  return session;
}

// 页面用 Darkroom.ready.then((session) => {...}) 拿登录态。
const ready = refreshMe().catch(() => null);

function formatDate(value) {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return new Intl.DateTimeFormat("zh-CN", {
    year: "numeric", month: "2-digit", day: "2-digit",
    hour: "2-digit", minute: "2-digit",
  }).format(date);
}

function imageURL(generation, position) {
  return api(`/api/generations/${encodeURIComponent(generation.id)}/images/${position}`);
}

window.Darkroom = { api, ready, refreshMe, setQuota, formatDate, imageURL };

renderSidebar();

})();
