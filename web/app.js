const PREFS_KEY = "darkroom.prefs";
const HISTORY_KEY = "darkroom.history";

const MODELS = {
  gpt: [
    ["gpt-image-2.5-flare", "gpt-image-2.5-flare"],
    ["gpt-image-1", "gpt-image-1"],
  ],
  nano: [
    ["gemini-2.5-flash-image", "Nano Banana · gemini-2.5-flash-image"],
    ["gemini-3-pro-image", "Nano Banana Pro · gemini-3-pro-image"],
    ["gemini-3.1-flash-image", "gemini-3.1-flash-image"],
  ],
  "gemini-official": [
    ["gemini-2.5-flash-image", "gemini-2.5-flash-image"],
    ["gemini-3-pro-image", "gemini-3-pro-image"],
    ["gemini-3.1-flash-image", "gemini-3.1-flash-image"],
  ],
};

const HINTS = {
  gpt: "使用管理端里的 GPT Key。接口是 /v1/images/generations。",
  nano: "使用 Nano Banana 的 Key。生图接口不收这个模型时，会改走对话生图。",
  "gemini-official": "使用「GEMINI官方直连-带生图」的 Key，走 Gemini 官方 generateContent。",
};

const CHANNEL_LABEL = { async: "异步", sync: "同步", chat: "对话生图", gemini: "官方格式" };

// 所有请求都从这里过，方便整体指向另一个后端。见 config.js。
const API_BASE = String(window.DARKROOM_API || "").replace(/\/+$/, "");
function api(path) {
  return `${API_BASE}${path}`;
}

const form = document.querySelector("#form");
const guestAuth = document.querySelector("#guestAuth");
const userAuth = document.querySelector("#userAuth");
const username = document.querySelector("#username");
const password = document.querySelector("#password");
const hello = document.querySelector("#hello");
const quota = document.querySelector("#quota");
const checkinBtn = document.querySelector("#checkinBtn");
const checkinHint = document.querySelector("#checkinHint");
const protocol = document.querySelector("#protocol");
const protocolHint = document.querySelector("#protocolHint");
const model = document.querySelector("#model");
const customModel = document.querySelector("#customModel");
const prompt = document.querySelector("#prompt");
const size = document.querySelector("#size");
const quality = document.querySelector("#quality");
const width = document.querySelector("#width");
const height = document.querySelector("#height");
const customSize = document.querySelector("#customSize");
const pixelSize = document.querySelector("#pixelSize");
const geminiSize = document.querySelector("#geminiSize");
const aspectRatio = document.querySelector("#aspectRatio");
const imageSize = document.querySelector("#imageSize");
const editFields = document.querySelector("#editFields");
const imageFile = document.querySelector("#imageFile");
const imageUrl = document.querySelector("#imageUrl");
const maskFile = document.querySelector("#maskFile");
const maskUrl = document.querySelector("#maskUrl");
const passwordBox = document.querySelector("#passwordBox");
const batchModel = document.querySelector("#batchModel");
const batchPrompts = document.querySelector("#batchPrompts");
const batchSize = document.querySelector("#batchSize");
const batchProvider = document.querySelector("#batchProvider");
const batchSubmit = document.querySelector("#batchSubmit");
const batchRefresh = document.querySelector("#batchRefresh");
const batchStatus = document.querySelector("#batchStatus");
const batchList = document.querySelector("#batchList");
const submit = document.querySelector("#submit");
const status = document.querySelector("#status");
const canvas = document.querySelector("#canvas");
const meta = document.querySelector("#meta");
const actions = document.querySelector("#actions");
const historyEl = document.querySelector("#history");

const sessionImages = new Map();
let history = [];
let currentId = "";
let tick = 0;
let me = null;
let checkinQuota = 5;
let generateCost = 1;
let batchBusy = false;

function loadPrefs() {
  try {
    const saved = JSON.parse(localStorage.getItem(PREFS_KEY) || "{}");
    if (saved.protocol) protocol.value = saved.protocol;
    if (saved.model) model.dataset.prefer = saved.model;
    if (saved.customModel) customModel.value = saved.customModel;
    if (saved.quality) quality.value = saved.quality;
    if (saved.size) size.value = saved.size;
    if (saved.width) width.value = saved.width;
    if (saved.height) height.value = saved.height;
    if (saved.aspectRatio) aspectRatio.value = saved.aspectRatio;
    if (saved.imageSize) imageSize.value = saved.imageSize;
    if (saved.mode) {
      const radio = form.querySelector(`input[name="mode"][value="${saved.mode}"]`);
      if (radio) radio.checked = true;
    }
  } catch {
    localStorage.removeItem(PREFS_KEY);
  }
  try {
    history = JSON.parse(localStorage.getItem(HISTORY_KEY) || "[]");
    if (!Array.isArray(history)) history = [];
  } catch {
    history = [];
  }
}

