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

function actionButton(label, handler, extra = "") {
  const button = document.createElement("button");
  button.type = "button";
  button.className = `small ${extra}`.trim();
  button.textContent = label;
  button.addEventListener("click", handler);
  return button;
}

function workCard(item) {
  const card = document.createElement("article");
  card.className = "work-card";

  const frame = document.createElement("div");
  frame.className = "work-frame";
  if (item.images.length) {
    const full = api(`/api/generations/${encodeURIComponent(item.id)}/images/${item.images[0].position}`);
    const link = document.createElement("a");
    link.href = full;
    link.target = "_blank";
    link.rel = "noopener";
    const img = document.createElement("img");
    img.src = full;
    img.alt = item.prompt;
    img.loading = "lazy";
    link.append(img);
    frame.append(link);
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
  if (!isWorks) {
    actions.append(
      actionButton(item.isPublic ? "取消公开" : "设为公开", () => togglePublic(item)),
      actionButton("删除", () => removeWork(item), "danger"),
    );
  } else if (item.mine) {
    const link = document.createElement("a");
    link.className = "btn small";
    link.href = "./gallery";
    link.textContent = "去作品集管理";
    actions.append(link);
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
