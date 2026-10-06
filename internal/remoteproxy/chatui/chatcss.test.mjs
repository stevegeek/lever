// The layout of the two panes is the cascade of chat.css, which no fake
// browser runs. This test applies chat.css's display rules the way a
// browser does (media query, specificity, order, !important) to the three
// elements whose display decides the layout, for a phone and a wide screen,
// with a chat open and without. It caught a wide screen with a chat open
// that hid the list, Back included: no way back without a reload.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const css = readFileSync(new URL('./chat.css', import.meta.url), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');

// rules is every `selector { ... display: x ... }` with its media condition
// (one level of @media nesting, as chat.css has) and its source order.
function rules(text) {
  const out = [];
  let order = 0;
  const walk = (body, media) => {
    let i = 0;
    while (i < body.length) {
      const open = body.indexOf('{', i);
      if (open < 0) break;
      const prelude = body.slice(i, open).trim();
      let depth = 1;
      let j = open + 1;
      while (depth > 0 && j < body.length) {
        if (body[j] === '{') depth++;
        else if (body[j] === '}') depth--;
        j++;
      }
      const inner = body.slice(open + 1, j - 1);
      if (prelude.startsWith('@media')) walk(inner, prelude.slice(6).trim());
      else {
        const m = /(?:^|;)\s*display\s*:\s*([^;!]+?)\s*(!important)?\s*(?:;|$)/.exec(inner);
        if (m) for (const sel of prelude.split(',')) out.push({ sel: sel.trim(), media, display: m[1], important: !!m[2], order: order++ });
      }
      i = j;
    }
  };
  walk(text, '');
  return out;
}

// mediaMatches knows the conditions chat.css uses: min-width, max-width and
// the colour scheme (which never sets display).
function mediaMatches(media, width) {
  if (!media) return true;
  return media.split(/\band\b/).every((part) => {
    const c = /\(\s*(min|max)-width\s*:\s*([\d.]+)px\s*\)/.exec(part);
    if (c) return c[1] === 'min' ? width >= Number(c[2]) : width <= Number(c[2]);
    if (/prefers-color-scheme/.test(part)) return true;
    throw new Error(`chatcss.test: unknown media condition ${part}`);
  });
}

// A compound selector: a tag, #id, .class, :not(.class) and [attr] parts.
function compound(s) {
  const c = { tag: '', id: '', classes: [], not: [], attrs: [], other: false };
  const re = /(#[\w-]+)|(\.[\w-]+)|(:not\(\.([\w-]+)\))|(\[([\w-]+)\])|([a-z][\w-]*)|(\*)|(::?[\w-]+)/g;
  let m;
  let seen = '';
  while ((m = re.exec(s))) {
    seen += m[0];
    if (m[1]) c.id = m[1].slice(1);
    else if (m[2]) c.classes.push(m[2].slice(1));
    else if (m[3]) c.not.push(m[4]);
    else if (m[5]) c.attrs.push(m[6]);
    else if (m[7]) c.tag = m[7];
    else if (m[9]) c.other = true; // :empty, ::before, ...: never one of the three elements below
  }
  if (seen !== s) throw new Error(`chatcss.test: cannot read selector part ${s}`);
  return c;
}

const specificity = (parts) => parts.reduce((a, c) => [a[0] + (c.id ? 1 : 0), a[1] + c.classes.length + c.not.length + c.attrs.length, a[2] + (c.tag ? 1 : 0)], [0, 0, 0]);

function matchesEl(c, el) {
  if (c.other) return false;
  if (c.tag && c.tag !== el.tag) return false;
  if (c.id && c.id !== el.id) return false;
  if (c.classes.some((k) => !el.classes.includes(k))) return false;
  if (c.not.some((k) => el.classes.includes(k))) return false;
  if (c.attrs.some((a) => !(el.attrs || []).includes(a))) return false;
  return true;
}

// later reports whether cascade key a beats b: compared left to right
// (importance, specificity, then source order).
function later(a, b) {
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return a[i] > b[i];
  return false;
}

// display of el (with its ancestors, nearest first) under the cascade.
function display(all, el, ancestors, width) {
  let best = null;
  for (const r of all) {
    if (!mediaMatches(r.media, width)) continue;
    const parts = r.sel.split(/\s+/).map(compound);
    if (!matchesEl(parts[parts.length - 1], el)) continue;
    // Descendant combinators: each earlier part matches some ancestor, in order.
    let k = parts.length - 2;
    for (const a of ancestors) if (k >= 0 && matchesEl(parts[k], a)) k--;
    if (k >= 0) continue;
    const key = [r.important ? 1 : 0, ...specificity(parts), r.order];
    if (!best || later(key, best.key)) {
      best = { key, display: r.display };
    }
  }
  return best ? best.display : 'block';
}

function layout(width, chatting) {
  const all = rules(css);
  const body = { tag: 'body', id: '', classes: chatting ? ['chatting'] : [] };
  const app = { tag: 'div', id: 'app', classes: ['app'] };
  const list = { tag: 'section', id: 'pane-list', classes: ['pane'] };
  const chat = { tag: 'section', id: 'pane-chat', classes: ['pane'] };
  const header = { tag: 'header', id: '', classes: [] };
  const back = { tag: 'button', id: 'back', classes: [] };
  return {
    list: display(all, list, [app, body], width),
    chat: display(all, chat, [app, body], width),
    back: display(all, back, [header, chat, app, body], width),
  };
}

test('the cascade reader works on a known case', () => {
  const all = rules('a{display:block} @media (min-width: 10px){ #x{display:flex} } body.c #x{display:none}');
  const x = { tag: 'a', id: 'x', classes: [] };
  assert.equal(display(all, x, [{ tag: 'body', id: '', classes: ['c'] }], 20), 'none', 'more specific wins over a media rule');
  assert.equal(display(all, x, [{ tag: 'body', id: '', classes: [] }], 20), 'flex');
  assert.equal(display(all, x, [{ tag: 'body', id: '', classes: [] }], 5), 'block');
});

test('a wide screen always shows the list beside the chat, with no Back', () => {
  for (const width of [720, 1280]) {
    for (const chatting of [false, true]) {
      const l = layout(width, chatting);
      assert.notEqual(l.list, 'none', `list at ${width}px, chatting=${chatting}`);
      assert.notEqual(l.chat, 'none', `chat pane at ${width}px, chatting=${chatting}`);
      assert.equal(l.back, 'none', `Back at ${width}px, chatting=${chatting}`);
    }
  }
});

test('a phone shows one pane, and Back whenever the list is hidden', () => {
  for (const width of [360, 719]) {
    const list = layout(width, false);
    assert.notEqual(list.list, 'none', `list at ${width}px`);
    assert.equal(list.chat, 'none', `chat pane hidden at ${width}px with no chat`);
    const chat = layout(width, true);
    assert.equal(chat.list, 'none', `list hidden at ${width}px with a chat open`);
    assert.notEqual(chat.chat, 'none', `chat pane at ${width}px`);
    assert.notEqual(chat.back, 'none', `Back at ${width}px with a chat open`);
  }
});
