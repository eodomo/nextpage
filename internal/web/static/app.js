// nextpage web client. Talks to the Go server via a JSON API and a
// Server-Sent Events stream of agent events.

const $ = (sel, root = document) => root.querySelector(sel);
const el = (tag, cls, text) => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
};

const state = {
  courses: [],
  dir: '',
  openPath: '',
  running: false,
  bubble: null,      // assistant message being streamed
  thinking: null,    // thinking block being streamed
  tools: new Map(),  // tool id -> element
  cards: new Map(),  // interaction id -> element
  quizId: null,
};

// ---------- API ----------

async function api(path, body) {
  const opts = body === undefined ? {} : {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  };
  const res = await fetch(path, opts);
  if (res.status === 401) { location.href = '/login'; throw new Error('logged out'); }
  const type = res.headers.get('Content-Type') || '';
  const data = type.includes('json') ? await res.json() : await res.text();
  if (!res.ok) throw new Error(data.error || data || res.statusText);
  return data;
}

// ---------- markdown + LaTeX + Obsidian ----------

const escapeHTML = (s) => s.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// Math and wikilinks are swapped for placeholders before markdown parsing so
// marked doesn't mangle backslashes and underscores, then restored after
// sanitizing. Code spans and fences are left untouched.
const TOKEN_RE = /(```[\s\S]*?```|~~~[\s\S]*?~~~|`[^`\n]+`)|\$\$([\s\S]+?)\$\$|\\\[([\s\S]+?)\\\]|\\\(([\s\S]+?)\\\)|\$(?![\s$])([^$\n]+?)(?<!\s)\$|\[\[([^\]|\n]+)(?:\|([^\]\n]+))?\]\]/g;

function renderMarkdown(src) {
  let tags = [];
  const fm = src.match(/^---\n([\s\S]*?)\n---\n?/);
  if (fm) {
    src = src.slice(fm[0].length);
    const t = fm[1].match(/^tags:\s*\[(.*)\]/m);
    if (t) tags = t[1].split(',').map((s) => s.trim().replace(/^["']|["']$/g, '')).filter(Boolean);
  }
  const slots = [];
  const marker = (i) => `⁣M${i}⁣`;
  src = src.replace(TOKEN_RE, (m, code, d1, d2, i1, i2, link, alias) => {
    if (code) return code;
    if (link) {
      slots.push({ link: link.trim(), text: (alias || link).trim() });
    } else {
      const display = d1 !== undefined || d2 !== undefined;
      slots.push({ tex: d1 ?? d2 ?? i1 ?? i2, display });
    }
    return marker(slots.length - 1);
  });
  let html = DOMPurify.sanitize(marked.parse(src, { gfm: true, breaks: false }));
  html = html.replace(/⁣M(\d+)⁣/g, (_, i) => {
    const s = slots[+i];
    if (s.link) return `<a class="wikilink" data-note="${escapeHTML(s.link)}">${escapeHTML(s.text)}</a>`;
    try {
      return katex.renderToString(s.tex, { displayMode: s.display, throwOnError: false, strict: 'ignore' });
    } catch {
      return `<code>${escapeHTML(s.tex)}</code>`;
    }
  });
  if (tags.length) {
    html = `<div class="frontmatter">${tags.map((t) => `<span class="tag">${escapeHTML(t)}</span>`).join('')}</div>` + html;
  }
  return html;
}

// Obsidian callouts: "> [!type]± Title" blockquotes.
function upgradeCallouts(root) {
  for (const bq of root.querySelectorAll('blockquote')) {
    const first = bq.firstElementChild;
    if (!first || first.tagName !== 'P') continue;
    const m = first.innerHTML.match(/^\[!(\w+)\]([+-]?)\s*([^\n<]*)(?:\n|<br>)?/);
    if (!m) continue;
    const [all, type, fold, title] = m;
    first.innerHTML = first.innerHTML.slice(all.length);
    if (!first.innerHTML.trim()) first.remove();
    const box = el(fold ? 'details' : 'div', `callout callout-${type.toLowerCase()}`);
    if (fold === '+') box.open = true;
    const head = el(fold ? 'summary' : 'div', 'callout-title', title || type[0].toUpperCase() + type.slice(1));
    box.append(head, ...bq.childNodes);
    bq.replaceWith(box);
  }
}

let mermaidLib = null;
async function renderMermaid(root) {
  const blocks = root.querySelectorAll('pre > code.language-mermaid');
  if (!blocks.length) return;
  if (!mermaidLib) {
    mermaidLib = (await import('https://cdn.jsdelivr.net/npm/mermaid@11.4.0/dist/mermaid.esm.min.mjs')).default;
    mermaidLib.initialize({
      startOnLoad: false,
      securityLevel: 'strict',
      theme: 'base',
      themeVariables: {
        darkMode: true,
        background: '#0a0a0b',
        primaryColor: '#16161a',
        primaryTextColor: '#ececee',
        primaryBorderColor: '#ff2800',
        lineColor: '#ff2800',
        secondaryColor: '#121214',
        tertiaryColor: '#000000',
        fontFamily: 'Titillium Web, sans-serif',
      },
    });
  }
  const nodes = [];
  for (const code of blocks) {
    const div = el('div', 'mermaid');
    div.textContent = code.textContent;
    code.parentElement.replaceWith(div);
    nodes.push(div);
  }
  try {
    await mermaidLib.run({ nodes });
  } catch (err) {
    console.warn('mermaid', err);
  }
}

function fillMarkdown(target, src) {
  target.innerHTML = renderMarkdown(src);
  upgradeCallouts(target);
  renderMermaid(target);
}

// ---------- courses & reader ----------

async function loadCourses() {
  try {
    const data = await api('/api/courses');
    state.courses = data.courses || [];
    state.dir = data.dir;
  } catch (err) {
    console.warn(err);
  }
  renderCourses();
}

function noteName(path) {
  return path.split('/').pop().replace(/\.(md|tex)$/, '');
}

function renderCourses() {
  const box = $('#courses');
  box.replaceChildren();
  if (!state.courses.length) {
    box.append(el('div', 'empty', state.dir
      ? 'No courses yet. Tell your tutor what you want to learn.'
      : 'No courses yet. You will choose where course notes are saved when you start your first course.'));
    return;
  }
  const activeFolder = state.openPath.split('/')[0];
  for (const c of state.courses) {
    const wrap = el('div', 'course' + (c.active ? ' active' : ''));
    const head = el('button', 'course-head');
    head.append(el('span', 'course-title', c.title));
    const pct = c.sections ? Math.round((100 * c.passed) / c.sections) : 0;
    const meta = c.phase === 'learning' || c.phase === 'complete'
      ? `${c.passed}/${c.sections} sections · ${c.phase}`
      : c.phase;
    head.append(el('span', 'course-meta', meta));
    const bar = el('div', 'progress');
    const fill = el('div');
    fill.style.width = pct + '%';
    bar.append(fill);
    head.append(bar);
    const del = el('button', 'course-delete', '×');
    del.type = 'button';
    del.title = 'Delete this course';
    del.setAttribute('aria-label', `Delete course ${c.title}`);
    del.onclick = (e) => { e.stopPropagation(); deleteCourse(c); };
    const list = el('ul', 'files');
    list.hidden = !(c.active || c.folder === activeFolder);
    head.onclick = () => { list.hidden = !list.hidden; };
    let quizHeader = false;
    for (const f of c.files || []) {
      if (f.includes('/Quizzes/') && !quizHeader) {
        list.append(el('li', 'group', 'Quizzes'));
        quizHeader = true;
      }
      const li = el('li');
      const b = el('button', f === state.openPath ? 'open' : '', noteName(f));
      b.title = noteName(f);
      b.onclick = () => openFile(f);
      li.append(b);
      list.append(li);
    }
    wrap.append(del, head, list);
    box.append(wrap);
  }
}

async function deleteCourse(c) {
  if (!confirm(`Delete the course "${c.title}" and all of its lessons and quizzes? This can't be undone.`)) return;
  try {
    await api('/api/courses/delete', { folder: c.folder });
  } catch (err) {
    alert(err.message);
    return;
  }
  if (state.openPath.startsWith(c.folder + '/')) {
    state.openPath = '';
    $('#reader-title').textContent = 'Reader';
    $('#reader').replaceChildren(el('div', 'empty', `Deleted "${c.title}".`));
  }
  loadCourses();
}

