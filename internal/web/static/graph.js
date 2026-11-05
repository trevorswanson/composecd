// Draws a graph of objects: cards are HTML over an SVG layer of edges. An
// app's graph shows its objects, laid out left to right by dagre, and on
// request the other stacks' containers sharing its networks and volumes. A
// host's shows everything on it, every object boxed by stack: stacks that
// share networks or volumes are clustered around them, the clusters packed in
// rows, and each stack can collapse to a card. It can be narrowed to one stack
// or object and what it is connected to, and searched. The server re-renders
// the graph's data on every live update; the view (pan, zoom, filters, focus,
// collapsed stacks) is kept per graph. A host's graph keeps one height, so
// nothing it draws moves the page.
(function () {
  'use strict';
  var SIZE = { service: [232, 64], container: [216, 52] };
  var DEFAULT_SIZE = [200, 52];
  // Room a stack box keeps around its nodes, and above them for its name.
  var PAD = 12, LABEL = 22;
  // A collapsed stack's card, the gap between cards, and the room under the
  // band of hubs.
  var STACK = [248, 84], GAP = 16, BAND = 64;
  // The gap between a box's networks and its containers, and between the
  // columns of a cluster; between clusters; and around the host graph.
  var RANK = 56, BLOCK = 40, MARGIN = 24;
  // A host with more stacks than this opens with them collapsed.
  var MANY = 6;
  // A focused object with more connections than this lists them in a grid.
  var FAN = 8;
  // NONE groups the objects no stack created.
  var NONE = 'stacks/';
  // How far a host graph zooms out to fit.
  var MIN_ZOOM = 0.15;
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
  // can be read; it pans from there), sizes the box to it and centers it. A
  // fixed box (a host's) keeps its height, and the graph shrinks to it.
  function fit(view, host, w, h, fixed) {
    var narrow = host.clientWidth < 560;
    var k = Math.min(1, (host.clientWidth - 16) / w);
    view.k = Math.max(narrow ? 0.6 : fixed ? MIN_ZOOM : 0.3, k);
    if (fixed) {
      view.k = Math.max(MIN_ZOOM, Math.min(view.k, (host.clientHeight - 16) / h));
      view.x = Math.max(8, (host.clientWidth - w * view.k) / 2);
      view.y = Math.max(8, (host.clientHeight - h * view.k) / 2);
      return;
    }
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

  // model reads a host graph: each stack's objects, the objects shared
  // between stacks, and which of those each stack uses.
  function model(graph) {
    var m = { graph: graph, byId: {}, names: {}, groups: {}, order: [], shared: [], links: edges(graph.nodes) };
    m.names[NONE] = 'No stack';
    (graph.stacks || []).forEach(function (st) { m.names[st.id] = st.name; m.groups[st.id] = { stack: st, nodes: [], uses: {} }; });
    graph.nodes.forEach(function (n) {
      m.byId[n.id] = n;
      if (n.shared || !n.stack) {
        m.shared.push(n);
        return;
      }
      if (!m.groups[n.stack]) {
        m.names[n.stack] = m.names[n.stack] || n.stack.slice(NONE.length) || 'No stack';
        m.groups[n.stack] = { stack: { id: n.stack, name: m.names[n.stack], kind: 'Compose project' }, nodes: [], uses: {} };
      }
      m.groups[n.stack].nodes.push(n);
    });
    m.order = Object.keys(m.groups).sort(function (a, b) {
      return a === NONE ? 1 : b === NONE ? -1 : m.names[a] < m.names[b] ? -1 : 1;
    });
    m.links.forEach(function (e) {
      var r = m.byId[e.from], c = m.byId[e.to];
      if (m.shared.indexOf(r) >= 0 && c.stack && m.groups[c.stack]) { m.groups[c.stack].uses[r.id] = true; }
    });
    return m;
  }

  // label says which stack a node outside the focus (or a shared one)
  // belongs to.
  function label(m, n) {
    if (!n.stack) {
      return n.owner ? 'from ' + (m.names[n.owner] || n.owner.slice(NONE.length)) : 'shared';
    }
    var name = m.names[n.stack];
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
      var copy = Object.assign({}, n, { outside: !core[id], sub: core[id] ? n.sub : label(m, n), focus: n.stack || n.id });
      delete copy.stack;
      return copy;
    });
    var stacks = m.groups[focus] ? [m.groups[focus].stack] : [];
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
    b.dataset.toggle = id;
    b.dataset.stack = id;
    b.setAttribute('aria-label', 'Expand ' + st.name);
    b.appendChild(el('span', 'gicon k-stack'));
    var text = el('span', 'gtext');
    var head = el('span', 'gname', st.name);
    if (st.href) { head.appendChild(el('span', 'tag', 'app')); }
    b.title = st.kind + ' · click to expand';
    text.appendChild(head);
    var count = { container: 0, network: 0, volume: 0 };
    grp.nodes.forEach(function (n) { count[n.kind] = (count[n.kind] || 0) + 1; });
    text.appendChild(el('span', 'gsub', [plural(count.container, 'container'),
      count.network ? plural(count.network, 'network') : '', count.volume ? plural(count.volume, 'volume') : '']
      .filter(Boolean).join(' · ')));
    var uses = Object.keys(grp.uses).sort();
    var chips = el('span', 'gchips');
    uses.slice(0, 3).forEach(function (r) {
      chips.appendChild(el('span', 'gchip k-' + m.byId[r].kind, m.byId[r].name));
      if (m.byId[r].hub) { chips.lastChild.dataset.hub = r; }
    });
    if (uses.length > 3) { chips.appendChild(el('span', 'gchip', '+' + (uses.length - 3))); }
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

  // boxLayout lays a stack's objects out inside its box: its networks and
  // volumes in a column, most used first, and its containers in a grid beside
  // them, in the order of what they use, so a big stack stays roughly square.
  // A collapsed stack is one card.
  function boxLayout(nodes, collapsed) {
    if (collapsed) { return { width: STACK[0], height: STACK[1], at: {}, collapsed: true }; }
    var res = nodes.filter(function (n) { return n.kind !== 'container'; });
    var cts = nodes.filter(function (n) { return n.kind === 'container'; });
    var used = {};
    cts.forEach(function (c) { (c.uses || []).forEach(function (u) { used[u] = (used[u] || 0) + 1; }); });
    res.sort(function (a, b) { return (used[b.id] || 0) - (used[a.id] || 0) || (a.name < b.name ? -1 : 1); });
    var idx = {};
    res.forEach(function (r, i) { idx[r.id] = i; });
    function first(c) {
      return Math.min.apply(null, [res.length].concat((c.uses || []).filter(function (u) { return u in idx; })
        .map(function (u) { return idx[u]; })));
    }
    cts.sort(function (a, b) { return first(a) - first(b) || (a.name < b.name ? -1 : 1); });
    var cw = SIZE.container[0], ch = SIZE.container[1], rw = DEFAULT_SIZE[0], rh = DEFAULT_SIZE[1];
    var cols = cts.length <= 6 ? 1 : Math.ceil(Math.sqrt(cts.length / 2.5));
    var rows = Math.ceil(cts.length / cols);
    var gridH = rows ? rows * (ch + GAP) - GAP : 0, resH = res.length ? res.length * (rh + GAP) - GAP : 0;
    var innerH = Math.max(gridH, resH), top = LABEL + PAD;
    var left = PAD + (res.length ? rw + (cts.length ? RANK : 0) : 0);
    var at = {};
    res.forEach(function (r, i) {
      at[r.id] = { x: PAD + rw / 2, y: top + (innerH - resH) / 2 + i * (rh + GAP) + rh / 2, width: rw, height: rh };
    });
    cts.forEach(function (c, i) {
      var col = Math.floor(i / rows), row = i % rows;
      at[c.id] = { x: left + col * (cw + GAP) + cw / 2, y: top + (innerH - gridH) / 2 + row * (ch + GAP) + ch / 2,
        width: cw, height: ch };
    });
    var width = cts.length ? left + cols * (cw + GAP) - GAP + PAD : PAD + rw + PAD;
    return { width: Math.max(width, STACK[0]), height: top + innerH + PAD, at: at };
  }

  // clusterLayout places a cluster's stacks around the networks and volumes
  // they share: dagre ranks the shared objects between two columns of stacks,
  // split to balance their heights, so each shared object sits between its
  // users. It returns the cluster's size and the centers of its parts.
  function clusterLayout(cl, boxes, users) {
    if (!cl.shared || !cl.shared.length) {
      var b = boxes[cl.stacks[0]];
      var one = {};
      one[cl.stacks[0]] = { x: b.width / 2, y: b.height / 2 };
      return { width: b.width, height: b.height, at: one };
    }
    var g = new dagre.graphlib.Graph();
    g.setGraph({ rankdir: 'LR', nodesep: 24, ranksep: RANK, marginx: 0, marginy: 0 });
    g.setDefaultEdgeLabel(function () { return {}; });
    var order = cl.stacks.slice().sort(function (a, b) { return boxes[b].height - boxes[a].height || (a < b ? -1 : 1); });
    var leftH = 0, rightH = 0, side = {};
    order.forEach(function (s) {
      side[s] = leftH <= rightH ? 'left' : 'right';
      if (side[s] === 'left') { leftH += boxes[s].height; } else { rightH += boxes[s].height; }
      g.setNode(s, { width: boxes[s].width, height: boxes[s].height });
    });
    cl.shared.forEach(function (r) {
      g.setNode(r, { width: DEFAULT_SIZE[0], height: DEFAULT_SIZE[1] });
      Object.keys(users[r] || {}).forEach(function (s) {
        if (!side[s]) { return; }
        if (side[s] === 'left') { g.setEdge(s, r); } else { g.setEdge(r, s); }
      });
    });
    dagre.layout(g);
    var at = {};
    g.nodes().forEach(function (id) { var p = g.node(id); at[id] = { x: p.x, y: p.y }; });
    return { width: g.graph().width, height: g.graph().height, at: at };
  }

  // pack places blocks, in order, each as high and then as far left as it
  // fits in width (a skyline packing), so small clusters fill in beside big
  // ones. It returns each block's top left and the size of the whole.
  function pack(blocks, width) {
    var sky = [{ x: 0, w: width, y: 0 }], at = [], w = 0, h = 0;
    blocks.forEach(function (b) {
      var bw = b.width + BLOCK, pick = null;
      sky.forEach(function (seg, i) {
        if (seg.x + b.width > width && seg.x > 0) { return; }
        var y = 0, j = i;
        for (var end = seg.x + bw; j < sky.length && sky[j].x < end; j++) { y = Math.max(y, sky[j].y); }
        if (!pick || y < pick.y) { pick = { x: seg.x, y: y }; }
      });
      at.push(pick);
      var next = [], end = pick.x + bw;
      sky.forEach(function (seg) {
        if (seg.x + seg.w <= pick.x || seg.x >= end) { next.push(seg); return; }
        if (seg.x < pick.x) { next.push({ x: seg.x, w: pick.x - seg.x, y: seg.y }); }
        if (seg.x + seg.w > end) { next.push({ x: end, w: seg.x + seg.w - end, y: seg.y }); }
      });
      next.push({ x: pick.x, w: bw, y: pick.y + b.height + BLOCK });
      sky = next.sort(function (a, c) { return a.x - c.x; });
      w = Math.max(w, pick.x + b.width);
      h = Math.max(h, pick.y + b.height);
    });
    return { at: at, width: w, height: h };
  }

  // hostLayout draws a host's graph: each cluster of stacks around what they
  // share, the clusters packed in rows to the shape of the box, and the hubs
  // (shared by so many stacks their edges would cross everything) in a band
  // above, their edges drawn only for what is pointed at.
  function hostLayout(host, m, graph, view) {
    function collapsed(id) { return id in view.collapsed ? view.collapsed[id] : view.mode === 'stacks'; }
    var boxes = {};
    m.order.forEach(function (id) { boxes[id] = boxLayout(m.groups[id].nodes, collapsed(id)); });
    var users = {};
    m.links.forEach(function (e) {
      var to = m.byId[e.to];
      if (m.byId[e.from].shared && to.stack) { (users[e.from] = users[e.from] || {})[to.stack] = true; }
    });
    var clusters = (graph.clusters || []).filter(function (cl) { return cl.stacks.every(function (s) { return boxes[s]; }); });
    var placed = {};
    clusters.forEach(function (cl) { cl.stacks.forEach(function (s) { placed[s] = true; }); });
    m.order.forEach(function (s) { if (!placed[s]) { clusters.push({ stacks: [s] }); } });
    var blocks = clusters.map(function (cl) { return clusterLayout(cl, boxes, users); });
    var hubs = m.shared.filter(function (n) { return n.hub; });
    var hw = hubs.length ? hubs.length * (DEFAULT_SIZE[0] + GAP) - GAP : 0;
    var top = MARGIN + (hubs.length ? 20 + DEFAULT_SIZE[1] + BAND : 0);
    // Try a range of widths and keep the packing that shows biggest in the box.
    var widest = hw, total = 0;
    blocks.forEach(function (b) { widest = Math.max(widest, b.width); total += b.width + BLOCK; });
    var best = null;
    for (var i = 0; i <= 24; i++) {
      var w = widest + (Math.max(total, widest) - widest) * Math.pow(i / 24, 2);
      var packed = pack(blocks, w);
      var k = Math.min(host.clientWidth / (packed.width + 2 * MARGIN), host.clientHeight / (packed.height + top + MARGIN));
      if (!best || k > best.k + 1e-9) { best = { k: k, packed: packed, width: Math.max(w, hw) }; }
    }
    var width = Math.max(best.packed.width, hw);
    var at = {}, boxAt = {};
    hubs.forEach(function (n, i) {
      at[n.id] = { x: MARGIN + (width - hw) / 2 + i * (DEFAULT_SIZE[0] + GAP) + DEFAULT_SIZE[0] / 2,
        y: MARGIN + 20 + DEFAULT_SIZE[1] / 2, width: DEFAULT_SIZE[0], height: DEFAULT_SIZE[1] };
    });
    blocks.forEach(function (b, i) {
      var x = best.packed.at[i].x, y = top + best.packed.at[i].y;
      Object.keys(b.at).forEach(function (id) {
        var p = b.at[id], cx = MARGIN + x + p.x, cy = y + p.y;
        if (boxes[id]) {
          var bx = boxes[id];
          boxAt[id] = { x: cx, y: cy, width: bx.width, height: bx.height };
          Object.keys(bx.at).forEach(function (n) {
            var q = bx.at[n];
            at[n] = { x: cx - bx.width / 2 + q.x, y: cy - bx.height / 2 + q.y, width: q.width, height: q.height };
          });
        } else {
          at[id] = { x: cx, y: cy, width: DEFAULT_SIZE[0], height: DEFAULT_SIZE[1] };
        }
      });
    });
    var size = { width: width + 2 * MARGIN, height: top + best.packed.height + MARGIN };

    var canvas = el('div', 'graph-canvas');
    canvas.style.width = size.width + 'px';
    canvas.style.height = size.height + 'px';
    if (hubs.length) {
      var band = el('span', 'gband', 'Shared by many stacks');
      band.style.top = MARGIN + 'px';
      band.style.left = (MARGIN + (width - (hubs.length * (DEFAULT_SIZE[0] + GAP) - GAP)) / 2) + 'px';
      canvas.appendChild(band);
    }
    var els = {};
    m.order.forEach(function (id) {
      var p = boxAt[id];
      if (!p) { return; }
      var st = m.groups[id].stack, c;
      if (boxes[id].collapsed) {
        c = stackCard(m, id);
        place(c, p);
      } else {
        c = el('div', 'gstack');
        place(c, p);
        var head = el('div', 'gstack-head');
        var tog = el('button', 'gtoggle');
        tog.type = 'button';
        tog.dataset.toggle = id;
        tog.setAttribute('aria-label', 'Collapse ' + st.name);
        tog.title = 'Collapse';
        head.appendChild(tog);
        var name = el(st.href ? 'a' : 'span', 'gstack-name', st.name);
        if (st.href) { name.href = st.href; }
        name.title = st.kind;
        head.appendChild(name);
        (st.hubs || []).forEach(function (h) {
          var chip = el('span', 'gchip k-' + m.byId[h].kind, m.byId[h].name);
          chip.dataset.hub = h;
          chip.title = 'Uses ' + m.byId[h].name + ', shared by many stacks';
          head.appendChild(chip);
        });
        var foc = el('button', 'gfocus');
        foc.type = 'button';
        foc.dataset.focus = id;
        foc.setAttribute('aria-label', 'Show only ' + st.name + ' and what it is connected to');
        foc.title = 'Show only this and what it is connected to';
        head.appendChild(foc);
        c.appendChild(head);
      }
      c.dataset.stack = id;
      els[id] = c;
      canvas.appendChild(c);
    });
    var lines = svg('svg', { width: size.width, height: size.height, 'class': 'graph-edges', 'aria-hidden': 'true' });
    canvas.appendChild(lines);
    // An object in a collapsed stack is drawn as its stack's card.
    function rep(id) {
      var n = m.byId[id];
      return n.stack && boxes[n.stack] && boxes[n.stack].collapsed ? n.stack : id;
    }
    function pos(id) { return at[id] || boxAt[id]; }
    var drawn = [], seen = {}, hubEdges = [];
    m.links.forEach(function (e) {
      var a = rep(e.from), b = rep(e.to);
      if (a === b || seen[a + '>' + b] || !pos(a) || !pos(b)) { return; }
      seen[a + '>' + b] = true;
      var d = { from: a, to: b, cls: 'gedge ' + e.cls };
      if (m.byId[e.from].hub) { hubEdges.push(d); return; }
      d.el = svg('path', { d: link(pos(a), pos(b)), 'class': d.cls });
      lines.appendChild(d.el);
      drawn.push(d);
    });
    m.graph.nodes.forEach(function (n) {
      if (!at[n.id]) { return; }
      var c = card(n.shared ? Object.assign({}, n, { sub: label(m, n) }) : n);
      place(c, at[n.id]);
      els[n.id] = c;
      canvas.appendChild(c);
    });

    // Pointing at an object (or a hub's chip) shows what it is connected to,
    // drawing a hub's edges, and dims the rest.
    var all = drawn.concat(hubEdges);
    var lit = [];
    function show(id) {
      lit.forEach(function (p) { p.remove(); });
      lit = [];
      canvas.classList.toggle('lit', Boolean(id));
      var touch = {};
      if (id) {
        touch[id] = true;
        all.forEach(function (d) {
          if (d.from !== id && d.to !== id) { return; }
          touch[d.from] = touch[d.to] = true;
          if (!d.el) {
            var p = svg('path', { d: link(pos(d.from), pos(d.to)), 'class': d.cls + ' hot' });
            lines.appendChild(p);
            lit.push(p);
          }
        });
      }
      drawn.forEach(function (d) {
        d.el.classList.toggle('hot', Boolean(id) && (d.from === id || d.to === id));
        d.el.classList.toggle('dim', Boolean(id) && d.from !== id && d.to !== id);
      });
      Object.keys(els).forEach(function (k) {
        var e = els[k];
        if (e.classList.contains('gstack')) {
          e.classList.toggle('dim', Boolean(id) && !m.groups[k].nodes.some(function (n) { return touch[n.id]; }));
        } else {
          e.classList.toggle('dim', Boolean(id) && !touch[k]);
        }
      });
    }
    function target(event) {
      var t = event.target.closest('[data-hub], .gnode');
      if (!t) { return ''; }
      return t.dataset.hub || t.dataset.id || t.dataset.stack || '';
    }
    canvas.addEventListener('pointerover', function (event) { if (!view.search) { show(target(event)); } });
    canvas.addEventListener('pointerleave', function () { if (!view.search) { show(''); } });
    canvas.addEventListener('focusin', function (event) { if (!view.search) { show(target(event)); } });
    return { canvas: canvas, size: size, at: function (id) { return pos(rep(id)); }, els: els, show: show };
  }

  // link joins a's side to b's facing side, or top to bottom when they overlap.
  function link(a, b) {
    if (b.x - b.width / 2 >= a.x + a.width / 2) { return curve(a, b); }
    if (a.x - a.width / 2 >= b.x + b.width / 2) { return curve(b, a); }
    return a.y < b.y ? vcurve(a, b) : vcurve(b, a);
  }

  // search marks the objects whose name, image or stack has q in it (or the
  // collapsed stacks they are in) and dims the rest; it returns the marked.
  function search(m, cur, q) {
    q = lower(q).trim();
    var hits = {};
    if (q) {
      m.graph.nodes.forEach(function (n) {
        var stack = n.stack ? m.names[n.stack] : '';
        if ([n.name, n.sub, stack].some(function (s) { return lower(s).indexOf(q) >= 0; })) {
          hits[cur.els[n.id] ? n.id : n.stack] = true;
        }
      });
    }
    Object.keys(cur.els).forEach(function (k) {
      var e = cur.els[k];
      if (e.classList.contains('gstack')) {
        e.classList.toggle('dim', Boolean(q) && !m.groups[k].nodes.some(function (n) { return hits[n.id]; }));
        return;
      }
      e.classList.toggle('match', Boolean(hits[k]));
      e.classList.toggle('dim', Boolean(q) && !hits[k]);
    });
    cur.canvas.classList.toggle('lit', Boolean(q));
    return Object.keys(hits).filter(function (k) { return cur.els[k]; });
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
      } else {
        cur = hostLayout(host, m, graph, view);
      }
      cur.model = m;
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
    // A host's graph keeps the height its stylesheet gives it, whatever it
    // draws: a box that grew or shrank with its content moved everything
    // under it, and left the page scrolled past the graph.
    var fixed = Boolean(graph.stacks);
    if (cur.natural) {
      // A grid of neighbors is as tall as it is; the page scrolls it.
      view.k = 1;
      view.x = Math.max(0, (host.clientWidth - cur.size.width) / 2);
      view.y = 0;
      host.style.height = cur.size.height + 'px';
    } else if (!view.moved || (!fixed && !view.height)) {
      // Until someone pans or zooms, each update fits the graph again.
      fit(view, host, cur.size.width, cur.size.height, fixed);
    }
    if (!fixed) { view.height = host.style.height; }
    host.classList.toggle('natural', Boolean(cur.natural));
    wrap.querySelectorAll('.zoom').forEach(function (z) { z.hidden = Boolean(cur.natural); });
    host.replaceChildren(cur.canvas);
    apply(view, cur.canvas);
    filter(wrap, view);
    find(wrap, view, cur, false);
    return cur;
  }

  // find applies the host graph's search; with go, it moves to the next
  // match, panning it into the middle of the box.
  function find(wrap, view, cur, go) {
    var input = wrap.querySelector('[data-search]');
    var count = wrap.querySelector('[data-matches]');
    if (!input || !cur.model || !cur.els) { return; }
    if (input.value !== view.search) { input.value = view.search; }
    var hits = search(cur.model, cur, view.search);
    if (count) { count.textContent = view.search ? hits.length + (hits.length === 1 ? ' match' : ' matches') : ''; }
    if (!go || !hits.length) { return; }
    view.hit = ((view.hit || 0) + 1) % hits.length;
    var p = cur.at(hits[view.hit]);
    var host = cur.canvas.parentNode;
    if (!p || !host) { return; }
    view.k = Math.max(view.k, 0.8);
    view.x = host.clientWidth / 2 - p.x * view.k;
    view.y = host.clientHeight / 2 - p.y * view.k;
    view.moved = true;
    apply(view, cur.canvas);
    var el = cur.els[hits[view.hit]];
    if (el) { el.focus({ preventScroll: true }); }
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
    var view = views[graph.key] || (views[graph.key] = { filter: 'all', focus: '', neighbors: false, search: '',
      collapsed: {}, mode: (graph.stacks || []).length > MANY ? 'stacks' : 'all' });
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
      if (graph.stacks) { tools(wrap, cur.model || model(graph), view); }
      filter(wrap, view);
      find(wrap, view, cur, false);
    } else {
      cur = render(host, wrap, graph, view);
    }
    view.cur = cur;
    view.data = data.textContent;
    function again() {
      view.moved = false;
      cur = view.cur = render(host, wrap, graph, view);
    }
    // relayout draws the graph again where it is, keeping the stack at id
    // where it was on screen.
    function relayout(id) {
      var before = cur.at && cur.at(id);
      var k = view.k;
      cur = view.cur = render(host, wrap, graph, view);
      var after = cur.at && cur.at(id);
      if (before && after && view.moved) {
        view.x += (before.x - before.width / 2 - (after.x - after.width / 2)) * k;
        view.y += (before.y - before.height / 2 - (after.y - after.height / 2)) * k;
        apply(view, cur.canvas);
      }
    }
    // The box is overflow: hidden, but focusing a card outside it would still
    // scroll it; pan to the card instead.
    host.addEventListener('scroll', function () { host.scrollTop = 0; host.scrollLeft = 0; });
    host.addEventListener('focusin', function (event) {
      var c = event.target.closest('.gnode');
      if (!c || drag) { return; }
      var r = c.getBoundingClientRect(), b = host.getBoundingClientRect();
      if (r.left >= b.left && r.right <= b.right && r.top >= b.top && r.bottom <= b.bottom) { return; }
      view.x += b.left + b.width / 2 - (r.left + r.width / 2);
      view.y += b.top + b.height / 2 - (r.top + r.height / 2);
      view.moved = true;
      apply(view, cur.canvas);
    });

    // Pan by dragging the background; a drag that starts on a card doesn't
    // open it. A grid of neighbors scrolls with the page instead.
    var drag = null;
    host.addEventListener('pointerdown', function (event) {
      if (event.button !== 0 || event.target.closest('a, input') || cur.natural) { return; }
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
      // A stack's card or toggle expands or collapses it in place.
      var t = event.target.closest('[data-toggle]');
      if (t) {
        event.preventDefault();
        var id = t.dataset.toggle;
        view.collapsed[id] = !(id in view.collapsed ? view.collapsed[id] : view.mode === 'stacks');
        view.moved = true;
        relayout(id);
        return;
      }
      // A focus button narrows the graph to its stack or object.
      var f = event.target.closest('[data-focus]');
      if (f) {
        event.preventDefault();
        view.focus = f.dataset.focus;
        again();
      }
    }, true);
    var input = wrap.querySelector('[data-search]');
    if (input) {
      input.addEventListener('input', function () {
        view.search = input.value;
        view.hit = -1;
        if (cur.show) { cur.show(''); }
        find(wrap, view, cur, false);
      });
      input.addEventListener('keydown', function (event) {
        if (event.key === 'Enter') {
          event.preventDefault();
          if (view.focus) { view.focus = ''; again(); }
          find(wrap, view, cur, true);
        } else if (event.key === 'Escape') {
          input.value = view.search = '';
          find(wrap, view, cur, false);
        }
      });
    }
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
          fit(view, host, cur.size.width, cur.size.height, Boolean(graph.stacks));
          if (!graph.stacks) { view.height = host.style.height; }
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
      b.addEventListener('click', function () { view.mode = b.dataset.mode; view.focus = ''; view.collapsed = {}; again(); });
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
    var k = Math.min(2, Math.max(MIN_ZOOM, view.k * factor));
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
