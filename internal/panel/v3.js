'use strict';
/* v3.js — panel-v3 新页面逻辑（在 app.js 之后加载，复用其 api / esc / toast / go / 渲染辅助）。
 *
 *   账号管理：插件风格账号卡片 + 备注（名称下方显示、铅笔就地编辑、可搜索）
 *   积分管理：每账号一行（默认折叠），展开看积分构成与逐包明细、积分变动
 *   用量管理：按 API Key 的用量（KPI / 每日堆叠图 / 按日 · 按模型明细）
 *   模型管理：启用 / 停用开关 + 测试模型
 *   密钥管理：多 API Key 创建（明文只显示一次）/ 改名 / 启停 / 删除
 *   任务管理：全部账号执行、每账号一行（成长任务 / 单项执行 / 任务记录）、运行历史
 *   调度管理：调度器状态与下一次执行时刻（设置表单在 app.js 的 cfgForm）
 *
 * app.js 经 v3('name') 调用这里的 v3_name 钩子。所有数据都来自 /panel/api/*（服务端校验管理密钥）。
 */
const V3 = {
  notes: {},            // uid → 备注（服务端）
  keys: [],             // API 密钥（不含明文）
  keyUsage7: null,      // 近 7 天按 key 汇总（密钥页列表用）
  ku: null,             // 用量页当前区间报表
  usTab: 'keys',
  last: {},             // uid → job → 最近一条任务记录
  records: [],          // 最近任务记录
  jobs: {},             // job → 展示名
  runs: [],
  active: {},
  open: { cr: new Set(), tk: new Set(), ku: new Set(), run: new Set() },
  packs: {},            // uid → 单账号积分包查询结果
  tasks: {},            // uid → 成长任务列表 / 状态
  credHist: {},         // uid → 积分变动
  editing: null,        // 正在编辑备注的 uid
  sched: null,
};

const PEN_SVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M10.8 2.8 13.2 5.2 5.6 12.8 2.6 13.4 3.2 10.4z"/><path d="m9.4 4.2 2.4 2.4"/></svg>';
const CHEV_SVG = '<svg class="chev" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="m6 3.5 4.5 4.5L6 12.5"/></svg>';
const JOB_ORDER = ['checkin', 'streak', 'travel', 'open_boxes', 'activity', 'intl_daily', 'blackcat', 'keepalive', 'growth', 'balance', 'compensation', 'task_sweep'];
const JOB_NAMES = {
  checkin: '每日签到', streak: '连登管家', travel: '猫猫旅行', open_boxes: '开 Buddy 盲盒', activity: '活跃上报',
  keepalive: 'Token 保活', blackcat: '夜猫子', intl_daily: '国际版每日活跃', growth: '成长任务', balance: '余额刷新',
  task_sweep: '任务补领', compensation: '官方补偿',
};
const OUTCOME_WORD = { done: '完成', claimed: '已领奖', skipped: '跳过', failed: '失败' };