function savePrefs() {
  localStorage.setItem(PREFS_KEY, JSON.stringify({
    protocol: protocol.value,
    model: model.value,
    customModel: customModel.value.trim(),
    quality: quality.value,
    size: size.value,
    width: width.value,
    height: height.value,
    aspectRatio: aspectRatio.value,
    imageSize: imageSize.value,
    mode: form.querySelector('input[name="mode"]:checked').value,
  }));
}

function fillModels() {
  const list = MODELS[protocol.value] || MODELS.gpt;
  const prefer = model.dataset.prefer || "";
  model.replaceChildren();
  for (const [value, label] of list) {
    const option = document.createElement("option");
    option.value = value;
    option.textContent = label;
    model.append(option);
  }
  const custom = document.createElement("option");
  custom.value = "custom";
  custom.textContent = "自定义";
  model.append(custom);
  const known = list.some(([value]) => value === prefer);
  model.value = known ? prefer : prefer === "custom" ? "custom" : list[0][0];
  if (model.value === "custom" && prefer && prefer !== "custom") customModel.value = customModel.value || prefer;
  delete model.dataset.prefer;
}

function syncFields() {
  const gemini = protocol.value !== "gpt";
  customModel.classList.toggle("hidden", model.value !== "custom");
  customSize.classList.toggle("hidden", size.value !== "custom" || gemini);
  pixelSize.classList.toggle("hidden", gemini);
  geminiSize.classList.toggle("hidden", !gemini);
  editFields.classList.toggle("hidden", form.querySelector('input[name="mode"]:checked').value !== "edit");
  protocolHint.textContent = HINTS[protocol.value] || "";
}

function selectedModel() {
  return model.value === "custom" ? customModel.value.trim() : model.value;
}

function selectedSize() {
  return size.value === "custom" ? `${Number(width.value)}x${Number(height.value)}` : size.value;
}

function setStatus(message, isError) {
  status.textContent = message;
  status.classList.toggle("error", Boolean(isError));
}

function renderAuth() {
  const loggedIn = Boolean(me && me.user);
  guestAuth.classList.toggle("hidden", loggedIn);
  userAuth.classList.toggle("hidden", !loggedIn);
  if (!loggedIn) return;
  hello.textContent = me.user.username;
  quota.textContent = String(me.user.quota);
  checkinBtn.disabled = me.user.checkedInToday;
  checkinBtn.textContent = me.user.checkedInToday ? "今日已签到" : "签到领额度";
  checkinHint.textContent = me.user.checkedInToday
    ? `今天已经领过。每次生图消耗 ${generateCost} 额度。`
    : `每天可领 ${checkinQuota} 额度，北京时间 0 点刷新。每次生图消耗 ${generateCost}。`;
}

async function refreshMe() {
  const response = await fetch(api("/api/me"));
  const data = await response.json();
  me = data;
  checkinQuota = data.checkinQuota;
  generateCost = data.generateCost;
  renderAuth();
  loadBatches({ quiet: true });
}

async function auth(path) {
  const response = await fetch(api(path), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username: username.value.trim(), password: password.value }),
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok || !data.ok) throw new Error(data.error || "登录失败");
  password.value = "";
  await refreshMe();
  setStatus(path.endsWith("register") ? "注册成功。" : "已登录。");
}

function imageSrc(image) {
  if (!image) return "";
  if (image.url) return image.url;
  if (image.b64) return `data:${image.mime || "image/png"};base64,${image.b64}`;
  return "";
}