async function openFile(path, { focus = true } = {}) {
  let text;
  try {
    text = await api('/api/file?path=' + encodeURIComponent(path));
  } catch (err) {
    return;
  }
  state.openPath = path;
  $('#reader-title').textContent = noteName(path);
  const reader = $('#reader');
  const doc = el('article', 'doc md');
  if (path.endsWith('.tex')) {
    doc.append(el('p', null, 'LaTeX source — compile it or open it in a LaTeX editor.'));
    doc.append(el('pre', 'tex', text));
  } else {
    fillMarkdown(doc, text);
  }
  reader.replaceChildren(doc);
  reader.scrollTop = 0;
  renderCourses();
  if (focus) showPane('reader');
}

// Wikilinks open the matching note in the same course.
document.addEventListener('click', (e) => {
  const a = e.target.closest('.wikilink');
  if (!a) return;
  e.preventDefault();
  const want = a.dataset.note.toLowerCase();
  const folder = state.openPath.split('/')[0];
  const files = state.courses.flatMap((c) => c.files || []);
  const hit = files.find((f) => f.startsWith(folder + '/') && noteName(f).toLowerCase() === want)
    || files.find((f) => noteName(f).toLowerCase() === want);
  if (hit) openFile(hit);
});

// ---------- chat ----------

