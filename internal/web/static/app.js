// Offgrid Workspace - Linear/Raycast Style Tabbed Console
// Zero runtime dependencies, safe DOM generation conforming to strict CSP.
"use strict";

const POLL_MS = 2000;
const $ = (id) => document.getElementById(id);
let last = null;
let lastAt = 0;
let failing = 0;
let busy = false;
let currentTab = "overview";
const expandedNotes = new Set();

function h(tag, attrs, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v == null) continue;
    if (k === "class") e.className = v;
    else if (k === "style") e.style.cssText = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v === true ? "" : v);
  }
  for (const k of kids.flat()) {
    if (k == null || k === false) continue;
    e.append(k.nodeType ? k : document.createTextNode(String(k)));
  }
  return e;
}

const clock = (iso) => new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });

function ago(iso, now) {
  if (!iso) return "never";
  const s = Math.max(0, Math.round((new Date(now) - new Date(iso)) / 1000));
  if (s < 5) return "just now";
  if (s < 60) return s + "s ago";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h " + Math.floor((s % 3600) / 60) + "m ago";
  return Math.floor(s / 86400) + "d ago";
}

function span(ms) {
  if (ms < 1000) return ms + " ms";
  const s = ms / 1000;
  if (s < 60) return s.toFixed(1) + " s";
  return Math.floor(s / 60) + "m " + Math.round(s % 60) + "s";
}

const label = (s) => String(s).replace(/_/g, " ");
const tag = (kind, text) => h("span", { class: "tag " + kind }, text ?? label(kind));
const empty = (t) => h("p", { class: "empty" }, t);

function card(title, pillText, ...kids) {
  const head = h("div", { class: "card-header" },
    h("h2", { class: "card-title" }, title),
    pillText ? h("span", { class: "card-pill" }, pillText) : null
  );
  return h("section", { class: "card" }, head, ...kids);
}

// ---- Summary Metrics ------------------------------------------------------

function metricsStrip(s) {
  const c = s.connection;
  const probeSub = c.probe ? (span(c.probe.latency_ms) + " latency · " + c.probes_ok + " probes ok") : "probe stand-alone";

  const cardConn = h("div", { class: "metric-card" },
    h("span", { class: "metric-lbl" }, "Connection"),
    h("div", { class: "metric-val" }, c.known ? c.state : "Offline"),
    h("div", { class: "metric-meta" }, probeSub)
  );

  const cardLog = h("div", { class: "metric-card" },
    h("span", { class: "metric-lbl" }, "Store Consensus"),
    h("div", { class: "metric-val" }, s.log.ops + " Ops · " + s.log.tasks + " Tasks"),
    h("div", { class: "metric-meta mono" }, "Hash: " + (s.log.hash ? s.log.hash.slice(0, 8) : "none"))
  );

  const staleCount = s.suggestions.filter((g) => g.stale).length;
  const readyCount = s.suggestions.length - staleCount;
  const sugSub = s.suggestions.length ? (readyCount + " ready to apply" + (staleCount ? " · " + staleCount + " conflict" : "")) : "all caught up";

  const cardSug = h("div", { class: "metric-card" },
    h("span", { class: "metric-lbl" }, "AI Proposals"),
    h("div", { class: "metric-val" }, s.suggestions.length + " Pending"),
    h("div", { class: "metric-meta" }, sugSub)
  );

  const q = s.queue || { queued: 0, running: 0, done: 0, dead: 0 };
  const cardQueue = h("div", { class: "metric-card" },
    h("span", { class: "metric-lbl" }, "Worker Queue"),
    h("div", { class: "metric-val" }, q.done + " Done · " + (q.queued + q.running) + " Active"),
    h("div", { class: "metric-meta" }, q.running > 0 ? "worker active" : "idle · drain ready")
  );

  return h("div", { class: "metrics-strip" }, cardConn, cardLog, cardSug, cardQueue);
}

// ---- Task Table Component -------------------------------------------------