function showResult(entry) {
  currentId = entry.id;
  const images = entry.images || [];
  canvas.replaceChildren();
  if (!images.length) {
    const empty = document.createElement("p");
    empty.className = "empty";
    empty.textContent = "这次没有拿到图片。";
    canvas.append(empty);
  } else {
    images.forEach((image, index) => {
      const img = document.createElement("img");
      img.alt = entry.prompt;
      img.src = imageSrc(image);
      canvas.append(img);
      if (index === 0 && images.length === 1) img.alt = entry.prompt;
    });
  }
  meta.textContent = [entry.model, entry.sizeLabel, CHANNEL_LABEL[entry.channel] || entry.channel, entry.taskId].filter(Boolean).join(" · ");
  actions.classList.remove("hidden");
  actions.replaceChildren();
  images.forEach((image, index) => {
    const src = imageSrc(image);
    const open = document.createElement("a");
    open.href = src;
    open.target = "_blank";
    open.rel = "noopener";
    open.textContent = images.length > 1 ? `打开 ${index + 1}` : "打开原图";
    const download = document.createElement("a");
    download.href = src;
    download.download = `darkroom-${entry.id}${images.length > 1 ? `-${index + 1}` : ""}.png`;
    download.textContent = "下载";
    actions.append(open, download);
  });
  renderHistory();
}

function renderHistory() {
  historyEl.replaceChildren();
  if (!history.length) {
    const empty = document.createElement("p");
    empty.textContent = "还没有记录。";
    historyEl.append(empty);
    return;
  }
  history.forEach((entry) => {
    const button = document.createElement("button");
    button.type = "button";
    button.title = entry.prompt;
    button.setAttribute("aria-label", entry.prompt);
    if (entry.id === currentId) button.setAttribute("aria-current", "true");
    const src = imageSrc(entry.images[0]);
    if (src && (src.startsWith("https://") || src.startsWith("http://") || src.startsWith("data:image/"))) {
      button.style.backgroundImage = `url("${src.replace(/["\\\n\r]/g, "")}")`;
    }
    button.addEventListener("click", () => {
      prompt.value = entry.prompt;
      showResult(sessionImages.get(entry.id) || entry);
    });
    historyEl.append(button);
  });
}

function remember(entry) {
  const stored = { ...entry, images: entry.images.filter((image) => image.url).map((image) => ({ url: image.url })) };
  if (entry.images.some((image) => image.b64)) sessionImages.set(entry.id, entry);
  history = [stored, ...history.filter((item) => item.id !== entry.id)].slice(0, 16);
  localStorage.setItem(HISTORY_KEY, JSON.stringify(history));
}

function readFile(file) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(new Error("读取参考图失败"));
    reader.onload = () => {
      const match = /^data:(.*?);base64,(.*)$/.exec(String(reader.result || ""));
      if (!match) {
        reject(new Error("读取参考图失败"));
        return;
      }
      resolve({ mime: match[1], data: match[2], name: file.name });
    };
    reader.readAsDataURL(file);
  });
}

