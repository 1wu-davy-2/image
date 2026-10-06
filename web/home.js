// 首页：额度、签到，外加最近几张作品。
// 外壳在 shell.js，这里只管 main 里的东西。

(() => {

const api = window.Darkroom.api;

const greeting = document.querySelector("#greeting");
const quota = document.querySelector("#quota");
const checkinBtn = document.querySelector("#checkinBtn");
const checkinHint = document.querySelector("#checkinHint");
const recent = document.querySelector("#recent");

// 签到额度是个闭区间，min = max 时就是固定值，别说成「2-2 额度」。
function quotaRange(session) {
  return session.checkinMin === session.checkinMax
    ? `${session.checkinMin}`
    : `${session.checkinMin}-${session.checkinMax}`;
}

function render(session) {
  const user = session.user;
  greeting.textContent = `欢迎回来，${user.displayName || user.username}`;
  quota.textContent = String(user.quota);

  checkinBtn.disabled = user.checkedInToday;
  checkinBtn.textContent = user.checkedInToday ? "今日已签到" : "签到领额度";
  checkinHint.textContent = user.checkedInToday
    ? `今天已经领过。每次生图消耗 ${session.generateCost} 额度，北京时间 0 点刷新。`
    : `每天可领 ${quotaRange(session)} 额度，北京时间 0 点刷新。每次生图消耗 ${session.generateCost}。`;
}

checkinBtn.addEventListener("click", async () => {
  checkinBtn.disabled = true;
  try {
    const response = await fetch(api("/api/checkin"), { method: "POST", credentials: "include" });
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.ok) throw new Error(data.error || "签到失败");
    render(await window.Darkroom.refreshMe());
    checkinHint.textContent = `签到成功，领取 ${data.amount} 额度。`;
    // 记录已经变了，下次打开重新拉。
    logLoaded = false;
  } catch (error) {
    checkinBtn.disabled = false;
    checkinHint.textContent = error.message;
  }
});

/* ---------- 签到记录弹窗 ---------- */

const PAGE_SIZE = 10;

const dialog = document.querySelector("#checkinDialog");
const logList = document.querySelector("#checkinLog");
const logStatus = document.querySelector("#checkinLogStatus");
const pageLabel = document.querySelector("#checkinPage");
const prevBtn = document.querySelector("#checkinPrev");
const nextBtn = document.querySelector("#checkinNext");

let logPage = 0;
let logTotal = 0;
let logLoaded = false;

function renderLog(items) {
  logList.replaceChildren();
  if (!items.length) {
    const empty = document.createElement("p");
    empty.className = "hint";
    empty.textContent = "还没有签到记录。";
    logList.append(empty);
    return;
  }
  for (const item of items) {
    const row = document.createElement("div");
    row.className = "log-row";
    const day = document.createElement("span");
    day.className = "log-day";
    day.textContent = item.day;
    const amount = document.createElement("span");
    amount.className = "log-amount";
    amount.textContent = `+${item.amount} 额度`;
    row.append(day, amount);
    logList.append(row);
  }
}

function renderPager() {
  const pages = Math.max(1, Math.ceil(logTotal / PAGE_SIZE));
  pageLabel.textContent = logTotal ? `第 ${logPage + 1} / ${pages} 页 · 共 ${logTotal} 次` : "";
  prevBtn.disabled = logPage === 0;
  nextBtn.disabled = logPage + 1 >= pages;
}

async function loadLog() {
  logStatus.textContent = "正在读…";
  try {
    const response = await fetch(api(`/api/checkins?limit=${PAGE_SIZE}&offset=${logPage * PAGE_SIZE}`), {
      credentials: "include",
    });
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.ok) throw new Error(data.error || "读不到签到记录");
    logTotal = data.total;
    renderLog(data.items || []);
    renderPager();
    logStatus.textContent = "";
    logLoaded = true;
  } catch (error) {
    logStatus.textContent = error.message;
    logStatus.classList.add("error");
    return;
  }
  logStatus.classList.remove("error");
}

document.querySelector("#checkinLogBtn").addEventListener("click", () => {
  if (!logLoaded) loadLog();
  dialog.showModal();
});

// 点遮罩关掉。判断落在 <dialog> 的框外，而不是 event.target === dialog——
// 后者连点内边距也会关。
dialog.addEventListener("click", (event) => {
  const box = dialog.getBoundingClientRect();
  const outside = event.clientX < box.left || event.clientX > box.right ||
    event.clientY < box.top || event.clientY > box.bottom;
  if (outside) dialog.close();
});

document.querySelector("#checkinClose").addEventListener("click", () => dialog.close());

prevBtn.addEventListener("click", () => {
  if (logPage === 0) return;
  logPage -= 1;
  loadLog();
});
nextBtn.addEventListener("click", () => {
  logPage += 1;
  loadLog();
});

async function loadRecent() {
  recent.replaceChildren();
  let items = [];
  try {
    const response = await fetch(api("/api/generations?limit=4"), { credentials: "include" });
    const data = await response.json();
    if (data.ok) items = data.items;
  } catch {
    // 拉不到就当作没有，不打扰首页。
  }

  if (!items.length) {
    const empty = document.createElement("p");
    empty.className = "hint";
    empty.textContent = "还没有作品。去创作页画一张，生成的图会自动收进作品集。";
    recent.append(empty);
    return;
  }

  for (const item of items) {
    const card = document.createElement("a");
    card.className = "work-card work-card-link";
    card.href = "./gallery";
    const frame = document.createElement("div");
    frame.className = "work-frame";
    if (item.images.length) {
      const img = document.createElement("img");
      img.src = window.Darkroom.imageURL(item, item.images[0].position);
      img.alt = item.prompt;
      img.loading = "lazy";
      frame.append(img);
    }
    const body = document.createElement("div");
    body.className = "work-body";
    const text = document.createElement("p");
    text.className = "work-prompt";
    text.textContent = item.prompt;
    const meta = document.createElement("p");
    meta.className = "work-meta";
    meta.textContent = window.Darkroom.formatDate(item.createdAt);
    body.append(text, meta);
    card.append(frame, body);
    recent.append(card);
  }
}

window.Darkroom.ready.then((session) => {
  if (!session) return;
  render(session);
  loadRecent();
});

})();
