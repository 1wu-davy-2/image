const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");

const DATA_DIR = path.join(__dirname, "data");
const STORE_PATH = path.join(DATA_DIR, "store.json");
const PASSWORD_NOTE = path.join(DATA_DIR, "initial-admin-password.txt");
const SESSION_MS = 14 * 24 * 60 * 60 * 1000;
const DEFAULT_ADMIN_USER = "admin";
const DEFAULT_ADMIN_PASSWORD = "admin@123";

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
    disabled: Boolean(user.disabled),
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
    userAgent: key.userAgent || "",
    balance: key.balance,
    balanceUnit: key.balanceUnit || "USD",
    balanceValid: typeof key.balanceValid === "boolean" ? key.balanceValid : null,
    balanceUpdatedAt: key.balanceUpdatedAt || "",
    balanceError: key.balanceError || "",
    enabled: key.enabled !== false,
    note: key.note || "",
    lastUsedAt: key.lastUsedAt || 0,
  };
}

function findUser(data, id) {
  const user = data.users.find((item) => item.id === id);
  if (!user) throw new StoreError(404, "找不到这个用户");
  return user;
}

function createSession(data, role, userId) {
  const token = crypto.randomBytes(24).toString("hex");
  data.sessions.push({ token, role, userId: userId || "", expiresAt: Date.now() + SESSION_MS });
  return token;
}

function ensureAdmin() {
  const envUser = String(process.env.ADMIN_USER || "").trim();
  const envPassword = String(process.env.ADMIN_PASSWORD || "");
  const username = envUser || DEFAULT_ADMIN_USER;
  const data = load();

  // An explicit ADMIN_PASSWORD wins every boot, so a forgotten password is
  // recoverable by restarting with the variable set.
  if (envPassword) {
    if (envPassword.length < 6) throw new Error("ADMIN_PASSWORD 至少 6 位");
    const unchanged = data.admin
      && data.admin.username === username
      && verifyPassword(envPassword, data.admin.salt, data.admin.hash);
    if (!unchanged) {
      data.admin = { username, ...hashPassword(envPassword) };
      save(data);
      fs.rmSync(PASSWORD_NOTE, { force: true });
      console.log(`管理端账号已按环境变量对齐：${username}`);
    }
    return;
  }

  if (data.admin) {
    // Stores written before the username existed have no account name.
    if (!data.admin.username) {
      data.admin.username = username;
      save(data);
    }
    return;
  }

  data.admin = { username, ...hashPassword(DEFAULT_ADMIN_PASSWORD) };
  save(data);
  console.log(`管理端默认账号：${username} / ${DEFAULT_ADMIN_PASSWORD}`);
  console.log("服务只监听 127.0.0.1。登录后请到管理端把密码改掉。");
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
      disabled: false,
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
    if (user.disabled) throw new StoreError(403, "这个账号已被停用");
    return { token: createSession(data, "user", user.id), user: publicUser(user) };
  });
}