const scroller = $('#messages-scroll');
const messages = $('#messages');

function nearBottom() {
  return scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 120;
}
function append(node) {
  const stick = nearBottom();
  messages.append(node);
  if (stick) scroller.scrollTop = scroller.scrollHeight;
  return node;
}

function addUser(text) {
  append(el('div', 'msg msg-user', text));
}

let renderQueued = false;
function streamText(text) {
  if (!state.bubble) {
    state.bubble = append(el('div', 'msg msg-assistant md'));
    state.bubble.raw = '';
  }
  state.bubble.raw += text;
  if (!renderQueued) {
    renderQueued = true;
    requestAnimationFrame(() => {
      renderQueued = false;
      if (!state.bubble) return;
      const stick = nearBottom();
      state.bubble.innerHTML = renderMarkdown(state.bubble.raw);
      if (stick) scroller.scrollTop = scroller.scrollHeight;
    });
  }
}

function finishBubble() {
  if (state.bubble) {
    fillMarkdown(state.bubble, state.bubble.raw);
    state.bubble = null;
  }
  state.thinking = null;
}

function streamThinking(text) {
  if (!state.thinking) {
    const d = el('details', 'msg msg-thinking');
    d.append(el('summary', null, 'Thinking…'), el('div'));
    state.thinking = append(d);
  }
  state.thinking.lastChild.textContent += text;
}

const TOOL_LABELS = {
  StartCourse: 'Starting course',
  OpenCourse: 'Opening course',
  CourseStatus: 'Checking progress',
  GiveQuiz: 'Quiz',
  GradeQuiz: 'Grading answers',
  SaveCoursePlan: 'Charting the course',
  WriteLesson: 'Lesson',
  WebFetch: 'Researching',
  Read: 'Reading',
};

function toolStart(ev) {
  finishBubble();
  const t = el('div', 'tool');
  t.append(el('span', 'dot'));
  const body = el('div');
  const name = el('b', null, TOOL_LABELS[ev.name] || ev.name);
  body.append(name);
  if (ev.subject) body.append(document.createTextNode(' · ' + ev.subject));
  t.append(body);
  state.tools.set(ev.id, t);
  setWorking(TOOL_LABELS[ev.name] || ev.name);
  append(t);
}

function toolEnd(ev, replay) {
  let t = state.tools.get(ev.id);
  if (ev.is_error) {
    // A failed call is the tutor retrying internally; details are in the
    // server log, not the chat.
    t?.remove();
    state.tools.delete(ev.id);
    setWorking('Thinking');
    return;
  }
  if (!t) { toolStart(ev); t = state.tools.get(ev.id); }
  t.classList.add('ok');
  const body = t.lastChild;
  if (ev.output) body.append(el('span', 'out', ev.output));
  // A lesson written as a captured reply streamed into the chat; it now lives
  // in the reader, so fold it away.
  const next = t.nextElementSibling;
  if (ev.name === 'WriteLesson' && ev.open && next?.classList.contains('msg-assistant')) {
    const fold = el('details', 'msg msg-thinking');
    fold.append(el('summary', null, 'Lesson text (saved, open it in the reader)'));
    next.replaceWith(fold);
    fold.append(next);
  }
  if (ev.open) {
    const link = el('a', null, 'Open');
    link.onclick = () => openFile(ev.open);
    body.querySelector('.out')?.append(' · ', link);
    loadCourses().then(() => { if (!replay && innerWidth > 900) openFile(ev.open, { focus: false }); });
  } else if (/^(GiveQuiz|GradeQuiz|StartCourse)$/.test(ev.name)) {
    loadCourses();
  }
  setWorking('Thinking');
}

