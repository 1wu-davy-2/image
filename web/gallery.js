// 作品集（自己的）和公开作品两页共用。看 body 上的 data-nav 决定拉哪个接口：
//   gallery → /api/generations（自己的，能改公开、能删）
//   works   → /api/works（所有人的公开作品，只读）

(() => {

const api = window.Darkroom.api;
const isWorks = document.body.dataset.nav === "works";

const grid = document.querySelector("#grid");
const status = document.querySelector("#status");
const moreBtn = document.querySelector("#moreBtn");

const PAGE = 24;
let offset = 0;
let total = 0;
let loading = false;

function setStatus(message, isError) {
  status.textContent = message || "";
  status.classList.toggle("error", Boolean(isError));
}

/* ---------- 放大查看 ---------- */

// 点图放大，而不是跳新标签页：翻作品的时候一跳走，回来得重新找看到哪儿了。
// 点图、点背景、按 Esc 都关掉。
let lightbox = null;

function closeLightbox() {
  if (!lightbox) return;
  lightbox.remove();
  lightbox = null;
  document.removeEventListener("keydown", onLightboxKey);
}

function onLightboxKey(event) {
  if (event.key === "Escape") closeLightbox();
}

function openLightbox(src, caption) {
  closeLightbox();
  lightbox = document.createElement("div");
  lightbox.className = "lightbox";
  lightbox.setAttribute("role", "dialog");
  lightbox.setAttribute("aria-modal", "true");
  lightbox.setAttribute("aria-label", caption || "放大查看");

  const img = document.createElement("img");
  img.src = src;
  img.alt = caption || "";
  lightbox.append(img);

  // 图下面的说明。空着就不占地方。
  if (caption) {
    const note = document.createElement("p");
    note.className = "lightbox-caption";
    note.textContent = caption;
    lightbox.append(note);
  }

  // 点图本身不该关——用户多半是想凑近看，不是想退出。
  img.addEventListener("click", (event) => event.stopPropagation());
  lightbox.addEventListener("click", closeLightbox);
  document.addEventListener("keydown", onLightboxKey);
  document.body.append(lightbox);
}

function actionButton(label, handler, extra = "") {
  const button = document.createElement("button");
  button.type = "button";
  button.className = `small ${extra}`.trim();
  button.textContent = label;
  button.addEventListener("click", handler);
  return button;
}

// navigator.clipboard 只在安全上下文里有。用 127.0.0.1 / localhost 打开算安全上下文，
// 换成局域网 IP（192.168.x.x）就不是了——那种时候退回老办法，别让按钮点了没反应。
async function copyText(text) {
  if (window.isSecureContext && navigator.clipboard) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 落到下面的兜底
    }
  }
  const scratch = document.createElement("textarea");
  scratch.value = text;
  scratch.setAttribute("readonly", "");
  scratch.style.cssText = "position:fixed;top:-1000px;left:0;opacity:0";
  document.body.append(scratch);
  scratch.select();
  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch {
    ok = false;
  }
  scratch.remove();
  return ok;
}

// 复制完把按钮本身改成「已复制」：反馈就在刚点的地方，不用去页面底下找那行状态。
function copyPromptButton(prompt) {
  const button = actionButton("复制提示词", async () => {
    if (!(await copyText(prompt))) {
      setStatus("复制失败——浏览器不让写剪贴板，手动选中提示词复制吧。", true);
      return;
    }
    setStatus("");
    button.textContent = "已复制";
    button.disabled = true;
    window.setTimeout(() => {
      button.textContent = "复制提示词";
      button.disabled = false;
    }, 1500);
  });
  return button;
}

