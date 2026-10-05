// 首页：额度、签到、账号信息，外加最近几张作品。
// 外壳在 shell.js，这里只管 main 里的东西。

(() => {

const api = window.Darkroom.api;

const greeting = document.querySelector("#greeting");
const quota = document.querySelector("#quota");
const checkinBtn = document.querySelector("#checkinBtn");
const checkinHint = document.querySelector("#checkinHint");
const recent = document.querySelector("#recent");

function render(session) {
  const user = session.user;
  greeting.textContent = `欢迎回来，${user.displayName || user.username}`;
  quota.textContent = String(user.quota);

  checkinBtn.disabled = user.checkedInToday;
  checkinBtn.textContent = user.checkedInToday ? "今日已签到" : "签到领额度";
  checkinHint.textContent = user.checkedInToday
    ? `今天已经领过。每次生图消耗 ${session.generateCost} 额度，北京时间 0 点刷新。`
    : `每天可领 ${session.checkinQuota} 额度，北京时间 0 点刷新。每次生图消耗 ${session.generateCost}。`;

  document.querySelector("#accountName").textContent = user.username;
  document.querySelector("#accountDisplay").textContent = user.displayName || "—";
  document.querySelector("#accountEmail").textContent = user.email || "—";
  document.querySelector("#accountPhone").textContent = user.phone || "—";
}

checkinBtn.addEventListener("click", async () => {
  checkinBtn.disabled = true;
  try {
    const response = await fetch(api("/api/checkin"), { method: "POST", credentials: "include" });
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.ok) throw new Error(data.error || "签到失败");
    render(await window.Darkroom.refreshMe());
    checkinHint.textContent = `签到成功，领取 ${data.amount} 额度。`;
  } catch (error) {
    checkinBtn.disabled = false;
    checkinHint.textContent = error.message;
  }
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
