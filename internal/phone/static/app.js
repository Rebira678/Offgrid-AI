// Offgrid Field PWA - Client Engine (Day 52)
// Handles offline reads, queued offline writes, Service Worker, and pairing.

const DB_NAME = 'offgrid_field_db';
const DB_VERSION = 1;
let db = null;
let currentFilter = 'all';
let isOnline = navigator.onLine;

// Register Service Worker for offline capability
if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch((err) => {
      console.warn('ServiceWorker registration error:', err);
    });
  });
}

// -------------------------------------------------------------
// IndexedDB Setup
// -------------------------------------------------------------
function openDatabase() {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, DB_VERSION);
    req.onupgradeneeded = (e) => {
      const d = e.target.result;
      if (!d.objectStoreNames.contains('meta')) {
        d.createObjectStore('meta');
      }
      if (!d.objectStoreNames.contains('tasks')) {
        d.createObjectStore('tasks', { keyPath: 'id' });
      }
      if (!d.objectStoreNames.contains('queue')) {
        d.createObjectStore('queue', { keyPath: 'op_id' });
      }
    };
    req.onsuccess = (e) => {
      db = e.target.result;
      resolve(db);
    };
    req.onerror = (e) => reject(e.target.error);
  });
}

function idbGet(storeName, key) {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(storeName, 'readonly');
    const req = tx.objectStore(storeName).get(key);
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

function idbSet(storeName, key, val) {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(storeName, 'readwrite');
    const req = key !== undefined ? tx.objectStore(storeName).put(val, key) : tx.objectStore(storeName).put(val);
    req.onsuccess = () => resolve();
    req.onerror = () => reject(req.error);
  });
}

function idbGetAll(storeName) {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(storeName, 'readonly');
    const req = tx.objectStore(storeName).getAll();
    req.onsuccess = () => resolve(req.result || []);
    req.onerror = () => reject(req.error);
  });
}

function idbDelete(storeName, key) {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(storeName, 'readwrite');
    const req = tx.objectStore(storeName).delete(key);
    req.onsuccess = () => resolve();
    req.onerror = () => reject(req.error);
  });
}

function idbClear(storeName) {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(storeName, 'readwrite');
    const req = tx.objectStore(storeName).clear();
    req.onsuccess = () => resolve();
    req.onerror = () => reject(req.error);
  });
}

// -------------------------------------------------------------
// Network & Sync Management
// -------------------------------------------------------------
window.addEventListener('online', () => {
  isOnline = true;
  updateStatusBadge();
  syncNow();
});

window.addEventListener('offline', () => {
  isOnline = false;
  updateStatusBadge();
});

async function updateStatusBadge() {
  const badge = document.getElementById('status-indicator');
  const text = document.getElementById('status-text');
  const queued = await idbGetAll('queue');

  badge.className = 'status-badge';
  if (!isOnline) {
    badge.classList.add('status-offline');
    text.textContent = queued.length > 0 ? `OFFLINE (${queued.length} QUEUED)` : 'OFFLINE';
  } else if (queued.length > 0) {
    badge.classList.add('status-syncing');
    text.textContent = `SYNCING (${queued.length})...`;
  } else {
    badge.classList.add('status-online');
    text.textContent = 'ONLINE';
  }
}

// -------------------------------------------------------------
// App Initialization
// -------------------------------------------------------------
document.addEventListener('DOMContentLoaded', async () => {
  await openDatabase();
  await checkSession();
  
  // Format code input with hyphen
  const codeInput = document.getElementById('input-pairing-code');
  if (codeInput) {
    codeInput.addEventListener('input', (e) => {
      let v = e.target.value.replace(/[^0-9]/g, '');
      if (v.length > 3) {
        v = v.slice(0, 3) + '-' + v.slice(3, 6);
      }
      e.target.value = v;
    });
  }

  // Periodic heartbeat / queue sync every 6 seconds
  setInterval(() => {
    if (isOnline) {
      syncNow();
    }
  }, 6000);
});

async function checkSession() {
  try {
    const res = await fetch('/api/phone/session', {
      headers: { 'X-Offgrid-Client': 'phone' }
    });
    if (res.ok) {
      showMainView();
      await loadTasks();
      await loadSuggestions();
    } else {
      showPairingView();
    }
  } catch (err) {
    // If network fails, check if we have cached session in IndexedDB
    const token = await idbGet('meta', 'token');
    if (token) {
      showMainView();
      await loadTasks(); // Offline reads from IndexedDB!
    } else {
      showPairingView();
    }
  }
  updateStatusBadge();
}

