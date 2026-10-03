const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");

const DATA_DIR = path.join(__dirname, "data");
const STORE_PATH = path.join(DATA_DIR, "store.json");
const PASSWORD_NOTE = path.join(DATA_DIR, "initial-admin-password.txt");
const SESSION_MS = 14 * 24 * 60 * 60 * 1000;

class StoreError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

function todayShanghai() {
  return new Intl.DateTimeFormat("en-CA", {
    timeZone: "Asia/Shanghai",
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
  }).format(new Date());
}

function emptyStore() {
  return {
    admin: null,
    settings: { checkinQuota: 5, generateCost: 1 },
    keys: [],
    users: [],
    sessions: [],
  };
}

function load() {
  try {
    const parsed = JSON.parse(fs.readFileSync(STORE_PATH, "utf8"));
    return {
      ...emptyStore(),
      ...parsed,
      settings: { ...emptyStore().settings, ...(parsed.settings || {}) },
      keys: Array.isArray(parsed.keys) ? parsed.keys : [],
      users: Array.isArray(parsed.users) ? parsed.users : [],
      sessions: Array.isArray(parsed.sessions) ? parsed.sessions : [],
    };
  } catch (error) {
    if (error.code === "ENOENT") return emptyStore();
    throw error;
  }
}

function save(data) {
  fs.mkdirSync(DATA_DIR, { recursive: true });
  const tmp = `${STORE_PATH}.tmp`;
  fs.writeFileSync(tmp, JSON.stringify(data, null, 2));
  fs.renameSync(tmp, STORE_PATH);
}

let chain = Promise.resolve();

function update(mutator) {
  const run = chain.then(async () => {
    const data = load();
    data.sessions = data.sessions.filter((session) => session.expiresAt > Date.now());
    const result = await mutator(data);
    save(data);
    return result;
  });
  chain = run.then(() => {}, () => {});
  return run;
}

function view() {
  return chain.then(() => load());
}

function hashPassword(password, salt = crypto.randomBytes(16)) {
  const hash = crypto.scryptSync(password, salt, 32);
  return { salt: salt.toString("hex"), hash: hash.toString("hex") };
}

function verifyPassword(password, saltHex, hashHex) {
  const actual = crypto.scryptSync(password, Buffer.from(saltHex, "hex"), 32);
  const expected = Buffer.from(hashHex, "hex");
  return actual.length === expected.length && crypto.timingSafeEqual(actual, expected);
}

function newId() {
  return crypto.randomBytes(8).toString("hex");
}

function publicUser(user) {
  const today = todayShanghai();
  return {
    id: user.id,
    username: user.username,
    quota: user.quota,
    lastCheckinDate: user.lastCheckinDate || "",
    checkedInToday: user.lastCheckinDate === today,
    createdAt: user.createdAt,
  };
}

function publicKey(key) {
  return {
    id: key.id,
    name: key.name,
    protocol: key.protocol,
    baseUrl: key.baseUrl,
    apiKey: key.apiKey,
    balance: key.balance,
    balanceUnit: key.balanceUnit || "USD",
    balanceUpdatedAt: key.balanceUpdatedAt || "",
    balanceError: key.balanceError || "",
    enabled: key.enabled !== false,
    note: key.note || "",
    lastUsedAt: key.lastUsedAt || 0,
  };
}

function createSession(data, role, userId) {
  const token = crypto.randomBytes(24).toString("hex");
  data.sessions.push({ token, role, userId: userId || "", expiresAt: Date.now() + SESSION_MS });
  return token;
}

function ensureAdmin() {
  const envPassword = String(process.env.ADMIN_PASSWORD || "");
  const data = load();
  if (envPassword) {
    if (envPassword.length < 6) throw new Error("ADMIN_PASSWORD 至少 6 位");
    if (!data.admin || !verifyPassword(envPassword, data.admin.salt, data.admin.hash)) {
      data.admin = hashPassword(envPassword);
      save(data);
      console.log("管理端密码已按环境变量 ADMIN_PASSWORD 设置");
    }
    return;
  }
  if (data.admin) return;
  const password = crypto.randomBytes(9).toString("base64url");
  data.admin = hashPassword(password);
  save(data);
  fs.mkdirSync(DATA_DIR, { recursive: true });
  fs.writeFileSync(PASSWORD_NOTE, `管理端初始密码：${password}\n登录 http://127.0.0.1:3780/admin 后请修改密码，并删除本文件。\n`);
  console.log(`管理端初始密码：${password}`);
  console.log(`已写入 ${PASSWORD_NOTE}`);
}