function jobName(j) { return V3.jobs[j] || JOB_NAMES[j] || j; }
function noteOf(uid) { return V3.notes[uid] || remarkOf(uid) || ''; }
function fmtNum(n) { n = Number(n || 0); return Number.isInteger(n) ? n.toLocaleString('zh-CN') : n.toLocaleString('zh-CN', { maximumFractionDigits: 2 }); }
function fmtTime(iso) {
  const ms = parseAPITime(iso);
  if (!ms) return '—';
  const d = new Date(ms), now = new Date();
  const p = n => String(n).padStart(2, '0');
  const same = d.toDateString() === now.toDateString();
  return (same ? '' : p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ') + p(d.getHours()) + ':' + p(d.getMinutes());
}
function accMatch(s, q) {
  if (!q) return true;
  const text = [noteOf(s.uid), s.nickname, s.uid, s.realm === 'global' ? '国际版 global' : '国内版 cn'].join(' ').toLowerCase();
  return q.toLowerCase().split(/\s+/).filter(Boolean).every(k => text.includes(k));
}
function accounts() { return (overviewData && overviewData.accounts) || []; }
function realmTags(s) {
  return (s.realm === 'global' ? '<span class="realm-tag">国际版</span>' : '<span class="realm-tag">国内版</span>') +
    (s.enterprise ? '<span class="realm-tag">企业版</span>' : '');
}
function accState(s) {
  const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
  const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
  const cool = Math.max(s.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
  if (s.disabled) return { cls: 'off', key: 'disabled', tag: '<span class="tag bad">已禁用</span>', cool };
  if (s.paused) return { cls: 'off', key: 'paused', tag: '<span class="tag warn">已暂停选号</span>', cool };
  if (cool > 0) {
    const kind = bl > Math.max(s.cool_remaining_sec || 0, dg > 0 ? dg : 0) ? '熔断'
      : (dg > (s.cool_remaining_sec || 0) ? '连败降权' : (s.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却'));
    return { cls: 'cool', key: 'cool', tag: '<span class="tag warn">' + kind + ' · ' + dur(cool) + '</span>', cool };
  }
  return { cls: '', key: 'ok', tag: '<span class="tag ok">可用</span>', cool };
}
function creditOf(s) {
  const unlimited = s.credits_total === -1;
  const text = unlimited ? '不限' : (s.credits == null ? '—' : fmtNum(s.credits) + (s.credits_total > 0 ? '<span class="of"> / ' + fmtNum(s.credits_total) + '</span>' : ''));
  const pct = unlimited ? 100 : (s.credits_total > 0 ? Math.min(100, Math.round((s.credits || 0) / s.credits_total * 100)) : 0);
  return { text, pct, unlimited };
}
function remarkHtml(uid, editable) {
  const n = noteOf(uid);
  if (editable && V3.editing === uid) {
    return '<div class="remark" data-uid="' + esc(uid) + '"><input type="text" data-note-input="' + esc(uid) + '" value="' + esc(n) + '" maxlength="60" placeholder="输入备注，回车保存">' +
      '<button class="xs primary ok" data-note-save="' + esc(uid) + '">保存</button><button class="xs ghost no" data-note-cancel="1">取消</button></div>';
  }
  return '<div class="remark"><span class="rt' + (n ? '' : ' empty-r') + '" title="' + esc(n) + '">' + (n ? esc(n) : '添加备注') + '</span>' +
    (editable ? '<button class="pen" data-note-edit="' + esc(uid) + '" title="编辑备注" aria-label="编辑备注">' + PEN_SVG + '</button>' : '') + '</div>';
}

/* ── 备注（服务端）─────────────────────────────────────────────────── */
async function v3_loadNotes() {
  try {
    const d = await api('notes');
    V3.notes = d.notes || {};
    // 旧版本机 localStorage 备注（键为 uid 前 8 位）一次性迁移到服务端。
    let legacy = null;
    try { legacy = JSON.parse(localStorage.getItem(LS_REMARKS) || 'null'); } catch (e) { legacy = null; }
    if (legacy && typeof legacy === 'object' && Object.keys(legacy).length) {
      try {
        const r = await api('notes/import', { method: 'POST', body: JSON.stringify({ notes: legacy }) });
        V3.notes = r.notes || V3.notes;
        localStorage.removeItem(LS_REMARKS);
        if (r.imported) toast('已把本机旧备注迁移到服务端（' + r.imported + ' 条）', 'ok');
      } catch (e) { /* 下次再试 */ }
    }
    const map = {};
    for (const [uid, n] of Object.entries(V3.notes)) map[remarkKey(uid)] = n;
    storeRemarks(map);
  } catch (e) { /* 401 由封面处理 */ }
}
async function saveNote(uid, note) {
  await api('accounts/' + encodeURIComponent(uid) + '/note', { method: 'POST', body: JSON.stringify({ note }) });
  await v3_loadNotes();
}
function v3_rerender() {
  if (overviewData) renderAccounts(overviewData.accounts || []);
  if (view === 'credits') v3_renderCredits();
  if (view === 'tasks') renderTaskRows();
  if (view === 'usage' && usageData) renderUsage(usageData);
  if (view === 'usage' && V3.ku) renderKeyUsage();
}
document.addEventListener('click', async ev => {
  const t = ev.target;
  const edit = t.closest && t.closest('[data-note-edit]');
  if (edit) { ev.stopPropagation(); V3.editing = edit.dataset.noteEdit; v3_rerender(); focusNoteInput(); return; }
  const cancel = t.closest && t.closest('[data-note-cancel]');
  if (cancel) { ev.stopPropagation(); V3.editing = null; v3_rerender(); return; }
  const save = t.closest && t.closest('[data-note-save]');
  if (save) {
    ev.stopPropagation();
    const uid = save.dataset.noteSave;
    const inp = document.querySelector('[data-note-input="' + CSS.escape(uid) + '"]');
    await commitNote(uid, inp ? inp.value : '');
  }
}, true);
document.addEventListener('keydown', async ev => {
  const inp = ev.target;
  if (!inp || !inp.dataset || !inp.dataset.noteInput) return;
  if (ev.key === 'Enter') { ev.preventDefault(); await commitNote(inp.dataset.noteInput, inp.value); }
  if (ev.key === 'Escape') { V3.editing = null; v3_rerender(); }
});
function focusNoteInput() {
  setTimeout(() => {
    const el = document.querySelector('[data-note-input]');
    if (el) { el.focus(); el.select(); }
  }, 20);
}
async function commitNote(uid, value) {
  try {
    await saveNote(uid, String(value || '').trim());
    V3.editing = null;
    v3_rerender();
    toast('备注已保存', 'ok');
  } catch (e) { toast('保存备注失败：' + e.message, 'err'); }
}

/* ── 账号管理：卡片 ─────────────────────────────────────────────────── */
function v3_renderAccountCards(list) {
  const box = $('accBody');
  if (!box) return true;
  if (V3.editing && box.querySelector('[data-note-input]') && document.activeElement && document.activeElement.dataset && document.activeElement.dataset.noteInput) {
    return true; // 正在输入备注：跳过本轮轮询重绘，避免吞掉输入
  }
  if (!list.length) {
    box.innerHTML = '<div class="empty cards-empty"><div class="big">还没有账号</div>点击右上角「添加账号」，用浏览器登录 / API Key / 文件导入一个 WorkBuddy 账号</div>';
    return true;
  }
  const q = ($('accQ') && $('accQ').value.trim()) || '';
  const f = ($('accFilter') && $('accFilter').value) || '';
  const shown = list.filter(s => {
    if (!accMatch(s, q)) return false;
    const st = accState(s);
    if (f === 'cn' || f === 'global') return (s.realm === 'global' ? 'global' : 'cn') === f;
    return !f || st.key === f;
  });
  if ($('accNote')) $('accNote').textContent = (q || f ? '命中 ' + shown.length + ' / ' + list.length + ' 个账号' : list.length + ' 个账号') +
    (overviewData && overviewData.in_flight_full ? ' · ' + overviewData.in_flight_full + ' 个在途占满' : '');
  if (!shown.length) { box.innerHTML = '<div class="empty cards-empty">没有符合条件的账号</div>'; return true; }
  box.innerHTML = shown.map(s => {
    const st = accState(s);
    const cr = creditOf(s);
    const tu = s.token_usage || {};
    const errHigh = (s.err_total || 0) > 0 && (s.err_total || 0) / ((s.success_count || 0) + (s.err_total || 0)) > 0.1;
    const nick = s.nickname || '未命名';
    const initial = (noteOf(s.uid) || nick || '?').trim().slice(0, 1).toUpperCase();
    const frozen = s.disabled || st.cool > 0;
    return '<div class="acard ' + st.cls + '" title="uid: ' + esc(s.uid) + '">' +
      '<div class="top"><div class="avatar" aria-hidden="true">' + esc(initial) + '</div>' +
      '<div class="ident"><div class="nm"><span class="t">' + esc(nick) + '</span>' + realmTags(s) + '</div>' +
      remarkHtml(s.uid, true) +
      '<div class="uid">' + esc(s.uid) + '</div></div>' +
      '<div class="st">' + st.tag + '</div></div>' +
      (s.reason ? '<div class="reason">' + esc(s.reason) + '</div>' : '') +
      rateLimitRowsHtml(s.rate_limited_models, Date.now()) +
      '<div class="credit"><div class="row"><span class="n">' + cr.text + '</span><span class="lb">' + (s.enterprise ? '企业额度' : '积分') + (cr.unlimited ? '' : ' · ' + cr.pct + '%') + '</span></div>' +
      '<div class="bar"><i style="width:' + cr.pct + '%"></i></div></div>' +
      '<div class="kv">' +
        '<div><div class="k">成功 / 失败</div><div class="v">' + (s.success_count || 0) + '<span class="slash">/</span><span class="err-n' + (errHigh ? ' has' : '') + '">' + (s.err_total || 0) + '</span></div></div>' +
        '<div><div class="k">在途 · 请求</div><div class="v">' + (s.in_flight || 0) + ' · ' + (tu.request_count || 0) + '</div></div>' +
        '<div><div class="k">最近成功</div><div class="v">' + ago(s.last_success) + '</div></div>' +
        '<div><div class="k">Token</div><div class="v">' + formatTokenCount(tu.total_tokens) + '</div></div>' +
        '<div><div class="k">延迟</div><div class="v">' + formatLatency(tu.last_latency_ms) + '</div></div>' +
        '<div><div class="k">签到</div><div class="v">' + (s.enterprise ? '—' : (s.checkin_done ? '今日已签' : '未签')) + '</div></div>' +
      '</div>' +
      '<div class="acts-wrap">' + accActsHtml(s, frozen) + '</div>' +
      '</div>';
  }).join('');
  if (V3.editing) focusNoteInput();
  return true;
}
if ($('accQ')) $('accQ').oninput = () => overviewData && renderAccounts(overviewData.accounts || []);
if ($('accFilter')) $('accFilter').onchange = () => overviewData && renderAccounts(overviewData.accounts || []);

function v3_afterOverview(d) {
  if (view === 'credits') v3_renderCredits();
  if (view === 'tasks') renderTaskRows();
}

/* ── 「全部账号执行」────────────────────────────────────────────────── */
document.addEventListener('click', async ev => {
  const b = ev.target.closest && ev.target.closest('[data-job-all]');
  if (!b) return;
  const job = b.dataset.jobAll;
  const name = jobName(job);
  if (job === 'blackcat' && !confirm('夜猫子只在 23:00–08:00 计数，窗口外执行会直接跳过。继续？')) return;
  b.disabled = true;
  try {
    await api('jobs/' + encodeURIComponent(job) + '/run_all', { method: 'POST' });
    toast(name + '：已开始对全部账号执行，结果见任务管理 → 运行历史', 'ok');
    setTimeout(() => { if (view === 'tasks') v3_loadTasksView(); if (job === 'balance') loadOverview(true); }, 2500);
  } catch (e) { toast(name + '：' + e.message, 'err'); }
  finally { setTimeout(() => { b.disabled = false; }, 1200); }
});

/* ── 积分管理：每账号一行 ───────────────────────────────────────────── */
function packsFor(uid) {
  if (V3.packs[uid]) return V3.packs[uid];
  const all = lastPackages && lastPackages.accounts;
  return all ? all.find(a => a.uid === uid) : null;
}
function v3_renderCredits() {
  const list = accounts();
  const remain = list.reduce((a, s) => a + (s.credits_total === -1 ? 0 : (s.credits || 0)), 0);
  const total = list.reduce((a, s) => a + (s.credits_total > 0 ? s.credits_total : 0), 0);
  const expiring = list.reduce((a, s) => a + (s.credits_expiring || 0), 0);
  let packN = 0;
  for (const s of list) { const p = packsFor(s.uid); if (p && p.packages) packN += p.packages.length; }
  const set = (id, v) => { if ($(id)) $(id).textContent = v; };
  set('crRemain', fmtNum(remain)); set('crTotal', total ? fmtNum(total) : '—');
  set('crExpiring', expiring ? fmtNum(expiring) : '0');
  set('crAccounts', list.filter(s => (s.credits || 0) > 0 || s.credits_total === -1).length + ' / ' + list.length);
  set('crPacks', packN ? String(packN) : '—');
  const box = $('crRows');
  if (!box) return;
  if (box.querySelector('[data-note-input]') && document.activeElement && document.activeElement.dataset && document.activeElement.dataset.noteInput) return;
  const q = ($('crQ') && $('crQ').value.trim()) || '';
  const shown = list.filter(s => accMatch(s, q));
  if ($('crNote')) $('crNote').textContent = q ? '命中 ' + shown.length + ' / ' + list.length : list.length + ' 个账号';
  if (!list.length) { box.innerHTML = '<div class="empty">还没有账号</div>'; return; }
  if (!shown.length) { box.innerHTML = '<div class="empty">没有符合条件的账号</div>'; return; }
  box.innerHTML = shown.map(s => {
    const cr = creditOf(s);
    const open = V3.open.cr.has(s.uid);
    const earliest = parseAPITime(s.credits_earliest_expiry);
    const pk = packsFor(s.uid);
    return '<div class="xrow' + (open ? ' open' : '') + '" data-xr="cr" data-uid="' + esc(s.uid) + '">' +
      '<div class="xhead" data-xt="cr">' + CHEV_SVG +
      '<div class="who"><div class="nm">' + esc(s.nickname || '未命名') + realmTags(s) + '</div>' + remarkHtml(s.uid, true) + '</div>' +
      '<div class="mid">' +
        '<div class="m meter"><span class="k">剩余 / 总额</span><span class="v">' + cr.text + '</span><div class="bar"><i style="width:' + cr.pct + '%"></i></div></div>' +
        '<div class="m"><span class="k">快过期</span><span class="v">' + (s.credits_expiring ? fmtNum(s.credits_expiring) : '0') + '</span></div>' +
        '<div class="m"><span class="k">最近到期</span><span class="v">' + (earliest ? fmtLocalDateTime(earliest).slice(5, 10) + ' · ' + fmtNum(s.credits_earliest_remaining || 0) : '—') + '</span></div>' +
        '<div class="m"><span class="k">积分包</span><span class="v">' + (pk && pk.packages ? pk.packages.length + ' 个' : (pk && pk.error ? '查询失败' : '未查询')) + '</span></div>' +
      '</div>' +
      '<div class="right">' +
        (s.enterprise ? '' : '<button class="xs ghost" data-cr-act="checkin" data-u="' + esc(s.uid) + '">' + (s.checkin_done ? '已签' : '签到') + '</button>') +
        '<button class="xs ghost" data-cr-act="balance" data-u="' + esc(s.uid) + '">刷新余额</button>' +
      '</div></div>' +
      '<div class="xbody">' + (open ? creditBodyHtml(s) : '') + '</div></div>';
  }).join('');
}
function creditBodyHtml(s) {
  const pk = packsFor(s.uid);
  let h = '';
  if (!pk || pk.loading) {
    h += '<div class="muted-pad">' + (pk && pk.loading ? '<span class="dots">正在向上游查询积分包</span>' : '还没有查询积分包。<button class="xs" data-cr-act="packs" data-u="' + esc(s.uid) + '">查询该账号积分构成</button>') + '</div>';
  } else if (pk.error) {
    h += '<div class="muted-pad" style="color:var(--bad)">积分包查询失败：' + esc(pk.error) + ' <button class="xs" data-cr-act="packs" data-u="' + esc(s.uid) + '">重试</button></div>';
  } else {
    const packs = pk.packages || [];
    const src = pkBySource(packs);
    const sum = src.reduce((a, e) => a + e.remain, 0) || 1;
    h += '<div class="sub-h">积分构成<span class="grow"></span><span class="note">' + packs.length + ' 个包 · 剩余 ' + fmtNum(pk.remain) + ' / 面额 ' + fmtNum(pk.size) + '</span>' +
      '<button class="xs ghost" data-cr-act="packs" data-u="' + esc(s.uid) + '">重新查询</button></div>';
    h += '<div class="comp-grid"><div><div class="mixbar">' + src.filter(e => e.remain > 0).map((e, i) =>
      '<i style="width:' + (e.remain / sum * 100).toFixed(2) + '%;background:' + pkColor(i) + '" title="' + esc(e.name) + ' ' + fmtNum(e.remain) + '"></i>').join('') + '</div>' +
      '<div class="comp-src">' + src.map((e, i) => '<div class="r"><i style="background:' + pkColor(i) + '"></i><span title="' + esc(e.name) + '">' + esc(e.name) + (e.n > 1 ? ' ×' + e.n : '') + '</span><b>' + fmtNum(e.remain) + ' / ' + fmtNum(e.size) + '</b></div>').join('') + '</div></div>';
    const g = pkDetailGroups(packs, 50);
    const rows = g.visible.concat(g.used.slice(0, 5));
    h += '<div class="tbl-wrap"><table class="acc"><thead><tr><th>积分包</th><th class="num">剩余</th><th class="num">面额</th><th>到期</th></tr></thead><tbody>' +
      (rows.length ? rows.map(p => {
        const exp = pkExpiryMs(p);
        return '<tr><td><div style="font-weight:500">' + esc(p.name || '(未命名)') + '</div><div class="muted" style="font-size:11.5px">' + esc(p.package_code || '') + '</div></td>' +
          '<td class="num">' + fmtNum(p.remain) + '</td><td class="num">' + fmtNum(p.size) + '</td>' +
          '<td>' + (exp ? esc(pkExpiryDateTime(exp)) + '<div class="muted" style="font-size:11.5px">' + esc(pkExpiryText(exp)) + '</div>' : '—') + '</td></tr>';
      }).join('') : '<tr><td colspan="4"><div class="empty">没有积分包</div></td></tr>') +
      '</tbody></table></div></div>';
  }
  const ch = V3.credHist[s.uid];
  h += '<div class="sub-h">积分变动<span class="grow"></span><span class="note">每次查到余额与上次比对</span></div>';
  if (!ch) h += '<div class="muted-pad"><span class="dots">读取中</span></div>';
  else if (!ch.length) h += '<div class="muted-pad">暂无变动记录</div>';
  else h += '<div class="rec-list">' + ch.slice(0, 12).map(e => '<div class="rec"><span class="t">' + esc(fmtTime(e.time)) + '</span>' +
    '<span class="j"><span class="cdelta ' + (e.delta >= 0 ? 'up' : 'down') + '">' + (e.delta >= 0 ? '+' : '') + fmtNum(e.delta) + '</span></span>' +
    '<span class="m">余额 ' + fmtNum(e.before) + ' → ' + fmtNum(e.after) + '</span><span></span></div>').join('') + '</div>';
  return h;
}
async function loadCreditHist(uid) {
  try {
    const d = await api('credit_history?uid=' + encodeURIComponent(uid) + '&limit=30');
    V3.credHist[uid] = d.entries || [];
  } catch (e) { V3.credHist[uid] = []; }
  if (view === 'credits') v3_renderCredits();
}
async function loadPacksFor(uid) {
  V3.packs[uid] = { loading: true };
  v3_renderCredits();
  try {
    const d = await api('packages?uid=' + encodeURIComponent(uid));
    V3.packs[uid] = (d.accounts || [])[0] || { error: '账号不存在' };
  } catch (e) { V3.packs[uid] = { error: e.message }; }
  v3_renderCredits();
}
document.addEventListener('click', async ev => {
  const t = ev.target;
  const act = t.closest && t.closest('[data-cr-act]');
  if (act) {
    ev.stopPropagation();
    const uid = act.dataset.u, a = act.dataset.crAct;
    if (a === 'packs') { loadPacksFor(uid); return; }
    act.disabled = true;
    try {
      const r = await api('accounts/' + encodeURIComponent(uid) + '/' + a, { method: 'POST' });
      toast(a === 'checkin' ? '签到完成' + (r.checkin_message ? '（' + r.checkin_message + '）' : '') : '余额已更新：' + r.credits, 'ok');
      await loadOverview(true);
    } catch (e) { toast(e.message, 'err'); }
    finally { act.disabled = false; }
    return;
  }
  const head = t.closest && t.closest('.xhead[data-xt]');
  if (!head || t.closest('button, input, a, label, details')) return;
  const row = head.closest('.xrow');
  const kind = head.dataset.xt, uid = row.dataset.uid;
  const set = V3.open[kind];
  if (!set) return;
  if (set.has(uid)) set.delete(uid); else set.add(uid);
  if (kind === 'cr') {
    if (set.has(uid)) { if (!V3.credHist[uid]) loadCreditHist(uid); }
    v3_renderCredits();
  } else if (kind === 'tk') {
    if (set.has(uid) && !V3.tasks[uid]) loadAccountTasks(uid);
    renderTaskRows();
  } else if (kind === 'ku') renderKeyUsage();
  else if (kind === 'run') { if (set.has(uid)) loadRunDetail(uid); renderRuns(); }
});
if ($('crQ')) $('crQ').oninput = v3_renderCredits;
if ($('btnCrFetch')) $('btnCrFetch').onclick = async () => {
  const b = $('btnCrFetch');
  b.disabled = true; b.textContent = '查询中…';
  try { await loadPackages(true); V3.packs = {}; v3_renderCredits(); toast('积分构成已更新', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '查询积分构成'; }
};

/* ── 密钥管理 ─────────────────────────────────────────────────────── */
async function v3_loadKeys() {
  try {
    const [d, u] = await Promise.all([api('keys'), api('keys/usage?days=7').catch(() => null)]);
    V3.keys = d.keys || [];
    V3.keyUsage7 = u;
    renderKeys(d);
  } catch (e) { if ($('keyBody')) $('keyBody').innerHTML = '<tr><td colspan="8"><div class="empty">' + esc(e.message) + '</div></td></tr>'; }
}
function usageOfKey(rep, id) { return ((rep && rep.keys) || []).find(k => k.id === id) || { total: {} }; }
function renderKeys(d) {
  const enabled = V3.keys.filter(k => k.enabled).length;
  const legacyU = usageOfKey(V3.keyUsage7, 'legacy').total;
  $('kInfo').innerHTML =
    '<div class="it"><div class="k">鉴权状态</div><div class="v">' + (d.auth_enabled ? '已开启' : '未开启（任何人可调用）') + '</div><div class="s">' + (d.auth_enabled ? 'api_key 或任一启用中的密钥可调用' : '设置 api_key 或创建一个密钥即开启') + '</div></div>' +
    '<div class="it"><div class="k">配置文件 api_key</div><div class="v">' + (d.legacy_configured ? esc(d.legacy_display) : '未设置') + '</div><div class="s">近 7 天 ' + fmtNum(legacyU.requests || 0) + ' 次请求 · 在「调度管理 → 服务与密钥」修改</div></div>' +
    '<div class="it"><div class="k">API 密钥</div><div class="v">' + V3.keys.length + ' 个 · ' + enabled + ' 个启用</div><div class="s">明文只在创建时显示一次，服务端只存摘要</div></div>';
  $('kAuthNote').textContent = '';
  const tb = $('keyBody');
  if (!V3.keys.length) {
    tb.innerHTML = '<tr><td colspan="8"><div class="empty"><div class="big">还没有创建密钥</div>在右上角输入名称并点击「创建密钥」，给不同客户端分配独立密钥，便于分别统计与停用</div></td></tr>';
    return;
  }
  tb.innerHTML = V3.keys.map(k => {
    const u = usageOfKey(V3.keyUsage7, k.id).total || {};
    const editing = V3.editingKey === k.id;
    const name = editing
      ? '<div class="kname"><input type="text" data-key-name="' + esc(k.id) + '" value="' + esc(k.name) + '" maxlength="40" style="height:26px"><button class="xs primary" data-key-act="rename-save" data-id="' + esc(k.id) + '">保存</button></div>'
      : '<div class="kname"><span class="t">' + esc(k.name) + '</span><button class="pen" data-key-act="rename" data-id="' + esc(k.id) + '" title="改名">' + PEN_SVG + '</button></div>';
    return '<tr' + (k.enabled ? '' : ' class="off"') + '><td>' + name + '<div class="muted" style="font:11px var(--mono)">' + esc(k.id) + '</div></td>' +
      '<td><span class="kdisp">' + esc(k.display) + '</span></td>' +
      '<td><span class="sw-cell"><label class="switch inline-sw" title="' + (k.enabled ? '启用中' : '已停用') + '"><input type="checkbox" data-key-enable="' + esc(k.id) + '"' + (k.enabled ? ' checked' : '') + '></label><span class="sw-l' + (k.enabled ? '' : ' off') + '">' + (k.enabled ? '启用' : '停用') + '</span></span></td>' +
      '<td class="num">' + fmtNum(u.requests || 0) + (u.failed ? '<span class="muted"> · 失败 ' + fmtNum(u.failed) + '</span>' : '') + '</td>' +
      '<td class="num">' + formatTokenCount(u.total_tokens || 0) + '</td>' +
      '<td class="num">' + esc(fmtTime(k.created_at)) + '</td>' +
      '<td class="num">' + (parseAPITime(k.last_used_at) ? ago(k.last_used_at) : '从未使用') + '</td>' +
      '<td class="acts"><div class="acts-in"><button class="xs ghost danger" data-key-act="delete" data-id="' + esc(k.id) + '">删除</button></div></td></tr>';
  }).join('');
  if (V3.editingKey) setTimeout(() => { const el = document.querySelector('[data-key-name]'); if (el) { el.focus(); el.select(); } }, 20);
}
if ($('btnKeyCreate')) $('btnKeyCreate').onclick = async () => {
  const name = $('kNewName').value.trim();
  const b = $('btnKeyCreate');
  b.disabled = true;
  try {
    const r = await api('keys', { method: 'POST', body: JSON.stringify({ name }) });
    $('kNewName').value = '';
    $('secretText').textContent = r.secret;
    $('secretVeil').classList.add('on');
    await v3_loadKeys();
  } catch (e) { toast('创建失败：' + e.message, 'err'); }
  finally { b.disabled = false; }
};
if ($('kNewName')) $('kNewName').addEventListener('keydown', e => { if (e.key === 'Enter') $('btnKeyCreate').click(); });
if ($('btnSecretCopy')) $('btnSecretCopy').onclick = () => copyText($('secretText').textContent);
if ($('btnSecretClose')) $('btnSecretClose').onclick = () => { $('secretVeil').classList.remove('on'); $('secretText').textContent = ''; };
async function renameKey(id, name) {
  try {
    await api('keys/' + encodeURIComponent(id) + '/rename', { method: 'POST', body: JSON.stringify({ name }) });
    V3.editingKey = null;
    await v3_loadKeys();
    toast('已改名', 'ok');
  } catch (e) { toast('改名失败：' + e.message, 'err'); }
}
document.addEventListener('click', async ev => {
  const b = ev.target.closest && ev.target.closest('[data-key-act]');
  if (!b) return;
  const id = b.dataset.id, a = b.dataset.keyAct;
  if (a === 'rename') { V3.editingKey = id; renderKeys(await api('keys')); return; }
  if (a === 'rename-save') { const el = document.querySelector('[data-key-name]'); await renameKey(id, el ? el.value : ''); return; }
  if (a === 'delete') {
    const k = V3.keys.find(x => x.id === id);
    if (!confirm('删除密钥「' + (k ? k.name : id) + '」？使用它的客户端会立即 401，历史用量保留。')) return;
    try { await api('keys/' + encodeURIComponent(id) + '/delete', { method: 'POST' }); toast('已删除', 'ok'); await v3_loadKeys(); }
    catch (e) { toast('删除失败：' + e.message, 'err'); }
  }
});
document.addEventListener('keydown', ev => {
  const el = ev.target;
  if (el && el.dataset && el.dataset.keyName && ev.key === 'Enter') { ev.preventDefault(); renameKey(el.dataset.keyName, el.value); }
  if (el && el.dataset && el.dataset.keyName && ev.key === 'Escape') { V3.editingKey = null; v3_loadKeys(); }
});
document.addEventListener('change', async ev => {
  const el = ev.target;
  if (el && el.dataset && el.dataset.keyEnable) {
    try {
      await api('keys/' + encodeURIComponent(el.dataset.keyEnable) + '/enabled', { method: 'POST', body: JSON.stringify({ enabled: el.checked }) });
      toast(el.checked ? '已启用' : '已停用（使用它的请求将 401）', 'ok');
    } catch (e) { toast(e.message, 'err'); el.checked = !el.checked; }
    v3_loadKeys();
  }
});

/* ── 用量管理：按密钥 ───────────────────────────────────────────────── */
const KU_COLORS = ['#4a6484', '#c49a52', '#3f8f7d', '#b8705f', '#8a7bb5', '#7d8b9c', '#8a9a5b', '#c27aa0'];
function v3_loadUsageView() { setUsTab(V3.usTab); }
function setUsTab(tab) {
  V3.usTab = tab;
  document.querySelectorAll('#usTabs button').forEach(b => b.classList.toggle('on', b.dataset.ut === tab));
  $('usKeysPane').hidden = tab !== 'keys';
  $('usPoolPane').hidden = tab !== 'pool';
  $('kuDays').hidden = tab !== 'keys';
  $('btnKu').hidden = tab !== 'keys';
  $('kuNote').hidden = tab !== 'keys';
  if (tab === 'keys') loadKeyUsage(); else loadUsage();
}
document.querySelectorAll('#usTabs button').forEach(b => { b.onclick = () => setUsTab(b.dataset.ut); });
if ($('kuDays')) $('kuDays').onchange = loadKeyUsage;
if ($('btnKu')) $('btnKu').onclick = loadKeyUsage;
async function loadKeyUsage() {
  const days = ($('kuDays') && $('kuDays').value) || 7;
  try {
    V3.ku = await api('keys/usage?days=' + days);
    renderKeyUsage();
  } catch (e) { $('kuRows').innerHTML = '<div class="empty">' + esc(e.message) + '</div>'; }
}
function renderKeyUsage() {
  const r = V3.ku;
  if (!r) return;
  const t = r.total || {};
  const rate = t.requests ? (t.success / t.requests * 100) : 0;
  const active = (r.keys || []).filter(k => (k.total || {}).requests > 0).length;
  $('kuNote').textContent = r.from + ' ~ ' + r.to;
  $('kuSub').textContent = (r.keys || []).length + ' 个密钥 · ' + active + ' 个有调用';
  $('kuStats').innerHTML =
    usKpi(fmtNum(t.requests || 0), '请求数', '', '成功 ' + fmtNum(t.success || 0) + ' · 失败 ' + fmtNum(t.failed || 0)) +
    usKpi(t.requests ? rate.toFixed(1) + '%' : '—', '成功率', t.requests && rate < 90 ? 'c-warn' : '', '') +
    usKpi(formatTokenCount(t.total_tokens || 0), '合计 Token', '', '输入 ' + formatTokenCount(t.prompt_tokens || 0) + ' · 输出 ' + formatTokenCount(t.completion_tokens || 0)) +
    usKpi(fmtCredit(t.credits || 0), '积分消耗', '', '') +
    usKpi(String(active), '活跃密钥', '', '共 ' + (r.keys || []).length + ' 个') +
    usKpi(String((r.days || []).filter(d => (r.keys || []).some(k => (((k.by_day || {})[d] || {}).requests || 0) > 0)).length), '有调用的天数', '', '区间共 ' + ((r.days || []).length) + ' 天');
  // 每日堆叠柱（按密钥）
  const days = [];
  { // 区间内全部日期（含无调用日）
    const from = new Date(r.from + 'T00:00:00'), to = new Date(r.to + 'T00:00:00');
    for (let d = new Date(from); d <= to && days.length < 120; d.setDate(d.getDate() + 1)) {
      const p = n => String(n).padStart(2, '0');
      days.push(d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()));
    }
  }
  const keys = (r.keys || []).filter(k => (k.total || {}).requests > 0);
  const color = i => KU_COLORS[i % KU_COLORS.length];
  $('kuLegend').innerHTML = keys.slice(0, 6).map((k, i) => '<i class="sw" style="background:' + color(i) + '"></i>' + esc(k.name)).join(' ');
  if (!keys.length) {
    $('kuChart').innerHTML = '<div class="us-empty">区间内没有调用记录</div>';
  } else {
    const W = Math.max(480, Math.min(1600, ($('kuChart') && $('kuChart').clientWidth) || 960)), H = 220, L = 40, R = 10, T = 12, B = 26;
    const max = Math.max(1, ...days.map(d => keys.reduce((a, k) => a + ((k.by_day || {})[d] || {}).requests || 0, 0)));
    const bw = (W - L - R) / days.length;
    let svg = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" aria-label="每日请求">';
    for (let i = 0; i <= 4; i++) {
      const y = T + (H - T - B) * i / 4;
      svg += '<line class="gl" x1="' + L + '" x2="' + (W - R) + '" y1="' + y + '" y2="' + y + '"/>' +
        '<text class="tk" x="' + (L - 6) + '" y="' + (y + 3) + '" text-anchor="end">' + Math.round(max * (4 - i) / 4) + '</text>';
    }
    days.forEach((d, i) => {
      let y = H - B;
      const x = L + i * bw + Math.max(1, bw * 0.18);
      const w = Math.max(2, bw * 0.64);
      const parts = [];
      keys.forEach((k, ki) => {
        const v = ((k.by_day || {})[d] || {}).requests || 0;
        if (!v) return;
        const h = (H - T - B) * v / max;
        y -= h;
        parts.push('<rect x="' + x.toFixed(1) + '" y="' + y.toFixed(1) + '" width="' + w.toFixed(1) + '" height="' + h.toFixed(1) + '" rx="2" fill="' + color(ki) + '"/>');
      });
      const tip = d + '\n' + keys.map(k => k.name + '：' + (((k.by_day || {})[d] || {}).requests || 0)).join('\n');
      svg += '<g><title>' + esc(tip) + '</title>' + parts.join('') + '<rect x="' + (L + i * bw).toFixed(1) + '" y="' + T + '" width="' + bw.toFixed(1) + '" height="' + (H - T - B) + '" fill="transparent"/></g>';
      const every = Math.ceil(days.length / 10);
      if (i % every === 0) svg += '<text class="tk" x="' + (L + i * bw + bw / 2).toFixed(1) + '" y="' + (H - 8) + '" text-anchor="middle">' + d.slice(5) + '</text>';
    });
    svg += '<line class="ax" x1="' + L + '" x2="' + (W - R) + '" y1="' + (H - B) + '" y2="' + (H - B) + '"/></svg>';
    $('kuChart').innerHTML = svg;
  }
  const rows = r.keys || [];
  if (!rows.length) { $('kuRows').innerHTML = '<div class="empty">还没有密钥与调用记录：在「密钥管理」创建密钥后，调用会在这里按密钥统计</div>'; return; }
  const maxReq = Math.max(1, ...rows.map(k => (k.total || {}).requests || 0));
  $('kuRows').innerHTML = rows.map((k, i) => {
    const tt = k.total || {};
    const open = V3.open.ku.has(k.id);
    const ci = keys.indexOf(k);
    return '<div class="xrow' + (open ? ' open' : '') + '" data-uid="' + esc(k.id) + '">' +
      '<div class="xhead" data-xt="ku">' + CHEV_SVG +
      '<div class="who"><div class="nm">' + (ci >= 0 ? '<i class="sw" style="width:8px;height:8px;border-radius:2px;display:inline-block;background:' + color(ci) + '"></i>' : '') + esc(k.name) + '</div><div class="rmk" style="font:11px var(--mono);color:var(--ink-3)">' + esc(k.id) + '</div></div>' +
      '<div class="mid">' +
        '<div class="m meter"><span class="k">请求</span><span class="v">' + fmtNum(tt.requests || 0) + '</span><div class="bar"><i style="width:' + Math.round((tt.requests || 0) / maxReq * 100) + '%"></i></div></div>' +
        '<div class="m"><span class="k">成功 / 失败</span><span class="v">' + fmtNum(tt.success || 0) + ' / <span class="err-n' + (tt.failed ? ' has' : '') + '">' + fmtNum(tt.failed || 0) + '</span></span></div>' +
        '<div class="m"><span class="k">Token</span><span class="v">' + formatTokenCount(tt.total_tokens || 0) + '</span></div>' +
        '<div class="m"><span class="k">积分</span><span class="v">' + fmtCredit(tt.credits || 0) + '</span></div>' +
      '</div><div class="right"></div></div>' +
      '<div class="xbody">' + (open ? keyUsageBody(k) : '') + '</div></div>';
  }).join('');
}
function bucketRow(name, b) {
  return '<tr><td>' + esc(name) + '</td><td class="num">' + fmtNum(b.requests || 0) + '</td><td class="num">' + fmtNum(b.success || 0) + '</td><td class="num"><span class="err-n' + (b.failed ? ' has' : '') + '">' + fmtNum(b.failed || 0) + '</span></td>' +
    '<td class="num">' + formatTokenCount(b.prompt_tokens || 0) + '</td><td class="num">' + formatTokenCount(b.completion_tokens || 0) + '</td><td class="num">' + formatTokenCount(b.total_tokens || 0) + '</td><td class="num">' + fmtCredit(b.credits || 0) + '</td></tr>';
}
function keyUsageBody(k) {
  const head = '<thead><tr><th>%s</th><th class="num">请求</th><th class="num">成功</th><th class="num">失败</th><th class="num">Prompt</th><th class="num">Completion</th><th class="num">合计 Token</th><th class="num">积分</th></tr></thead>';
  const byModel = Object.entries(k.by_model || {}).sort((a, b) => (b[1].requests || 0) - (a[1].requests || 0));
  const byDay = Object.entries(k.by_day || {}).sort((a, b) => a[0] < b[0] ? 1 : -1);
  if (!byModel.length) return '<div class="muted-pad">区间内没有调用</div>';
  return '<div class="sub-h">按模型</div><div class="tbl-wrap"><table class="acc">' + head.replace('%s', '模型') + '<tbody>' + byModel.map(([m, b]) => bucketRow(m, b)).join('') + '</tbody></table></div>' +
    '<div class="sub-h">按日</div><div class="tbl-wrap"><table class="acc">' + head.replace('%s', '日期') + '<tbody>' + byDay.map(([d, b]) => bucketRow(d, b)).join('') + '</tbody></table></div>';
}

/* ── 模型管理：启停 + 测试 ─────────────────────────────────────────── */
document.addEventListener('change', async ev => {
  const el = ev.target;
  if (!el || !el.dataset || !el.dataset.mdToggle) return;
  const id = el.dataset.mdToggle, on = el.checked;
  el.disabled = true;
  try {
    await api('models/toggle', { method: 'POST', body: JSON.stringify({ id, enabled: on }) });
    const m = mdAll.find(x => x.id === id);
    if (m) m.disabled = !on;
    toast(on ? id + ' 已启用' : id + ' 已停用：不再出现在 /v1/models，调用返回 model_disabled', 'ok');
  } catch (e) { el.checked = !on; toast('切换失败：' + e.message, 'err'); }
  finally { el.disabled = false; renderModels(); }
});
async function testModel(id) {
  mdTests[id] = { running: true };
  renderModels();
  try {
    mdTests[id] = await api('models/test', { method: 'POST', body: JSON.stringify({ model: id }) });
  } catch (e) { mdTests[id] = { ok: false, error: e.message }; }
  renderModels();
  return mdTests[id];
}
document.addEventListener('click', async ev => {
  const b = ev.target.closest && ev.target.closest('[data-md-test]');
  if (!b) return;
  const r = await testModel(b.dataset.mdTest);
  toast(r.ok ? b.dataset.mdTest + ' 正常 · ' + r.latency_ms + ' ms' : b.dataset.mdTest + ' 失败：' + (r.error || ''), r.ok ? 'ok' : 'err');
});
if ($('btnMdTestAll')) $('btnMdTestAll').onclick = async () => {
  const list = mdAll.filter(m => mdMatch(m));
  if (!list.length) return;
  if (list.length > 8 && !confirm('将对 ' + list.length + ' 个模型逐个发送极短请求（会消耗少量积分），继续？')) return;
  const b = $('btnMdTestAll');
  b.disabled = true;
  let ok = 0;
  for (const m of list) { const r = await testModel(m.id); if (r.ok) ok++; }
  b.disabled = false;
  toast('测试完成：' + ok + ' / ' + list.length + ' 个正常', ok === list.length ? 'ok' : 'err');
};

/* ── 任务管理 ─────────────────────────────────────────────────────── */
async function v3_loadTasksView() {
  try {
    const [r, runs] = await Promise.all([api('task_records?limit=500'), api('runs?limit=60')]);
    V3.records = r.records || [];
    V3.last = r.last || {};
    V3.jobs = Object.assign({}, JOB_NAMES, r.jobs || {});
    V3.runs = runs.runs || [];
    V3.active = runs.active || {};
  } catch (e) { /* 401 → 封面 */ }
  renderTaskRows();
  renderRuns();
}
let tkPollN = 0;
function v3_pollTasks() {
  if (++tkPollN % 3 === 0) v3_loadTasksView(); // 每 15 秒刷新一次记录
}
function lastChips(uid) {
  const m = V3.last[uid] || {};
  const jobs = JOB_ORDER.filter(j => m[j]);
  if (!jobs.length) return '<span class="muted" style="font-size:12px">暂无任务记录</span>';
  return '<div class="lastbadges">' + jobs.slice(0, 6).map(j => {
    const r = m[j];
    const cls = r.outcome === 'skipped' ? 'skip' : (r.ok ? 'ok' : 'bad');
    return '<span class="lb-chip ' + cls + '" title="' + esc(fmtTime(r.at) + ' ' + jobName(j) + '：' + (r.message || '')) + '">' + esc(jobName(j)) + ' · ' + esc(fmtTime(r.at)) + '</span>';
  }).join('') + '</div>';
}
function renderTaskRows() {
  const box = $('tkRows');
  if (!box) return;
  if (box.querySelector('[data-note-input]') && document.activeElement && document.activeElement.dataset && document.activeElement.dataset.noteInput) return;
  const list = accounts();
  const q = ($('tkQ') && $('tkQ').value.trim()) || '';
  const shown = list.filter(s => accMatch(s, q));
  if ($('tkNote')) $('tkNote').textContent = q ? '命中 ' + shown.length + ' / ' + list.length : list.length + ' 个账号';
  const act = Object.keys(V3.active || {}).map(jobName);
  if ($('jobActive')) $('jobActive').textContent = act.length ? '执行中：' + act.join('、') : '';
  if (!list.length) { box.innerHTML = '<div class="empty">还没有账号</div>'; return; }
  if (!shown.length) { box.innerHTML = '<div class="empty">没有符合条件的账号</div>'; return; }
  box.innerHTML = shown.map(s => {
    const open = V3.open.tk.has(s.uid);
    return '<div class="xrow' + (open ? ' open' : '') + '" data-uid="' + esc(s.uid) + '">' +
      '<div class="xhead" data-xt="tk">' + CHEV_SVG +
      '<div class="who"><div class="nm">' + esc(s.nickname || '未命名') + realmTags(s) + '</div>' + remarkHtml(s.uid, true) + '</div>' +
      '<div class="mid">' + lastChips(s.uid) + '</div>' +
      '<div class="right">' + accState(s).tag + '</div></div>' +
      '<div class="xbody">' + (open ? taskBodyHtml(s) : '') + '</div></div>';
  }).join('');
}
function jobsFor(s) {
  const cn = s.realm !== 'global', personal = !s.enterprise;
  const out = [];
  if (cn && personal) out.push('checkin', 'streak', 'travel', 'open_boxes');
  if (personal) out.push('activity');
  if (!cn) out.push('intl_daily');
  out.push('keepalive');
  return out;
}
function taskBodyHtml(s) {
  const uid = s.uid;
  let h = '<div class="sub-h">立即执行（仅该账号）</div><div class="jobbar">' +
    jobsFor(s).map(j => '<button class="xs" data-run-job="' + j + '" data-u="' + esc(uid) + '">' + esc(jobName(j)) + '</button>').join('') +
    (s.enterprise ? '<span class="muted" style="font-size:12px">企业版无个人成长体系，只能保活 / 刷新额度</span>' : '') + '</div>';
  if (s.realm !== 'global' && !s.enterprise) {
    const t = V3.tasks[uid];
    h += '<div class="sub-h">成长任务<span class="grow"></span>' +
      '<button class="xs ghost" data-tk-act="reload" data-u="' + esc(uid) + '">重新查询</button>' +
      '<button class="xs" data-tk-act="accept_all" data-u="' + esc(uid) + '">全部接受</button>' +
      '<button class="xs primary" data-tk-act="auto_all" data-u="' + esc(uid) + '">一键完成可自动任务</button></div>';
    if (!t || t.loading) h += '<div class="muted-pad"><span class="dots">查询中</span></div>';
    else if (t.error) h += '<div class="muted-pad" style="color:var(--bad)">' + esc(t.error) + '</div>';
    else if (!t.list.length) h += '<div class="muted-pad">该账号暂无任务</div>';
    else h += '<div class="tbl-wrap"><table class="acc md"><thead><tr><th class="mark" aria-hidden="true"></th><th>任务</th><th>进度</th><th>奖励</th><th>状态</th><th class="c-acts"></th></tr></thead><tbody>' +
      t.list.map(x => taskRowHtml(uid, x)).join('') + '</tbody></table></div>';
  }
  const recs = V3.records.filter(r => r.uid === uid).slice(0, 15);
  h += '<div class="sub-h">任务记录<span class="grow"></span><span class="note">最近 ' + recs.length + ' 条（服务端保存，重启不丢）</span></div>';
  h += recs.length ? '<div class="rec-list">' + recs.map(recHtml).join('') + '</div>' : '<div class="muted-pad">还没有任务记录：调度到点或手动执行后会出现在这里</div>';
  return h;
}
function recHtml(r) {
  const cls = r.outcome === 'skipped' ? 'mute' : (r.ok ? 'ok' : 'bad');
  return '<div class="rec"><span class="t">' + esc(fmtTime(r.at)) + (r.trigger === 'schedule' ? ' · 排程' : ' · 手动') + '</span>' +
    '<span class="j"><span class="tag ' + cls + '">' + esc(r.job_name || jobName(r.job)) + '</span></span>' +
    '<span class="m">' + esc(r.message || OUTCOME_WORD[r.outcome] || '') + '</span>' +
    '<span class="c">' + (r.credits ? '+' + fmtNum(r.credits) : '') + '</span></div>';
}
function taskRowHtml(uid, t) {
  const cur = t.current ?? 0, tgt = t.target ?? 0;
  const prog = tgt ? cur + ' / ' + tgt : (cur > 0 ? String(cur) : '—');
  const parts = [];
  if (t.credit) parts.push('+' + t.credit + ' 分');
  if (t.energy) parts.push('+' + t.energy + ' 能');
  if (t.reward_buddy) parts.push('Buddy');
  const badge = t.claimed ? '<span class="tag ok">已领取</span>' : t.claimable ? '<span class="tag warn">可领取</span>'
    : t.locked ? '<span class="tag mute">未解锁</span>' : t.accept_status === 'accepted' ? '<span class="tag mute">进行中</span>' : '<span class="tag mute">未接受</span>';
  const c = esc(t.task_code), u = esc(uid);
  const acted = t.claimed || t.locked ? ''
    : t.claimable ? '<button class="xs primary" data-tk-act="claim" data-c="' + c + '" data-u="' + u + '">领取</button>'
    : AUTO_TASKS[t.task_code] ? '<button class="xs primary" data-tk-act="auto" data-c="' + c + '" data-u="' + u + '" title="' + esc(AUTO_TASKS[t.task_code]) + '">一键完成</button>'
    : t.accept_status === 'accepted' ? '' : '<button class="xs" data-tk-act="accept" data-c="' + c + '" data-u="' + u + '">接受</button>';
  const tip = [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
  return '<tr title="' + esc(tip) + '"><td class="mark" aria-hidden="true"><i></i></td><td class="who"><div class="nm">' + esc(t.title || t.task_code) + '</div><div class="id">' + c + (t.tag ? ' · ' + esc(t.tag) : '') + '</div></td>' +
    '<td class="num">' + esc(prog) + '</td><td class="num">' + esc(parts.join(' ') || '—') + '</td><td>' + badge + '</td><td class="acts">' + acted + '</td></tr>';
}
async function loadAccountTasks(uid) {
  V3.tasks[uid] = { loading: true };
  renderTaskRows();
  try {
    const d = await api('accounts/' + encodeURIComponent(uid) + '/tasks');
    const list = d.tasks || [];
    list.sort((a, b) => (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
    V3.tasks[uid] = { list };
  } catch (e) { V3.tasks[uid] = { error: e.message }; }
  renderTaskRows();
}
document.addEventListener('click', async ev => {
  const t = ev.target;
  const run = t.closest && t.closest('[data-run-job]');
  if (run) {
    ev.stopPropagation();
    const job = run.dataset.runJob, uid = run.dataset.u;
    run.disabled = true;
    const label = run.textContent;
    run.textContent = '执行中…';
    try {
      const r = await api('accounts/' + encodeURIComponent(uid) + '/run', { method: 'POST', body: JSON.stringify({ job }) });
      const res = r.result || {};
      toast(jobName(job) + '：' + (res.message || OUTCOME_WORD[res.outcome] || '完成'), res.ok ? 'ok' : 'err');
    } catch (e) { toast(jobName(job) + '：' + e.message, 'err'); }
    finally { run.disabled = false; run.textContent = label; v3_loadTasksView(); loadOverview(true); }
    return;
  }
  const b = t.closest && t.closest('[data-tk-act]');
  if (!b) return;
  ev.stopPropagation();
  const uid = b.dataset.u, a = b.dataset.tkAct, code = b.dataset.c;
  const base = 'accounts/' + encodeURIComponent(uid) + '/tasks';
  if (a === 'reload') { loadAccountTasks(uid); return; }
  if (a === 'auto_all' && !confirm('将依次执行该账号全部可自动完成的任务（含真实对话），约 1-2 分钟，继续？')) return;
  b.disabled = true;
  const label = b.textContent;
  b.textContent = '执行中…';
  try {
    if (a === 'accept_all') {
      const r = await api(base + '/accept_all', { method: 'POST' });
      toast(r.accepted ? '已接受 ' + r.accepted + ' 个任务' : (r.message || '所有任务均已接受'), 'ok');
    } else if (a === 'auto_all') {
      const r = await api(base + '/auto_all', { method: 'POST' });
      const res = r.results || [];
      const okN = res.filter(x => x.status === 'done').length, errN = res.filter(x => x.status === 'error').length;
      toast('执行完成：成功 ' + okN + ' 项' + (errN ? '，失败 ' + errN + ' 项' : ''), errN ? 'err' : 'ok');
    } else if (a === 'auto') {
      const r = await api(base + '/auto', { method: 'POST', body: JSON.stringify({ task_code: code }) });
      toast(r.message || '已执行', (r.claimed || r.progress_before !== r.progress_after || r.skipped) ? 'ok' : 'err');
    } else if (a === 'claim') {
      await api(base + '/claim', { method: 'POST', body: JSON.stringify({ task_code: code }) });
      toast('已领取奖励', 'ok');
    } else if (a === 'accept') {
      await api(base + '/accept', { method: 'POST', body: JSON.stringify({ task_codes: [code] }) });
      toast('已接受任务', 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = label; loadAccountTasks(uid); v3_loadTasksView(); }
});
if ($('tkQ')) $('tkQ').oninput = renderTaskRows;
function v3_gotoTasks(uid) {
  V3.open.tk.add(uid);
  if (!V3.tasks[uid]) loadAccountTasks(uid);
  go('tasks');
  history.replaceState(null, '', '#tasks');
  setTimeout(() => {
    const row = document.querySelector('#tkRows .xrow[data-uid="' + CSS.escape(uid) + '"]');
    if (row && row.scrollIntoView) row.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }, 120);
  return true;
}

/* 运行历史 */
const RUN_DETAIL = {};
function renderRuns() {
  const box = $('runRows');
  if (!box) return;
  $('runNote').textContent = V3.runs.length ? '最近 ' + V3.runs.length + ' 轮' : '';
  if (!V3.runs.length) { box.innerHTML = '<div class="empty">还没有运行记录：排程到点或点「全部账号执行」后会出现在这里</div>'; return; }
  box.innerHTML = V3.runs.map(r => {
    const open = V3.open.run.has(r.id);
    const secs = Math.max(0, (parseAPITime(r.finished) - parseAPITime(r.started)) / 1000);
    const det = RUN_DETAIL[r.id];
    return '<div class="xrow' + (open ? ' open' : '') + '" data-uid="' + esc(r.id) + '">' +
      '<div class="xhead" data-xt="run">' + CHEV_SVG +
      '<div class="who"><div class="nm">' + esc(r.job_name || jobName(r.job)) + '</div><div class="rmk">' + esc(fmtTime(r.started)) + ' · ' + (r.trigger === 'schedule' ? '排程' : '手动') + ' · 用时 ' + dur(secs) + '</div></div>' +
      '<div class="mid">' +
        '<div class="m"><span class="k">账号</span><span class="v">' + (r.accounts || 0) + '</span></div>' +
        '<div class="m"><span class="k">成功</span><span class="v" style="color:var(--ok)">' + (r.ok || 0) + '</span></div>' +
        '<div class="m"><span class="k">失败</span><span class="v"' + (r.failed ? ' style="color:var(--bad)"' : '') + '>' + (r.failed || 0) + '</span></div>' +
        '<div class="m"><span class="k">跳过</span><span class="v">' + (r.skipped || 0) + '</span></div>' +
        '<div class="m"><span class="k">积分</span><span class="v">' + (r.credits ? '+' + fmtNum(r.credits) : '—') + '</span></div>' +
      '</div><div class="right"></div></div>' +
      '<div class="xbody">' + (open ? (!det ? '<div class="muted-pad"><span class="dots">读取中</span></div>'
        : det.length ? '<div class="rec-list">' + det.map(x => '<div class="rec"><span class="t">' + esc(fmtTime(x.at)) + '</span><span class="j">' + esc(noteOf(x.uid) || x.nickname || String(x.uid).slice(0, 10)) + '</span><span class="m"><span class="tag ' + (x.outcome === 'skipped' ? 'mute' : (x.ok ? 'ok' : 'bad')) + '">' + esc(OUTCOME_WORD[x.outcome] || x.outcome) + '</span> ' + esc(x.message || '') + '</span><span class="c">' + (x.credits ? '+' + fmtNum(x.credits) : '') + '</span></div>').join('') + '</div>'
        : '<div class="muted-pad">这一轮没有符合条件的账号</div>') : '') + '</div></div>';
  }).join('');
}
async function loadRunDetail(id) {
  try { RUN_DETAIL[id] = (await api('runs/' + encodeURIComponent(id))).records || []; }
  catch (e) { RUN_DETAIL[id] = []; }
  renderRuns();
}
if ($('btnRunsReload')) $('btnRunsReload').onclick = v3_loadTasksView;
if ($('btnRunsClear')) $('btnRunsClear').onclick = async () => {
  if (!confirm('清空全部任务记录与运行历史？（备注不受影响）')) return;
  try { await api('runs/clear', { method: 'POST' }); toast('已清空', 'ok'); v3_loadTasksView(); }
  catch (e) { toast(e.message, 'err'); }
};

/* ── 调度管理 ─────────────────────────────────────────────────────── */
async function v3_loadSchedule() {
  try {
    V3.sched = await api('schedule/status');
  } catch (e) { return; }
  const s = V3.sched;
  V3.jobs = Object.assign({}, JOB_NAMES, s.jobs || {});
  const m = $('schMaster');
  m.classList.toggle('off', !s.enabled);
  const act = Object.keys(s.active || {}).map(jobName);
  $('schMasterT').textContent = s.enabled ? '调度器运行中' : '调度总开关已关闭';
  $('schMasterD').textContent = (s.enabled ? '定时任务按下方时点自动执行（北京时间整点）' : '所有定时任务都不会自动运行，手动执行照常可用') +
    (act.length ? ' · 正在执行：' + act.join('、') : '') +
    ((s.runs || []).length ? ' · 最近一轮：' + jobName(s.runs[0].job) + ' ' + fmtTime(s.runs[0].started) : '');
  const next = Object.entries(s.next || {}).sort((a, b) => parseAPITime(a[1]) - parseAPITime(b[1]));
  $('schNext').innerHTML = next.length ? next.map(([j, t]) => '<div class="it"><b>' + esc(jobName(j)) + '</b><span>' + esc(fmtTime(t)) + '</span></div>').join('')
    : '<div class="muted-pad" style="padding:0">' + (s.enabled ? '没有启用的定时任务' : '总开关关闭中') + '</div>';
  document.querySelectorAll('[data-next]').forEach(el => {
    const t = (s.next || {})[el.dataset.next];
    el.textContent = t ? '下一次：' + fmtTime(t) : (s.enabled ? '' : '总开关关闭');
  });
}
let schPollN = 0;
function v3_pollSchedule() { if (++schPollN % 6 === 0) v3_loadSchedule(); }
if ($('btnSchReload')) $('btnSchReload').onclick = v3_loadSchedule;

/* ── 日志管理：按密钥筛选请求记录 ───────────────────────────────────── */
async function v3_loadKeyFilter() {
  const sel = $('reqKey');
  if (!sel || sel.dataset.loaded) return;
  try {
    const d = await api('keys');
    const cur = sel.value;
    sel.innerHTML = '<option value="">全部密钥</option><option value="legacy">默认密钥（config api_key）</option><option value="panel">面板测试</option>' +
      (d.keys || []).map(k => '<option value="' + esc(k.id) + '">' + esc(k.name) + '</option>').join('');
    sel.value = cur;
    sel.dataset.loaded = '1';
  } catch (e) { /* ignore */ }
}
if ($('reqKey')) $('reqKey').onchange = loadLogs;
