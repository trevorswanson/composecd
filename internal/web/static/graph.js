// Draws a graph of objects: dagre lays it out left to right, cards are HTML
// over an SVG layer of edges. An app's graph shows its objects, and on request
// the other stacks' containers sharing its networks and volumes; a host's
// shows everything on it: an overview of its stacks and what they share, or
// every object boxed by stack, and either narrowed to one stack or object and
// what it is connected to. The server re-renders the graph's data on every
// live update; the view (pan, zoom, filters, focus) is kept per graph.
(function () {
  'use strict';
  var SIZE = { service: [232, 64], container: [216, 52] };
  var DEFAULT_SIZE = [200, 52];
  // Room a stack box keeps around its nodes, and above them for its name.
  var PAD = 12, LABEL = 22;
  // The overview's stack cards, and the gaps of its grid.
  var STACK = [248, 84], GAP = 16, BAND = 64;
  // A host with more stacks than this opens on the overview.
  var MANY = 6;
  // A focused object with more connections than this lists them in a grid.
  var FAN = 8;
  // NONE groups the objects no stack created.
  var NONE = 'stacks/';
  var views = {};
  var SVG = 'http://www.w3.org/2000/svg';

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) { e.className = cls; }
    if (text) { e.textContent = text; }
    return e;
  }

  function svg(tag, attrs) {
    var e = document.createElementNS(SVG, tag);
    Object.keys(attrs).forEach(function (k) { e.setAttribute(k, attrs[k]); });
    return e;
  }

  function lower(s) { return String(s || '').toLowerCase(); }

  // card is a node; one without a dialog is only shown.
  function card(n) {
    var cls = 'gnode k-' + n.kind + (n.sync ? ' s-' + lower(n.sync) : '') + (n.neighbor ? ' neighbor' : '') +
      (n.outside ? ' outside' : '');
    var b = el(n.dialog ? 'button' : 'div', cls);
    var state = [n.sync, n.health].filter(Boolean).join(', ');
    if (n.dialog) {
      b.type = 'button';
      b.dataset.dialog = n.dialog;
      b.setAttribute('aria-label', n.kind + ' ' + n.name + (state ? ': ' + state : ''));
    } else {
      b.classList.add('static');
    }
    b.dataset.id = n.id;
    b.dataset.sync = n.sync || '';
    // Outside a focus, a card with nothing to open moves the focus to its stack.
    if (n.outside && !n.dialog) {
      b.dataset.focus = n.focus;
      b.title = 'Show ' + n.sub.split(' · ')[0];
    }
    b.title = b.title || n.message || '';
    b.appendChild(el('span', 'gicon k-' + n.kind));
    var text = el('span', 'gtext');
    text.appendChild(el('span', 'gname', n.name));
    text.appendChild(el('span', 'gsub', n.sub || n.kind));
    b.appendChild(text);
    var badges = el('span', 'gbadges');
    if (n.sync) {
      badges.appendChild(el('span', 'gb sync ' + lower(n.sync)));
      badges.lastChild.title = n.sync;
    }
    if (n.health) {
      badges.appendChild(el('span', 'gb health ' + lower(n.health)));
      badges.lastChild.title = n.health;
    }
    b.appendChild(badges);
    return b;
  }

  // edges are [from, to, class, label] for layout; draw says which way the
  // arrow points (depends_on points at the dependency, laid out to its left).
  function edges(nodes) {
    var ids = {};
    nodes.forEach(function (n) { ids[n.id] = n; });
    var out = [];
    nodes.forEach(function (n) {
      var cls = n.neighbor ? ' e-neighbor' : '';
      (n.uses || []).forEach(function (u) {
        if (ids[u]) { out.push({ from: u, to: n.id, cls: 'e-' + ids[u].kind + cls }); }
      });
      (n.dependsOn || []).forEach(function (d) {
        if (ids[d]) { out.push({ from: d, to: n.id, cls: 'e-depends', label: 'depends_on', reverse: true }); }
      });
      if (n.parent && ids[n.parent]) { out.push({ from: n.parent, to: n.id, cls: 'e-container' }); }
    });
    return out;
  }

  // layout boxes nodes by stack when it can: dagre's compound layout fails on
  // some graphs, which are then laid out unboxed.
  function layout(nodes, links, stacks) {
    if (stacks.length > 0) {
      try {
        return dagreLayout(nodes, links, stacks);
      } catch (err) {
        return dagreLayout(nodes, links, []);
      }
    }
    return dagreLayout(nodes, links, []);
  }

  function dagreLayout(nodes, links, stacks) {
    var boxed = stacks.length > 0;
    var g = new dagre.graphlib.Graph({ compound: boxed });
    // edgesep also spaces a stack box's edges from the nodes around it.
    g.setGraph({ rankdir: 'LR', nodesep: 20, edgesep: boxed ? 2 * (PAD + LABEL) : 20, ranksep: 72,
      marginx: 24, marginy: 24 });
    g.setDefaultEdgeLabel(function () { return {}; });
    stacks.forEach(function (s) { g.setNode(s.id, {}); });
    nodes.forEach(function (n) {
      var size = SIZE[n.kind] || DEFAULT_SIZE;
      g.setNode(n.id, { width: size[0], height: size[1] });
      if (boxed && n.stack) { g.setParent(n.id, n.stack); }
    });
    links.forEach(function (e) { g.setEdge(e.from, e.to); });
    dagre.layout(g);
    return g;
  }

  // curve joins a's right side to b's left side; with reverse, the arrow
  // starts at b and ends on a.
  function curve(a, b, reverse) {
    var x1 = a.x + a.width / 2, y1 = a.y, x2 = b.x - b.width / 2, y2 = b.y;
    var dx = Math.max(32, (x2 - x1) / 2);
    if (reverse) {
      return 'M' + x2 + ',' + y2 + ' C' + (x2 - dx) + ',' + y2 + ' ' + (x1 + dx) + ',' + y1 + ' ' + (x1 + 6) + ',' + y1;
    }
    return 'M' + x1 + ',' + y1 + ' C' + (x1 + dx) + ',' + y1 + ' ' + (x2 - dx) + ',' + y2 + ' ' + x2 + ',' + y2;
  }

  function apply(view, canvas) {
    canvas.style.transform = 'translate(' + view.x + 'px,' + view.y + 'px) scale(' + view.k + ')';
  }

  // fit scales the graph to the box's width (on a phone, no smaller than
  // can be read; it pans from there), sizes the box to it and centers it.
  function fit(view, host, w, h) {
    var narrow = host.clientWidth < 560;
    var k = Math.min(1, (host.clientWidth - 16) / w);
    view.k = Math.max(narrow ? 0.6 : 0.3, k);
    host.style.height = Math.min(Math.max(h * view.k + 48, 240), Math.max(window.innerHeight * 0.7, 360)) + 'px';
    view.k = Math.min(view.k, Math.max(narrow ? 0.6 : 0.3, (host.clientHeight - 16) / h));
    view.x = Math.max(8, (host.clientWidth - w * view.k) / 2);
    view.y = Math.max(8, (host.clientHeight - h * view.k) / 2);
  }

  // filter dims what the sync filter leaves out.
  function filter(wrap, view) {
    wrap.querySelectorAll('[data-filter]').forEach(function (b) {
      b.setAttribute('aria-pressed', String(b.dataset.filter === view.filter));
    });
    wrap.querySelectorAll('.gnode[data-sync]').forEach(function (n) {
      n.classList.toggle('dim', view.filter !== 'all' && n.dataset.sync !== view.filter);
    });
  }

  // model reads a host graph: each object's group (its stack, or NONE), and
  // the networks and volumes shared across groups.
  function model(graph) {
    var m = { byId: {}, names: {}, groups: {}, order: [], shared: [], links: edges(graph.nodes) };
    m.names[NONE] = 'No stack';
    (graph.stacks || []).forEach(function (st) { m.names[st.id] = st.name; m.groups[st.id] = { stack: st, nodes: [], uses: {} }; });
    graph.nodes.forEach(function (n) {
      m.byId[n.id] = n;
      var id = n.stack || NONE;
      if (!m.groups[id]) { m.groups[id] = { stack: { id: NONE, name: 'No stack', kind: 'Standalone' }, nodes: [], uses: {} }; }
      m.groups[id].nodes.push(n);
    });
    m.order = Object.keys(m.groups).sort(function (a, b) {
      return a === NONE ? 1 : b === NONE ? -1 : m.names[a] < m.names[b] ? -1 : 1;
    });
    var users = {};
    m.links.forEach(function (e) {
      var r = m.byId[e.from], c = m.byId[e.to];
      if (r.kind !== 'network' && r.kind !== 'volume') { return; }
      (users[r.id] = users[r.id] || {})[c.stack || NONE] = true;
    });
    graph.nodes.forEach(function (r) {
      var by = Object.keys(users[r.id] || {});
      if (by.some(function (g) { return g !== (r.stack || NONE); })) {
        m.shared.push(r);
        by.forEach(function (g) { m.groups[g].uses[r.id] = true; });
      }
    });
    return m;
  }

  // label says which stack a node outside the focus belongs to.
  function label(m, n) {
    var name = m.names[n.stack || NONE];
    return n.kind === 'container' ? name + ' · ' + (n.sub || '') : name;
  }

  // focused narrows a host graph to one stack (or object) and what it is
  // connected to; only the focused stack keeps its box.
  function focused(m, focus) {
    var core = {};
    if (m.groups[focus]) {
      m.groups[focus].nodes.forEach(function (n) { core[n.id] = true; });
    } else if (m.byId[focus]) {
      core[focus] = true;
    }
    var keep = {};
    Object.keys(core).forEach(function (id) { keep[id] = true; });
    m.links.forEach(function (e) {
      if (core[e.from] || core[e.to]) { keep[e.from] = true; keep[e.to] = true; }
    });
    var nodes = Object.keys(keep).map(function (id) {
      var n = m.byId[id];
      if (core[id] && m.groups[focus]) { return n; }
      var copy = Object.assign({}, n, { outside: !core[id], sub: core[id] ? n.sub : label(m, n), focus: n.stack || NONE });
      delete copy.stack;
      return copy;
    });
    var stacks = m.groups[focus] && focus !== NONE ? [m.groups[focus].stack] : [];
    // Outside nodes that hang off one core node are its fan; a big fan is
    // drawn as a grid under its own heading rather than one tall rank.
    var fans = {};
    m.links.forEach(function (e) {
      var leaf = core[e.from] ? e.to : core[e.to] ? e.from : '';
      var hub = leaf === e.to ? e.from : e.to;
      if (leaf && !core[leaf]) {
        var f = fans[leaf] || (fans[leaf] = {});
        f[hub] = true;
      }
    });
    var byHub = {};
    Object.keys(fans).forEach(function (leaf) {
      var hubs = Object.keys(fans[leaf]);
      if (hubs.length === 1) { (byHub[hubs[0]] = byHub[hubs[0]] || []).push(leaf); }
    });
    var grids = [];
    Object.keys(byHub).sort().forEach(function (hub) {
      if (byHub[hub].length > FAN) { grids.push({ hub: hub, leaves: byHub[hub].sort() }); }
    });
    return { nodes: nodes, stacks: stacks, grids: grids };
  }

  // worst is a group's health from its containers'.
  function worst(nodes) {
    var rank = { degraded: 3, missing: 2, progressing: 1, healthy: 0 };
    var out = '';
    nodes.forEach(function (n) {
      var h = lower(n.health);
      if (n.kind === 'container' && h && (out === '' || (rank[h] || 0) > (rank[out] || 0))) { out = h; }
    });
    return out;
  }

  function plural(n, word) { return n + ' ' + word + (n === 1 ? '' : 's'); }

  function stackCard(m, id) {
    var grp = m.groups[id], st = grp.stack;
    var b = el('button', 'gnode gstack-card' + (id === NONE ? ' none' : ''));
    b.type = 'button';
    b.dataset.focus = id;
    b.dataset.id = id;
    b.setAttribute('aria-label', 'Show ' + st.name + ' and what it is connected to');
    b.appendChild(el('span', 'gicon k-stack'));
    var text = el('span', 'gtext');
    var head = el('span', 'gname', st.name);
    if (st.href) { head.appendChild(el('span', 'tag', 'app')); }
    b.title = st.kind;
    text.appendChild(head);
    var count = { container: 0, network: 0, volume: 0 };
    grp.nodes.forEach(function (n) { count[n.kind] = (count[n.kind] || 0) + 1; });
    text.appendChild(el('span', 'gsub', [plural(count.container, 'container'),
      count.network ? plural(count.network, 'network') : '', count.volume ? plural(count.volume, 'volume') : '']
      .filter(Boolean).join(' · ')));
    var uses = Object.keys(grp.uses).filter(function (r) { return (m.byId[r].stack || NONE) !== id; });
    var owned = Object.keys(grp.uses).length - uses.length;
    var chips = el('span', 'gchips');
    uses.slice(0, 3).forEach(function (r) { chips.appendChild(el('span', 'gchip k-' + m.byId[r].kind, m.byId[r].name)); });
    if (uses.length > 3) { chips.appendChild(el('span', 'gchip', '+' + (uses.length - 3))); }
    if (owned) { chips.appendChild(el('span', 'gchip shares', 'shares ' + owned)); }
    text.appendChild(chips);
    b.appendChild(text);
    var badges = el('span', 'gbadges');
    var h = worst(grp.nodes);
    if (h) {
      badges.appendChild(el('span', 'gb health ' + h));
      badges.lastChild.title = h;
    }
    b.appendChild(badges);
    return b;
  }

  // vcurve joins a's bottom to b's top.
  function vcurve(a, b) {
    var x1 = a.x, y1 = a.y + a.height / 2, x2 = b.x, y2 = b.y - b.height / 2;
    var dy = Math.max(24, (y2 - y1) / 2);
    return 'M' + x1 + ',' + y1 + ' C' + x1 + ',' + (y1 + dy) + ' ' + x2 + ',' + (y2 - dy) + ' ' + x2 + ',' + y2;
  }

  // overview draws the shared networks and volumes in a band above a grid of
  // stacks, and their links only for the card pointed at, so a host with
  // dozens of stacks on one proxy network stays legible.
  function overview(host, wrap, m, view) {
    var width = Math.max(host.clientWidth, STACK[0] + 48);
    var at = {};
    var y = 24;
    function grid(ids, size) {
      var cols = Math.max(1, Math.floor((width - 48 + GAP) / (size[0] + GAP)));
      var left = (width - (cols * (size[0] + GAP) - GAP)) / 2;
      ids.forEach(function (id, i) {
        at[id] = { x: left + (i % cols) * (size[0] + GAP) + size[0] / 2, y: y + Math.floor(i / cols) * (size[1] + GAP) + size[1] / 2,
          width: size[0], height: size[1] };
      });
      if (ids.length) { y += Math.ceil(ids.length / cols) * (size[1] + GAP) - GAP; }
    }
    var shared = m.shared.map(function (r) { return r.id; });
    grid(shared, DEFAULT_SIZE);
    if (shared.length) { y += BAND; }
    grid(m.order, STACK);
    var size = { width: width, height: y + 24 };

    var canvas = el('div', 'graph-canvas overview');
    canvas.style.width = size.width + 'px';
    canvas.style.height = size.height + 'px';
    var lines = svg('svg', { width: size.width, height: size.height, 'class': 'graph-edges', 'aria-hidden': 'true' });
    canvas.appendChild(lines);
    if (shared.length) {
      var band = el('span', 'gband', 'Shared between stacks');
      band.style.top = (at[shared[0]].y - DEFAULT_SIZE[1] / 2 - 20) + 'px';
      canvas.appendChild(band);
    }
    m.shared.forEach(function (r) {
      var c = card(Object.assign({}, r, { sub: label(m, r), dialog: '' }));
      c.dataset.focus = r.id;
      place(c, at[r.id]);
      canvas.appendChild(c);
    });
    m.order.forEach(function (id) {
      var c = stackCard(m, id);
      place(c, at[id]);
      canvas.appendChild(c);
    });

    // Pointing at a card draws its links and dims what it doesn't touch.
    function show(id) {
      lines.replaceChildren();
      var touch = {};
      if (id) {
        touch[id] = true;
        m.order.forEach(function (g) {
          Object.keys(m.groups[g].uses).forEach(function (r) {
            if (g !== id && r !== id) { return; }
            touch[g] = touch[r] = true;
            lines.appendChild(svg('path', { d: vcurve(at[r], at[g]), 'class': 'gedge e-' + m.byId[r].kind }));
          });
        });
      }
      canvas.querySelectorAll('[data-focus]').forEach(function (c) {
        c.classList.toggle('dim', Boolean(id) && !touch[c.dataset.focus]);
      });
    }
    canvas.addEventListener('pointerover', function (event) {
      var c = event.target.closest('[data-focus]');
      show(c ? c.dataset.focus : '');
    });
    canvas.addEventListener('pointerleave', function () { show(''); });
    canvas.addEventListener('focusin', function (event) {
      var c = event.target.closest('[data-focus]');
      show(c ? c.dataset.focus : '');
    });
    return { canvas: canvas, size: size, natural: true };
  }

  function place(c, p) {
    c.style.left = (p.x - p.width / 2) + 'px';
    c.style.top = (p.y - p.height / 2) + 'px';
    c.style.width = p.width + 'px';
    c.style.height = p.height + 'px';
  }

  // laidOut lays nodes out with dagre, boxed by stacks.
  function laidOut(nodes, stacks, grids, room) {
    grids = grids || [];
    var gridded = {};
    grids.forEach(function (gr) { gr.leaves.forEach(function (id) { gridded[id] = true; }); });
    var all = nodes;
    nodes = nodes.filter(function (n) { return !gridded[n.id]; });
    var links = edges(nodes);
    var g = layout(nodes, links, stacks);
    var size = g.graph();
    var at = {};
    nodes.forEach(function (n) { at[n.id] = g.node(n.id); });
    // Each grid goes under the graph: a heading, then its nodes in rows.
    var heads = [];
    if (grids.length) {
      var width = Math.max(size.width, room || 0, 4 * (SIZE.container[0] + GAP) + 48);
      var cols = Math.floor((width - 48 + GAP) / (SIZE.container[0] + GAP));
      var y = size.height;
      grids.forEach(function (gr) {
        heads.push({ hub: gr.hub, count: gr.leaves.length, x: 24, y: y + 8 });
        y += 40;
        gr.leaves.forEach(function (id, i) {
          at[id] = { x: 24 + (i % cols) * (SIZE.container[0] + GAP) + SIZE.container[0] / 2,
            y: y + Math.floor(i / cols) * (SIZE.container[1] + GAP) + SIZE.container[1] / 2,
            width: SIZE.container[0], height: SIZE.container[1] };
        });
        y += Math.ceil(gr.leaves.length / cols) * (SIZE.container[1] + GAP) + 24;
      });
      size = { width: width, height: y };
    }
    var canvas = el('div', 'graph-canvas');
    canvas.style.width = size.width + 'px';
    canvas.style.height = size.height + 'px';
    stacks.forEach(function (st) {
      var p = g.node(st.id);
      if (!p || !p.width) { return; }
      var box = el('div', 'gstack');
      box.dataset.stack = st.id;
      // dagre leaves the room for PAD and LABEL inside the box (see layout).
      place(box, p);
      var name = el(st.href ? 'a' : 'span', 'gstack-name', st.name);
      if (st.href) { name.href = st.href; }
      name.title = st.kind;
      box.appendChild(name);
      box.appendChild(el('span', 'gstack-kind', st.kind));
      canvas.appendChild(box);
    });
    var lines = svg('svg', { width: size.width, height: size.height, 'class': 'graph-edges', 'aria-hidden': 'true' });
    var defs = svg('defs', {});
    var marker = svg('marker', { id: 'g-arrow', viewBox: '0 0 10 10', refX: 9, refY: 5, markerWidth: 8, markerHeight: 8, orient: 'auto-start-reverse' });
    marker.appendChild(svg('path', { d: 'M0,0 L10,5 L0,10 z', 'class': 'g-arrowhead' }));
    defs.appendChild(marker);
    lines.appendChild(defs);
    links.forEach(function (e) {
      var a = at[e.from], b = at[e.to];
      var path = svg('path', { d: curve(a, b, e.reverse), 'class': 'gedge ' + e.cls });
      if (e.reverse) { path.setAttribute('marker-end', 'url(#g-arrow)'); }
      lines.appendChild(path);
      if (e.label) {
        var t = svg('text', { x: (a.x + a.width / 2 + b.x - b.width / 2) / 2, y: (a.y + b.y) / 2 - 6, 'class': 'glabel' });
        t.textContent = e.label;
        lines.appendChild(t);
      }
    });
    heads.forEach(function (h) {
      var hub = all.filter(function (n) { return n.id === h.hub; })[0];
      var head = el('span', 'gfan', (hub ? hub.name : h.hub) + ' · ' + h.count + ' connected');
      head.style.left = h.x + 'px';
      head.style.top = h.y + 'px';
      canvas.appendChild(head);
      // One line from the hub to its heading stands for the whole grid.
      var a = at[h.hub];
      lines.appendChild(svg('path', { d: vcurve(a, { x: h.x + 60, y: h.y + 10, width: 0, height: 0 }),
        'class': 'gedge e-' + (hub ? hub.kind : 'container') + ' e-fan' }));
    });
    canvas.appendChild(lines);
    all.forEach(function (n) {
      var c = card(n);
      if (n.stack) { c.dataset.stack = n.stack; }
      place(c, at[n.id]);
      canvas.appendChild(c);
    });
    return { canvas: canvas, size: size };
  }

  // render draws the graph as the view asks and replaces the canvas; it
  // returns what the controls act on.
  function render(host, wrap, graph, view) {
    var cur;
    if (graph.stacks) {
      var m = model(graph);
      if (view.focus && !m.groups[view.focus] && !m.byId[view.focus]) { view.focus = ''; }
      if (view.focus) {
        var f = focused(m, view.focus);
        cur = laidOut(f.nodes, f.stacks, f.grids, host.clientWidth);
        // A focus as narrow as the box scrolls with the page, unshrunk.
        cur.natural = cur.size.width <= host.clientWidth && !view.moved;
      } else if (view.mode === 'stacks') {
        cur = overview(host, wrap, m, view);
      } else {
        cur = laidOut(graph.nodes, graph.stacks);
      }
      tools(wrap, m, view);
    } else {
      var shown = graph.nodes.filter(function (n) { return view.neighbors || !n.neighbor; });
      var byHub = {};
      shown.forEach(function (n) {
        if (n.neighbor && (n.uses || []).length === 1) { (byHub[n.uses[0]] = byHub[n.uses[0]] || []).push(n.id); }
      });
      var grids = Object.keys(byHub).sort().filter(function (hub) { return byHub[hub].length > FAN; })
        .map(function (hub) { return { hub: hub, leaves: byHub[hub] }; });
      cur = laidOut(shown, [], grids, host.clientWidth);
      // A grid of neighbors reads best unshrunk, scrolling with the page.
      cur.natural = grids.length > 0 && cur.size.width <= host.clientWidth && !view.moved;
    }
    if (cur.natural) {
      // The overview is as tall as its grid; the page scrolls it.
      view.k = 1;
      view.x = Math.max(0, (host.clientWidth - cur.size.width) / 2);
      view.y = 0;
      host.style.height = cur.size.height + 'px';
    } else if (!view.moved || !view.height) {
      // Until someone pans or zooms, each update fits the graph again.
      fit(view, host, cur.size.width, cur.size.height);
    }
    view.height = host.style.height;
    host.classList.toggle('natural', Boolean(cur.natural));
    wrap.querySelectorAll('.zoom').forEach(function (z) { z.hidden = Boolean(cur.natural); });
    host.replaceChildren(cur.canvas);
    apply(view, cur.canvas);
    filter(wrap, view);
    return cur;
  }

  // tools shows the host graph's mode and focus.
  function tools(wrap, m, view) {
    wrap.querySelectorAll('[data-mode]').forEach(function (b) {
      b.setAttribute('aria-pressed', String(!view.focus && b.dataset.mode === view.mode));
    });
    var pick = wrap.querySelector('[data-stack]');
    if (pick) { pick.value = m.groups[view.focus] ? view.focus : ''; }
    var clear = wrap.querySelector('[data-clear]');
    if (clear) { clear.hidden = !view.focus; }
    var summary = wrap.querySelector('[data-summary]');
    if (summary) {
      summary.textContent = view.focus ? (m.names[view.focus] || (m.byId[view.focus] || {}).name) + ' and what it is connected to'
        : summary.dataset.summary;
    }
  }

  function draw() {
    // No ids: htmx's settle step would reset the attributes set here.
    var host = document.querySelector('.graph');
    var wrap = host && host.closest('.graph-wrap');
    var data = wrap && wrap.querySelector('script[type="application/json"]');
    if (!data || host.dataset.drawn || typeof dagre === 'undefined') { return; }
    var graph = JSON.parse(data.textContent);
    var view = views[graph.key] || (views[graph.key] = { filter: 'all', focus: '', neighbors: false,
      mode: (graph.stacks || []).length > MANY ? 'stacks' : 'all' });
    // A live update swaps in a fresh, unsized box: keep the last one's height
    // before anything reads the layout, or the page shrinks and the browser
    // scrolls up to its new end.
    if (view.height) { host.style.height = view.height; }
    // A swap can run this before the new graph is laid out; try once it is.
    if (!host.isConnected || host.clientWidth === 0) { requestAnimationFrame(draw); return; }
    host.dataset.drawn = 'true';
    // Live updates re-render the page for any change, often just a clock:
    // when the graph's data is the same, its drawing (and hover) moves over.
    var cur;
    if (view.cur && view.data === data.textContent) {
      cur = view.cur;
      host.style.height = view.height;
      host.classList.toggle('natural', Boolean(cur.natural));
      wrap.querySelectorAll('.zoom').forEach(function (z) { z.hidden = Boolean(cur.natural); });
      host.replaceChildren(cur.canvas);
      if (graph.stacks) { tools(wrap, model(graph), view); }
      filter(wrap, view);
    } else {
      cur = render(host, wrap, graph, view);
    }
    view.cur = cur;
    view.data = data.textContent;
    function again() {
      view.moved = false;
      cur = view.cur = render(host, wrap, graph, view);
    }

    // Pan by dragging the background; a drag that starts on a card doesn't
    // open it. The overview scrolls with the page instead.
    var drag = null;
    host.addEventListener('pointerdown', function (event) {
      if (event.button !== 0 || event.target.closest('a') || cur.natural) { return; }
      drag = { x: event.clientX, y: event.clientY, vx: view.x, vy: view.y, moved: false };
    });
    host.addEventListener('pointermove', function (event) {
      if (!drag) { return; }
      var dx = event.clientX - drag.x, dy = event.clientY - drag.y;
      if (!drag.moved && Math.abs(dx) + Math.abs(dy) < 4) { return; }
      if (!drag.moved) { host.setPointerCapture(event.pointerId); }
      drag.moved = true;
      view.x = drag.vx + dx;
      view.y = drag.vy + dy;
      view.moved = true;
      apply(view, cur.canvas);
    });
    host.addEventListener('pointerup', function () {
      if (drag && drag.moved) { host.dataset.dragged = 'true'; }
      drag = null;
    });
    host.addEventListener('click', function (event) {
      if (host.dataset.dragged) {
        delete host.dataset.dragged;
        event.stopPropagation();
        event.preventDefault();
        return;
      }
      // A stack or shared object on the overview focuses the graph on it.
      var f = event.target.closest('[data-focus]');
      if (f) {
        view.focus = f.dataset.focus;
        again();
      }
    }, true);
    // Pinch (and ctrl+wheel) zooms; a plain wheel scrolls the page.
    host.addEventListener('wheel', function (event) {
      if ((!event.ctrlKey && !event.metaKey) || cur.natural) { return; }
      event.preventDefault();
      var rect = host.getBoundingClientRect();
      zoom(view, cur.canvas, Math.exp(-event.deltaY / 200), event.clientX - rect.left, event.clientY - rect.top);
    }, { passive: false });
    wrap.querySelectorAll('[data-zoom]').forEach(function (b) {
      b.addEventListener('click', function () {
        if (cur.natural) { return; }
        if (b.dataset.zoom === 'fit') {
          view.moved = false;
          fit(view, host, cur.size.width, cur.size.height);
          view.height = host.style.height;
          apply(view, cur.canvas);
          return;
        }
        zoom(view, cur.canvas, b.dataset.zoom === 'in' ? 1.25 : 0.8, host.clientWidth / 2, host.clientHeight / 2);
      });
    });
    wrap.querySelectorAll('[data-filter]').forEach(function (b) {
      b.addEventListener('click', function () { view.filter = b.dataset.filter; filter(wrap, view); });
    });
    wrap.querySelectorAll('[data-mode]').forEach(function (b) {
      b.addEventListener('click', function () { view.mode = b.dataset.mode; view.focus = ''; again(); });
    });
    var pick = wrap.querySelector('[data-stack]');
    if (pick) {
      pick.addEventListener('change', function () { view.focus = pick.value; again(); });
    }
    var clear = wrap.querySelector('[data-clear]');
    if (clear) {
      clear.addEventListener('click', function () { view.focus = ''; again(); });
    }
    var toggle = wrap.querySelector('[data-neighbors]');
    if (toggle) {
      toggle.setAttribute('aria-pressed', String(view.neighbors));
      toggle.addEventListener('click', function () {
        view.neighbors = !view.neighbors;
        toggle.setAttribute('aria-pressed', String(view.neighbors));
        again();
      });
    }
  }

  function zoom(view, canvas, factor, cx, cy) {
    var k = Math.min(2, Math.max(0.25, view.k * factor));
    view.x = cx - (cx - view.x) * (k / view.k);
    view.y = cy - (cy - view.y) * (k / view.k);
    view.k = k;
    view.moved = true;
    apply(view, canvas);
  }

  document.addEventListener('DOMContentLoaded', draw);
  // Live updates replace #content out of band.
  document.addEventListener('htmx:oobAfterSwap', draw);
  document.addEventListener('htmx:afterSettle', draw);
  if (document.readyState !== 'loading') { draw(); }
})();