function showPairingView() {
  document.getElementById('view-pairing').classList.remove('hidden');
  document.getElementById('view-main').classList.add('hidden');
}

function showMainView() {
  document.getElementById('view-pairing').classList.add('hidden');
  document.getElementById('view-main').classList.remove('hidden');
}

// -------------------------------------------------------------
// Pairing Flow
// -------------------------------------------------------------
async function pairDevice() {
  const code = document.getElementById('input-pairing-code').value.trim();
  const label = document.getElementById('input-device-label').value.trim() || 'Field Device';
  const errDiv = document.getElementById('pairing-error');

  errDiv.classList.add('hidden');
  try {
    const res = await fetch('/api/phone/pair', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Offgrid-Client': 'phone'
      },
      body: JSON.stringify({ code, label })
    });

    const data = await res.json();
    if (!res.ok) {
      errDiv.textContent = data.error || 'Pairing failed';
      errDiv.classList.remove('hidden');
      return;
    }

    // Save token in IndexedDB for offline awareness
    await idbSet('meta', 'token', data.token);
    await idbSet('meta', 'label', data.label);

    showMainView();
    await loadTasks();
    await loadSuggestions();
    updateStatusBadge();
  } catch (err) {
    errDiv.textContent = 'Cannot reach site node: ' + err.message;
    errDiv.classList.remove('hidden');
  }
}

// -------------------------------------------------------------
// Tasks & Offline Storage
// -------------------------------------------------------------
async function loadTasks() {
  // 1. First render from local IndexedDB cache (offline reads!)
  const cached = await idbGetAll('tasks');
  if (cached && cached.length > 0) {
    renderTasks(cached);
  }

  // 2. Fetch fresh data from network if online
  if (isOnline) {
    try {
      const res = await fetch('/api/phone/tasks', {
        headers: { 'X-Offgrid-Client': 'phone' }
      });
      if (res.ok) {
        const fresh = await res.json();
        await idbClear('tasks');
        for (const t of fresh) {
          await idbSet('tasks', undefined, t);
        }
        renderTasks(fresh);
      }
    } catch (err) {
      console.warn('Using offline cached tasks due to network error:', err);
    }
  }
  updateStatusBadge();
}

async function loadSuggestions() {
  if (!isOnline) return;
  try {
    const res = await fetch('/api/phone/suggestions', {
      headers: { 'X-Offgrid-Client': 'phone' }
    });
    if (res.ok) {
      const sugs = await res.json();
      renderSuggestions(sugs);
    }
  } catch (err) {
    console.warn('Cannot fetch suggestions:', err);
  }
}

function setFilter(filter, el) {
  currentFilter = filter;
  document.querySelectorAll('.filter-pill').forEach(p => p.classList.remove('active'));
  if (el) el.classList.add('active');
  loadTasks();
}

