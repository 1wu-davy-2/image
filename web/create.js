// 创作页：生图表单 + 批量生图。
// 外壳（左侧菜单栏、顶栏用户菜单、登录态）在 shell.js 里，这里只管 main 里的东西。
// 生成的图由服务端存进作品集，这里不再往 localStorage 塞历史。

(() => {

const api = window.Darkroom.api;
const prefsKey = "darkroom.prefs";

// 生图类型：用户只挑这个，不挑模型——模型走管理端给每个类型配的默认模型。
// 类型下面的 Key 用什么调用方式发请求，由服务端挑中的那把 Key 决定。
//
// 拉不到类型列表时留个空壳，页面照样能开，只是选不了。
let generationTypes = [];
let typeSynced = false;

const CHANNEL_LABEL = { async: "异步", sync: "同步", chat: "对话生图", gemini: "官方格式" };

// 画幅 × 分辨率 算出具体像素。上游只认 "宽x高"，不认 "2K" 这种档位写法
// （实测发 "2K" 会被打回「图片尺寸无效」），所以档位只是界面上的说法。
// 长边定档，短边按比例算；这几个比例在这三档上都能整除。
const TIER_LONG_SIDE = { "1K": 1024, "2K": 2048, "4K": 4096 };
const GPT_RATIOS = { "1:1": [1, 1], "4:3": [4, 3], "3:4": [3, 4], "16:9": [16, 9] };

const form = document.querySelector("#form");
const modelType = document.querySelector("#modelType");
const model = document.querySelector("#model");
const typeHint = document.querySelector("#typeHint");
const prompt = document.querySelector("#prompt");
const gptRatio = document.querySelector("#gptRatio");
const gptResolution = document.querySelector("#gptResolution");
const quality = document.querySelector("#quality");
const pixelHint = document.querySelector("#pixelHint");
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

let tick = 0;
let batchBusy = false;
let batchLoaded = false;

/* ---------- 画面 / 批量生图 两个标签 ---------- */

const tabs = [...document.querySelectorAll(".tab")];

function showTab(name) {
  for (const tab of tabs) {
    tab.setAttribute("aria-selected", String(tab.dataset.tab === name));
  }
  for (const panel of document.querySelectorAll("[data-panel]")) {
    panel.classList.toggle("hidden", panel.dataset.panel !== name);
  }
  // 批量列表和模型列表都等真的切过去再拉，省得每次打开创作页都空跑一趟上游。
  if (name === "batch" && !batchLoaded) {
    batchLoaded = true;
    loadBatchModels();
    loadBatches({ quiet: true });
  }
}

for (const tab of tabs) {
  tab.addEventListener("click", () => showTab(tab.dataset.tab));
}

function loadPrefs() {
  try {
    const saved = JSON.parse(localStorage.getItem(prefsKey) || "{}");
    // 类型和模型都要等 /api/types 回来才知道有哪些，先记着，fillTypes 里再挑。
    if (saved.type) modelType.dataset.prefer = saved.type;
    if (saved.model) model.dataset.prefer = saved.model;
    if (saved.quality) quality.value = saved.quality;
    if (saved.gptRatio) gptRatio.value = saved.gptRatio;
    if (saved.gptResolution) gptResolution.value = saved.gptResolution;
    if (saved.width) width.value = saved.width;
    if (saved.height) height.value = saved.height;
    if (saved.aspectRatio) aspectRatio.value = saved.aspectRatio;
    if (saved.imageSize) imageSize.value = saved.imageSize;
    if (saved.mode) {
      const radio = form.querySelector(`input[name="mode"][value="${saved.mode}"]`);
      if (radio) radio.checked = true;
    }
  } catch {
    localStorage.removeItem(prefsKey);
  }
}

function savePrefs() {
  localStorage.setItem(prefsKey, JSON.stringify({
    type: modelType.value,
    model: model.value,
    quality: quality.value,
    gptRatio: gptRatio.value,
    gptResolution: gptResolution.value,
    width: width.value,
    height: height.value,
    aspectRatio: aspectRatio.value,
    imageSize: imageSize.value,
    mode: form.querySelector('input[name="mode"]:checked').value,
  }));
}

function currentType() {
  return generationTypes.find((item) => item.type === modelType.value) || null;
}

// 下拉里带上倍率：同一个类型下面各模型的价不一样，挑之前就得看得见。
// 补进来的默认模型没有倍率数据（Key 还没拉到它），那就只写名字——
// 不写等于「不知道」，写 1x 等于「确定不加价」，两回事。
function modelLabel(entry, item) {
  const name = entry.id === item.defaultModel ? `${entry.name}（默认）` : entry.name;
  return entry.multiplier > 0 ? `${name} · ${entry.multiplier}x` : name;
}

// 模型列表是这个类型下面各把 Key 拉到的并集。默认选中管理端配的那个，
// 用户不动它就是「走系统默认」；想换再自己挑。
function fillModels() {
  const item = currentType();
  const prefer = model.dataset.prefer || "";
  model.replaceChildren();
  if (!item) {
    const option = document.createElement("option");
    option.value = "";
    option.textContent = "先选生图类型";
    option.disabled = true;
    model.append(option);
    return;
  }
  const list = (item.models || []).slice();
  // 默认模型可能不在列表里（Key 还没拉到，或者模型列表是旧的），补进去，
  // 不然默认值选不中，用户看到的是另一个模型。
  if (item.defaultModel && !list.some((entry) => entry.id === item.defaultModel)) {
    list.unshift({ id: item.defaultModel, name: item.defaultModel });
  }
  for (const entry of list) {
    const option = document.createElement("option");
    option.value = entry.id;
    option.textContent = modelLabel(entry, item);
    model.append(option);
  }
  const known = list.find((entry) => entry.id === prefer);
  model.value = known ? prefer : item.defaultModel || (list[0] && list[0].id) || "";
  delete model.dataset.prefer;
  typeHint.textContent = typeLine();
}

function fillTypes() {
  // 当前选中的优先，其次才是上次存下来的偏好。不认当前值的话，用户刚换完类型、
  // change 处理器再调一次这里，就会把他换的那个又顶回第一个类型。
  const prefer = modelType.value || modelType.dataset.prefer || "";
  modelType.replaceChildren();
  for (const item of generationTypes) {
    const option = document.createElement("option");
    option.value = item.type;
    option.textContent = item.usable ? item.label : `${item.label}（没有可用 Key）`;
    if (!item.usable) option.disabled = true;
    modelType.append(option);
  }
  if (!generationTypes.length) {
    const option = document.createElement("option");
    option.value = "";
    option.textContent = "管理端还没挂 Key";
    option.disabled = true;
    modelType.append(option);
  }
  // 上次选的那个要是还在（而且可用）就接着用它。不可用说明它下面的 Key 出了问题，
  // 换个能用的，别让用户停在上面生不了图。
  const known = generationTypes.find((item) => item.type === prefer);
  const fallback = generationTypes.find((item) => item.usable) || generationTypes[0];
  if (known && known.usable) modelType.value = known.type;
  else if (fallback) modelType.value = fallback.type;
  delete modelType.dataset.prefer;

  // 提示归 fillModels 管：它才知道最后选中的是哪个模型，而提示里要写这个模型扣多少。
  fillModels();
}

// 服务端算的是 每次消耗 × 类型倍率 × 模型倍率（向上取整），这里跟着算一遍给用户看。
// 改一边记得改另一边。
function costLine(item) {
  const chosen = model.value || item.defaultModel;
  const entry = (item.models || []).find((m) => m.id === chosen);
  // /api/types 已经把每个模型的倍率和算好的 cost 带回来了，优先用它。
  if (entry && typeof entry.cost === "number") return entry.cost;
  const rate = entry && entry.multiplier > 0 ? entry.multiplier : 1;
  const cost = Math.ceil((window.Darkroom.cost || 1) * item.multiplier * rate);
  return cost < 1 ? 1 : cost;
}

// 用户不挑模型也能用，所以这里要把他实际会用到的模型和这次扣多少写出来——
// 出问题描述得清楚。
function typeLine() {
  const item = currentType();
  if (!item) return "管理端还没配置生图类型，先去「API 管理」挂一把 Key。";

  const chosen = model.value || item.defaultModel;
  const entry = (item.models || []).find((m) => m.id === chosen);
  const rate = entry && entry.multiplier > 0 ? entry.multiplier : 1;
  const factor = item.multiplier * rate;
  const cost = costLine(item);
  const money = factor === 1 ? `每次消耗 ${cost} 额度` : `每次消耗 ${cost} 额度（${factor} 倍）`;

  if (!item.usable) return `${money}。这个类型下面没有可用的 Key，先去管理端看看余额和状态。`;
  if (!item.defaultModel) return `${money}。管理端还没给这个类型配默认模型，自己选一个。`;
  if (chosen === item.defaultModel) {
    if (!item.modelKnown) return `${money}。模型 ${chosen}——这个类型的 Key 还没拉到这个模型，生图可能会失败。`;
    return `${money}。模型 ${chosen}。`;
  }
  return `${money}。模型 ${chosen}（不是默认的 ${item.defaultModel}）。`;
}

async function loadTypes() {
  try {
    const response = await fetch(api("/api/types"), { credentials: "include" });
    const data = await response.json().catch(() => ({}));
    if (data.ok && data.types) {
      generationTypes = data.types;
      typeSynced = true;
    }
  } catch {
    // 拉不到就空着，页面照开，只是选不了。
  }
}

// 尺寸控件按类型分两套：GEMINI 走官方格式，收的是画幅 + 档位；
// GPT / GROK 走 images 接口，收的是具体像素 + 质量。
//
// 类型不直接决定调用方式（那是 Key 的事），所以这里只是「这个类型通常是哪套」。
// 表单两套字段都会发出去，服务端按挑中的 Key 取用得上的那套——万一管理端
// 给这个类型挂了别的调用方式的 Key，也不会因为少发了字段而失败。
function isGeminiType() {
  return modelType.value === "gemini";
}

function syncFields() {
  const gemini = isGeminiType();
  const custom = gptResolution.value === "custom";
  customSize.classList.toggle("hidden", gemini || !custom);
  pixelSize.classList.toggle("hidden", gemini);
  geminiSize.classList.toggle("hidden", !gemini);
  editFields.classList.toggle("hidden", form.querySelector('input[name="mode"]:checked').value !== "edit");

  pixelHint.classList.toggle("hidden", gemini);
  pixelHint.textContent = gemini ? ""
    : custom ? "单边不超过 8192 像素，总像素不超过 64Mi。"
      : `发出 size = ${selectedSize()}。`;
  // 类型那行提示归 fillTypes 管——它才知道模型是从管理端配来的。
}

function selectedSize() {
  if (gptResolution.value === "custom") return `${Number(width.value)}x${Number(height.value)}`;
  const [w, h] = GPT_RATIOS[gptRatio.value] || GPT_RATIOS["1:1"];
  const long = TIER_LONG_SIDE[gptResolution.value] || TIER_LONG_SIDE["2K"];
  return w >= h ? `${long}x${Math.round((long * h) / w)}` : `${Math.round((long * w) / h)}x${long}`;
}

function setStatus(message, isError) {
  status.textContent = message;
  status.classList.toggle("error", Boolean(isError));
}

function imageSrc(image) {
  if (!image) return "";
  if (image.url) return image.url;
  if (image.b64) return `data:${image.mime || "image/png"};base64,${image.b64}`;
  return "";
}

function showResult(entry) {
  const images = entry.images || [];
  canvas.replaceChildren();
  if (!images.length) {
    const empty = document.createElement("p");
    empty.className = "empty";
    empty.textContent = "这次没有拿到图片。";
    canvas.append(empty);
  } else {
    for (const image of images) {
      const img = document.createElement("img");
      img.alt = entry.prompt;
      img.src = imageSrc(image);
      canvas.append(img);
    }
  }
  meta.textContent = [entry.model, entry.sizeLabel, CHANNEL_LABEL[entry.channel] || entry.channel, entry.taskId]
    .filter(Boolean).join(" · ");
  actions.classList.remove("hidden");
  actions.replaceChildren();
  images.forEach((image, index) => {
    const src = imageSrc(image);
    // 点开看是页内放大，不跳新标签页。想存下来用旁边的「下载」。
    const open = document.createElement("button");
    open.type = "button";
    open.className = "small";
    open.textContent = images.length > 1 ? `放大 ${index + 1}` : "放大查看";
    open.addEventListener("click", () => window.Darkroom.openLightbox(src, entry.prompt));
    const download = document.createElement("a");
    download.href = src;
    download.download = `darkroom-${Date.now()}${images.length > 1 ? `-${index + 1}` : ""}.png`;
    download.textContent = "下载";
    actions.append(open, download);
  });
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

  const item = currentType();
  if (!item) {
    setStatus("管理端还没配置生图类型。", true);
    return;
  }
  const text = prompt.value.trim();
  if (!text) {
    setStatus("请填写画面描述。", true);
    return;
  }
  const mode = form.querySelector('input[name="mode"]:checked').value;
  // 两套尺寸字段都发：调用方式是服务端挑完 Key 才知道的，那边取用得上的那套。
  const payload = {
    type: item.type,
    // 模型可以不选——不选（或选的就是默认那个）就不发，让服务端用它配的默认值。
    model: model.value && model.value !== item.defaultModel ? model.value : "",
    prompt: text,
    mode,
    size: selectedSize(),
    quality: quality.value,
    aspectRatio: aspectRatio.value,
    imageSize: imageSize.value,
  };
  const gemini = isGeminiType();
  const sizeLabel = gemini
    ? `${payload.aspectRatio} · ${payload.imageSize}`
    : `${payload.size} · ${payload.quality}`;

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
      credentials: "include",
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    const data = await response.json().catch(() => ({}));
    if (typeof data.quota === "number") window.Darkroom.setQuota(data.quota);
    if (!response.ok || !data.ok) throw new Error(data.error || `请求失败（${response.status}）`);
    showResult({
      prompt: text,
      model: model.value || item.defaultModel,
      sizeLabel,
      channel: data.channel,
      taskId: data.taskId || "",
      images: data.images || [],
    });
    if (data.channel === "chat") setStatus("完成。这次走的是对话生图，已收进作品集。");
    else if (data.channel === "gemini") setStatus("完成。这次走的是 Gemini 官方格式，已收进作品集。");
    else if (data.channel === "sync") setStatus("完成。这次走的是同步接口，已收进作品集。");
    else setStatus("完成。已收进作品集。");
  } catch (error) {
    setStatus(error.message || "生成失败", true);
  } finally {
    window.clearInterval(tick);
    submit.disabled = false;
  }
}

form.addEventListener("change", (event) => {
  // 换类型要换模型下拉和一套尺寸控件，顺手把那行提示也刷新。
  if (event.target === modelType) {
    fillTypes();
  } else if (event.target === model) {
    typeHint.textContent = typeLine();
  }
  syncFields();
  savePrefs();
});
form.addEventListener("submit", onSubmit);
prompt.addEventListener("keydown", (event) => {
  if ((event.ctrlKey || event.metaKey) && event.key === "Enter") form.requestSubmit();
});

/* ---------- 批量生图 ---------- */

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

// 上游的列表形状不固定，把可能的外壳都认一遍。
function batchEntries(result) {
  if (Array.isArray(result)) return result;
  for (const key of ["data", "batches", "items"]) {
    if (result && Array.isArray(result[key])) return result[key];
  }
  return [];
}

function batchSummary(batch) {
  const state = String(batch?.status || "");
  const counts = batch?.item_count ?? batch?.request_counts?.total ?? batch?.outputs;
  return [state, counts === undefined ? "" : `${counts} 条`].filter(Boolean).join(" · ") || "未知状态";
}

// 批量模型的下拉只能选中转站认的模型，所以列表得问它要（/api/batches/models 透传过去）。
// 拉不到就禁用并把原因写在下拉里——留个空框让用户乱填，错的要等提交才知道。
async function loadBatchModels() {
  const prefer = batchModel.value;
  setBatchModelOptions([{ value: "", text: "正在读取模型…", disabled: true }], true);
  try {
    const response = await fetch(api("/api/batches/models"), { credentials: "include" });
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.ok) throw new Error(data.error || `读不到模型列表（${response.status}）`);
    const list = batchModelEntries(data.result);
    if (!list.length) throw new Error("中转站没返回可用的批量模型。");
    setBatchModelOptions(list.map((entry) => ({
      value: entry.id,
      text: entry.name === entry.id ? entry.id : `${entry.name}（${entry.id}）`,
    })));
    // 上次选的那个还在就接着用，不然退回第一个。
    if (list.some((entry) => entry.id === prefer)) batchModel.value = prefer;
  } catch (error) {
    setBatchModelOptions([{ value: "", text: error.message || "读不到模型列表", disabled: true }], true);
  }
}