function notice(text, isError) {
  append(el('div', isError ? 'msg msg-error' : 'msg msg-notice', text));
}

function setRunning(on) {
  state.running = on;
  $('#status-dot').classList.toggle('live', on);
  $('#working').classList.toggle('on', on);
  $('#stop').hidden = !on;
  $('#send').disabled = on;
  if (on) setWorking('Thinking');
}
function setWorking(label) {
  $('#working-label').textContent = label;
}

// ---------- interactions ----------

function resolveInteraction(id) {
  const card = state.cards.get(id);
  if (card) { card.remove(); state.cards.delete(id); }
  if (state.quizId === id) closeQuiz();
}

async function reply(id, value) {
  try {
    await api('/api/reply', { id, value });
    resolveInteraction(id);
  } catch (err) {
    notice(err.message, true);
  }
}

function showInteraction(ev) {
  if (state.cards.has(ev.id) || state.quizId === ev.id) return;
  if (ev.type === 'quiz') { openQuiz(ev.id, ev.quiz); return; }
  finishBubble();
  const card = el('div', 'card');
  const row = el('div', 'row');
  const button = (label, primary, fn) => {
    const b = el('button', 'btn' + (primary ? ' btn-primary' : ''), label);
    b.type = 'button';
    b.onclick = fn;
    row.append(b);
    return b;
  };
  switch (ev.type) {
    case 'permission': {
      card.append(el('h4', null, `Allow ${ev.tool}?`));
      if (ev.subject) card.append(el('div', null, ev.subject));
      if (ev.preview && ev.preview !== ev.subject) card.append(el('pre', null, ev.preview));
      button('Allow', true, () => reply(ev.id, { allow: true }));
      button(`Always allow ${ev.suggestion} this session`, false, () => reply(ev.id, { allow: true, remember: true }));
      button('Deny', false, () => reply(ev.id, { allow: false }));
      card.append(row);
      break;
    }
    case 'question': {
      card.append(el('h4', null, ev.question));
      for (const o of ev.options || []) button(o, false, () => reply(ev.id, o));
      const input = el('input');
      input.type = 'text';
      input.placeholder = 'Or type an answer and press Enter';
      input.onkeydown = (e) => { if (e.key === 'Enter' && input.value.trim()) reply(ev.id, input.value.trim()); };
      card.append(row, input);
      break;
    }
    case 'plan': {
      card.append(el('h4', null, 'Approve this plan?'));
      const plan = el('div', 'md');
      fillMarkdown(plan, ev.plan);
      card.append(plan);
      button('Approve', true, () => reply(ev.id, true));
      button('Keep planning', false, () => reply(ev.id, false));
      card.append(row);
      break;
    }
    case 'directory': {
      card.append(el('h4', null, 'Where should course notes be saved?'));
      card.append(el('div', 'course-meta', 'Choose a folder inside your Obsidian vault (a path on the server).'));
      const input = el('input');
      input.type = 'text';
      input.value = ev.default;
      button('Save here', true, () => reply(ev.id, input.value.trim() || ev.default));
      card.append(input, row);
      break;
    }
    default:
      return;
  }
  state.cards.set(ev.id, card);
  append(card);
  showPane('chat');
}

// ---------- quiz ----------

