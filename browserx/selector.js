(() => {
  const norm = (s) => String(s == null ? '' : s).replace(/\s+/g, ' ').trim();

  // ---- selector engine: CSS (piercing open shadow roots), text=, xpath=, `>>` chains,
  // and the :has-text() / :text() / :text-is() / :visible extensions.
  const splitChain = (selector) => {
    const parts = [];
    let depth = 0, quote = '', current = '';
    for (let i = 0; i < selector.length; i++) {
      const ch = selector[i];
      if (quote) {
        if (ch === '\\') { current += ch + (selector[++i] || ''); continue; }
        if (ch === quote) quote = '';
      } else if (ch === '"' || ch === "'") quote = ch;
      else if (ch === '[' || ch === '(') depth++;
      else if (ch === ']' || ch === ')') depth--;
      else if (ch === '>' && selector[i + 1] === '>' && depth === 0) {
        parts.push(current); current = ''; i++; continue;
      }
      current += ch;
    }
    parts.push(current);
    return parts.map((p) => p.trim());
  };

  // :has-text(), :text() and :text-is() take a quoted string or a /regex/: the
  // quotes only delimit, matching stays case-insensitive unless it is -is.
  const pseudoMatcher = (arg) => {
    const re = arg.match(/^\/(.*)\/([a-z]*)$/s);
    if (re) { const rx = new RegExp(re[1], re[2]); return (t) => rx.test(t); }
    const want = norm(arg.replace(/^(["'])(.*)\1$/s, '$2')).toLowerCase();
    return (t) => norm(t).toLowerCase().includes(want);
  };
  const textMatcher = (raw) => {
    raw = raw.trim();
    const re = raw.match(/^\/(.*)\/([a-z]*)$/s);
    if (re) { const rx = new RegExp(re[1], re[2]); return (t) => rx.test(t); }
    const q = raw.match(/^(["'])(.*)\1$/s);
    if (q) { const want = norm(q[2]); return (t) => norm(t) === want; }
    const want = norm(raw).toLowerCase();
    return (t) => norm(t).toLowerCase().includes(want);
  };
  const exactMatcher = (raw) => {
    const q = raw.trim().match(/^(["'])(.*)\1$/s);
    const want = norm(q ? q[2] : raw);
    return (t) => norm(t) === want;
  };
  const textOf = (el) => {
    if (el.nodeName === 'INPUT' && ['button', 'submit', 'reset'].includes(el.type)) return el.value;
    return el.textContent || '';
  };

  const cssAll = (root, css) => {
    const out = [], seen = new Set();
    const visit = (r) => {
      for (const el of r.querySelectorAll(css)) if (!seen.has(el)) { seen.add(el); out.push(el); }
      for (const host of r.querySelectorAll('*')) if (host.shadowRoot) visit(host.shadowRoot);
    };
    visit(root);
    return out;
  };
  const allElements = (root) => cssAll(root, '*');

  const PSEUDO = /:(has-text|text-is|text|visible)(?:\(\s*("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|\/(?:[^/\\]|\\.)*\/[a-z]*)\s*\))?/g;

  const evalPart = (part, root) => {
    if (/^(xpath=|\/\/|\.\.)/.test(part)) {
      const expr = part.replace(/^xpath=/, '');
      const doc = root.ownerDocument || root;
      const snap = doc.evaluate(expr, root, null, XPathResult.ORDERED_NODE_SNAPSHOT_TYPE, null);
      const out = [];
      for (let i = 0; i < snap.snapshotLength; i++) {
        const n = snap.snapshotItem(i);
        if (n.nodeType === 1) out.push(n);
      }
      return out;
    }
    if (part.startsWith('text=')) {
      const matches = textMatcher(part.slice(5));
      const all = allElements(root).filter((el) => matches(textOf(el)));
      return all.filter((el) => !Array.from(el.children).some((c) => matches(textOf(c))));
    }
    if (part.startsWith('css=')) part = part.slice(4);
    if (part.startsWith('id=')) part = '[id="' + part.slice(3).replace(/"/g, '\\"') + '"]';
    const filters = [];
    let css = part.replace(PSEUDO, (_m, name, arg) => { filters.push([name, arg]); return ''; }).trim();
    if (!css) css = '*';
    let els = cssAll(root, css);
    for (const [name, arg] of filters) {
      if (name === 'visible') els = els.filter(visible);
      else if (name === 'has-text') { const m = pseudoMatcher(arg); els = els.filter((el) => m(textOf(el))); }
      else if (name === 'text-is') { const m = exactMatcher(arg); els = els.filter((el) => m(textOf(el)) && !Array.from(el.children).some((c) => m(textOf(c)))); }
      else if (name === 'text') { const m = pseudoMatcher(arg); els = els.filter((el) => m(textOf(el)) && !Array.from(el.children).some((c) => m(textOf(c)))); }
    }
    return els;
  };

  const queryAll = (selector, root = document) => {
    let current = [root];
    for (const part of splitChain(selector)) {
      const nth = part.match(/^nth=(-?\d+)$/);
      if (nth) {
        const i = Number(nth[1]);
        const picked = current[i < 0 ? current.length + i : i];
        current = picked ? [picked] : [];
        continue;
      }
      const next = [], seen = new Set();
      for (const r of current) for (const el of evalPart(part, r)) if (!seen.has(el)) { seen.add(el); next.push(el); }
      current = next;
    }
    return current;
  };

  // ---- element state, as in Playwright's injected script.
  const visible = (el) => {
    const style = getComputedStyle(el);
    if (style.display === 'contents') {
      for (let c = el.firstChild; c; c = c.nextSibling) {
        if (c.nodeType === 1 && visible(c)) return true;
        if (c.nodeType === 3) {
          const range = el.ownerDocument.createRange(); range.selectNode(c);
          const r = range.getBoundingClientRect();
          if (r.width > 0 && r.height > 0) return true;
        }
      }
      return false;
    }
    if (el.checkVisibility && !el.checkVisibility()) return false;
    if (style.visibility !== 'visible') return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const NATIVE = ['BUTTON', 'INPUT', 'SELECT', 'TEXTAREA', 'OPTION', 'OPTGROUP'];
  const disabled = (el) => {
    if (NATIVE.includes(el.tagName)) {
      if (el.hasAttribute('disabled')) return true;
      if (el.tagName === 'OPTION' && el.closest('optgroup[disabled]')) return true;
      const fs = el.closest('fieldset[disabled]');
      if (fs) { const legend = fs.querySelector(':scope > legend'); if (!legend || !legend.contains(el)) return true; }
    }
    for (let e = el; e; e = e.parentElement || (e.getRootNode && e.getRootNode().host)) {
      const a = (e.getAttribute('aria-disabled') || '').toLowerCase();
      if (a === 'true') return true;
      if (a === 'false') return false;
    }
    return false;
  };
  const readonly = (el) => {
    if (['INPUT', 'TEXTAREA', 'SELECT'].includes(el.tagName)) return el.hasAttribute('readonly');
    if (el.getAttribute('aria-readonly') === 'true') return true;
    if (el.isContentEditable) return false;
    if (el.getAttribute('role') && el.hasAttribute('aria-readonly')) return false;
    return 'error';
  };
  const retarget = (node, follow) => {
    let el = node.nodeType === 1 ? node : node.parentElement;
    if (!el) return null;
    if (!el.matches('input, textarea, select') && !el.isContentEditable)
      el = el.closest('button, [role=button], [role=checkbox], [role=radio]') || el;
    if (follow && !el.matches('a, input, textarea, button, select, [role=link], [role=button], [role=checkbox], [role=radio]') && !el.isContentEditable) {
      const label = el.closest('label');
      if (label && label.control) el = label.control;
    }
    return el;
  };
  const selectText = (el) => {
    if (el.nodeName === 'INPUT') { el.select(); el.focus(); return; }
    if (el.nodeName === 'TEXTAREA') { el.selectionStart = 0; el.selectionEnd = el.value.length; el.focus(); return; }
    el.focus();
    const range = el.ownerDocument.createRange(); range.selectNodeContents(el);
    const sel = el.ownerDocument.defaultView.getSelection();
    if (sel) { sel.removeAllRanges(); sel.addRange(range); }
  };
  // fill returns 'done' when the value was set directly, 'needsinput' when the
  // caller must type/insert it into the now selected field.
  const fill = (el, value) => {
    if (el.nodeName === 'INPUT') {
      const type = el.type.toLowerCase();
      const setDirectly = ['color', 'date', 'time', 'datetime-local', 'month', 'range', 'week'];
      const typed = ['', 'email', 'number', 'password', 'search', 'tel', 'text', 'url'];
      if (!typed.includes(type) && !setDirectly.includes(type)) throw new Error('Input of type "' + type + '" cannot be filled');
      if (type === 'number') {
        value = value.trim();
        if (isNaN(Number(value))) throw new Error('Cannot type text into input[type=number]');
      }
      if (type === 'color') value = value.toLowerCase();
      if (setDirectly.includes(type)) {
        value = value.trim();
        el.focus(); el.value = value;
        if (el.value !== value) throw new Error('Malformed value');
        el.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
        el.dispatchEvent(new Event('change', { bubbles: true }));
        return 'done';
      }
    } else if (el.nodeName !== 'TEXTAREA' && !el.isContentEditable) {
      throw new Error('Element is not an <input>, <textarea> or [contenteditable] element');
    }
    selectText(el);
    return 'needsinput';
  };
  const box = (el) => {
    if (el.getClientRects().length === 0) return null;
    const r = el.getBoundingClientRect();
    return { x: r.x, y: r.y, width: r.width, height: r.height };
  };
  return { norm, queryAll, visible, disabled, readonly, retarget, selectText, fill, box };
})()
