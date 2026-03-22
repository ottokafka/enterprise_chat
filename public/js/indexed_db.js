/**
 * IndexedDB Service — mirrors the Flutter IndexedDBService (indexed_db.dart)
 * Stores conversations, chats (with parent_id branching), images, and audio
 * entirely client-side in the browser's IndexedDB.
 */
const IndexedDBService = (() => {
  const DB_NAME = 'media_db';
  const DB_VERSION = 1;
  const STORE_CONVERSATIONS = 'conversations';
  const STORE_CHATS = 'chats';
  const STORE_IMAGES = 'images';
  const STORE_AUDIO = 'audio';

  let _db = null;

  function _open() {
    if (_db) return Promise.resolve(_db);
    return new Promise((resolve, reject) => {
      const req = indexedDB.open(DB_NAME, DB_VERSION);
      req.onupgradeneeded = (e) => {
        const db = e.target.result;
        if (!db.objectStoreNames.contains(STORE_CONVERSATIONS)) {
          const cs = db.createObjectStore(STORE_CONVERSATIONS, { keyPath: 'id', autoIncrement: true });
          cs.createIndex('created_at', 'created_at', { unique: false });
        }
        if (!db.objectStoreNames.contains(STORE_CHATS)) {
          const ch = db.createObjectStore(STORE_CHATS, { keyPath: 'id', autoIncrement: true });
          ch.createIndex('conversation_id', 'conversation_id', { unique: false });
          ch.createIndex('parent_id', 'parent_id', { unique: false });
          ch.createIndex('timestamp', 'timestamp', { unique: false });
        }
        if (!db.objectStoreNames.contains(STORE_IMAGES)) {
          db.createObjectStore(STORE_IMAGES, { keyPath: 'id', autoIncrement: true });
        }
        if (!db.objectStoreNames.contains(STORE_AUDIO)) {
          db.createObjectStore(STORE_AUDIO, { keyPath: 'id', autoIncrement: true });
        }
      };
      req.onsuccess = (e) => { _db = e.target.result; resolve(_db); };
      req.onerror = (e) => reject(e.target.error);
    });
  }

  function _tx(storeName, mode = 'readonly') {
    return _open().then(db => {
      const tx = db.transaction(storeName, mode);
      const store = tx.objectStore(storeName);
      return { tx, store };
    });
  }

  function _req(idbRequest) {
    return new Promise((resolve, reject) => {
      idbRequest.onsuccess = e => resolve(e.target.result);
      idbRequest.onerror = e => reject(e.target.error);
    });
  }

  function _getAll(store) {
    return new Promise((resolve, reject) => {
      const results = [];
      const cursor = store.openCursor();
      cursor.onsuccess = e => {
        const c = e.target.result;
        if (c) { results.push(c.value); c.continue(); }
        else resolve(results);
      };
      cursor.onerror = e => reject(e.target.error);
    });
  }

  // ── Conversation Methods ──────────────────────────────────────────

  async function saveConversation(title) {
    const { store } = await _tx(STORE_CONVERSATIONS, 'readwrite');
    return _req(store.add({ title, created_at: new Date().toISOString() }));
  }

  async function getConversations() {
    const { store } = await _tx(STORE_CONVERSATIONS, 'readonly');
    const all = await _getAll(store);
    return all.sort((a, b) => b.created_at.localeCompare(a.created_at));
  }

  async function deleteConversation(id) {
    // Delete all chats in this conversation first
    const { store: chatStore, tx } = await _tx(STORE_CHATS, 'readwrite');
    const index = chatStore.index('conversation_id');
    await new Promise((resolve, reject) => {
      const cursor = index.openCursor(IDBKeyRange.only(id));
      cursor.onsuccess = e => {
        const c = e.target.result;
        if (c) { c.delete(); c.continue(); } else resolve();
      };
      cursor.onerror = e => reject(e.target.error);
    });
    await new Promise((res, rej) => { tx.oncomplete = res; tx.onerror = rej; });

    // Then delete the conversation
    const { store: convStore } = await _tx(STORE_CONVERSATIONS, 'readwrite');
    return _req(convStore.delete(id));
  }

  async function updateConversationTitle(id, title) {
    const { store } = await _tx(STORE_CONVERSATIONS, 'readwrite');
    const rec = await _req(store.get(id));
    if (rec) { rec.title = title; return _req(store.put(rec)); }
  }

  // ── Chat Methods ───────────────────────────────────────────────────

  async function saveChat(conversationId, role, content, parentId, timestamp) {
    const { store } = await _tx(STORE_CHATS, 'readwrite');
    return _req(store.add({
      conversation_id: conversationId,
      parent_id: parentId ?? null,
      role,
      content,
      timestamp: timestamp ?? new Date().toISOString(),
    }));
  }

  async function getChatById(id) {
    const { store } = await _tx(STORE_CHATS, 'readonly');
    return _req(store.get(id));
  }

  async function deleteChat(id) {
    const { store } = await _tx(STORE_CHATS, 'readwrite');
    return _req(store.delete(id));
  }

  async function getChildren(parentId, conversationId) {
    const { store } = await _tx(STORE_CHATS, 'readonly');
    const all = await _getAll(store);
    return all
      .filter(m => {
        const convMatch = m.conversation_id === conversationId;
        if (parentId === null || parentId === undefined) {
          return convMatch && (m.parent_id === null || m.parent_id === undefined);
        }
        return convMatch && m.parent_id === parentId;
      })
      .sort((a, b) => a.timestamp.localeCompare(b.timestamp));
  }

  async function getLatestLeafId(conversationId) {
    const { store } = await _tx(STORE_CHATS, 'readonly');
    const all = await _getAll(store);
    const conversationChats = all.filter(m => m.conversation_id === conversationId);
    const parentIds = new Set(conversationChats.map(m => m.parent_id).filter(p => p != null));
    const leaves = conversationChats
      .filter(m => !parentIds.has(m.id))
      .sort((a, b) => b.timestamp.localeCompare(a.timestamp));
    return leaves.length > 0 ? leaves[0].id : null;
  }

  async function getPath(leafId) {
    const path = [];
    let current = leafId;
    while (current != null) {
      const msg = await getChatById(current);
      if (!msg) break;
      path.push(msg);
      current = msg.parent_id ?? null;
    }
    return path.reverse();
  }

  async function updateChatContent(id, content) {
    const { store } = await _tx(STORE_CHATS, 'readwrite');
    const rec = await _req(store.get(id));
    if (rec) { rec.content = content; return _req(store.put(rec)); }
  }

  // ── Image Methods ─────────────────────────────────────────────────

  async function saveImage(prompt, base64Image) {
    const { store } = await _tx(STORE_IMAGES, 'readwrite');
    return _req(store.add({ prompt, image: base64Image }));
  }

  async function getImages() {
    const { store } = await _tx(STORE_IMAGES, 'readonly');
    const all = await _getAll(store);
    return all.sort((a, b) => b.id - a.id);
  }

  async function deleteImage(id) {
    const { store } = await _tx(STORE_IMAGES, 'readwrite');
    return _req(store.delete(id));
  }

  // ── Audio Methods ─────────────────────────────────────────────────

  async function saveAudio(lyrics, audioData) {
    const { store } = await _tx(STORE_AUDIO, 'readwrite');
    return _req(store.add({ lyrics, audio: audioData }));
  }

  async function getAudioHistory() {
    const { store } = await _tx(STORE_AUDIO, 'readonly');
    const all = await _getAll(store);
    return all.sort((a, b) => b.id - a.id);
  }

  async function deleteAudio(id) {
    const { store } = await _tx(STORE_AUDIO, 'readwrite');
    return _req(store.delete(id));
  }

  return {
    saveConversation,
    getConversations,
    deleteConversation,
    updateConversationTitle,
    saveChat,
    getChatById,
    deleteChat,
    getChildren,
    getLatestLeafId,
    getPath,
    updateChatContent,
    saveImage,
    getImages,
    deleteImage,
    saveAudio,
    getAudioHistory,
    deleteAudio,
  };
})();