function loginAdmin(username, password) {
  const name = String(username || "").trim();
  const secret = String(password || "");
  return update((data) => {
    if (!data.admin) throw new StoreError(401, "管理端还没初始化，重启一次服务");
    const expected = data.admin.username || DEFAULT_ADMIN_USER;
    // Both halves are checked on every attempt so a wrong account name and a
    // wrong password cost the same.
    const nameOk = name.toLowerCase() === expected.toLowerCase();
    const passwordOk = verifyPassword(secret, data.admin.salt, data.admin.hash);
    if (!nameOk || !passwordOk) throw new StoreError(401, "管理账号或密码不对");
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
  return user && !user.disabled ? publicUser(user) : null;
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
  // balanceValid === false means /v1/usage reported is_active false: the relay
  // has already rejected this credential, so stop handing it work.
  const list = data.keys.filter(
    (key) => key.enabled !== false && key.protocol === protocol && key.apiKey && key.balanceValid !== false,
  );
  const known = list
    .filter((key) => typeof key.balance === "number" && key.balance > 0)
    .sort((a, b) => b.balance - a.balance || (a.lastUsedAt || 0) - (b.lastUsedAt || 0));
  const unknown = list
    .filter((key) => typeof key.balance !== "number")
    .sort((a, b) => (a.lastUsedAt || 0) - (b.lastUsedAt || 0));
  return known[0] || unknown[0] || null;
}

function requireKeyFor(data, protocol) {
  const key = pickKey(data, protocol);
  if (!key) throw new StoreError(400, "没有可用的生图 Key。请在管理端添加，或确认剩余余额大于 0。");
  return key;
}

// Read-only counterpart of reserveGeneration: batch polling and downloads still
// need a Key, but they must not spend the user's quota.
function keyFor(protocol) {
  return view().then((data) => publicKey(requireKeyFor(data, protocol)));
}

function reserveGeneration(userId, protocol, units = 1) {
  const count = Math.max(1, Math.floor(Number(units) || 1));
  return update((data) => {
    const user = data.users.find((item) => item.id === userId);
    if (!user) throw new StoreError(401, "请先登录");
    if (user.disabled) throw new StoreError(403, "这个账号已被停用");
    const cost = (Number(data.settings.generateCost) || 0) * count;
    if (user.quota < cost) {
      const hint = user.lastCheckinDate === todayShanghai() ? "额度不足，明天可以再签到。" : "额度不足，可以先签到领取。";
      throw new StoreError(402, hint);
    }
    const key = requireKeyFor(data, protocol);
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
        balanceValid: null,
        balanceUpdatedAt: "",
        balanceError: "",
        lastUsedAt: 0,
      };
      data.keys.push(key);
    }
    // A cached balance belongs to the credential it was read with.
    const credentialsChanged = (Boolean(input.apiKey) && input.apiKey !== key.apiKey) || (Boolean(input.baseUrl) && input.baseUrl !== key.baseUrl);
    key.name = String(input.name || "").trim();
    if (!key.name || key.name.length > 40) throw new StoreError(400, "请填写 40 字以内的名称");
    key.protocol = input.protocol;
    key.baseUrl = input.baseUrl;
    if (input.apiKey) key.apiKey = input.apiKey;
    if (!key.apiKey) throw new StoreError(400, "请填写 API Key");
    key.userAgent = String(input.userAgent || "").trim().slice(0, 200);
    key.balance = input.balance;
    key.balanceUnit = input.balanceUnit || key.balanceUnit || "USD";
    if (credentialsChanged) {
      key.balanceValid = null;
      key.balanceError = "";
    }
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
      key.balanceValid = typeof fresh.valid === "boolean" ? fresh.valid : null;
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
    const user = findUser(data, id);
    user.quota = quota;
    return publicUser(user);
  });
}

function setUserDisabled(id, disabled) {
  return update((data) => {
    const user = findUser(data, id);
    user.disabled = Boolean(disabled);
    if (user.disabled) data.sessions = data.sessions.filter((session) => session.userId !== id);
    return publicUser(user);
  });
}

function changeUserPassword(userId, oldPassword, newPassword) {
  const next = validatePassword(newPassword);
  return update((data) => {
    const user = findUser(data, userId);
    if (!verifyPassword(String(oldPassword || ""), user.salt, user.hash)) throw new StoreError(401, "原密码不对");
    Object.assign(user, hashPassword(next));
    return publicUser(user);
  });
}

function resetUserPassword(id, newPassword) {
  const next = validatePassword(newPassword);
  return update((data) => {
    const user = findUser(data, id);
    Object.assign(user, hashPassword(next));
    data.sessions = data.sessions.filter((session) => session.userId !== id);
    return publicUser(user);
  });
}

function deleteUser(id) {
  return update((data) => {
    const before = data.users.length;
    data.users = data.users.filter((user) => user.id !== id);
    if (data.users.length === before) throw new StoreError(404, "找不到这个用户");
    data.sessions = data.sessions.filter((session) => session.userId !== id);
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
  keyFor,
  reserveGeneration,
  refund,
  adminState,
  updateSettings,
  saveKey,
  deleteKey,
  setKeyBalance,
  setUserQuota,
  setUserDisabled,
  changeUserPassword,
  resetUserPassword,
  deleteUser,
};