function workCard(item) {
  const card = document.createElement("article");
  card.className = "work-card";

  const frame = document.createElement("div");
  frame.className = "work-frame";
  if (item.images.length) {
    const full = api(`/api/generations/${encodeURIComponent(item.id)}/images/${item.images[0].position}`);
    const img = document.createElement("img");
    img.src = full;
    img.alt = item.prompt;
    img.loading = "lazy";
    if (isWorks) {
      // 公开作品是「逛」：点开放大，别把人从这一页带走，回来还得重新找看到哪儿了。
      const button = document.createElement("button");
      button.type = "button";
      button.className = "work-zoom";
      button.title = "放大查看";
      button.setAttribute("aria-label", "放大查看");
      button.append(img);
      button.addEventListener("click", () => openLightbox(full, item.prompt));
      frame.append(button);
    } else {
      // 作品集是「管」：多半想把原图拿到手，还是跳新标签页。
      const link = document.createElement("a");
      link.href = full;
      link.target = "_blank";
      link.rel = "noopener";
      link.append(img);
      frame.append(link);
    }
  }
  if (item.isPublic) {
    const badge = document.createElement("span");
    badge.className = "work-badge";
    badge.textContent = "已公开";
    frame.append(badge);
  }
  card.append(frame);

  const body = document.createElement("div");
  body.className = "work-body";

  const prompt = document.createElement("p");
  prompt.className = "work-prompt";
  prompt.textContent = item.prompt;
  prompt.title = item.prompt;
  body.append(prompt);

  const meta = document.createElement("p");
  meta.className = "work-meta";
  const parts = [item.model, item.sizeLabel, window.Darkroom.formatDate(item.createdAt)];
  // 公开作品页要标出是谁画的。
  if (isWorks) parts.unshift(item.displayName || item.username);
  meta.textContent = parts.filter(Boolean).join(" · ");
  body.append(meta);

  const actions = document.createElement("div");
  actions.className = "work-actions";
  if (isWorks) {
    // 这页上多半是别人的图。看到喜欢的想照着自己的意思再画一张，
    // 得先能把提示词抄走，所以复制按钮对谁都给。
    actions.append(copyPromptButton(item.prompt));
    if (item.mine) {
      const link = document.createElement("a");
      link.className = "btn small";
      link.href = "./gallery";
      link.textContent = "去作品集管理";
      actions.append(link);
    }
  } else {
    actions.append(
      actionButton(item.isPublic ? "取消公开" : "设为公开", () => togglePublic(item)),
      actionButton("删除", () => removeWork(item), "danger"),
    );
  }
  if (actions.childElementCount) body.append(actions);

  card.append(body);
  return card;
}

async function togglePublic(item) {
  const next = !item.isPublic;
  setStatus("");
  try {
    const response = await fetch(api(`/api/generations/${encodeURIComponent(item.id)}`), {
      credentials: "include",
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ isPublic: next }),
    });
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.ok) throw new Error(data.error || "改不动");
    setStatus(next ? "已公开，别人在「公开作品」里能看到。" : "已取消公开。");
    await reload();
  } catch (error) {
    setStatus(error.message, true);
  }
}

async function removeWork(item) {
  if (!window.confirm("删除这张作品？删了就找不回来了。")) return;
  setStatus("");
  try {
    const response = await fetch(api(`/api/generations/${encodeURIComponent(item.id)}`), {
      credentials: "include",
      method: "DELETE",
    });
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.ok) throw new Error(data.error || "删不掉");
    setStatus("已删除。");
    await reload();
  } catch (error) {
    setStatus(error.message, true);
  }
}

function showEmpty() {
  const box = document.createElement("div");
  box.className = "empty-state";
  if (isWorks) {
    box.textContent = "还没有人公开作品。";
  } else {
    box.append(document.createTextNode("作品集还是空的。去"));
    const link = document.createElement("a");
    link.href = "./create";
    link.textContent = "创作";
    box.append(link, document.createTextNode("页画一张，生成的图会自动收进来。"));
  }
  grid.append(box);
}

function renderMore() {
  const left = total - offset;
  moreBtn.classList.toggle("hidden", left <= 0);
  if (left > 0) moreBtn.textContent = `再看 ${Math.min(left, PAGE)} 张（还有 ${left} 张）`;
}

async function load({ reset = false } = {}) {
  if (loading) return;
  loading = true;
  if (reset) {
    offset = 0;
    grid.replaceChildren();
  }
  try {
    const path = isWorks ? "/api/works" : "/api/generations";
    const response = await fetch(api(`${path}?limit=${PAGE}&offset=${offset}`), { credentials: "include" });
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.ok) throw new Error(data.error || "拉不到作品");
    total = data.total;
    for (const item of data.items) grid.append(workCard(item));
    offset += data.items.length;
    if (!grid.childElementCount) showEmpty();
    renderMore();
  } catch (error) {
    setStatus(error.message, true);
  } finally {
    loading = false;
  }
}

function reload() {
  return load({ reset: true });
}

moreBtn.addEventListener("click", () => load());

window.Darkroom.ready.then((session) => {
  if (!session) return;
  load({ reset: true });
});

})();