function renderTasksTable(s, limit) {
  const list = limit ? s.tasks.slice(0, limit) : s.tasks;
  if (!list.length) return empty("No tasks recorded in this replica.");

  const rows = [];
  list.forEach((t) => {
    const isExpanded = expandedNotes.has(t.id);
    const hasNotes = t.notes_list && t.notes_list.length > 0;
    const tooltip = hasNotes ? t.notes_list.map((n, i) => `${i + 1}. ${n}`).join("\n") : "No notes attached";

    rows.push(h("tr", {
      class: hasNotes ? "clickable-task" : "",
      onclick: () => {
        if (!hasNotes) return;
        if (expandedNotes.has(t.id)) expandedNotes.delete(t.id);
        else expandedNotes.add(t.id);
        render(s);
      }
    },
      h("td", null, h("span", { class: "task-id" }, t.short_id)),
      h("td", null,
        h("span", { style: "font-weight:600;" }, t.title),
        t.site ? h("span", { class: "site-badge" }, "Site " + t.site) : null
      ),
      h("td", null, tag(t.status)),
      h("td", null, tag(t.priority)),
      h("td", { class: "num muted", title: tooltip },
        t.notes ? h("span", { class: hasNotes ? "notes-badge" : "" }, t.notes) : "—"
      ),
      h("td", { class: "num muted" }, ago(t.updated_at, s.now))
    ));

    if (isExpanded && hasNotes) {
      rows.push(h("tr", { class: "task-notes-row" },
        h("td", { colspan: 6 },
          h("div", { class: "task-notes-expanded" },
            h("div", { class: "task-notes-title" }, "Notes (" + t.notes_list.length + ")"),
            h("ul", { class: "task-notes-list" },
              t.notes_list.map((note) => h("li", null, note))
            )
          )
        )
      ));
    }
  });

  return h("div", { class: "table-wrap" },
    h("table", null,
      h("tr", null,
        h("th", null, "ID"),
        h("th", null, "Title"),
        h("th", null, "Status"),
        h("th", null, "Priority"),
        h("th", { class: "num" }, "Notes"),
        h("th", { class: "num" }, "Updated")
      ),
      rows
    )
  );
}

// ---- AI Review Cards Component -------------------------------------------

function renderSuggestionsList(s) {
  if (!s.suggestions.length) {
    return empty("No pending AI proposals. Offline models generate reviewable drafts as tasks evolve.");
  }

  const items = s.suggestions.map((g) => {
    const diffs = [];
    if (g.status) {
      diffs.push(h("div", { class: "diff-cell" },
        h("span", { class: "muted" }, "Status:"),
        tag(g.now_status || "—"),
        h("span", { class: "diff-arrow" }, "→"),
        tag(g.status)
      ));
    }
    if (g.priority) {
      diffs.push(h("div", { class: "diff-cell" },
        h("span", { class: "muted" }, "Priority:"),
        tag(g.now_priority || "—"),
        h("span", { class: "diff-arrow" }, "→"),
        tag(g.priority)
      ));
    }
    if (g.note) {
      diffs.push(h("div", { class: "diff-cell" },
        h("span", { class: "muted" }, "Note:"),
        h("span", null, "“" + g.note + "”")
      ));
    }

    const conflict = g.stale
      ? h("div", { class: "conflict-alert" },
          "⚠️ Remote Conflict: This task was updated on another device after this proposal was drafted. Auto-merge is locked to preserve integrity.")
      : null;

    const acceptBtn = h("button", {
      class: "btn-primary",
      disabled: g.stale || s.read_only || busy,
      onclick: () => resolve(g.id, "accept")
    }, "Accept Proposal");

    const dismissBtn = h("button", {
      disabled: s.read_only || busy,
      onclick: () => resolve(g.id, "dismiss")
    }, "Dismiss");

    return h("div", { class: "review-card" + (g.stale ? " has-conflict" : "") },
      h("div", { class: "review-card-head" },
        h("span", { class: "review-target" }, g.task_name),
        h("div", { class: "review-author" },
          g.by ? h("span", { class: "model-pill" }, g.by) : null,
          h("span", null, ago(g.at, s.now))
        )
      ),
      h("div", { class: "diff-box" }, diffs),
      conflict,
      h("div", { class: "review-actions" }, acceptBtn, dismissBtn)
    );
  });

  return h("div", { class: "review-list" }, items);
}

// ---- Connection Component ------------------------------------------------