function renderTasks(tasks) {
  const container = document.getElementById('tasks-list');
  const countEl = document.getElementById('task-count');

  let filtered = tasks;
  if (currentFilter === 'open') {
    filtered = tasks.filter(t => t.status === 'open');
  } else if (currentFilter === 'in_progress') {
    filtered = tasks.filter(t => t.status === 'in_progress');
  } else if (currentFilter === 'done') {
    filtered = tasks.filter(t => t.status === 'done');
  } else if (currentFilter === 'high') {
    filtered = tasks.filter(t => t.priority === 'high');
  }

  countEl.textContent = `${filtered.length} of ${tasks.length}`;
  container.innerHTML = '';

  if (filtered.length === 0) {
    container.innerHTML = `<div style="text-align: center; color: var(--text-muted); padding: 40px 0;">No tasks found</div>`;
    return;
  }

  for (const t of filtered) {
    const card = document.createElement('div');
    card.className = 'card';

    // Header
    const hdr = document.createElement('div');
    hdr.className = 'card-header';
    
    const title = document.createElement('div');
    title.className = 'card-title';
    title.textContent = t.title;

    const meta = document.createElement('div');
    meta.className = 'card-meta';
    meta.innerHTML = `
      <span class="pill pill-id">${escapeHtml(t.short_id || t.id.slice(0, 8))}</span>
      <span class="pill pill-${t.status}">${escapeHtml(t.status)}</span>
      <span class="pill pill-${t.priority}">${escapeHtml(t.priority)}</span>
    `;

    hdr.appendChild(title);
    hdr.appendChild(meta);
    card.appendChild(hdr);

    // Notes
    if (t.notes && t.notes.length > 0) {
      const notesBox = document.createElement('div');
      notesBox.className = 'card-notes';
      for (const n of t.notes) {
        const item = document.createElement('div');
        item.className = 'note-item';
        if (n.text.includes('[late sync') || n.text.includes('[queued offline')) {
          item.classList.add('late-sync');
        }
        item.textContent = n.text;
        notesBox.appendChild(item);
      }
      card.appendChild(notesBox);
    }

    // Attachments
    if (t.attachments && t.attachments.length > 0) {
      const attBox = document.createElement('div');
      attBox.className = 'card-attachments';
      for (const a of t.attachments) {
        const link = document.createElement('a');
        link.className = 'attachment-badge';
        link.href = a.url;
        link.target = '_blank';
        link.rel = 'noopener';
        link.textContent = '📎 ' + (a.name || a.hash.slice(0, 8));
        attBox.appendChild(link);
      }
      card.appendChild(attBox);
    }

    // Actions
    const acts = document.createElement('div');
    acts.className = 'card-actions';

    if (t.status !== 'done') {
      if (t.status === 'open') {
        const btnProg = document.createElement('button');
        btnProg.className = 'btn btn-secondary btn-sm';
        btnProg.textContent = 'Start';
        btnProg.onclick = () => updateStatus(t.id, 'in_progress');
        acts.appendChild(btnProg);
      }
      const btnDone = document.createElement('button');
      btnDone.className = 'btn btn-secondary btn-sm';
      btnDone.textContent = 'Complete';
      btnDone.onclick = () => updateStatus(t.id, 'done');
      acts.appendChild(btnDone);
    }

    const btnNote = document.createElement('button');
    btnNote.className = 'btn btn-secondary btn-sm';
    btnNote.textContent = '+ Note';
    btnNote.onclick = () => openAddNoteModal(t.id);
    acts.appendChild(btnNote);

    const btnPhoto = document.createElement('button');
    btnPhoto.className = 'btn btn-secondary btn-sm';
    btnPhoto.textContent = '+ Photo';
    btnPhoto.onclick = () => openAttachPhotoModal(t.id);
    acts.appendChild(btnPhoto);

    const btnDel = document.createElement('button');
    btnDel.className = 'btn btn-danger btn-sm';
    btnDel.textContent = 'Delete';
    btnDel.onclick = () => deleteTask(t.id);
    acts.appendChild(btnDel);

    card.appendChild(acts);
    container.appendChild(card);
  }
}

function renderSuggestions(sugs) {
  const section = document.getElementById('section-suggestions');
  const container = document.getElementById('suggestions-list');
  container.innerHTML = '';

  if (!sugs || sugs.length === 0) {
    section.classList.add('hidden');
    return;
  }
  section.classList.remove('hidden');

  for (const s of sugs) {
    const card = document.createElement('div');
    card.className = 'card';
    card.style.borderColor = 'rgba(88, 166, 255, 0.4)';

    const hdr = document.createElement('div');
    hdr.className = 'card-header';
    hdr.innerHTML = `
      <div>
        <div style="font-size: 0.75rem; color: var(--accent-blue); font-weight: 700; text-transform: uppercase;">AI Proposed Change</div>
        <div class="card-title">${escapeHtml(s.task_title || s.task_id)}</div>
      </div>
      <span class="pill pill-id">${escapeHtml(s.task_id.slice(0, 8))}</span>
    `;
    card.appendChild(hdr);

    const body = document.createElement('div');
    body.style.fontSize = '0.9rem';
    body.innerHTML = `
      <div style="color: var(--text-muted); margin: 4px 0;">
        Change: <b>${escapeHtml(s.current_status)}</b> → <b>${escapeHtml(s.proposed_status)}</b>,
        <b>${escapeHtml(s.current_priority)}</b> → <b>${escapeHtml(s.proposed_priority)}</b>
      </div>
      <div style="font-size: 0.85rem; color: var(--text-dim); font-style: italic;">
        "${escapeHtml(s.rationale)}"
      </div>
    `;
    card.appendChild(body);

    const acts = document.createElement('div');
    acts.className = 'card-actions';

    const btnAcc = document.createElement('button');
    btnAcc.className = 'btn btn-primary btn-sm';
    btnAcc.textContent = 'Accept';
    btnAcc.onclick = () => resolveSuggestion(s.id, true);
    acts.appendChild(btnAcc);

    const btnDism = document.createElement('button');
    btnDism.className = 'btn btn-secondary btn-sm';
    btnDism.textContent = 'Dismiss';
    btnDism.onclick = () => resolveSuggestion(s.id, false);
    acts.appendChild(btnDism);

    card.appendChild(acts);
    container.appendChild(card);
  }
}