async function onSubmit(event) {
  event.preventDefault();
  if (submit.disabled) return;
  savePrefs();
  if (!me || !me.user) {
    setStatus("请先登录。注册后可以签到领取额度。", true);
    return;
  }
  const chosenModel = selectedModel();
  if (!chosenModel) {
    setStatus("请填写模型名。", true);
    return;
  }
  const text = prompt.value.trim();
  if (!text) {
    setStatus("请填写画面描述。", true);
    return;
  }
  const mode = form.querySelector('input[name="mode"]:checked').value;
  const payload = { protocol: protocol.value, model: chosenModel, prompt: text, mode };
  let sizeLabel = "";
  if (protocol.value === "gpt") {
    payload.size = selectedSize();
    payload.quality = quality.value;
    sizeLabel = `${payload.size} · ${payload.quality}`;
  } else {
    payload.aspectRatio = aspectRatio.value;
    payload.imageSize = imageSize.value;
    sizeLabel = `${payload.aspectRatio} · ${payload.imageSize}`;
  }

  submit.disabled = true;
  const started = Date.now();
  window.clearInterval(tick);
  tick = window.setInterval(() => {
    setStatus(`正在生成… ${Math.floor((Date.now() - started) / 1000)} 秒`);
  }, 400);
  try {
    if (mode === "edit") {
      if (imageFile.files[0]) payload.image = await readFile(imageFile.files[0]);
      else if (imageUrl.value.trim()) payload.imageUrl = imageUrl.value.trim();
      else throw new Error("图生图需要参考图或图片 URL。");
      if (maskFile.files[0]) payload.mask = await readFile(maskFile.files[0]);
      else if (maskUrl.value.trim()) payload.maskUrl = maskUrl.value.trim();
    }
    const response = await fetch(api("/api/generate"), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    const data = await response.json().catch(() => ({}));
    if (typeof data.quota === "number" && me.user) {
      me.user.quota = data.quota;
      renderAuth();
    }
    if (!response.ok || !data.ok) throw new Error(data.error || `请求失败（${response.status}）`);
    const entry = {
      id: String(Date.now()),
      prompt: text,
      model: chosenModel,
      sizeLabel,
      channel: data.channel,
      taskId: data.taskId || "",
      images: data.images || [],
    };
    remember(entry);
    showResult(sessionImages.get(entry.id) || entry);
    const onlyInline = entry.images.length > 0 && entry.images.every((image) => image.b64 && !image.url);
    if (onlyInline) setStatus("完成。结果是内联图片，刷新页面后不会留在历史里。");
    else if (data.channel === "chat") setStatus("完成。这次走的是对话生图。");
    else if (data.channel === "gemini") setStatus("完成。这次走的是 Gemini 官方格式。");
    else if (data.channel === "sync") setStatus("完成。这次走的是同步接口。");
    else setStatus("完成。");
  } catch (error) {
    setStatus(error.message || "生成失败", true);
  } finally {
    window.clearInterval(tick);
    submit.disabled = false;
  }
}

document.querySelector("#loginBtn").addEventListener("click", () => auth("/api/auth/login").catch((error) => setStatus(error.message, true)));
document.querySelector("#registerBtn").addEventListener("click", () => auth("/api/auth/register").catch((error) => setStatus(error.message, true)));
password.addEventListener("keydown", (event) => {
  if (event.key === "Enter") {
    event.preventDefault();
    document.querySelector("#loginBtn").click();
  }
});
document.querySelector("#logoutBtn").addEventListener("click", async () => {
  await fetch(api("/api/auth/logout"), { method: "POST" });
  me = { user: null, checkinQuota, generateCost };
  renderAuth();
  loadBatches({ quiet: true });
  setStatus("已退出。");
});
document.querySelector("#passwordToggle").addEventListener("click", () => {
  passwordBox.classList.toggle("hidden");
});
document.querySelector("#changePasswordBtn").addEventListener("click", async () => {
  try {
    const response = await fetch(api("/api/me/password"), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        oldPassword: document.querySelector("#oldPassword").value,
        newPassword: document.querySelector("#newPassword").value,
      }),
    });
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.ok) throw new Error(data.error || "改密码失败");
    document.querySelector("#oldPassword").value = "";
    document.querySelector("#newPassword").value = "";
    passwordBox.classList.add("hidden");
    setStatus("密码已修改。");
  } catch (error) {
    setStatus(error.message, true);
  }
});
checkinBtn.addEventListener("click", async () => {
  checkinBtn.disabled = true;
  try {
    const response = await fetch(api("/api/checkin"), { method: "POST" });
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.ok) throw new Error(data.error || "签到失败");
    me.user = data.user;
    renderAuth();
    setStatus(`签到成功，领取 ${data.amount} 额度。`);
  } catch (error) {
    checkinBtn.disabled = false;
    setStatus(error.message, true);
  }
});

form.addEventListener("change", () => {
  syncFields();
  savePrefs();
});
form.addEventListener("submit", onSubmit);
prompt.addEventListener("keydown", (event) => {
  if ((event.ctrlKey || event.metaKey) && event.key === "Enter") form.requestSubmit();
});

function setBatchStatus(message, isError) {
  batchStatus.textContent = message || "";
  batchStatus.classList.toggle("error", Boolean(isError));
}

function batchButton(label, handler) {
  const button = document.createElement("button");
  button.type = "button";
  button.textContent = label;
  button.addEventListener("click", handler);
  return button;
}

// The relay's list shape is not fixed, so accept the wrappers it is likely to use.
function batchEntries(result) {
  if (Array.isArray(result)) return result;
  for (const key of ["data", "batches", "items"]) {
    if (result && Array.isArray(result[key])) return result[key];
  }
  return [];
}

function batchSummary(batch) {
  const status = String(batch?.status || "");
  const counts = batch?.item_count ?? batch?.request_counts?.total ?? batch?.outputs;
  return [status, counts === undefined ? "" : `${counts} 条`].filter(Boolean).join(" · ") || "未知状态";
}

