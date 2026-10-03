const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");
const store = require("./store");

const PORT = Number(process.env.PORT) || 3780;
const PUBLIC_DIR = path.resolve(__dirname, "public");
const ALLOWED_HOSTS = new Set(["uuapi.io", "uuapi.net", "uuapi.shop", "uuapi.cc"]);
const BODY_LIMIT = 14 * 1024 * 1024;
const IMAGE_LIMIT = 8 * 1024 * 1024;
const POLL_DEADLINE_MS = 180000;
const UPSTREAM_TIMEOUT_MS = 120000;

const MIME = {
  ".html": "text/html; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".svg": "image/svg+xml",
  ".png": "image/png",
  ".ico": "image/x-icon",
};

class HttpError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

function sendJson(res, status, body, cookies) {
  const raw = Buffer.from(JSON.stringify(body));
  if (cookies) res.setHeader("Set-Cookie", cookies);
  res.writeHead(status, {
    "Content-Type": "application/json; charset=utf-8",
    "Content-Length": raw.length,
    "Cache-Control": "no-store",
    "X-Content-Type-Options": "nosniff",
  });
  res.end(raw);
}

function cookiesOf(req) {
  const out = {};
  for (const part of String(req.headers.cookie || "").split(";")) {
    const index = part.indexOf("=");
    if (index > 0) out[part.slice(0, index).trim()] = decodeURIComponent(part.slice(index + 1).trim());
  }
  return out;
}

function sessionCookie(name, token) {
  if (!token) return `${name}=; HttpOnly; SameSite=Lax; Path=/; Max-Age=0`;
  return `${name}=${token}; HttpOnly; SameSite=Lax; Path=/; Max-Age=1209600`;
}