function openQuiz(id, quiz) {
  state.quizId = id;
  $('#quiz-kicker').textContent = (quiz.kind === 'placement' ? 'Placement quiz' : 'Checkpoint') + (quiz.course ? ' · ' + quiz.course : '');
  $('#quiz-title').textContent = quiz.title;
  const body = $('#quiz-body');
  body.replaceChildren();
  quiz.questions.forEach((q, i) => {
    const box = el('fieldset', 'q');
    box.style.border = '0';
    box.style.margin = '0';
    box.append(el('div', 'q-num', `QUESTION ${i + 1} OF ${quiz.questions.length}`));
    const prompt = el('div', 'q-prompt md');
    fillMarkdown(prompt, q.prompt);
    box.append(prompt);
    if (q.type === 'multiple_choice') {
      q.options.forEach((o, j) => {
        const label = el('label', 'opt');
        const input = el('input');
        input.type = 'radio';
        input.name = 'q' + i;
        input.value = o;
        input.onchange = updateQuizCount;
        const text = el('span', 'md');
        text.innerHTML = renderMarkdown(o.replace(/^[A-Ha-h][).:]\s*/, '')).replace(/^<p>|<\/p>\s*$/g, '');
        label.append(input, el('span', 'key', String.fromCharCode(65 + j)), text);
        box.append(label);
      });
    } else {
      const ta = el('textarea');
      ta.name = 'q' + i;
      ta.rows = 3;
      ta.placeholder = 'Your answer…';
      ta.oninput = updateQuizCount;
      box.append(ta);
    }
    body.append(box);
  });
  $('#quiz').onsubmit = (e) => {
    e.preventDefault();
    const answers = quiz.questions.map((q, i) => {
      if (q.type === 'multiple_choice') return document.querySelector(`#quiz input[name=q${i}]:checked`)?.value || '';
      return document.querySelector(`#quiz textarea[name=q${i}]`).value.trim();
    });
    const blank = answers.filter((a) => !a).length;
    if (blank && !confirm(`${blank} question(s) unanswered. Submit anyway?`)) return;
    reply(id, answers);
  };
  updateQuizCount();
  $('#quiz-overlay').hidden = false;
  body.scrollTop = 0;
}

function updateQuizCount() {
  const qs = [...document.querySelectorAll('#quiz .q')];
  const done = qs.filter((q) => q.querySelector('input:checked') || q.querySelector('textarea')?.value.trim()).length;
  $('#quiz-count').textContent = `${done} of ${qs.length} answered`;
}

function closeQuiz() {
  state.quizId = null;
  $('#quiz-overlay').hidden = true;
}

// ---------- settings ----------

const settingsMsg = (text, cls = '') => {
  const m = $('#settings-msg');
  m.textContent = text;
  m.className = 'settings-msg ' + cls;
};

function fillModels(models, current) {
  const sel = $('#set-model');
  sel.replaceChildren();
  const all = [...new Set([...(models || []), ...(current ? [current] : [])])].sort();
  if (!all.length) sel.append(new Option('Test the connection to list models', ''));
  for (const m of all) sel.append(new Option(m, m, false, m === current));
}

function settingsBody() {
  return {
    server: $('#set-server').value.trim(),
    username: $('#set-username').value.trim(),
    password: $('#set-password').value,
    keep_password: true,
    model: $('#set-model').value,
  };
}

async function testSettings() {
  settingsMsg('Connecting…');
  try {
    const { models } = await api('/api/settings/test', settingsBody());
    fillModels(models, $('#set-model').value);
    settingsMsg(`Connected · ${models.length} model${models.length === 1 ? '' : 's'} available.`, 'ok');
    return true;
  } catch (err) {
    settingsMsg(err.message, 'err');
    return false;
  }
}

async function openSettings(reason) {
  const s = await api('/api/settings');
  $('#set-server').value = s.server || '';
  $('#set-username').value = s.username || '';
  $('#set-password').value = '';
  $('#set-password').placeholder = s.password_set ? '•••••••• (saved; leave blank to keep)' : '';
  fillModels([], s.model);
  settingsMsg(reason || '');
  $('#settings-overlay').hidden = false;
  if (s.server) testSettings();
  $('#set-server').focus();
}

$('#open-settings').onclick = () => openSettings();
$('#settings-cancel').onclick = () => { $('#settings-overlay').hidden = true; };
$('#set-test').onclick = testSettings;
$('#settings').onsubmit = async (e) => {
  e.preventDefault();
  if (!$('#set-model').value && !(await testSettings())) return;
  if (!$('#set-model').value) { settingsMsg('Choose a model.', 'err'); return; }
  settingsMsg('Saving…');
  try {
    await api('/api/settings', settingsBody());
    $('#settings-overlay').hidden = true;
  } catch (err) {
    settingsMsg(err.message, 'err');
  }
};

// ---------- events ----------