function validateUsername(username) {
  const value = String(username || "").trim();
  if (!/^[\p{L}\p{N}_-]{2,20}$/u.test(value)) throw new StoreError(400, "用户名需要 2 到 20 位，可用字母、数字、下划线");
  return value;
}

function validatePassword(password) {
  const value = String(password || "");
  if (value.length < 6 || value.length > 72) throw new StoreError(400, "密码需要 6 到 72 位");
  return value;
}

function register(username, password) {
  const name = validateUsername(username);
  const secret = validatePassword(password);
  return update((data) => {
    if (data.users.some((user) => user.username.toLowerCase() === name.toLowerCase())) {
      throw new StoreError(409, "这个用户名已经注册过");
    }
    const user = {
      id: newId(),
      username: name,
      ...hashPassword(secret),
      quota: 0,
      lastCheckinDate: "",
      createdAt: new Date().toISOString(),
    };
    data.users.push(user);
    const token = createSession(data, "user", user.id);
    return { token, user: publicUser(user) };
  });
}

function login(username, password) {
  const name = String(username || "").trim();
  const secret = String(password || "");
  return update((data) => {
    const user = data.users.find((item) => item.username.toLowerCase() === name.toLowerCase());
    if (!user || !verifyPassword(secret, user.salt, user.hash)) throw new StoreError(401, "用户名或密码不对");
    return { token: createSession(data, "user", user.id), user: publicUser(user) };
  });
}

function loginAdmin(password) {
  const secret = String(password || "");
  return update((data) => {
    if (!data.admin || !verifyPassword(secret, data.admin.salt, data.admin.hash)) throw new StoreError(401, "管理密码不对");
    return { token: createSession(data, "admin", "") };
  });
}

function logout(token) {
  return update((data) => {
    data.sessions = data.sessions.filter((session) => session.token !== token);
  });
}

async function sessionUser(token) {
  if (!token) return null;
  const data = await view();
  const session = data.sessions.find((item) => item.token === token && item.role === "user" && item.expiresAt > Date.now());
  if (!session) return null;
  const user = data.users.find((item) => item.id === session.userId);
  return user ? publicUser(user) : null;
}

async function sessionAdmin(token) {
  if (!token) return false;
  const data = await view();
  return data.sessions.some((item) => item.token === token && item.role === "admin" && item.expiresAt > Date.now());
}

function changeAdminPassword(oldPassword, newPassword) {
  const next = validatePassword(newPassword);
  return update((data) => {
    if (!data.admin || !verifyPassword(String(oldPassword || ""), data.admin.salt, data.admin.hash)) {
      throw new StoreError(401, "原密码不对");
    }
    data.admin = hashPassword(next);
    return true;
  }).then((result) => {
    fs.rmSync(PASSWORD_NOTE, { force: true });
    return result;
  });
}

function checkin(userId) {
  return update((data) => {
    const user = data.users.find((item) => item.id === userId);
    if (!user) throw new StoreError(401, "请先登录");
    const today = todayShanghai();
    if (user.lastCheckinDate === today) throw new StoreError(400, "今天已经签过到了");
    const amount = Number(data.settings.checkinQuota) || 0;
    user.quota += amount;
    user.lastCheckinDate = today;
    return { amount, user: publicUser(user) };
  });
}

function pickKey(data, protocol) {
  const list = data.keys.filter((key) => key.enabled !== false && key.protocol === protocol && key.apiKey);
  const known = list
    .filter((key) => typeof key.balance === "number" && key.balance > 0)
    .sort((a, b) => b.balance - a.balance || (a.lastUsedAt || 0) - (b.lastUsedAt || 0));
  const unknown = list
    .filter((key) => typeof key.balance !== "number")
    .sort((a, b) => (a.lastUsedAt || 0) - (b.lastUsedAt || 0));
  return known[0] || unknown[0] || null;
}

