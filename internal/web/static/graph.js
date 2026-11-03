// Draws a graph of objects: dagre lays it out left to right, cards are HTML
// over an SVG layer of edges. An app's graph shows its objects, and on request
// the other stacks' containers sharing its networks and volumes; a host's
// shows everything on it, boxed by stack. The server re-renders the graph's
// data on every live update; the view (pan, zoom, filters) is kept per graph.
(function () {
  'use strict';
  var SIZE = { service: [232, 64], container: [216, 52] };
  var DEFAULT_SIZE = [200, 52];
  // Room a stack box keeps around its nodes, and above them for its name.
  var PAD = 12, LABEL = 22;
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
    var cls = 'gnode k-' + n.kind + (n.sync ? ' s-' + lower(n.sync) : '') + (n.neighbor ? ' neighbor' : '');
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
    b.title = n.message || '';
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

  function layout(nodes, links, stacks) {
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

  // highlight dims what the sync filter or the chosen stack leaves out. A
  // stack keeps what its objects use and what uses them.
  function highlight(wrap, view, links) {
    wrap.querySelectorAll('[data-filter]').forEach(function (b) {
      b.setAttribute('aria-pressed', String(b.dataset.filter === view.filter));
    });
    var keep = null;
    if (view.stack) {
      keep = {};
      wrap.querySelectorAll('.gnode').forEach(function (n) {
        if (n.dataset.stack === view.stack) { keep[n.dataset.id] = true; }
      });
      links.forEach(function (e) {
        if (keep[e.from] === true || keep[e.to] === true) { keep[e.from] = keep[e.from] || 'near'; keep[e.to] = keep[e.to] || 'near'; }
      });
    }
    wrap.querySelectorAll('.gnode').forEach(function (n) {
      var off = view.filter !== 'all' && n.dataset.sync !== view.filter;
      n.classList.toggle('dim', off || (keep !== null && !keep[n.dataset.id]));
    });
    wrap.querySelectorAll('.gedge').forEach(function (p) {
      p.classList.toggle('dim', keep !== null && !(keep[p.dataset.from] === true || keep[p.dataset.to] === true));
    });
    wrap.querySelectorAll('.gstack').forEach(function (s) {
      s.classList.toggle('dim', keep !== null && s.dataset.stack !== view.stack);
    });
  }

  // render lays out the graph's nodes (with neighbors only when asked for)
  // and replaces the canvas; it returns what the controls act on.
  function render(host, wrap, graph, view) {
    var stacks = graph.stacks || [];
    var nodes = graph.nodes.filter(function (n) { return view.neighbors || !n.neighbor; });
    var links = edges(nodes);
    var g = layout(nodes, links, stacks);
    var size = g.graph();
    // Until someone pans or zooms, each update fits the graph again.
    if (!view.moved || !view.height) {
      fit(view, host, size.width, size.height);
      view.height = host.style.height;
    } else {
      host.style.height = view.height;
    }

    var canvas = el('div', 'graph-canvas');
    canvas.style.width = size.width + 'px';
    canvas.style.height = size.height + 'px';
    stacks.forEach(function (s) {
      var p = g.node(s.id);
      if (!p || !p.width) { return; }
      var box = el('div', 'gstack');
      box.dataset.stack = s.id;
      // dagre leaves the room for PAD and LABEL inside the box (see layout).
      box.style.left = (p.x - p.width / 2) + 'px';
      box.style.top = (p.y - p.height / 2) + 'px';
      box.style.width = p.width + 'px';
      box.style.height = p.height + 'px';
      var label = el(s.href ? 'a' : 'span', 'gstack-name', s.name);
      if (s.href) { label.href = s.href; }
      label.title = s.kind;
      box.appendChild(label);
      box.appendChild(el('span', 'gstack-kind', s.kind));
      canvas.appendChild(box);
    });
    var lines = svg('svg', { width: size.width, height: size.height, 'class': 'graph-edges', 'aria-hidden': 'true' });
    var defs = svg('defs', {});
    var marker = svg('marker', { id: 'g-arrow', viewBox: '0 0 10 10', refX: 9, refY: 5, markerWidth: 8, markerHeight: 8, orient: 'auto-start-reverse' });
    marker.appendChild(svg('path', { d: 'M0,0 L10,5 L0,10 z', 'class': 'g-arrowhead' }));
    defs.appendChild(marker);
    lines.appendChild(defs);
    links.forEach(function (e) {
      var a = g.node(e.from), b = g.node(e.to);
      var path = svg('path', { d: curve(a, b, e.reverse), 'class': 'gedge ' + e.cls, 'data-from': e.from, 'data-to': e.to });
      if (e.reverse) { path.setAttribute('marker-end', 'url(#g-arrow)'); }
      lines.appendChild(path);
      if (e.label) {
        var t = svg('text', { x: (a.x + a.width / 2 + b.x - b.width / 2) / 2, y: (a.y + b.y) / 2 - 6, 'class': 'glabel' });
        t.textContent = e.label;
        lines.appendChild(t);
      }
    });
    canvas.appendChild(lines);
    nodes.forEach(function (n) {
      var p = g.node(n.id);
      var c = card(n);
      if (n.stack) { c.dataset.stack = n.stack; }
      c.style.left = (p.x - p.width / 2) + 'px';
      c.style.top = (p.y - p.height / 2) + 'px';
      c.style.width = p.width + 'px';
      c.style.height = p.height + 'px';
      canvas.appendChild(c);
    });
    host.replaceChildren(canvas);
    apply(view, canvas);
    highlight(wrap, view, links);
    return { canvas: canvas, size: size, links: links };
  }

  function draw() {
    // No ids: htmx's settle step would reset the attributes set here.
    var host = document.querySelector('.graph');
    var wrap = host && host.closest('.graph-wrap');
    var data = wrap && wrap.querySelector('script[type="application/json"]');
    if (!data || host.dataset.drawn || typeof dagre === 'undefined') { return; }
    // A swap can run this before the new graph is laid out; try once it is.
    if (!host.isConnected || host.clientWidth === 0) { requestAnimationFrame(draw); return; }
    host.dataset.drawn = 'true';
    var graph = JSON.parse(data.textContent);
    var view = views[graph.key] || (views[graph.key] = { filter: 'all', stack: '', neighbors: false });
    var cur = render(host, wrap, graph, view);

    // Pan by dragging the background; a drag that starts on a card doesn't
    // open it.
    var drag = null;
    host.addEventListener('pointerdown', function (event) {
      if (event.button !== 0 || event.target.closest('a')) { return; }
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
      }
    }, true);
    // Pinch (and ctrl+wheel) zooms; a plain wheel scrolls the page.
    host.addEventListener('wheel', function (event) {
      if (!event.ctrlKey && !event.metaKey) { return; }
      event.preventDefault();
      var rect = host.getBoundingClientRect();
      zoom(view, cur.canvas, Math.exp(-event.deltaY / 200), event.clientX - rect.left, event.clientY - rect.top);
    }, { passive: false });
    wrap.querySelectorAll('[data-zoom]').forEach(function (b) {
      b.addEventListener('click', function () {
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
      b.addEventListener('click', function () { view.filter = b.dataset.filter; highlight(wrap, view, cur.links); });
    });
    var pick = wrap.querySelector('[data-stack]');
    if (pick) {
      pick.value = view.stack;
      pick.addEventListener('change', function () { view.stack = pick.value; highlight(wrap, view, cur.links); });
    }
    var toggle = wrap.querySelector('[data-neighbors]');
    if (toggle) {
      toggle.setAttribute('aria-pressed', String(view.neighbors));
      toggle.addEventListener('click', function () {
        view.neighbors = !view.neighbors;
        view.moved = false;
        toggle.setAttribute('aria-pressed', String(view.neighbors));
        cur = render(host, wrap, graph, view);
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