async function readJson(req) {
  const raw = await readBody(req);
  try {
    return JSON.parse(raw.toString("utf8") || "{}");
  } catch {
    throw new HttpError(400, "请求不是 JSON");
  }
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on("data", (chunk) => {
      size += chunk.length;
      if (size > BODY_LIMIT) {
        reject(new HttpError(413, "请求体过大"));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on("end", () => resolve(Buffer.concat(chunks)));
    req.on("error", reject);
  });
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

const PROTOCOLS = new Set(["gpt", "nano", "gemini-official"]);
const RATIOS = new Set(["1:1", "3:2", "2:3", "4:3", "3:4", "16:9", "9:16"]);
const TIERS = new Set(["1K", "2K", "4K"]);
const STUDIO_SIZES = {
  "1:1|1K": "1024x1024",
  "4:3|1K": "1024x768",
  "3:4|1K": "768x1024",
  "16:9|1K": "1024x576",
  "1:1|2K": "2048x2048",
  "4:3|2K": "2048x1536",
  "3:4|2K": "1536x2048",
  "16:9|2K": "2048x1152",
  "1:1|4K": "4096x4096",
  "4:3|4K": "4096x3072",
  "3:4|4K": "3072x4096",
  "16:9|4K": "3840x2160",
};

function parseRelayUrl(input) {
  let url;
  try {
    url = new URL(String(input || "").trim());
  } catch {
    throw new HttpError(400, "中转地址不是合法的 URL");
  }
  if (url.protocol !== "https:") throw new HttpError(400, "中转地址只允许 https");
  if (!ALLOWED_HOSTS.has(url.hostname)) {
    throw new HttpError(400, "中转地址只允许 uuapi.io、uuapi.net、uuapi.shop、uuapi.cc");
  }
  return url;
}

function openaiBase(input) {
  const url = parseRelayUrl(input || "https://uuapi.io/v1");
  let pathname = url.pathname.replace(/\/+$/, "");
  if (pathname.endsWith("/v1beta")) pathname = pathname.slice(0, -"/v1beta".length);
  if (!pathname.endsWith("/v1")) pathname = `${pathname}/v1`;
  return `${url.origin}${pathname}`;
}

function relayOrigin(input) {
  return parseRelayUrl(input || "https://uuapi.io").origin;
}

function parseSize(size) {
  const match = /^(\d{3,4})x(\d{3,4})$/.exec(String(size || "").trim());
  if (!match) throw new HttpError(400, "尺寸格式应为 宽x高，例如 1024x1024");
  const width = Number(match[1]);
  const height = Number(match[2]);
  if (width < 256 || height < 256 || width > 8192 || height > 8192) {
    throw new HttpError(400, "宽和高都要在 256 到 8192 之间");
  }
  return `${width}x${height}`;
}

function parseModel(model) {
  const value = String(model || "").trim().replace(/^models\//, "").replace(/:generateContent$/, "");
  if (!/^[\w.+-]{1,120}$/.test(value)) throw new HttpError(400, "模型名不合法");
  return value;
}

function parseProtocol(protocol) {
  const value = String(protocol || "gpt").trim();
  if (!PROTOCOLS.has(value)) throw new HttpError(400, "请选择调用方式");
  return value;
}

function parseRatio(ratio) {
  const value = String(ratio || "1:1").trim();
  if (!RATIOS.has(value)) throw new HttpError(400, "画幅不支持");
  return value;
}

function parseTier(tier) {
  const value = String(tier || "1K").trim();
  if (!TIERS.has(value)) throw new HttpError(400, "分辨率只支持 1K、2K、4K");
  return value;
}

function pixelsFor(ratio, tier) {
  const known = STUDIO_SIZES[`${ratio}|${tier}`];
  if (known) return known;
  const [a, b] = ratio.split(":").map(Number);
  const long = tier === "4K" ? 4096 : tier === "2K" ? 2048 : 1024;
  const width = a >= b ? long : Math.max(256, Math.round((long * a) / b));
  const height = a >= b ? Math.max(256, Math.round((long * b) / a)) : long;
  return `${width}x${height}`;
}

function parseQuality(quality) {
  const value = String(quality || "medium").trim();
  if (!["auto", "medium", "high"].includes(value)) throw new HttpError(400, "质量只支持 auto、medium、high");
  return value;
}

function errorMessage(payload, status) {
  if (payload && typeof payload === "object") {
    const nested = payload.error;
    if (typeof nested === "string" && nested.trim()) return nested.trim();
    if (nested && typeof nested === "object" && typeof nested.message === "string" && nested.message.trim()) {
      return nested.message.trim();
    }
    if (typeof payload.message === "string" && payload.message.trim()) return payload.message.trim();
    if (payload.promptFeedback && payload.promptFeedback.blockReason) {
      return `内容被拦截：${payload.promptFeedback.blockReason}`;
    }
  }
  return `上游返回 HTTP ${status}`;
}

function parseJson(text) {
  if (!text) return {};
  try {
    return JSON.parse(text);
  } catch {
    return { message: text.slice(0, 500) };
  }
}

function sniffMime(b64) {
  if (b64.startsWith("/9j/")) return "image/jpeg";
  if (b64.startsWith("UklGR")) return "image/webp";
  if (b64.startsWith("R0lGOD")) return "image/gif";
  return "image/png";
}

function collectImages(payload) {
  const images = [];
  const seen = new Set();
  const push = (image) => {
    const key = image.url || image.b64;
    if (!key || seen.has(key)) return;
    seen.add(key);
    images.push(image);
  };
  if (payload && typeof payload.image_url === "string") push({ url: payload.image_url });
  const buckets = [payload?.data, payload?.result?.data, payload?.result];
  for (const bucket of buckets) {
    const list = Array.isArray(bucket) ? bucket : bucket && typeof bucket === "object" && Array.isArray(bucket.data) ? bucket.data : null;
    if (!list) continue;
    for (const item of list) {
      if (typeof item === "string" && /^https:\/\//.test(item)) {
        push({ url: item });
        continue;
      }
      if (!item || typeof item !== "object") continue;
      if (typeof item.url === "string" && item.url) push({ url: item.url });
      else if (typeof item.b64_json === "string" && item.b64_json) {
        push({ b64: item.b64_json, mime: sniffMime(item.b64_json) });
      }
    }
  }
  return images;
}

function taskIdOf(payload, response) {
  if (payload && typeof payload.task_id === "string" && payload.task_id.trim()) return payload.task_id.trim();
  const location = response.headers.get("location") || payload?.poll_url || "";
  const match = String(location).match(/images\/tasks\/([^/?#]+)/);
  return match ? decodeURIComponent(match[1]) : "";
}

function isDoneStatus(status) {
  return ["completed", "succeeded", "success"].includes(status);
}

function isFailedStatus(status) {
  return ["failed", "error", "cancelled", "canceled"].includes(status);
}

async function callUpstream(url, options) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), UPSTREAM_TIMEOUT_MS);
  try {
    return await fetch(url, { ...options, signal: controller.signal, redirect: "manual" });
  } catch (error) {
    if (error.name === "AbortError") throw new HttpError(504, "上游超时");
    throw new HttpError(502, "连接中转站失败");
  } finally {
    clearTimeout(timer);
  }
}

async function readUpstream(response) {
  const text = await response.text();
  return parseJson(text);
}

function decodeImage(image) {
  const mime = String(image?.mime || "");
  if (!["image/png", "image/jpeg", "image/webp"].includes(mime)) {
    throw new HttpError(400, "参考图只支持 PNG、JPEG、WebP");
  }
  const data = String(image?.data || "").replace(/\s+/g, "");
  if (!data) throw new HttpError(400, "参考图是空的");
  const buffer = Buffer.from(data, "base64");
  if (!buffer.length || buffer.length > IMAGE_LIMIT) throw new HttpError(400, "参考图需小于 8MB");
  const name = String(image.name || "reference.png").replace(/[^\w.-]+/g, "_").slice(0, 80) || "reference.png";
  return { mime, buffer, name };
}

function assertPublicImageUrl(raw) {
  let url;
  try {
    url = new URL(String(raw || "").trim());
  } catch {
    throw new HttpError(400, "参考图 URL 不合法");
  }
  if (url.protocol !== "https:") throw new HttpError(400, "参考图 URL 只允许 https");
  const host = url.hostname.toLowerCase();
  if (host === "localhost" || host.endsWith(".local") || host.endsWith(".internal")) {
    throw new HttpError(400, "参考图 URL 不能指向本机");
  }
  if (/^(127\.|10\.|192\.168\.|0\.0\.0\.0|169\.254\.)/.test(host) || /^172\.(1[6-9]|2\d|3[0-1])\./.test(host)) {
    throw new HttpError(400, "参考图 URL 不能指向内网");
  }
  return url.href;
}

function buildRequest(key, payload, file) {
  if (!file) {
    return {
      headers: {
        Authorization: `Bearer ${key}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify(payload),
    };
  }
  const form = new FormData();
  form.set("model", payload.model);
  form.set("prompt", payload.prompt);
  form.set("size", payload.size);
  if (payload.quality) form.set("quality", payload.quality);
  form.set("image", new Blob([file.buffer], { type: file.mime }), file.name);
  return { headers: { Authorization: `Bearer ${key}` }, body: form };
}

function chatText(payload) {
  const content = payload?.choices?.[0]?.message?.content;
  let text = "";
  if (typeof content === "string") text = content;
  else if (Array.isArray(content)) text = content.map((part) => (part && typeof part.text === "string" ? part.text : "")).join("");
  return text.replace(/data:image\/[a-z0-9.+-]+;base64,[a-z0-9+/=\s]+/gi, "").trim().slice(0, 300);
}

function collectChatImages(payload) {
  const images = [];
  const seen = new Set();
  const push = (image) => {
    const key = image.url || image.b64;
    if (!key || seen.has(key)) return;
    seen.add(key);
    images.push(image);
  };
  const content = payload?.choices?.[0]?.message?.content;
  const chunks = [];
  if (typeof content === "string") chunks.push(content);
  if (Array.isArray(content)) {
    for (const part of content) {
      if (!part || typeof part !== "object") continue;
      if (typeof part.text === "string") chunks.push(part.text);
      const url = part.image_url?.url || part.url;
      if (typeof url === "string") chunks.push(url);
      if (typeof part.b64_json === "string") push({ b64: part.b64_json, mime: sniffMime(part.b64_json) });
    }
  }
  const extra = payload?.choices?.[0]?.message?.images;
  if (Array.isArray(extra)) {
    for (const item of extra) {
      const url = typeof item === "string" ? item : item?.image_url?.url || item?.url;
      if (typeof url === "string") chunks.push(url);
    }
  }
  for (const chunk of chunks) addImageStrings(push, chunk);
  return images;
}

function addImageStrings(push, chunk) {
  const dataUrls = String(chunk).matchAll(/data:image\/([a-z0-9.+-]+);base64,([a-z0-9+/=\s]+)/gi);
  for (const match of dataUrls) push({ b64: match[2].replace(/\s+/g, ""), mime: `image/${match[1].toLowerCase()}` });
  const links = String(chunk).matchAll(/https:\/\/[^\s)"']+/g);
  for (const match of links) {
    if (/\.(png|jpe?g|webp|gif)(\?|$)/i.test(match[0]) || /image/i.test(match[0])) push({ url: match[0] });
  }
}

function collectGeminiImages(payload) {
  const images = [];
  const seen = new Set();
  const push = (image) => {
    const key = image.url || image.b64;
    if (!key || seen.has(key)) return;
    seen.add(key);
    images.push(image);
  };
  const candidates = Array.isArray(payload?.candidates) ? payload.candidates : [];
  for (const candidate of candidates) {
    const parts = candidate?.content?.parts;
    if (!Array.isArray(parts)) continue;
    for (const part of parts) {
      const inline = part?.inlineData || part?.inline_data;
      if (inline && typeof inline.data === "string" && inline.data) {
        const mime = inline.mimeType || inline.mime_type || sniffMime(inline.data);
        push({ b64: inline.data.replace(/\s+/g, ""), mime });
      }
      if (typeof part?.text === "string") addImageStrings(push, part.text);
    }
  }
  return images;
}

function shouldTryChat(status) {
  return status === 400 || status === 404 || status === 405 || status === 422 || status === 501;
}

async function syncImages(base, key, syncPath, payload, file) {
  const synced = await callUpstream(`${base}${syncPath}`, { method: "POST", ...buildRequest(key, payload, file) });
  const payloadSync = await readUpstream(synced);
  if (!synced.ok) throw new HttpError(synced.status, errorMessage(payloadSync, synced.status));
  const images = collectImages(payloadSync);
  if (!images.length) throw new HttpError(502, "同步接口没有返回图片");
  return { channel: "sync", taskId: "", images };
}

async function generateViaChat(base, key, spec) {
  const content = [{ type: "text", text: spec.prompt }];
  if (spec.file) {
    content.push({
      type: "image_url",
      image_url: { url: `data:${spec.file.mime};base64,${spec.file.buffer.toString("base64")}` },
    });
  } else if (spec.imageUrl) {
    content.push({ type: "image_url", image_url: { url: spec.imageUrl } });
  }
  const response = await callUpstream(`${base}/chat/completions`, {
    method: "POST",
    headers: { Authorization: `Bearer ${key}`, "Content-Type": "application/json" },
    body: JSON.stringify({
      model: spec.model,
      stream: false,
      messages: [{ role: "user", content: content.length === 1 ? spec.prompt : content }],
      ...(spec.size ? { size: spec.size } : {}),
    }),
  });
  const payload = await readUpstream(response);
  if (!response.ok) throw new HttpError(response.status, errorMessage(payload, response.status));
  const images = collectChatImages(payload);
  if (!images.length) throw new HttpError(502, chatText(payload) || "对话接口没有返回图片");
  return { channel: "chat", taskId: "", images };
}

async function downloadReference(raw) {
  let current = assertPublicImageUrl(raw);
  for (let hop = 0; hop < 3; hop += 1) {
    const response = await callUpstream(current, { method: "GET" });
    if (response.status >= 300 && response.status < 400) {
      const next = response.headers.get("location");
      if (!next) throw new HttpError(400, "参考图下载失败");
      current = assertPublicImageUrl(new URL(next, current).href);
      continue;
    }
    if (!response.ok) throw new HttpError(400, "参考图下载失败");
    const mime = String(response.headers.get("content-type") || "").split(";")[0].trim().toLowerCase();
    if (!["image/png", "image/jpeg", "image/webp"].includes(mime)) {
      throw new HttpError(400, "参考图只支持 PNG、JPEG、WebP");
    }
    const buffer = Buffer.from(await response.arrayBuffer());
    if (!buffer.length || buffer.length > IMAGE_LIMIT) throw new HttpError(400, "参考图需小于 8MB");
    return { mime, buffer, name: "reference" };
  }
  throw new HttpError(400, "参考图重定向过多");
}

async function generateOfficial(origin, key, spec) {
  const parts = [];
  if (spec.file) {
    parts.push({ inlineData: { mimeType: spec.file.mime, data: spec.file.buffer.toString("base64") } });
  } else if (spec.imageUrl) {
    const downloaded = await downloadReference(spec.imageUrl);
    parts.push({ inlineData: { mimeType: downloaded.mime, data: downloaded.buffer.toString("base64") } });
  }
  parts.push({ text: spec.prompt });
  const response = await callUpstream(`${origin}/v1beta/models/${encodeURIComponent(spec.model)}:generateContent`, {
    method: "POST",
    headers: { Authorization: `Bearer ${key}`, "Content-Type": "application/json" },
    body: JSON.stringify({
      contents: [{ role: "user", parts }],
      generationConfig: {
        responseModalities: ["TEXT", "IMAGE"],
        imageConfig: { aspectRatio: spec.aspectRatio, imageSize: spec.imageSize },
      },
    }),
  });
  const payload = await readUpstream(response);
  if (response.status >= 300 && response.status < 400) throw new HttpError(502, "上游返回了重定向，已中止");
  if (!response.ok) throw new HttpError(response.status, errorMessage(payload, response.status));
  const images = collectGeminiImages(payload);
  if (!images.length) {
    const reason = payload?.candidates?.[0]?.finishReason;
    throw new HttpError(502, reason ? `官方接口没有返回图片（${reason}）` : "官方接口没有返回图片");
  }
  return { channel: "gemini", taskId: "", images };
}

async function generateOpenAI(base, key, spec, fallback) {
  const payload = { model: spec.model, prompt: spec.prompt, size: spec.size, response_format: "url" };
  if (spec.quality) payload.quality = spec.quality;
  if (spec.mode === "edit" && spec.imageUrl) payload.images = [{ image_url: spec.imageUrl }];
  const asyncPath = spec.mode === "edit" ? "/images/edits/async" : "/images/generations/async";
  const syncPath = spec.mode === "edit" ? "/images/edits" : "/images/generations";
  const queued = await callUpstream(`${base}${asyncPath}`, { method: "POST", ...buildRequest(key, payload, spec.file) });
  if (queued.status >= 300 && queued.status < 400) throw new HttpError(502, "上游返回了重定向，已中止");

  if (fallback === "chat" && shouldTryChat(queued.status)) {
    const errPayload = await readUpstream(queued);
    const imagesError = errorMessage(errPayload, queued.status);
    try {
      return await generateViaChat(base, key, spec);
    } catch (error) {
      if (queued.status === 404 || queued.status === 405 || queued.status === 501) throw error;
      const message = error instanceof HttpError ? error.message : "对话生图失败";
      throw new HttpError(error.status || 502, `${message}；生图接口：${imagesError}`);
    }
  }
  if (fallback === "sync" && (queued.status === 404 || queued.status === 405 || queued.status === 501)) {
    return syncImages(base, key, syncPath, payload, spec.file);
  }

  const queuedPayload = await readUpstream(queued);
  if (!queued.ok && queued.status !== 202) {
    throw new HttpError(queued.status, errorMessage(queuedPayload, queued.status));
  }

  const immediate = collectImages(queuedPayload);
  const queuedStatus = String(queuedPayload.status || "").toLowerCase();
  if (isFailedStatus(queuedStatus)) throw new HttpError(502, errorMessage(queuedPayload, queued.status));
  if (immediate.length && (isDoneStatus(queuedStatus) || !queuedStatus)) {
    return { channel: "async", taskId: taskIdOf(queuedPayload, queued), images: immediate };
  }

  const taskId = taskIdOf(queuedPayload, queued);
  if (!taskId) throw new HttpError(502, errorMessage(queuedPayload, queued.status) || "异步接口没有返回任务编号");
  if (immediate.length && isDoneStatus(queuedStatus)) return { channel: "async", taskId, images: immediate };
  const polled = await pollTask(base, key, taskId);
  return { channel: "async", ...polled };
}

async function pollTask(base, key, taskId) {
  const deadline = Date.now() + POLL_DEADLINE_MS;
  while (Date.now() < deadline) {
    const response = await callUpstream(`${base}/images/tasks/${encodeURIComponent(taskId)}`, {
      method: "GET",
      headers: { Authorization: `Bearer ${key}` },
    });
    const payload = await readUpstream(response);
    if (response.status >= 300 && response.status < 400) throw new HttpError(502, "轮询被重定向，已中止");
    if (!response.ok) throw new HttpError(response.status, errorMessage(payload, response.status));
    const status = String(payload.status || "").toLowerCase();
    if (isFailedStatus(status)) throw new HttpError(502, errorMessage(payload, payload.http_status || 502));
    const images = collectImages(payload);
    if (isDoneStatus(status) || (images.length && status !== "processing" && status !== "pending" && status !== "queued" && status !== "running")) {
      if (!images.length) throw new HttpError(502, "任务已完成，但响应里没有图片");
      return { taskId, images };
    }
    const retryAfter = Number(response.headers.get("retry-after"));
    const wait = Number.isFinite(retryAfter) && retryAfter > 0 ? Math.min(retryAfter, 15) : 3;
    await sleep(wait * 1000);
  }
  throw new HttpError(504, "生图超时，任务仍在处理。可稍后用任务号到中转站查询");
}

function asNumber(value) {
  if (typeof value === "number" && Number.isFinite(value)) return value;
  if (typeof value === "string" && value.trim() && Number.isFinite(Number(value))) return Number(value);
  return null;
}

function pickBalance(payload) {
  if (!payload || typeof payload !== "object") return null;
  const quotaRemaining = payload.quota && typeof payload.quota === "object" ? asNumber(payload.quota.remaining) : null;
  const candidates = [
    asNumber(payload.balance),
    asNumber(payload.remaining),
    quotaRemaining,
    asNumber(payload.data?.balance),
    asNumber(payload.data?.remaining),
  ];
  const balance = candidates.find((value) => value !== null && value !== undefined);
  if (balance === undefined) return null;
  return { balance, unit: payload.unit || payload.quota?.unit || "USD" };
}

async function fetchKeyBalance(key) {
  const base = openaiBase(key.baseUrl);
  const headers = { Authorization: `Bearer ${key.apiKey}` };
  let lastError = "没有查到余额";
  for (const url of [`${base}/usage`, `${base}/sub2api/billing`]) {
    const response = await callUpstream(url, { method: "GET", headers });
    const payload = await readUpstream(response);
    if (!response.ok) {
      lastError = errorMessage(payload, response.status);
      if (response.status === 401 || response.status === 403) throw new HttpError(response.status, lastError);
      continue;
    }
    const picked = pickBalance(payload);
    if (picked) return picked;
    lastError = "余额接口没有返回数字";
  }
  throw new HttpError(502, lastError);
}

async function rememberBalance(id, fresh, error) {
  try {
    await store.setKeyBalance(id, fresh, error);
  } catch (saveError) {
    console.error(saveError);
  }
}

function requireKey(apiKey) {
  const key = String(apiKey || "").trim();
  if (key.length < 8 || key.length > 400) throw new HttpError(400, "请填写 API Key");
  return key;
}

function requirePrompt(prompt) {
  const value = String(prompt || "").trim();
  if (!value) throw new HttpError(400, "请填写画面描述");
  if (value.length > 4000) throw new HttpError(400, "画面描述请少于 4000 字");
  return value;
}

async function generate(input) {
  const protocol = parseProtocol(input.protocol);
  const key = requireKey(input.apiKey);
  const model = parseModel(input.model);
  const prompt = requirePrompt(input.prompt);
  const mode = input.mode === "edit" ? "edit" : "generate";
  let file = null;
  let imageUrl = "";
  if (mode === "edit") {
    if (input.image && input.image.data) file = decodeImage(input.image);
    else if (String(input.imageUrl || "").trim()) imageUrl = assertPublicImageUrl(input.imageUrl);
    else throw new HttpError(400, "图生图需要上传参考图，或填写 https 图片地址");
  }
  const spec = { model, prompt, mode, file, imageUrl };

  if (protocol === "gemini-official") {
    spec.aspectRatio = parseRatio(input.aspectRatio);
    spec.imageSize = parseTier(input.imageSize);
    return generateOfficial(relayOrigin(input.baseUrl || "https://uuapi.io"), key, spec);
  }

  const base = openaiBase(input.baseUrl || "https://uuapi.io/v1");
  if (protocol === "nano") {
    spec.size = parseSize(pixelsFor(parseRatio(input.aspectRatio), parseTier(input.imageSize)));
    return generateOpenAI(base, key, spec, "chat");
  }

  spec.size = parseSize(input.size);
  spec.quality = parseQuality(input.quality);
  return generateOpenAI(base, key, spec, "sync");
}

function normalizeKeyInput(input) {
  const protocol = parseProtocol(input.protocol);
  const baseUrl = String(input.baseUrl || "").trim();
  if (protocol === "gemini-official") relayOrigin(baseUrl || "https://uuapi.io");
  else openaiBase(baseUrl || "https://uuapi.io/v1");
  const apiKey = String(input.apiKey || "").trim();
  if (apiKey && (apiKey.length < 8 || apiKey.length > 400)) throw new HttpError(400, "API Key 长度不对");
  let balance = null;
  if (input.balance !== null && input.balance !== undefined && input.balance !== "") {
    balance = Number(input.balance);
    if (!Number.isFinite(balance)) throw new HttpError(400, "余额需要是数字");
  }
  return {
    id: String(input.id || ""),
    name: input.name,
    protocol,
    baseUrl: baseUrl || (protocol === "gemini-official" ? "https://uuapi.io" : "https://uuapi.io/v1"),
    apiKey,
    balance,
    enabled: input.enabled !== false,
    note: input.note,
  };
}

async function requireUser(req) {
  const user = await store.sessionUser(cookiesOf(req).darkroom_user);
  if (!user) throw new HttpError(401, "请先登录");
  return user;
}

async function requireAdmin(req) {
  const ok = await store.sessionAdmin(cookiesOf(req).darkroom_admin);
  if (!ok) throw new HttpError(401, "请先登录管理端");
}

function resolvePublic(urlPath) {
  const pathname = decodeURIComponent(urlPath.split("?")[0]);
  const relative = pathname === "/" ? "index.html" : pathname.replace(/^\/+/, "");
  const file = path.resolve(PUBLIC_DIR, relative);
  if (file !== PUBLIC_DIR && !file.startsWith(PUBLIC_DIR + path.sep)) return null;
  return file;
}

const server = http.createServer(async (req, res) => {
  try {
    const url = new URL(req.url, `http://127.0.0.1:${PORT}`);
    if (url.pathname === "/admin" || url.pathname === "/admin/") url.pathname = "/admin.html";

    if (req.method === "GET" && url.pathname === "/health") {
      sendJson(res, 200, { ok: true });
      return;
    }
    if (req.method === "POST" && url.pathname === "/api/auth/register") {
      const body = await readJson(req);
      const result = await store.register(body.username, body.password);
      sendJson(res, 200, { ok: true, user: result.user }, sessionCookie("darkroom_user", result.token));
      return;
    }
    if (req.method === "POST" && url.pathname === "/api/auth/login") {
      const body = await readJson(req);
      const result = await store.login(body.username, body.password);
      sendJson(res, 200, { ok: true, user: result.user }, sessionCookie("darkroom_user", result.token));
      return;
    }
    if (req.method === "POST" && url.pathname === "/api/auth/logout") {
      await store.logout(cookiesOf(req).darkroom_user);
      sendJson(res, 200, { ok: true }, sessionCookie("darkroom_user", ""));
      return;
    }
    if (req.method === "GET" && url.pathname === "/api/me") {
      const user = await store.sessionUser(cookiesOf(req).darkroom_user);
      const state = await store.adminState();
      sendJson(res, 200, {
        ok: true,
        user,
        checkinQuota: state.settings.checkinQuota,
        generateCost: state.settings.generateCost,
      });
      return;
    }
    if (req.method === "POST" && url.pathname === "/api/checkin") {
      const user = await requireUser(req);
      const result = await store.checkin(user.id);
      sendJson(res, 200, { ok: true, ...result });
      return;
    }
    if (req.method === "POST" && url.pathname === "/api/generate") {
      const user = await requireUser(req);
      const input = await readJson(req);
      const protocol = parseProtocol(input.protocol);
      const reserved = await store.reserveGeneration(user.id, protocol);
      try {
        const result = await generate({ ...input, protocol, apiKey: reserved.key.apiKey, baseUrl: reserved.key.baseUrl });
        console.log(`[generate] ${result.channel} protocol=${protocol} model=${parseModel(input.model)} images=${result.images.length}`);
        const latest = await store.sessionUser(cookiesOf(req).darkroom_user);
        sendJson(res, 200, { ok: true, ...result, quota: latest ? latest.quota : reserved.quota });
        fetchKeyBalance(reserved.key).then(
          (fresh) => rememberBalance(reserved.key.id, fresh),
          (error) => rememberBalance(reserved.key.id, null, error.message || "刷新余额失败"),
        );
      } catch (error) {
        const quota = await store.refund(user.id, reserved.cost);
        if (error instanceof HttpError || error.status) error.quota = quota;
        throw error;
      }
      return;
    }
    if (req.method === "POST" && url.pathname === "/api/admin/login") {
      const body = await readJson(req);
      const result = await store.loginAdmin(body.password);
      sendJson(res, 200, { ok: true }, sessionCookie("darkroom_admin", result.token));
      return;
    }
    if (req.method === "POST" && url.pathname === "/api/admin/logout") {
      await store.logout(cookiesOf(req).darkroom_admin);
      sendJson(res, 200, { ok: true }, sessionCookie("darkroom_admin", ""));
      return;
    }
    if (req.method === "GET" && url.pathname === "/api/admin/state") {
      await requireAdmin(req);
      sendJson(res, 200, { ok: true, ...(await store.adminState()) });
      return;
    }
    if (req.method === "PUT" && url.pathname === "/api/admin/settings") {
      await requireAdmin(req);
      const settings = await store.updateSettings(await readJson(req));
      sendJson(res, 200, { ok: true, settings });
      return;
    }
    if (req.method === "POST" && url.pathname === "/api/admin/password") {
      await requireAdmin(req);
      const body = await readJson(req);
      await store.changeAdminPassword(body.oldPassword, body.newPassword);
      sendJson(res, 200, { ok: true });
      return;
    }
    if (req.method === "POST" && url.pathname === "/api/admin/keys") {
      await requireAdmin(req);
      const key = await store.saveKey(normalizeKeyInput(await readJson(req)));
      sendJson(res, 200, { ok: true, key });
      return;
    }
    if (req.method === "DELETE" && url.pathname.startsWith("/api/admin/keys/")) {
      await requireAdmin(req);
      await store.deleteKey(decodeURIComponent(url.pathname.slice("/api/admin/keys/".length)));
      sendJson(res, 200, { ok: true });
      return;
    }
    if (req.method === "POST" && /^\/api\/admin\/keys\/[^/]+\/balance$/.test(url.pathname)) {
      await requireAdmin(req);
      const id = decodeURIComponent(url.pathname.slice("/api/admin/keys/".length, -"/balance".length));
      const state = await store.adminState();
      const key = state.keys.find((item) => item.id === id);
      if (!key) throw new HttpError(404, "找不到这把 Key");
      try {
        const fresh = await fetchKeyBalance(key);
        sendJson(res, 200, { ok: true, key: await store.setKeyBalance(id, fresh) });
      } catch (error) {
        const message = error.message || "刷新余额失败";
        await store.setKeyBalance(id, null, message);
        throw error;
      }
      return;
    }
    if (req.method === "PATCH" && url.pathname.startsWith("/api/admin/users/")) {
      await requireAdmin(req);
      const id = decodeURIComponent(url.pathname.slice("/api/admin/users/".length));
      const body = await readJson(req);
      const user = await store.setUserQuota(id, Number(body.quota));
      sendJson(res, 200, { ok: true, user });
      return;
    }
    if (req.method === "GET") {
      const file = resolvePublic(url.pathname);
      if (!file) {
        sendJson(res, 404, { ok: false, error: "找不到页面" });
        return;
      }
      fs.readFile(file, (error, data) => {
        if (error) {
          sendJson(res, error.code === "ENOENT" ? 404 : 500, { ok: false, error: "找不到页面" });
          return;
        }
        res.writeHead(200, {
          "Content-Type": MIME[path.extname(file)] || "application/octet-stream",
          "Cache-Control": "no-store",
          "X-Content-Type-Options": "nosniff",
        });
        res.end(data);
      });
      return;
    }
    sendJson(res, 405, { ok: false, error: "不支持的方法" });
  } catch (error) {
    const status = error.status || (error instanceof HttpError ? error.status : 500);
    const message = error.status || error instanceof HttpError ? error.message : "服务器内部错误";
    if (!error.status && !(error instanceof HttpError)) console.error(error);
    if (!res.headersSent) sendJson(res, status, { ok: false, error: message, quota: error.quota });
  }
});

store.ensureAdmin();
server.listen(PORT, "127.0.0.1", () => {
  console.log(`暗房已启动 http://127.0.0.1:${PORT}`);
  console.log(`管理端 http://127.0.0.1:${PORT}/admin`);
});