function renderBatches(result) {
  batchList.replaceChildren();
  const list = batchEntries(result);
  if (!list.length) {
    const empty = document.createElement("p");
    empty.className = "hint";
    empty.textContent = "还没有批量任务。";
    batchList.append(empty);
    return;
  }
  list.slice(0, 12).forEach((batch) => {
    const id = String(batch?.id || batch?.batch_id || "");
    const row = document.createElement("div");
    row.className = "batch-row";
    const label = document.createElement("div");
    const title = document.createElement("strong");
    title.textContent = id || "（上游没给编号）";
    const meta = document.createElement("small");
    meta.textContent = batchSummary(batch);
    label.append(title, meta);
    const actions = document.createElement("div");
    actions.className = "row-actions";
    if (id) {
      actions.append(
        batchButton("刷新", () => refreshBatch(id)),
        batchButton("下载", () => {
          window.location.href = api(`/api/batches/${encodeURIComponent(id)}/download`);
        }),
        batchButton("取消", () => cancelBatch(id)),
        batchButton("删除", () => deleteBatch(id)),
      );
    }
    row.append(label, actions);
    batchList.append(row);
  });
}

async function loadBatches({ quiet = false } = {}) {
  if (!me || !me.user) {
    renderBatches([]);
    if (!quiet) setBatchStatus("请先登录。", true);
    return;
  }
  try {
    const response = await fetch(api("/api/batches"));
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.ok) throw new Error(data.error || `查不到批量任务（${response.status}）`);
    renderBatches(data.result);
    if (!quiet) setBatchStatus("列表已刷新。");
  } catch (error) {
    renderBatches([]);
    if (!quiet) setBatchStatus(error.message, true);
  }
}

async function batchAction(id, path, options, done) {
  try {
    const response = await fetch(api(`/api/batches/${encodeURIComponent(id)}${path}`), options);
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.ok) throw new Error(data.error || `请求失败（${response.status}）`);
    setBatchStatus(done(data));
    await loadBatches({ quiet: true });
  } catch (error) {
    setBatchStatus(error.message, true);
  }
}

function refreshBatch(id) {
  return batchAction(id, "", {}, (data) => `${id}：${batchSummary(data.result)}`);
}

function cancelBatch(id) {
  return batchAction(id, "/cancel", { method: "POST" }, (data) => `${id}：${batchSummary(data.result)}`);
}

function deleteBatch(id) {
  if (!window.confirm(`删除批量任务 ${id}？`)) return undefined;
  return batchAction(id, "", { method: "DELETE" }, () => `已删除 ${id}。`);
}

async function submitBatch() {
  if (batchBusy) return;
  if (!me || !me.user) {
    setBatchStatus("请先登录。", true);
    return;
  }
  const prompts = batchPrompts.value.split("\n").map((line) => line.trim()).filter(Boolean);
  if (!prompts.length) {
    setBatchStatus("至少写一条提示词。", true);
    return;
  }
  const model = batchModel.value.trim();
  if (!model) {
    setBatchStatus("请填写模型名。", true);
    return;
  }
  batchBusy = true;
  batchSubmit.disabled = true;
  setBatchStatus(`正在提交 ${prompts.length} 条…`);
  try {
    const response = await fetch(api("/api/batches"), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        model,
        provider: batchProvider.value,
        image_size: batchSize.value,
        response_mime_type: "image/png",
        items: prompts.map((prompt, index) => ({ custom_id: `item_${index + 1}`, prompt, output_count: 1 })),
      }),
    });
    const data = await response.json().catch(() => ({}));
    if (typeof data.quota === "number" && me.user) {
      me.user.quota = data.quota;
      renderAuth();
    }
    if (!response.ok || !data.ok) throw new Error(data.error || `提交失败（${response.status}）`);
    batchPrompts.value = "";
    setBatchStatus(`已提交 ${data.outputs} 条，扣了 ${data.cost} 额度。`);
    await loadBatches({ quiet: true });
  } catch (error) {
    setBatchStatus(error.message, true);
  } finally {
    batchBusy = false;
    batchSubmit.disabled = false;
  }
}

batchSubmit.addEventListener("click", submitBatch);
batchRefresh.addEventListener("click", () => loadBatches());

loadPrefs();
fillModels();
syncFields();
renderHistory();
refreshMe().catch(() => setStatus("暂时连不上本机服务。", true));