function setBatchModelOptions(options, disabled = false) {
  batchModel.replaceChildren();
  for (const option of options) {
    const node = document.createElement("option");
    node.value = option.value;
    node.textContent = option.text;
    if (option.disabled) node.disabled = true;
    batchModel.append(node);
  }
  batchModel.disabled = disabled;
}

// 上游 /models 的形状不固定，跟 batchEntries 一样把可能的外壳都认一遍。
function batchModelEntries(result) {
  let list = Array.isArray(result) ? result : null;
  if (!list) {
    for (const key of ["data", "models", "items"]) {
      if (result && Array.isArray(result[key])) {
        list = result[key];
        break;
      }
    }
  }
  if (!list) return [];
  const out = [];
  const seen = new Set();
  for (const entry of list) {
    const text = typeof entry === "string" ? entry : "";
    const id = text || String(entry?.id || entry?.model || entry?.name || "");
    if (!id || seen.has(id)) continue;
    seen.add(id);
    const name = text || String(entry?.display_name || entry?.displayName || entry?.name || id);
    out.push({ id, name });
  }
  return out;
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
    const info = document.createElement("small");
    info.textContent = batchSummary(batch);
    label.append(title, info);
    const rowActions = document.createElement("div");
    rowActions.className = "row-actions";
    if (id) {
      rowActions.append(
        batchButton("刷新", () => refreshBatch(id)),
        batchButton("下载", () => {
          window.location.href = api(`/api/batches/${encodeURIComponent(id)}/download`);
        }),
        batchButton("取消", () => cancelBatch(id)),
        batchButton("删除", () => deleteBatch(id)),
      );
    }
    row.append(label, rowActions);
    batchList.append(row);
  });
}

