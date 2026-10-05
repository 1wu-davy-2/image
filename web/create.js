// 创作页：生图表单 + 批量生图。
// 外壳（左侧菜单栏、顶栏用户菜单、登录态）在 shell.js 里，这里只管 main 里的东西。
// 生成的图由服务端存进作品集，这里不再往 localStorage 塞历史。

(() => {

const api = window.Darkroom.api;
const prefsKey = "darkroom.prefs";

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

function loadPrefs() {
  try {
    const saved = JSON.parse(localStorage.getItem(prefsKey) || "{}");
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
    localStorage.removeItem(prefsKey);
  }
}

function savePrefs() {
  localStorage.setItem(prefsKey, JSON.stringify({
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

form.addEventListener("change", () => {
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
fillModels();
syncFields();
window.Darkroom.ready.then(() => loadBatches({ quiet: true }));

})();