function applyEvent(ev, replay = false) {
  switch (ev.type) {
    case 'user': addUser(ev.text); setRunning(true); break;
    case 'text': streamText(ev.text); break;
    case 'thinking': streamThinking(ev.text); break;
    case 'assistant_done': finishBubble(); break;
    case 'tool_start': toolStart(ev); break;
    case 'tool_end': toolEnd(ev, replay); break;
    case 'notice': notice(ev.text, ev.level === 2); break;
    case 'interaction': showInteraction({ ...ev.event, id: ev.id }); break;
    case 'resolved': resolveInteraction(ev.id); break;
    case 'courses_changed': loadCourses(); break;
    case 'cleared': messages.replaceChildren(); state.tools.clear(); break;
    case 'settings': $('#model').textContent = ev.model; notice(`Now using ${ev.model}.`); break;
    case 'done':
      finishBubble();
      setRunning(false);
      for (const id of [...state.cards.keys()]) resolveInteraction(id);
      closeQuiz();
      if (ev.error) notice(ev.error, true);
      break;
  }
}

function renderHistory(items) {
  messages.replaceChildren();
  state.tools.clear();
  for (const it of items || []) {
    if (it.role === 'user') addUser(it.text);
    else if (it.role === 'assistant') {
      const b = append(el('div', 'msg msg-assistant md'));
      fillMarkdown(b, it.text);
    } else if (it.role === 'tool') {
      const t = el('div', 'tool ' + (it.is_error ? 'err' : 'ok'));
      t.append(el('span', 'dot'));
      const body = el('div');
      body.append(el('b', null, TOOL_LABELS[it.name] || it.name));
      if (it.subject) body.append(document.createTextNode(' · ' + it.subject));
      if (it.output) body.append(el('span', 'out', it.output));
      t.append(body);
      append(t);
    }
  }
}

async function sync() {
  const s = await api('/api/state');
  $('#model').textContent = s.model || 'no model';
  if (!s.configured) openSettings('Connect nextpage to your Ollama server to get started.');
  state.bubble = null;
  state.thinking = null;
  state.cards.clear();
  closeQuiz();
  // History holds completed messages; while a run is going, its live events
  // (from the user prompt on) are replayed instead of the run's history tail.
  renderHistory(s.running ? trimRunning(s.history, s.live) : s.history);
  setRunning(s.running);
  for (const raw of s.live || []) applyEvent(raw, true);
  for (const p of s.pending || []) showInteraction({ ...p.event, id: p.id });
  scroller.scrollTop = scroller.scrollHeight;
}

// trimRunning drops history entries belonging to the in-flight run (they are
// re-created from the live event log).
function trimRunning(history, live) {
  const first = (live || []).find((e) => e.type === 'user');
  if (!first) return history;
  for (let i = history.length - 1; i >= 0; i--) {
    if (history[i].role === 'user' && history[i].text === first.text) return history.slice(0, i);
  }
  return history;
}

function connect() {
  const es = new EventSource('/api/events');
  let opened = false;
  es.onopen = () => {
    if (opened) sync(); // reconnected: resync anything missed
    opened = true;
  };
  es.onmessage = (m) => applyEvent(JSON.parse(m.data));
}

// ---------- composer & layout ----------

const input = $('#input');
input.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    $('#composer').requestSubmit();
  }
});
input.addEventListener('input', () => {
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, 200) + 'px';
});
$('#composer').addEventListener('submit', async (e) => {
  e.preventDefault();
  const text = input.value.trim();
  if (!text || state.running) return;
  try {
    await api('/api/chat', { text });
    input.value = '';
    input.style.height = 'auto';
  } catch (err) {
    notice(err.message, true);
  }
});
$('#stop').onclick = () => api('/api/interrupt', {});
$('#new-chat').onclick = async () => {
  if (!confirm('Start a new conversation? Your courses and progress are kept.')) return;
  try { await api('/api/new', {}); } catch (err) { notice(err.message, true); }
};

function showPane(name) {
  for (const p of document.querySelectorAll('.pane')) p.classList.toggle('show', p.id === 'pane-' + name);
  for (const b of document.querySelectorAll('#tabs button')) b.classList.toggle('on', b.dataset.pane === name);
}
for (const b of document.querySelectorAll('#tabs button')) b.onclick = () => showPane(b.dataset.pane);

(async () => {
  await Promise.all([sync(), loadCourses()]);
  connect();
  const active = state.courses.find((c) => c.active) || state.courses[0];
  const plan = active?.files?.find((f) => f.endsWith('00 Course Plan.md'));
  if (plan) openFile(plan, { focus: false });
})();
