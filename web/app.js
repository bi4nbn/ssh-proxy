/* ================= 工具函数 ================= */
const $ = (id) => document.getElementById(id);

const STATE_TEXT = {
  idle: '未连接',
  connecting: '连接中…',
  connected: '已连接',
  disconnecting: '断开中…',
  error: '连接失败',
  reconnecting: '重连中…',
};

const AUTH_TEXT = { password: '密码认证', key: '私钥认证', both: '密码 + 私钥' };

const app = {
  profiles: [],
  selectedId: localStorage.getItem('sshproxy.selected') || '',
  status: null,
  logSeq: 0,
  logCount: 0,
  editing: null,       // 正在编辑的配置（null 表示新建）
  formForwards: [],
  authType: 'password',
  es: null,
  lastStatusKey: '',
};

/* ================= HTTP ================= */
async function api(path, options = {}) {
  const opt = { headers: { 'Content-Type': 'application/json' }, ...options };
  if (opt.body && typeof opt.body !== 'string') opt.body = JSON.stringify(opt.body);
  const res = await fetch(path, opt);
  const data = await res.json().catch(() => ({ ok: false, error: '响应解析失败' }));
  if (!res.ok || data.ok === false) throw new Error(data.error || `请求失败 (${res.status})`);
  return data;
}

function toast(msg, type = '') {
  const el = document.createElement('div');
  el.className = 'toast ' + type;
  el.textContent = msg;
  $('toastWrap').appendChild(el);
  setTimeout(() => {
    el.style.transition = 'opacity .3s';
    el.style.opacity = '0';
    setTimeout(() => el.remove(), 320);
  }, 2600);
}

function fmtBytes(n) {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)) + ' ' + u[i];
}

function fmtUptime(sec) {
  sec = Math.max(0, Math.floor(sec || 0));
  const h = String(Math.floor(sec / 3600)).padStart(2, '0');
  const m = String(Math.floor((sec % 3600) / 60)).padStart(2, '0');
  const s = String(sec % 60).padStart(2, '0');
  return `${h}:${m}:${s}`;
}