async function loadBatches({ quiet = false } = {}) {
  try {
    const response = await fetch(api("/api/batches"), { credentials: "include" });
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
    const response = await fetch(api(`/api/batches/${encodeURIComponent(id)}${path}`), { credentials: "include", ...options });
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
  const prompts = batchPrompts.value.split("\n").map((line) => line.trim()).filter(Boolean);
  if (!prompts.length) {
    setBatchStatus("至少写一条提示词。", true);
    return;
  }
  const chosen = batchModel.value.trim();
  if (!chosen) {
    setBatchStatus("还没有可选的模型——下拉里写着原因，点「刷新列表」重试。", true);
    return;
  }
  batchBusy = true;
  batchSubmit.disabled = true;
  setBatchStatus(`正在提交 ${prompts.length} 条…`);
  try {
    const response = await fetch(api("/api/batches"), {
      credentials: "include",
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        model: chosen,
        provider: batchProvider.value,
        image_size: batchSize.value,
        response_mime_type: "image/png",
        items: prompts.map((text, index) => ({ custom_id: `item_${index + 1}`, prompt: text, output_count: 1 })),
      }),
    });
    const data = await response.json().catch(() => ({}));
    if (typeof data.quota === "number") window.Darkroom.setQuota(data.quota);
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
// 刷新也带上模型列表：刚在管理端挂完批量 Key 的人，多半是在这儿点。
batchRefresh.addEventListener("click", () => {
  loadBatchModels();
  loadBatches();
});

loadPrefs();
// 类型是管理端配的，得等 /api/types 回来才知道。先按空的重画一次占位，
// 拿到之后 fillTypes 会挑回上次用的那个类型。
fillTypes();
syncFields();
showTab("draw");
// 费用提示要用 generateCost，登录态回来才知道。
window.Darkroom.ready.then((session) => {
  if (session) window.Darkroom.cost = session.generateCost || 1;
  return loadTypes();
}).then(() => {
  fillTypes();
  syncFields();
});

})();