function reserveGeneration(userId, protocol) {
  return update((data) => {
    const user = data.users.find((item) => item.id === userId);
    if (!user) throw new StoreError(401, "请先登录");
    const cost = Number(data.settings.generateCost) || 0;
    if (user.quota < cost) {
      const hint = user.lastCheckinDate === todayShanghai() ? "额度不足，明天可以再签到。" : "额度不足，可以先签到领取。";
      throw new StoreError(402, hint);
    }
    const key = pickKey(data, protocol);
    if (!key) throw new StoreError(400, "没有可用的生图 Key。请在管理端添加，或确认剩余余额大于 0。");
    user.quota -= cost;
    key.lastUsedAt = Date.now();
    return { cost, quota: user.quota, key: publicKey(key) };
  });
}

function refund(userId, cost) {
  return update((data) => {
    const user = data.users.find((item) => item.id === userId);
    if (user) user.quota += cost;
    return user ? user.quota : 0;
  });
}

function adminState() {
  return view().then((data) => ({
    settings: data.settings,
    keys: data.keys.map(publicKey),
    users: data.users.map(publicUser),
    checkinDate: todayShanghai(),
  }));
}

function updateSettings(input) {
  return update((data) => {
    const checkinQuota = Number(input.checkinQuota);
    const generateCost = Number(input.generateCost);
    if (!Number.isInteger(checkinQuota) || checkinQuota < 0 || checkinQuota > 1000) {
      throw new StoreError(400, "签到额度需要是 0 到 1000 的整数");
    }
    if (!Number.isInteger(generateCost) || generateCost < 0 || generateCost > 1000) {
      throw new StoreError(400, "每次消耗需要是 0 到 1000 的整数");
    }
    data.settings.checkinQuota = checkinQuota;
    data.settings.generateCost = generateCost;
    return data.settings;
  });
}

function saveKey(input) {
  return update((data) => {
    let key = input.id ? data.keys.find((item) => item.id === input.id) : null;
    if (input.id && !key) throw new StoreError(404, "找不到这把 Key");
    if (!key) {
      key = {
        id: newId(),
        createdAt: new Date().toISOString(),
        balance: null,
        balanceUnit: "USD",
        balanceUpdatedAt: "",
        balanceError: "",
        lastUsedAt: 0,
      };
      data.keys.push(key);
    }
    key.name = String(input.name || "").trim();
    if (!key.name || key.name.length > 40) throw new StoreError(400, "请填写 40 字以内的名称");
    key.protocol = input.protocol;
    key.baseUrl = input.baseUrl;
    if (input.apiKey) key.apiKey = input.apiKey;
    if (!key.apiKey) throw new StoreError(400, "请填写 API Key");
    key.balance = input.balance;
    key.balanceUnit = input.balanceUnit || key.balanceUnit || "USD";
    key.enabled = input.enabled !== false;
    key.note = String(input.note || "").trim().slice(0, 200);
    return publicKey(key);
  });
}

function deleteKey(id) {
  return update((data) => {
    const before = data.keys.length;
    data.keys = data.keys.filter((key) => key.id !== id);
    if (data.keys.length === before) throw new StoreError(404, "找不到这把 Key");
  });
}

function setKeyBalance(id, fresh, error) {
  return update((data) => {
    const key = data.keys.find((item) => item.id === id);
    if (!key) throw new StoreError(404, "找不到这把 Key");
    if (fresh) {
      key.balance = fresh.balance;
      key.balanceUnit = fresh.unit || "USD";
      key.balanceUpdatedAt = new Date().toISOString();
      key.balanceError = "";
    } else if (error) {
      key.balanceError = String(error).slice(0, 300);
    }
    return publicKey(key);
  });
}

function setUserQuota(id, quota) {
  return update((data) => {
    if (!Number.isInteger(quota) || quota < 0 || quota > 1000000) throw new StoreError(400, "额度需要是 0 到 1000000 的整数");
    const user = data.users.find((item) => item.id === id);
    if (!user) throw new StoreError(404, "找不到这个用户");
    user.quota = quota;
    return publicUser(user);
  });
}

module.exports = {
  StoreError,
  PASSWORD_NOTE,
  todayShanghai,
  ensureAdmin,
  register,
  login,
  loginAdmin,
  logout,
  sessionUser,
  sessionAdmin,
  changeAdminPassword,
  checkin,
  reserveGeneration,
  refund,
  adminState,
  updateSettings,
  saveKey,
  deleteKey,
  setKeyBalance,
  setUserQuota,
};
