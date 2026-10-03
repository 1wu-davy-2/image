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
  const response = await fetch("/api/me");
  const data = await response.json();
  me = data;
  checkinQuota = data.checkinQuota;
  generateCost = data.generateCost;
  renderAuth();
}

async function auth(path) {
  const response = await fetch(path, {
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
    }
    const response = await fetch("/api/generate", {
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
  await fetch("/api/auth/logout", { method: "POST" });
  me = { user: null, checkinQuota, generateCost };
  renderAuth();
  setStatus("已退出。");
});
checkinBtn.addEventListener("click", async () => {
  checkinBtn.disabled = true;
  try {
    const response = await fetch("/api/checkin", { method: "POST" });
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

loadPrefs();
fillModels();
syncFields();
renderHistory();
refreshMe().catch(() => setStatus("暂时连不上本机服务。", true));