function renderConnectionCard(s) {
  const c = s.connection;
  if (!c.known) return card("Connection Monitor", "idle", empty("Prober subsystem inactive. Start `offgrid drain` to activate."));

  const kids = [];
  if (c.probe) {
    kids.push(h("div", { style: "display:flex; justify-content:space-between; align-items:center; margin-bottom:6px;" },
      h("span", { class: "muted" }, "Probe Status:"),
      h("span", { class: c.probe.ok ? "ok" : "bad" }, c.probe.ok ? "Healthy (" + c.probe.latency_ms + " ms)" : "Unreachable")
    ));
    kids.push(h("div", { style: "display:flex; justify-content:space-between; align-items:center; font-size:13px; color:var(--text-muted);" },
      h("span", null, "Historical Probes:"),
      h("span", null, c.probes_ok + " ok / " + c.probes_failed + " failed")
    ));
  }

  if (c.probe_ms.length) {
    const top = Math.max(1, ...c.probe_ms);
    const bars = c.probe_ms.map((v) =>
      h("i", {
        class: v < 0 ? "fail" : "",
        style: "height:" + (v < 0 ? 100 : Math.max(8, Math.round((v / top) * 100))) + "%",
        title: v < 0 ? "Probe failed" : v + " ms"
      })
    );
    kids.push(h("div", { class: "spark" }, bars));
  }

  if (c.flips.length) {
    const flipItems = c.flips.slice(-4).reverse().map((f) =>
      h("li", null,
        h("time", { class: "feed-time" }, clock(f.at)),
        h("div", null,
          h("span", { style: "font-weight:600;" }, f.from + " → " + f.to),
          f.reason ? h("span", { class: "muted", style: "display:block; font-size:12px;" }, f.reason) : null
        )
      )
    );
    kids.push(h("div", { style: "margin-top:12px; border-top:1px solid var(--line); padding-top:8px;" },
      h("h2", { class: "card-subhead" }, "Recent Connectivity Flips"),
      h("ul", { class: "feed-list" }, flipItems)
    ));
  }

  return card("Connection Monitor", c.state, ...kids);
}

// ---- Routing & Queue Components -------------------------------------------

function renderRoutingCard(s) {
  const total = s.tiers.reduce((a, t) => a + t.n, 0);
  const rows = [];
  
  if (!total) {
    rows.push(empty("No messages classified yet."));
  } else {
    for (const t of s.tiers) {
      const pct = Math.max(4, Math.round(t.n * 100 / total));
      const lat = (t.p50_ms || t.p95_ms) ? " · " + span(t.p50_ms) : "";
      rows.push(h("div", { class: "tier-row" },
        h("span", { style: "font-weight:600;" }, label(t.name)),
        h("div", { class: "bar-track" }, h("div", { class: "bar " + t.name, style: "width:" + pct + "%" })),
        h("span", { class: "muted num", style: "font-size:13px;" }, t.n + (t.n === 1 ? " query" : " queries") + lat)
      ));
    }
  }

  const decisions = s.decisions.length
    ? h("ul", { class: "feed-list", style: "margin-top:10px; border-top:1px solid var(--line); padding-top:8px;" },
        s.decisions.slice(0, 4).map((d) =>
          h("li", null,
            h("time", { class: "feed-time" }, clock(d.at)),
            h("div", null,
              h("div", null, tag(d.tier), h("span", { style: "margin-left:6px; font-weight:500;" }, "“" + d.text + "”")),
              d.reason ? h("span", { class: "muted", style: "font-size:12px; display:block; margin-top:2px;" }, "— " + d.reason) : null
            )
          )
        )
      )
    : null;

  return card("Message Routing Tiers", total + " routed", ...rows, decisions);
}

function renderQueueCard(s) {
  if (!s.queue) return card("Worker Queue", "idle", empty("Job queue is inactive."));

  const kids = [];
  if (s.jobs.length) {
    const jobRows = s.jobs.map((j) => h("tr", null,
      h("td", null, h("b", null, j.kind)),
      h("td", { class: "num" }, j.done),
      h("td", { class: "num muted" }, (j.p50_ms || j.p95_ms) ? span(j.p50_ms) : "—")
    ));

    kids.push(h("div", { class: "table-wrap" },
      h("table", null,
        h("tr", null,
          h("th", null, "Worker"),
          h("th", { class: "num" }, "Done"),
          h("th", { class: "num" }, "p50")
        ),
        jobRows
      )
    ));
  } else {
    kids.push(empty("No worker jobs logged."));
  }

  if (s.procs && s.procs.length) {
    const procPills = s.procs.map((p) =>
      h("span", {
        class: "tag " + (p.stopped ? "low" : (p.fresh ? "done" : "in_progress")),
        style: "margin-right:6px;"
      }, p.name + ": " + (p.stopped ? "stopped" : "active"))
    );
    kids.push(h("div", { style: "margin-top:12px; border-top:1px solid var(--line); padding-top:8px;" },
      h("h2", { class: "card-subhead" }, "Background Daemons"),
      h("div", null, procPills)
    ));
  }

  return card("Worker Performance", (s.queue.done) + " completed", ...kids);
}