// -------------------------------------------------------------
// Mutations: Online Direct vs Offline Queued
// -------------------------------------------------------------
async function submitNewTask() {
  const title = document.getElementById('new-task-title').value.trim();
  const priority = document.getElementById('new-task-priority').value;
  const note = document.getElementById('new-task-note').value.trim();

  if (!title) return;
  closeModal('modal-new-task');

  const op = {
    op_id: 'op_' + Math.random().toString(36).slice(2, 11),
    action: 'create_task',
    client_time: new Date().toISOString(),
    payload: { title, priority, note }
  };

  if (!isOnline) {
    // Offline queued write
    await idbSet('queue', undefined, op);
    // Optimistic local add
    const fakeId = 'local_' + op.op_id;
    const notes = note ? [{ text: note + ' [queued offline]', time: op.client_time }] : [{ text: '[queued offline]', time: op.client_time }];
    await idbSet('tasks', undefined, {
      id: fakeId,
      short_id: 'QUEUED',
      title: title,
      status: 'open',
      priority: priority,
      notes: notes,
      created_at: op.client_time,
      updated_at: op.client_time
    });
    loadTasks();
    updateStatusBadge();
    return;
  }

  // Online request
  try {
    const res = await fetch('/api/phone/tasks', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Offgrid-Client': 'phone'
      },
      body: JSON.stringify({ title, priority, note })
    });
    if (!res.ok) throw new Error(await res.text());
    loadTasks();
  } catch (err) {
    // Failed: queue offline
    await idbSet('queue', undefined, op);
    loadTasks();
    updateStatusBadge();
  }
}

async function updateStatus(taskId, status) {
  const op = {
    op_id: 'op_' + Math.random().toString(36).slice(2, 11),
    action: 'update_status',
    client_time: new Date().toISOString(),
    task_id: taskId,
    payload: { status }
  };

  if (!isOnline) {
    // Queue offline
    await idbSet('queue', undefined, op);
    const t = await idbGet('tasks', taskId);
    if (t) {
      t.status = status;
      t.notes = t.notes || [];
      t.notes.push({ text: `[queued offline]: status -> ${status}`, time: op.client_time });
      await idbSet('tasks', undefined, t);
    }
    loadTasks();
    updateStatusBadge();
    return;
  }

  try {
    const res = await fetch(`/api/phone/tasks/${encodeURIComponent(taskId)}/status`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Offgrid-Client': 'phone'
      },
      body: JSON.stringify({ status })
    });
    if (!res.ok) throw new Error(await res.text());
    loadTasks();
  } catch (err) {
    await idbSet('queue', undefined, op);
    loadTasks();
    updateStatusBadge();
  }
}

async function submitAddNote() {
  const taskId = document.getElementById('note-task-id').value;
  const text = document.getElementById('note-text').value.trim();
  if (!text) return;
  closeModal('modal-add-note');

  const op = {
    op_id: 'op_' + Math.random().toString(36).slice(2, 11),
    action: 'add_note',
    client_time: new Date().toISOString(),
    task_id: taskId,
    payload: { text }
  };

  if (!isOnline) {
    await idbSet('queue', undefined, op);
    const t = await idbGet('tasks', taskId);
    if (t) {
      t.notes = t.notes || [];
      t.notes.push({ text: `${text} [queued offline]`, time: op.client_time });
      await idbSet('tasks', undefined, t);
    }
    loadTasks();
    updateStatusBadge();
    return;
  }

  try {
    const res = await fetch(`/api/phone/tasks/${encodeURIComponent(taskId)}/notes`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Offgrid-Client': 'phone'
      },
      body: JSON.stringify({ note: text })
    });
    if (!res.ok) throw new Error(await res.text());
    loadTasks();
  } catch (err) {
    await idbSet('queue', undefined, op);
    loadTasks();
    updateStatusBadge();
  }
}

