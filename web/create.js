// 创作页：生图表单 + 批量生图。
// 外壳（左侧菜单栏、顶栏用户菜单、登录态）在 shell.js 里，这里只管 main 里的东西。
// 生成的图由服务端存进作品集，这里不再往 localStorage 塞历史。

(() => {

const api = window.Darkroom.api;
const prefsKey = "darkroom.prefs";

const MODELS = {
  gpt: [
    ["gpt-image-2.5-flare", "gpt-image-2.5-flare"],
    ["gpt-image-2.5", "gpt-image-2.5"],
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

// 管理端「拉取模型」问来的、各调用方式下中转站实际支持的模型。
// 拉不到就退回上面那份内置的，页面照样能用。
let relayModels = null;

const CHANNEL_LABEL = { async: "异步", sync: "同步", chat: "对话生图", gemini: "官方格式" };

// 画幅 × 分辨率 算出具体像素。上游只认 "宽x高"，不认 "2K" 这种档位写法
// （实测发 "2K" 会被打回「图片尺寸无效」），所以档位只是界面上的说法。
// 长边定档，短边按比例算；这几个比例在这三档上都能整除。
const TIER_LONG_SIDE = { "1K": 1024, "2K": 2048, "4K": 4096 };
const GPT_RATIOS = { "1:1": [1, 1], "4:3": [4, 3], "3:4": [3, 4], "16:9": [16, 9] };

const form = document.querySelector("#form");
const protocol = document.querySelector("#protocol");
const protocolHint = document.querySelector("#protocolHint");
const model = document.querySelector("#model");
const customModel = document.querySelector("#customModel");
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
  // 批量列表等真的切过去再拉，省得每次打开创作页都空跑一趟上游。
  if (name === "batch" && !batchLoaded) {
    batchLoaded = true;
    loadBatches({ quiet: true });
  }
}

for (const tab of tabs) {
  tab.addEventListener("click", () => showTab(tab.dataset.tab));
}

function loadPrefs() {
  try {
    const saved = JSON.parse(localStorage.getItem(prefsKey) || "{}");
    if (saved.protocol) protocol.value = saved.protocol;
    if (saved.model) model.dataset.prefer = saved.model;
    if (saved.customModel) customModel.value = saved.customModel;
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
    protocol: protocol.value,
    model: model.value,
    customModel: customModel.value.trim(),
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

// 当前调用方式下该显示哪些模型。中转站拉到了就用它，没有就用内置的那份。
function modelSource() {
  const relayed = relayModels ? relayModels[protocol.value] : null;
  if (relayed && relayed.length) return { list: relayed, synced: true };
  return {
    list: (MODELS[protocol.value] || MODELS.gpt).map(([id, name]) => ({ id, name, available: true })),
    synced: false,
  };
}

function fillModels() {
  const { list, synced } = modelSource();
  const prefer = model.dataset.prefer || "";
  // 拉取过就按实际情况来：不可用的禁掉，选不了就不会白跑一次。
  const selectable = (item) => item.available || !synced;
  const fallback = list.find(selectable) || list[0];

  model.replaceChildren();
  for (const item of list) {
    const option = document.createElement("option");
    option.value = item.id;
    option.textContent = item.available ? item.name : `${item.name}（没有可用 Key）`;
    if (!selectable(item)) option.disabled = true;
    model.append(option);
  }
  const custom = document.createElement("option");
  custom.value = "custom";
  custom.textContent = "自定义";
  model.append(custom);

  const known = list.find((item) => item.id === prefer && selectable(item));
  model.value = known ? prefer : prefer === "custom" ? "custom" : fallback.id;
  if (model.value === "custom" && prefer && prefer !== "custom") customModel.value = customModel.value || prefer;
  delete model.dataset.prefer;

  protocolHint.textContent = modelHint(synced, list);
}

function modelHint(synced, list) {
  const base = HINTS[protocol.value] || "";
  if (!synced) return `${base} 模型列表是内置的，去管理端点「拉取模型」可以换成中转站实际支持的那些。`;
  const usable = list.filter((item) => item.available).length;
  if (!usable) return `${base} 中转站报了 ${list.length} 个模型，但没有一把 Key 现在可用，先去管理端看看 Key 的余额和状态。`;
  return `${base} 模型列表来自中转站，${usable} 个可用。`;
}

async function loadModels() {
  try {
    const response = await fetch(api("/api/models"), { credentials: "include" });
    const data = await response.json().catch(() => ({}));
    if (data.ok && data.models) relayModels = data.models;
  } catch {
    // 拉不到就用内置的，不打扰创作。
  }
}

function syncFields() {
  const gemini = protocol.value !== "gpt";
  const custom = gptResolution.value === "custom";
  customModel.classList.toggle("hidden", model.value !== "custom");
  customSize.classList.toggle("hidden", gemini || !custom);
  pixelSize.classList.toggle("hidden", gemini);
  geminiSize.classList.toggle("hidden", !gemini);
  editFields.classList.toggle("hidden", form.querySelector('input[name="mode"]:checked').value !== "edit");

  pixelHint.classList.toggle("hidden", gemini);
  pixelHint.textContent = gemini ? ""
    : custom ? "单边不超过 8192 像素，总像素不超过 64Mi。"
      : `发出 size = ${selectedSize()}。`;
  // 协议提示归 fillModels 管——它才知道模型是从中转站拉的还是内置的。
}

function selectedModel() {
  return model.value === "custom" ? customModel.value.trim() : model.value;
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
    const open = document.createElement("a");
    open.href = src;
    open.target = "_blank";
    open.rel = "noopener";
    open.textContent = images.length > 1 ? `打开 ${index + 1}` : "打开原图";
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
      model: chosenModel,
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
  // 换调用方式要重新填模型下拉，不然还停在上一种方式的模型上。
  if (event.target === protocol) fillModels();
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
    setBatchStatus("请填写模型名。", true);
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
batchRefresh.addEventListener("click", () => loadBatches());

loadPrefs();
// 先用内置列表渲染一次，页面立刻能用；中转站那份拉回来之后再刷一遍。
fillModels();
syncFields();
showTab("draw");
loadModels().then(() => {
  fillModels();
  syncFields();
});

})();