function renderFeedCard(s) {
  const items = s.feed.length
    ? s.feed.slice(0, 10).map((f) =>
        h("li", null,
          h("time", { class: "feed-time" }, clock(f.at)),
          h("span", { class: f.kind === "ok" || f.kind === "warn" || f.kind === "bad" ? f.kind : "" },
            f.proc ? h("span", { class: "proc-pill" }, f.proc) : null,
            f.text
          )
        )
      )
    : [empty("No system events logged.")];

  return card("Recent Activity", "realtime", h("ul", { class: "feed-list" }, items));
}

function renderReplicationCard(s) {
  const rows = s.replicas.map((r) => h("tr", null,
    h("td", null,
      h("b", null, r.node),
      r.local ? h("span", { class: "tag", style: "margin-left:8px;" }, "this machine") : h("span", { class: "tag open", style: "margin-left:8px;" }, "peer replica")
    ),
    h("td", { class: "num" }, r.ops + " ops"),
    h("td", { class: "num muted" }, r.ops ? ago(r.newest, s.now) : "—")
  ));

  const table = h("div", { class: "table-wrap" },
    h("table", null,
      h("tr", null,
        h("th", null, "Replica Node"),
        h("th", { class: "num" }, "Ops Synced"),
        h("th", { class: "num" }, "Last Activity")
      ),
      rows
    )
  );

  const merges = s.merges.length
    ? h("ul", { class: "feed-list", style: "margin-top:8px;" }, s.merges.map((m) =>
        h("li", null,
          h("time", { class: "feed-time" }, clock(m.at)),
          h("span", null, h("b", null, m.node), ": " + m.text)
        )))
    : empty("No inbound ops from peer replicas.");

  return card("P2P Replication & Merges", s.replicas.length + " nodes",
    table,
    h("div", { style: "margin-top:14px; border-top:1px solid var(--line); padding-top:10px;" },
      h("h2", { class: "card-subhead", style: "color:var(--text-main);" }, "Inbound Peer Commits Stream"),
      merges
    )
  );
}

// ---- Render Engine --------------------------------------------------------

function render(s) {
  // Update header replica identifier
  $("node").textContent = s.node;
  
  // Update connection pill
  const c = s.connection;
  const pill = $("pill");
  const stateCls = c.known ? String(c.state).toLowerCase() : "unknown";
  pill.className = "pill " + stateCls + (c.known && c.stale ? " stale" : "");
  pill.textContent = c.known ? c.state + (c.stale ? " (last known)" : "") : "offline";

  // Mode banner
  $("mode").textContent = s.read_only ? "🔒 Read-only enforcement active." : "";

  // Update tab counts
  if ($("count-tasks")) $("count-tasks").textContent = s.tasks.length;
  if ($("count-suggestions")) $("count-suggestions").textContent = s.suggestions.length;
  if ($("count-replicas")) $("count-replicas").textContent = s.replicas.length;

  const main = $("main");
  const viewWrap = h("div", { class: "tab-view" });

  if (currentTab === "overview") {
    const metrics = metricsStrip(s);
    const leftCol = h("div", { style: "display:flex; flex-direction:column; gap:16px;" },
      card("AI Proposals Queue", s.suggestions.length + " pending", renderSuggestionsList(s)),
      card("Tasks Registry (Overview)", "Top " + Math.min(3, s.tasks.length), renderTasksTable(s, 3))
    );
    const rightCol = h("div", { style: "display:flex; flex-direction:column; gap:16px;" },
      renderConnectionCard(s),
      renderFeedCard(s)
    );
    const split = h("div", { class: "view-split" }, leftCol, rightCol);
    viewWrap.append(metrics, split);
  } else if (currentTab === "tasks") {
    viewWrap.append(card("Tasks Registry", s.tasks.length + " tasks", renderTasksTable(s, 0)));
  } else if (currentTab === "suggestions") {
    viewWrap.append(card("AI Copilot Proposals", s.suggestions.length + " waiting for review", renderSuggestionsList(s)));
  } else if (currentTab === "replicas") {
    viewWrap.append(renderReplicationCard(s));
  } else if (currentTab === "diagnostics") {
    const diagLeft = h("div", { style: "display:flex; flex-direction:column; gap:16px;" },
      renderConnectionCard(s),
      renderRoutingCard(s)
    );
    const diagRight = h("div", { style: "display:flex; flex-direction:column; gap:16px;" },
      renderQueueCard(s),
      renderFeedCard(s)
    );
    viewWrap.append(h("div", { class: "view-split" }, diagLeft, diagRight));
  }

  main.replaceChildren(viewWrap);

  if (s.problems && s.problems.length) {
    const probCards = s.problems.map((p) => h("p", { class: "bad" }, "⚠️ " + p));
    main.prepend(h("section", { class: "card", style: "border-color: var(--line-strong); margin-bottom:16px;" },
      h("h2", { style: "color: #ffffff;" }, "Diagnostic Warnings"),
      probCards
    ));
  }

  main.setAttribute("aria-busy", "false");
}