function fmtTime(ms) {
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

/* ================= 渲染：配置列表 ================= */
function currentProfile() {
  return app.profiles.find((p) => p.id === app.selectedId) || null;
}

function renderProfiles() {
  const kw = ($('search').value || '').trim().toLowerCase();
  const list = $('profileList');
  const items = app.profiles.filter((p) => {
    if (!kw) return true;
    return (p.name + ' ' + p.host + ' ' + p.user).toLowerCase().includes(kw);
  });

  $('profileCount').textContent = app.profiles.length;
  list.innerHTML = '';

  if (!items.length) {
    list.innerHTML = `<div class="empty-list">${app.profiles.length ? '没有匹配的配置' : '还没有连接配置<br>点击下方「新建连接」开始'}</div>`;
    return;
  }

  const st = app.status || {};
  for (const p of items) {
    const el = document.createElement('div');
    el.className = 'profile-item' + (p.id === app.selectedId ? ' active' : '');
    const live = p.id === st.profileId && (st.state === 'connected' || st.state === 'connecting' || st.state === 'reconnecting');
    el.innerHTML = `
      <div class="p-name">${escapeHtml(p.name)}${live ? '<i class="p-live"></i>' : ''}</div>
      <div class="p-addr">${escapeHtml(p.user)}@${escapeHtml(p.host)}:${p.port}</div>`;
    el.onclick = () => {
      app.selectedId = p.id;
      localStorage.setItem('sshproxy.selected', p.id);
      renderProfiles();
      renderStatus(app.status);
    };
    el.ondblclick = () => openDialog(p);
    list.appendChild(el);
  }
}

function escapeHtml(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

/* ================= 渲染：连接状态 ================= */
function renderStatus(st) {
  if (!st) return;
  app.status = st;

  const p = currentProfile();
  const isLive = st.state === 'connected' || st.state === 'connecting' || st.state === 'reconnecting';
  const sameProfile = p && st.profileId === p.id;
  // 连接进行中时，无论列表选中哪一条，面板都展示"当前连接"的数据
  const liveProfile = isLive && !!st.profileId;

  // 状态胶囊
  const pill = $('statusPill');
  pill.className = 'status-pill ' + st.state;
  $('statusText').textContent = STATE_TEXT[st.state] || st.state;

  // 标题
  if (sameProfile || (isLive && st.profileName)) {
    $('connName').textContent = st.profileName || p.name;
    $('connAddr').textContent = `${st.user}@${st.addr}`;
  } else if (p) {
    $('connName').textContent = p.name;
    $('connAddr').textContent = `${p.user}@${p.host}:${p.port}`;
  } else {
    $('connName').textContent = '未选择配置';
    $('connAddr').textContent = '从左侧选择一个配置，或新建一个 SSH 连接';
  }

  // 徽章
  const badges = [];
  const auth = (liveProfile ? st.authType : p && p.authType) || '';
  if (auth) badges.push(`<span class="badge">${AUTH_TEXT[auth] || auth}</span>`);
  if (liveProfile && st.state === 'connected') {
    if (st.socksAddr) badges.push('<span class="badge ok">SOCKS5 已就绪</span>');
    if (st.forwards && st.forwards.length) badges.push(`<span class="badge ok">${st.forwards.length} 条转发</span>`);
    if (st.fingerprint) badges.push(`<span class="badge ${st.trusted ? 'ok' : 'warn'}">${st.trusted ? '指纹已信任' : '指纹未信任'}</span>`);
  } else if (liveProfile && (st.state === 'connecting' || st.state === 'reconnecting')) {
    badges.push(`<span class="badge warn">第 ${st.attempt || 1} 次尝试</span>`);
  }
  $('badges').innerHTML = badges.join('');

  // 主按钮
  const btn = $('btnToggle');
  const txt = $('btnToggleText');
  btn.className = 'power';
  if (st.state === 'connected') {
    btn.classList.add('on');
    txt.textContent = '断开';
  } else if (st.state === 'connecting' || st.state === 'reconnecting') {
    btn.classList.add('busy');
    txt.textContent = '连接中';
  } else if (st.state === 'disconnecting') {
    btn.classList.add('busy');
    txt.textContent = '断开中';
  } else {
    txt.textContent = '连接';
  }

  // 统计
  const connected = st.state === 'connected';
  $('statUptime').textContent = connected ? fmtUptime(st.uptime) : '00:00:00';
  $('statSince').textContent = connected && st.since ? '开始于 ' + fmtTime(st.since) : '尚未建立连接';

  if (connected && st.latency > 0) {
    const lat = st.latency < 10 ? st.latency.toFixed(1) : Math.round(st.latency);
    $('statLatency').innerHTML = `${lat}<small>ms</small>`;
    const pct = Math.max(4, Math.min(100, 100 - Math.min(st.latency, 400) / 4));
    $('latencyBar').style.width = pct + '%';
  } else if (connected && st.latency === -1) {
    $('statLatency').innerHTML = '超时';
    $('latencyBar').style.width = '100%';
  } else {
    $('statLatency').innerHTML = '—<small>ms</small>';
    $('latencyBar').style.width = '0%';
  }

  $('statTraffic').textContent = `${fmtBytes(st.bytesIn)} / ${fmtBytes(st.bytesOut)}`;
  $('statRate').textContent = connected ? '下行 / 上行（本次连接）' : '实时流量统计';

  $('statSocks').textContent = st.socksAddr ? st.socksAddr : '—';
  $('statSocksHint').textContent = st.socksAddr ? 'socks5://' + st.socksAddr : 'SOCKS5 未启动';

  // 代理面板
  $('proxySocks').textContent = st.socksAddr ? 'socks5://' + st.socksAddr : '—';
  const fwdList = $('fwdList');
  if (liveProfile && st.forwards && st.forwards.length) {
    fwdList.innerHTML = st.forwards.map((f) => {
      const [l, r] = String(f).split(' -> ');
      return `<div class="fwd-item"><b>${escapeHtml(l)}</b> → <span>${escapeHtml(r || '')}</span></div>`;
    }).join('');
  } else {
    fwdList.innerHTML = '<div class="empty-hint">未配置端口转发</div>';
  }

  // 详情
  $('detailAuth').textContent = auth ? (AUTH_TEXT[auth] || auth) : '—';
  $('detailFp').textContent = st.fingerprint || '—';
  $('detailAttempt').textContent = String(st.attempt || 0);
  const errEl = $('detailErr');
  errEl.textContent = st.error || '无';
  errEl.classList.toggle('err-text', !!st.error);
  $('btnTrust').style.display = (st.fingerprint && !st.trusted && liveProfile) ? '' : 'none';

  // 列表里的在线标记
  const key = JSON.stringify([st.state, st.profileId, app.profiles.length, app.selectedId]);
  if (key !== app.lastStatusKey) {
    app.lastStatusKey = key;
    renderProfiles();
  }
}

/* ================= 日志 ================= */
function appendLogs(entries) {
  if (!entries || !entries.length) return;
  const body = $('logBody');
  const auto = $('autoScroll').checked;
  for (const e of entries) {
    if (e.seq <= app.logSeq) continue;
    app.logSeq = e.seq;
    const line = document.createElement('div');
    line.className = 'log-line';
    line.innerHTML = `<span class="t">${e.time}</span><span class="lv ${e.level}">${levelText(e.level)}</span><span class="m">${escapeHtml(e.msg)}</span>`;
    body.appendChild(line);
    app.logCount++;
  }
  while (body.childElementCount > 400) body.removeChild(body.firstChild);
  if (auto) body.scrollTop = body.scrollHeight;
}

function levelText(lv) {
  return { info: 'INFO', success: 'OK', warn: 'WARN', error: 'ERROR', debug: 'DEBUG' }[lv] || lv;
}

/* ================= 数据同步 ================= */
async function bootstrap() {
  try {
    const data = await api('/api/state?since=0');
    app.profiles = data.profiles || [];
    $('dataDir').textContent = data.dataDir || '';
    $('dataDir').title = data.dataDir || '';
    if (!app.selectedId && app.profiles.length) {
      app.selectedId = app.profiles[0].id;
    }
    if (app.selectedId && !app.profiles.some((p) => p.id === app.selectedId)) {
      app.selectedId = app.profiles[0] ? app.profiles[0].id : '';
    }
    renderProfiles();
    renderStatus(data.status);
    appendLogs(data.logs);
    startStream();
  } catch (err) {
    toast('初始化失败: ' + err.message, 'bad');
  }
}

function startStream() {
  if (app.es) app.es.close();
  const es = new EventSource('/api/events?since=' + app.logSeq);
  app.es = es;
  es.onmessage = (ev) => {
    try {
      const data = JSON.parse(ev.data);
      renderStatus(data.status);
      appendLogs(data.logs);
    } catch (_) { /* 忽略单帧解析错误 */ }
  };
  es.onerror = () => { /* EventSource 会自动重连 */ };
}

async function reloadProfiles(keepId) {
  const data = await api('/api/profiles');
  app.profiles = data.profiles || [];
  if (keepId) app.selectedId = keepId;
  renderProfiles();
}

/* ================= 连接控制 ================= */
async function toggleConnection() {
  const st = app.status || {};
  const busy = st.state === 'connecting' || st.state === 'disconnecting' || st.state === 'reconnecting';
  if (busy) return;

  try {
    if (st.state === 'connected') {
      await api('/api/disconnect', { method: 'POST', body: {} });
      toast('已断开连接');
    } else {
      if (!app.selectedId) {
        toast('请先选择一个配置', 'bad');
        openDialog(null);
        return;
      }
      await api('/api/connect', { method: 'POST', body: { id: app.selectedId } });
    }
  } catch (err) {
    toast(err.message, 'bad');
  }
}

/* ================= 弹窗 ================= */
function openDialog(profile) {
  app.editing = profile || null;
  app.authType = profile ? profile.authType || 'password' : 'password';
  app.formForwards = profile && profile.forwards ? profile.forwards.map((f) => ({ ...f })) : [];

  $('dialogTitle').textContent = profile ? '编辑连接' : '新建连接';
  $('btnDelete').style.display = profile ? '' : 'none';
  $('testResult').className = 'test-result';
  $('testResult').textContent = '';

  $('f-name').value = profile ? profile.name : '';
  $('f-user').value = profile ? profile.user : '';
  $('f-host').value = profile ? profile.host : '';
  $('f-port').value = profile ? profile.port : 22;
  $('f-password').value = profile ? profile.password : '';
  $('f-keypath').value = profile ? profile.keyPath : '';
  $('f-passphrase').value = profile ? profile.keyPassphrase : '';
  $('f-keydata').value = '';
  $('f-socks').checked = profile ? !!profile.socksEnabled : true;
  $('f-socksport').value = profile ? profile.socksPort : 1080;
  $('f-keepalive').value = profile ? profile.keepAlive : 15;
  $('f-timeout').value = profile ? profile.timeout : 15;
  $('f-autoreconnect').checked = profile ? !!profile.autoReconnect : false;
  $('f-strict').checked = profile ? !!profile.strictHostKey : false;

  syncAuthUI();
  renderFwdEditor();
  loadKeyList();
  $('overlay').classList.add('show');
  setTimeout(() => $('f-name').focus(), 60);
}

function closeDialog() {
  $('overlay').classList.remove('show');
  app.editing = null;
}

function syncAuthUI() {
  document.querySelectorAll('#authSeg button').forEach((b) => {
    b.classList.toggle('on', b.dataset.value === app.authType);
  });
  $('pwdGroup').style.display = app.authType === 'password' || app.authType === 'both' ? '' : 'none';
  $('keyGroup').style.display = app.authType === 'key' || app.authType === 'both' ? '' : 'none';
}

function renderFwdEditor() {
  const box = $('fwdEditor');
  box.innerHTML = '';
  if (!app.formForwards.length) {
    box.innerHTML = '<div class="empty-hint">暂无规则，点击右上角「+ 添加规则」</div>';
    return;
  }
  const head = document.createElement('div');
  head.className = 'fwd-head';
  head.innerHTML = '<span>本地端口</span><span>目标主机</span><span>目标端口</span><span>备注</span><span></span>';
  box.appendChild(head);

  app.formForwards.forEach((f, i) => {
    const row = document.createElement('div');
    row.className = 'fwd-row';
    row.innerHTML = `
      <input type="number" min="1" max="65535" placeholder="8080" value="${f.localPort || ''}">
      <input type="text" placeholder="127.0.0.1" value="${escapeHtml(f.remoteHost || '')}">
      <input type="number" min="1" max="65535" placeholder="80" value="${f.remotePort || ''}">
      <input type="text" placeholder="备注（可选）" value="${escapeHtml(f.comment || '')}">
      <button type="button" class="del" title="删除">×</button>`;
    const [lp, rh, rp, cm] = row.querySelectorAll('input');
    lp.oninput = () => (f.localPort = parseInt(lp.value, 10) || 0);
    rh.oninput = () => (f.remoteHost = rh.value.trim());
    rp.oninput = () => (f.remotePort = parseInt(rp.value, 10) || 0);
    cm.oninput = () => (f.comment = cm.value.trim());
    row.querySelector('.del').onclick = () => {
      app.formForwards.splice(i, 1);
      renderFwdEditor();
    };
    box.appendChild(row);
  });
}

async function loadKeyList() {
  try {
    const data = await api('/api/keys');
    const sel = $('f-keylist');
    sel.innerHTML = '<option value="">扫描 ~/.ssh …</option>';
    (data.keys || []).forEach((k) => {
      const o = document.createElement('option');
      o.value = k;
      o.textContent = k.split(/[\\/]/).pop();
      sel.appendChild(o);
    });
    if (!data.keys || !data.keys.length) {
      sel.innerHTML = '<option value="">未找到可用私钥</option>';
    }
  } catch (_) { /* 忽略 */ }
}

function collectForm() {
  const p = {
    id: app.editing ? app.editing.id : '',
    name: $('f-name').value.trim(),
    user: $('f-user').value.trim(),
    host: $('f-host').value.trim(),
    port: parseInt($('f-port').value, 10) || 22,
    authType: app.authType,
    password: $('f-password').value,
    keyPath: $('f-keypath').value.trim(),
    keyData: $('f-keydata').value.trim(),
    keyPassphrase: $('f-passphrase').value,
    socksEnabled: $('f-socks').checked,
    socksPort: parseInt($('f-socksport').value, 10) || 1080,
    keepAlive: parseInt($('f-keepalive').value, 10) || 15,
    timeout: parseInt($('f-timeout').value, 10) || 15,
    autoReconnect: $('f-autoreconnect').checked,
    strictHostKey: $('f-strict').checked,
    forwards: app.formForwards.filter((f) => f.localPort && f.remoteHost && f.remotePort),
  };
  if (!p.host) throw new Error('请填写主机地址');
  if (!p.user) throw new Error('请填写用户名');
  if (p.authType === 'password' && !p.password) throw new Error('请填写登录密码');
  if ((p.authType === 'key' || p.authType === 'both') && !p.keyPath && !p.keyData) {
    throw new Error('请选择私钥文件或粘贴私钥内容');
  }
  return p;
}

async function saveProfile(andConnect) {
  let payload;
  try {
    payload = collectForm();
  } catch (err) {
    toast(err.message, 'bad');
    return;
  }
  try {
    const data = await api('/api/profiles', { method: 'POST', body: payload });
    const saved = data.profile;
    app.selectedId = saved.id;
    localStorage.setItem('sshproxy.selected', saved.id);
    await reloadProfiles(saved.id);
    renderStatus(app.status);
    closeDialog();
    toast('配置已保存', 'ok');
    if (andConnect) {
      await api('/api/connect', { method: 'POST', body: { id: saved.id } });
    }
  } catch (err) {
    toast('保存失败: ' + err.message, 'bad');
  }
}

async function testConnection() {
  let payload;
  try {
    payload = collectForm();
  } catch (err) {
    toast(err.message, 'bad');
    return;
  }
  const box = $('testResult');
  box.className = 'test-result show';
  box.textContent = '正在测试连接…';
  try {
    const r = await api('/api/test', { method: 'POST', body: payload });
    box.className = 'test-result show ok';
    box.innerHTML = `连接成功 · TCP ${r.tcpMs}ms · 握手 ${r.handshakeMs}ms · 延迟 ${r.latencyMs}ms<br>
      <code>${escapeHtml(r.server || '')}</code><br>
      <code>${escapeHtml(r.fingerprint || '')}</code>`;
  } catch (err) {
    box.className = 'test-result show bad';
    box.textContent = '测试失败: ' + err.message;
  }
}

async function deleteProfile() {
  if (!app.editing) return;
  if (!confirm(`确定删除配置「${app.editing.name}」吗？`)) return;
  try {
    await api('/api/profiles?id=' + encodeURIComponent(app.editing.id), { method: 'DELETE' });
    if (app.selectedId === app.editing.id) {
      app.selectedId = '';
      localStorage.removeItem('sshproxy.selected');
    }
    closeDialog();
    await reloadProfiles();
    renderStatus(app.status);
    toast('已删除', 'ok');
  } catch (err) {
    toast(err.message, 'bad');
  }
}

/* ================= 事件绑定 ================= */
function bind() {
  $('btnToggle').onclick = toggleConnection;
  $('btnNew').onclick = () => openDialog(null);
  $('search').oninput = renderProfiles;
  $('btnClearLogs').onclick = async () => {
    await api('/api/logs/clear', { method: 'POST', body: {} }).catch(() => {});
    $('logBody').innerHTML = '';
    app.logCount = 0;
  };
  $('btnCopySocks').onclick = async () => {
    const st = app.status || {};
    if (!st.socksAddr) return toast('代理未启动', 'bad');
    try {
      await navigator.clipboard.writeText('socks5://' + st.socksAddr);
      toast('已复制 socks5://' + st.socksAddr, 'ok');
    } catch (_) {
      toast('socks5://' + st.socksAddr);
    }
  };
  $('btnTrust').onclick = async () => {
    const st = app.status || {};
    try {
      await api('/api/trust', { method: 'POST', body: { addr: st.addr } });
      toast('已信任该主机指纹', 'ok');
    } catch (err) {
      toast(err.message, 'bad');
    }
  };

  // 弹窗
  $('btnCloseDialog').onclick = closeDialog;
  $('btnCancel').onclick = closeDialog;
  $('overlay').onclick = (e) => { if (e.target === $('overlay')) closeDialog(); };
  $('btnSave').onclick = () => saveProfile(false);
  $('btnSaveConnect').onclick = () => saveProfile(true);
  $('btnTest').onclick = testConnection;
  $('btnDelete').onclick = deleteProfile;
  $('btnAddFwd').onclick = () => {
    app.formForwards.push({ localPort: 8080, remoteHost: '127.0.0.1', remotePort: 80, comment: '' });
    renderFwdEditor();
  };
  $('btnEye').onclick = () => {
    const inp = $('f-password');
    inp.type = inp.type === 'password' ? 'text' : 'password';
    $('btnEye').textContent = inp.type === 'password' ? '显示' : '隐藏';
  };
  document.querySelectorAll('#authSeg button').forEach((b) => {
    b.onclick = () => { app.authType = b.dataset.value; syncAuthUI(); };
  });
  $('btnPickKey').onclick = async () => {
    try {
      const r = await api('/api/pickfile', {
        method: 'POST',
        body: { title: '选择 SSH 私钥文件', filter: '私钥文件|id_*;*.pem;*.key|所有文件 (*.*)|*.*' },
      });
      if (r.path) $('f-keypath').value = r.path;
    } catch (err) {
      toast(err.message, 'bad');
    }
  };
  $('f-keylist').onchange = (e) => {
    if (e.target.value) $('f-keypath').value = e.target.value;
  };

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && $('overlay').classList.contains('show')) closeDialog();
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey) && $('overlay').classList.contains('show')) saveProfile(false);
  });
}

bind();
bootstrap().then(() => {
  // 支持通过地址栏 #new / #edit 直接打开配置弹窗
  if (location.hash === '#new') openDialog(null);
  if (location.hash === '#edit') openDialog(currentProfile());
});