async function deleteTask(taskId) {
  if (!confirm('Are you sure you want to delete this task?')) return;

  const op = {
    op_id: 'op_' + Math.random().toString(36).slice(2, 11),
    action: 'delete_task',
    client_time: new Date().toISOString(),
    task_id: taskId
  };

  if (!isOnline) {
    await idbSet('queue', undefined, op);
    await idbDelete('tasks', taskId);
    loadTasks();
    updateStatusBadge();
    return;
  }

  try {
    const res = await fetch(`/api/phone/tasks/${encodeURIComponent(taskId)}/delete`, {
      method: 'POST',
      headers: { 'X-Offgrid-Client': 'phone' }
    });
    if (!res.ok) throw new Error(await res.text());
    loadTasks();
  } catch (err) {
    await idbSet('queue', undefined, op);
    await idbDelete('tasks', taskId);
    loadTasks();
    updateStatusBadge();
  }
}

async function resolveSuggestion(sugId, accept) {
  const op = {
    op_id: 'op_' + Math.random().toString(36).slice(2, 11),
    action: 'resolve_suggestion',
    client_time: new Date().toISOString(),
    suggestion_id: sugId,
    payload: { accept }
  };

  if (!isOnline) {
    await idbSet('queue', undefined, op);
    loadSuggestions();
    updateStatusBadge();
    return;
  }

  const endpoint = accept ? 'accept' : 'dismiss';
  try {
    const res = await fetch(`/api/phone/suggestions/${encodeURIComponent(sugId)}/${endpoint}`, {
      method: 'POST',
      headers: { 'X-Offgrid-Client': 'phone' }
    });
    if (!res.ok) throw new Error(await res.text());
    loadSuggestions();
    loadTasks();
  } catch (err) {
    await idbSet('queue', undefined, op);
    updateStatusBadge();
  }
}

async function submitPhoto() {
  const taskId = document.getElementById('photo-task-id').value;
  const fileInput = document.getElementById('photo-file');
  if (!fileInput.files || fileInput.files.length === 0) return;
  const file = fileInput.files[0];
  closeModal('modal-attach-photo');

  if (!isOnline) {
    alert('Photo uploads require an active connection to the site node.');
    return;
  }

  const formData = new FormData();
  formData.append('photo', file);

  try {
    const res = await fetch(`/api/phone/tasks/${encodeURIComponent(taskId)}/attachments`, {
      method: 'POST',
      headers: {
        'X-Offgrid-Client': 'phone'
      },
      body: formData
    });
    if (!res.ok) throw new Error(await res.text());
    loadTasks();
  } catch (err) {
    alert('Upload failed: ' + err.message);
  }
}

// -------------------------------------------------------------
// Offline Queue Replay Engine
// -------------------------------------------------------------
async function syncNow() {
  const queue = await idbGetAll('queue');
  if (!queue || queue.length === 0) {
    updateStatusBadge();
    return;
  }

  const btnSync = document.getElementById('btn-sync');
  if (btnSync) btnSync.disabled = true;

  try {
    const batch = {
      client_id: await idbGet('meta', 'label') || 'phone-client',
      operations: queue
    };

    const res = await fetch('/api/phone/sync/replay', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Offgrid-Client': 'phone'
      },
      body: JSON.stringify(batch)
    });

    if (res.ok) {
      const result = await res.json();
      console.log('Replay sync finished:', result);
      // Clear replayed queue items
      await idbClear('queue');
      await loadTasks();
      await loadSuggestions();
    }
  } catch (err) {
    console.warn('Sync attempt failed:', err);
  } finally {
    if (btnSync) btnSync.disabled = false;
    updateStatusBadge();
  }
}

// -------------------------------------------------------------
// UI Modals & Helpers
// -------------------------------------------------------------
function openNewTaskModal() {
  document.getElementById('new-task-title').value = '';
  document.getElementById('new-task-note').value = '';
  document.getElementById('modal-new-task').classList.remove('hidden');
  document.getElementById('new-task-title').focus();
}

function openAddNoteModal(taskId) {
  document.getElementById('note-task-id').value = taskId;
  document.getElementById('note-text').value = '';
  document.getElementById('modal-add-note').classList.remove('hidden');
  document.getElementById('note-text').focus();
}

function openAttachPhotoModal(taskId) {
  document.getElementById('photo-task-id').value = taskId;
  document.getElementById('photo-file').value = '';
  document.getElementById('modal-attach-photo').classList.remove('hidden');
}

function closeModal(id) {
  document.getElementById(id).classList.add('hidden');
}

function escapeHtml(str) {
  if (!str) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