function showStale() {
  const b = $("banner");
  if (failing === 0) {
    b.hidden = true;
    $("main").classList.remove("old");
    return;
  }
  b.hidden = false;
  b.textContent = "Unable to connect to local Offgrid daemon. Showing last known state as of " + (last ? clock(last.now) : "startup") + ".";
  $("main").classList.add("old");
}

function tick() {
  if (!last) return;
  const now = new Date(new Date(last.now).getTime() + (Date.now() - lastAt)).toISOString();
  $("clock").textContent = clock(now);
}

let toastTimer;
function toast(msg, isErr) {
  const t = $("toast");
  t.textContent = msg;
  t.className = "toast" + (isErr ? " err" : "");
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, isErr ? 8000 : 3500);
}

async function poll() {
  try {
    const r = await fetch("/api/state", { cache: "no-store" });
    if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || "HTTP " + r.status);
    last = await r.json();
    lastAt = Date.now();
    failing = 0;
    render(last);
  } catch (e) {
    failing++;
  }
  showStale();
}

async function resolve(id, action) {
  busy = true;
  if (last) render(last);
  try {
    const r = await fetch("/api/suggestions/" + encodeURIComponent(id) + "/" + action, {
      method: "POST",
      headers: { "X-Offgrid-Action": "1" }
    });
    const body = await r.json().catch(() => ({}));
    if (r.ok) toast("Proposal " + action + "ed.", false);
    else toast("Action failed: " + (body.error || "HTTP " + r.status), true);
  } catch (e) {
    toast("Connection error: Local daemon unreachable.", true);
  }
  busy = false;
  await poll();
}

// Tab Switching & Hash Routing
function updateTabUI() {
  const tabs = document.querySelectorAll(".tab-btn");
  tabs.forEach((b) => {
    const isSel = b.getAttribute("data-tab") === currentTab;
    b.classList.toggle("active", isSel);
    b.setAttribute("aria-selected", isSel ? "true" : "false");
  });
}

function syncFromHash() {
  const h = window.location.hash.replace(/^#/, "");
  if (["overview", "tasks", "suggestions", "replicas", "diagnostics"].includes(h)) {
    currentTab = h;
    updateTabUI();
    if (last) render(last);
  }
}

function setupTabs() {
  const tabs = document.querySelectorAll(".tab-btn");
  tabs.forEach((btn) => {
    btn.addEventListener("click", () => {
      const target = btn.getAttribute("data-tab");
      if (!target || target === currentTab) return;
      currentTab = target;
      window.location.hash = "#" + target;
      updateTabUI();
      if (last) render(last);
    });
  });
  window.addEventListener("hashchange", syncFromHash);
  syncFromHash();
}

function loop() {
  if (!document.hidden) poll();
  setTimeout(loop, POLL_MS);
}

document.addEventListener("visibilitychange", () => { if (!document.hidden) poll(); });
setInterval(tick, 1000);
setupTabs();
loop();
