/**
 * ChatApp — HTMX Chat Controller
 * Mirrors Flutter's ChatController (chat_management.dart) and ChatFeatures (chat_features.dart).
 * Uses IndexedDBService for local persistence and SSE/fetch for streaming LLM responses.
 */
const ChatApp = (() => {
  // ── State ────────────────────────────────────────────────────────────────
  let currentConversationId = null;
  let currentLeafId = null;
  let messages = []; // { id, role, content, parent_id, timestamp }
  let isStreaming = false;
  let abortController = null;
  let attachedFiles = []; // { file: File, dataUrl: string | null }
  let activeDocumentNames = new Set();
  let systemPrompts = [];
  let selectedSystemPromptId = 'none';

  // ── DOM Helpers ───────────────────────────────────────────────────────────
  const $ = id => document.getElementById(id);

  function scrollToBottom() {
    const log = $('chat-log');
    if (log) log.scrollTop = log.scrollHeight;
  }

  // ── Markdown Rendering ────────────────────────────────────────────────────
  // Uses marked + highlight.js (loaded in HTML)
  function renderMarkdown(text) {
    if (!window.marked) return escapeHtml(text).replace(/\n/g, '<br>');
    try {
      const md = window.marked.parse(text, { breaks: true, gfm: true });
      return md;
    } catch { return escapeHtml(text); }
  }

  function escapeHtml(str) {
    return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

  // ── Sidebar ──────────────────────────────────────────────────────────────
  async function loadSidebar() {
    const conversations = await IndexedDBService.getConversations();
    const list = $('conversation-list');
    if (!list) return;
    if (conversations.length === 0) {
      list.innerHTML = '<div class="sidebar-empty">No conversations yet</div>';
      return;
    }
    list.innerHTML = conversations.map(c => `
      <div class="conv-item ${c.id === currentConversationId ? 'active' : ''}"
           data-id="${c.id}" role="button" tabindex="0" 
           onclick="ChatApp.switchConversation(${c.id})"
           onkeydown="if(event.key==='Enter') ChatApp.switchConversation(${c.id})">
        <span class="conv-title" style="color: whitesmoke;">${escapeHtml(c.title)}</span>
        <button class="conv-delete" title="Delete" 
                onclick="event.stopPropagation(); ChatApp.deleteConversation(${c.id})" 
                aria-label="Delete conversation">
          <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2">
            <polyline points="3,6 5,6 21,6"/><path d="M19,6l-1,14H6L5,6"/><path d="M10,11v6"/><path d="M14,11v6"/><path d="M9,6V4h6v2"/>
          </svg>
        </button>
      </div>`).join('');
  }

  // ── Chat Rendering ────────────────────────────────────────────────────────
  function renderEmptyState() {
    const log = $('chat-log');
    if (!log) return;
    log.innerHTML = `
      <div class="empty-state" id="empty-state">
        <svg viewBox="0 0 24 24" width="40" height="40" fill="none" stroke="currentColor" stroke-width="1.5" class="empty-icon">
          <circle cx="12" cy="12" r="10"/><path d="M12 8v4l3 3"/>
        </svg>
        <h2>What do you want to know?</h2>
        <p class="empty-sub">Supported files: images, pdf, csv, docx</p>
      </div>`;
    document.querySelector('.main-area')?.classList.add('is-empty');
  }

  function renderMessages() {
    const log = $('chat-log');
    if (!log) return;
    if (messages.length === 0) { renderEmptyState(); return; }
    document.querySelector('.main-area')?.classList.remove('is-empty');
    log.innerHTML = messages.map((msg, i) =>
      msg.role === 'user' ? buildUserCard(msg, i) : buildAssistantCard(msg, i)
    ).join('');
    // Re-highlight code after render
    if (window.hljs) log.querySelectorAll('pre code').forEach(el => hljs.highlightElement(el));
    scrollToBottom();
    // Async: populate branch nav for each user message
    messages.forEach((msg, i) => { if (msg.role === 'user') updateBranchNav(msg, i); });
  }

  function buildUserCard(msg, index) {
    const msgId = msg.id || 'u' + index;
    return `
      <div class="msg-wrapper msg-user" data-index="${index}" id="msg-${msgId}">
        <div class="msg-bubble user-bubble">
          <div class="msg-content">${escapeHtml(msg.content)}</div>
        </div>
        <div class="msg-actions" id="msg-actions-${msgId}">
          <div class="branch-nav" id="branch-nav-${msgId}" style="visibility:hidden">
            <button class="branch-btn" id="branch-prev-${msgId}" title="Previous edit"
                    onclick="ChatApp.switchSiblingByEl(this)" data-dir="-1" data-index="${index}">
              <svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" stroke-width="2.5"><polyline points="15 18 9 12 15 6"/></svg>
            </button>
            <span class="branch-label" id="branch-label-${msgId}">1 / 1</span>
            <button class="branch-btn" id="branch-next-${msgId}" title="Next edit"
                    onclick="ChatApp.switchSiblingByEl(this)" data-dir="1" data-index="${index}">
              <svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" stroke-width="2.5"><polyline points="9 18 15 12 9 6"/></svg>
            </button>
          </div>
          <button class="msg-edit-btn" title="Edit message"
                  onclick="ChatApp.startEditMessage(${index})" aria-label="Edit message">
            <svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2">
              <path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/>
              <path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/>
            </svg>
            Edit
          </button>
          <button class="msg-edit-btn" title="Copy" aria-label="Copy message"
                  onclick="ChatApp.copyMessageText(this, ${index})">
            <svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2">
              <rect x="9" y="9" width="13" height="13" rx="2"/>
              <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/>
            </svg>
            Copy
          </button>
        </div>
      </div>`;
  }

  function buildAssistantCard(msg, index, streaming = false) {
    const reasoning = msg.reasoning || '';
    const duration = msg.reasoningDuration || null;
    const content = msg.content || '';

    const reasoningBlock = reasoning ? `
      <div class="reasoning-block">
        <button class="reasoning-toggle" onclick="ChatApp.toggleReasoning(this)">
          <svg class="chevron" viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2"><polyline points="6 9 12 15 18 9"/></svg>
          <span>${duration ? `Thought for ${duration}` : 'Thinking...'}</span>
        </button>
        <div class="reasoning-content collapsed">${escapeHtml(reasoning)}</div>
      </div>` : '';

    const assistMsgId = msg.id || 'a' + index;
    return `
      <div class="msg-wrapper msg-assistant" data-index="${index}" id="msg-${assistMsgId}">
        ${reasoningBlock}
        <div class="msg-bubble assistant-bubble${streaming ? ' streaming' : ''}">
          <div class="msg-content markdown-body" id="${streaming ? 'streaming-content' : 'content-' + index}">${streaming ? '<span class="cursor-blink">▋</span>' : renderMarkdown(content)}</div>
        </div>
        ${streaming ? '' : `
        <div class="msg-actions assist-actions">
          <button class="msg-edit-btn" title="Copy" aria-label="Copy response"
                  onclick="ChatApp.copyMessageText(this, ${index})">
            <svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2">
              <rect x="9" y="9" width="13" height="13" rx="2"/>
              <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/>
            </svg>
            Copy
          </button>
        </div>`}
      </div>`;
  }

  // ── Conversation Management ────────────────────────────────────────────────
  async function switchConversation(id) {
    if (isStreaming) return;
    if (id === currentConversationId) return;
    currentConversationId = id;
    currentLeafId = await IndexedDBService.getLatestLeafId(id);
    if (currentLeafId != null) {
      const path = await IndexedDBService.getPath(currentLeafId);
      messages = path.map(m => ({ ...m }));
    } else {
      messages = [];
    }
    renderMessages();
    loadSidebar();
    // Close sidebar on mobile
    document.querySelector('.sidebar')?.classList.remove('open');
  }

  async function newConversation() {
    if (isStreaming) return;
    currentConversationId = null;
    currentLeafId = null;
    messages = [];
    renderEmptyState();
    loadSidebar();
    $('chat-input')?.focus();
  }

  async function deleteConversation(id) {
    await IndexedDBService.deleteConversation(id);
    if (currentConversationId === id) {
      await newConversation();
    } else {
      await loadSidebar();
    }
  }

  // ── Sending Messages ──────────────────────────────────────────────────────
  async function sendMessage(text) {
    text = text.trim();
    if ((!text && attachedFiles.length === 0) || isStreaming) return;

    // Create conversation if needed
    if (!currentConversationId) {
      const title = text.length > 40 ? text.substring(0, 40) + '…' : (text || 'File Chat');
      currentConversationId = await IndexedDBService.saveConversation(title);
      loadSidebar();
    }

    const parentId = messages.length > 0 ? messages[messages.length - 1].id : null;
    const timestamp = new Date().toISOString();
    let displayContent = text;
    if (attachedFiles.length > 0) {
      const names = attachedFiles.map(f => f.file.name).join(', ');
      displayContent = text ? `${text}\n\n[Attached: ${names}]` : `[Attached: ${names}]`;
    }

    // Save user message to IndexedDB
    const userId = await IndexedDBService.saveChat(
      currentConversationId, 'user', displayContent, parentId, timestamp
    );

    const userMsg = { id: userId, role: 'user', content: displayContent, parent_id: parentId, timestamp };
    messages.push(userMsg);

    // Render user message + streaming placeholder
    const log = $('chat-log');
    if (log) {
      const idx = messages.length - 1;
      const emptyState = log.querySelector('.empty-state');
      if (emptyState) emptyState.remove();
      document.querySelector('.main-area')?.classList.remove('is-empty');
      log.insertAdjacentHTML('beforeend', buildUserCard(userMsg, idx));
      log.insertAdjacentHTML('beforeend', `
        <div class="msg-wrapper msg-assistant streaming-wrapper" id="streaming-wrapper">
          <div class="reasoning-wrapper" id="streaming-reasoning-wrapper" style="display:none">
            <button class="reasoning-toggle" onclick="ChatApp.toggleReasoning(this)">
              <svg class="chevron" viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2"><polyline points="6 9 12 15 18 9"/></svg>
              <span id="streaming-reasoning-label">Thinking...</span>
            </button>
            <div class="reasoning-content collapsed" id="streaming-reasoning-body"></div>
          </div>
          <div class="msg-bubble assistant-bubble streaming">
            <div class="msg-content markdown-body" id="streaming-content"><span class="cursor-blink">▋</span></div>
          </div>
        </div>`);
      scrollToBottom();
    }

    // Build form data for multipart request
    const formData = new FormData();
    const conversation = messages.slice(0, -1).map(m => ({ role: m.role, content: m.content }));
    
    // Prepend system prompt if selected
    if (selectedSystemPromptId !== 'none') {
      const selected = systemPrompts.find(p => p.id == selectedSystemPromptId);
      if (selected) {
        conversation.unshift({ role: 'system', content: selected.content });
      }
    }
    
    conversation.push({ role: 'user', content: text || ' ' });
    formData.append('messages', JSON.stringify(conversation));
    formData.append('stream', 'true');
    attachedFiles.forEach(({ file }) => formData.append('files', file, file.name));

    // Clear file attachments
    clearFileAttachments();

    // Set streaming state
    isStreaming = true;
    abortController = new AbortController();
    setStreamingUI(true);

    // Start SSE stream
    let reasoningBuffer = '';
    let contentBuffer = '';
    let hasReasoning = false;
    const reasoningStart = Date.now();

    try {
      let requestConfig;
      if (activeDocumentNames.size > 0) {
        requestConfig = {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            query: text,
            document_names: Array.from(activeDocumentNames),
            stream: true
          }),
          signal: abortController.signal,
        };
      } else {
        requestConfig = {
          method: 'POST',
          body: formData,
          signal: abortController.signal,
        };
      }

      const endpoint = activeDocumentNames.size > 0 ? '/v1/rag' : '/v1/chat/completions';
      const response = await fetch(endpoint, requestConfig);

      if (!response.ok) throw new Error(`HTTP ${response.status}`);

      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = '';

      // Throttle UI updates
      let lastUpdate = 0;

      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split('\n');
        buffer = lines.pop(); // keep the incomplete last line

        for (const line of lines) {
          if (!line.startsWith('data: ')) continue;
          const data = line.slice(6).trim();
          if (data === '[DONE]') break;
          try {
            const parsed = JSON.parse(data);

            if (parsed.type === 'citations') {
              if (parsed.citations && parsed.citations.length > 0) {
                contentBuffer += '\\n\\n**Sources:**\\n<div class="citations-container">\\n';
                parsed.citations.forEach(c => {
                  contentBuffer += `
<details class="source-accordion">
  <summary>📄 ${escapeHtml(c.document_name)} (Chunk #${c.chunk_index})</summary>
  <div class="source-content">
    <strong>Official Policy Text:</strong><br/>
    "${escapeHtml(c.exact_text)}"
  </div>
</details>
`;
                });
                contentBuffer += '</div>\\n';
              }
              continue;
            }

            const delta = parsed?.choices?.[0]?.delta;
            if (!delta) continue;

            if (delta.reasoning_content) {
              reasoningBuffer += delta.reasoning_content;
              hasReasoning = true;
            }
            if (delta.content) {
              contentBuffer += delta.content;
            }

            // Throttle DOM updates to ~20fps
            const now = Date.now();
            if (now - lastUpdate > 50) {
              updateStreamingDOM(reasoningBuffer, contentBuffer, hasReasoning, reasoningStart);
              lastUpdate = now;
              scrollToBottom();
            }
          } catch { }
        }
      }

      // Final update
      updateStreamingDOM(reasoningBuffer, contentBuffer, hasReasoning, reasoningStart, true);
      scrollToBottom();

      // Finalize: save assistant message to IndexedDB
      const duration = hasReasoning ? formatDuration(Date.now() - reasoningStart) : null;
      const assistTimestamp = new Date().toISOString();
      const assistId = await IndexedDBService.saveChat(
        currentConversationId, 'assistant', contentBuffer, userId, assistTimestamp
      );

      const assistMsg = {
        id: assistId,
        role: 'assistant',
        content: contentBuffer,
        reasoning: reasoningBuffer,
        reasoningDuration: duration,
        parent_id: userId,
        timestamp: assistTimestamp,
      };
      messages.push(assistMsg);
      currentLeafId = assistId;

      // Replace streaming DOM with final rendered card
      const wrapper = $('streaming-wrapper');
      if (wrapper) {
        const finalIdx = messages.length - 1;
        wrapper.outerHTML = buildAssistantCard(assistMsg, finalIdx);
        const log = $('chat-log');
        if (window.hljs) log?.querySelectorAll('pre code').forEach(el => hljs.highlightElement(el));
      }

      // Refresh sidebar (title might have been set for first message)
      loadSidebar();

    } catch (err) {
      if (err.name === 'AbortError') {
        // User stopped streaming — already handled by stopStreaming()
      } else {
        console.error('Stream error:', err);
        const wrapper = $('streaming-wrapper');
        if (wrapper) wrapper.outerHTML = buildAssistantCard({ content: `Error: ${err.message}`, role: 'assistant' }, messages.length);
      }
    } finally {
      isStreaming = false;
      abortController = null;
      setStreamingUI(false);
    }
  }

  function updateStreamingDOM(reasoning, content, hasReasoning, reasoningStart, isFinal = false) {
    const contentEl = $('streaming-content');
    if (contentEl) {
      contentEl.innerHTML = content
        ? renderMarkdown(content)
        : '<span class="cursor-blink">▋</span>';
    }
    if (hasReasoning) {
      const wrapper = $('streaming-reasoning-wrapper');
      const body = $('streaming-reasoning-body');
      const label = $('streaming-reasoning-label');
      if (wrapper) wrapper.style.display = '';
      if (body) body.textContent = reasoning;
      if (label && isFinal) {
        label.textContent = `Thought for ${formatDuration(Date.now() - reasoningStart)}`;
      }
    }
  }

  function stopStreaming() {
    if (!isStreaming || !abortController) return;
    abortController.abort();
    isStreaming = false;
    setStreamingUI(false);

    // Remove streaming placeholder and last user message from DOM + IndexedDB
    const wrapper = $('streaming-wrapper');
    if (wrapper) wrapper.remove();

    const lastMsg = messages.pop(); // remove placeholder assistant
    const userMsg = messages[messages.length - 1];

    if (userMsg?.role === 'user') {
      // Restore user message in input
      const input = $('chat-input');
      if (input) {
        // strip file attachment suffix from content
        input.value = userMsg.content.replace(/\n\n\[Attached:.*\]$/, '');
        input.focus();
      }
      IndexedDBService.deleteChat(userMsg.id);
      messages.pop();
      currentLeafId = messages.length > 0 ? messages[messages.length - 1].id : null;
      // Remove last user card from DOM
      const log = $('chat-log');
      const lastCard = log?.querySelector(`.msg-wrapper:last-child`);
      if (lastCard) lastCard.remove();
    }

    if (messages.length === 0) renderEmptyState();
  }

  function setStreamingUI(streaming) {
    const stopBtn = $('stop-btn');
    const sendBtn = $('send-btn');
    if (stopBtn) stopBtn.style.display = streaming ? '' : 'none';
    if (sendBtn) sendBtn.disabled = streaming;
  }

  // ── Edit Message ─────────────────────────────────────────────────────────
  function startEditMessage(index) {
    const msg = messages[index];
    if (!msg || msg.role !== 'user') return;
    const modal = $('edit-modal');
    const editInput = $('edit-input');
    if (!modal || !editInput) return;
    editInput.value = msg.content;
    modal.dataset.editIndex = index;
    modal.classList.add('open');
    editInput.focus();
  }

  async function confirmEdit() {
    const modal = $('edit-modal');
    const editInput = $('edit-input');
    if (!modal || !editInput) return;
    const index = parseInt(modal.dataset.editIndex);
    const newContent = editInput.value.trim();
    modal.classList.remove('open');
    if (!newContent || isNaN(index)) return;
    await performEditMessage(index, newContent);
  }

  function cancelEdit() {
    const modal = $('edit-modal');
    if (modal) modal.classList.remove('open');
  }

  async function performEditMessage(index, newContent) {
    if (!currentConversationId || isStreaming) return;
    const parentId = index === 0 ? null : messages[index - 1].id;
    const timestamp = new Date().toISOString();
    const newUserId = await IndexedDBService.saveChat(
      currentConversationId, 'user', newContent, parentId, timestamp
    );
    const prefix = messages.slice(0, index);
    const newUserMsg = { id: newUserId, role: 'user', content: newContent, parent_id: parentId, timestamp };
    messages = [...prefix, newUserMsg];
    renderMessages();
    scrollToBottom();
    // Trigger send for the new edited message
    await _streamFromMessages(newUserId);
  }

  // Internal: send stream continuing from a new user message that's already saved & in `messages`
  async function _streamFromMessages(newUserId) {
    const log = $('chat-log');
    log?.insertAdjacentHTML('beforeend', `
      <div class="msg-wrapper msg-assistant streaming-wrapper" id="streaming-wrapper">
        <div class="reasoning-wrapper" id="streaming-reasoning-wrapper" style="display:none">
          <button class="reasoning-toggle" onclick="ChatApp.toggleReasoning(this)">
            <svg class="chevron" viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2"><polyline points="6 9 12 15 18 9"/></svg>
            <span id="streaming-reasoning-label">Thinking...</span>
          </button>
          <div class="reasoning-content collapsed" id="streaming-reasoning-body"></div>
        </div>
        <div class="msg-bubble assistant-bubble streaming">
          <div class="msg-content markdown-body" id="streaming-content"><span class="cursor-blink">▋</span></div>
        </div>
      </div>`);
    scrollToBottom();

    const formData = new FormData();
    const conversation = messages.map(m => ({ role: m.role, content: m.content }));
    
    // Prepend system prompt if selected
    if (selectedSystemPromptId !== 'none') {
      const selected = systemPrompts.find(p => p.id == selectedSystemPromptId);
      if (selected) {
        conversation.unshift({ role: 'system', content: selected.content });
      }
    }
    
    formData.append('messages', JSON.stringify(conversation));
    formData.append('stream', 'true');

    isStreaming = true;
    abortController = new AbortController();
    setStreamingUI(true);

    let reasoningBuffer = '', contentBuffer = '', hasReasoning = false;
    const reasoningStart = Date.now();

    try {
      let requestConfig;
      if (activeDocumentNames.size > 0) {
        const lastMsg = conversation[conversation.length - 1];
        requestConfig = {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            query: lastMsg.content,
            document_names: Array.from(activeDocumentNames),
            stream: true
          }),
          signal: abortController.signal,
        };
      } else {
        requestConfig = {
          method: 'POST',
          body: formData,
          signal: abortController.signal,
        };
      }

      const endpoint = activeDocumentNames.size > 0 ? '/v1/rag' : '/v1/chat/completions';
      const response = await fetch(endpoint, requestConfig);
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let lineBuffer = '';
      let lastUpdate = 0;
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        lineBuffer += decoder.decode(value, { stream: true });
        const lines = lineBuffer.split('\n');
        lineBuffer = lines.pop();
        for (const line of lines) {
          if (!line.startsWith('data: ')) continue;
          const data = line.slice(6).trim();
          if (data === '[DONE]') break;
          try {
            const parsed = JSON.parse(data);

            if (parsed.type === 'citations') {
              if (parsed.citations && parsed.citations.length > 0) {
                contentBuffer += '\\n\\n**Sources:**\\n<div class="citations-container">\\n';
                parsed.citations.forEach(c => {
                  contentBuffer += `
<details class="source-accordion">
  <summary>📄 ${escapeHtml(c.document_name)} (Chunk #${c.chunk_index})</summary>
  <div class="source-content">
    <strong>Official Policy Text:</strong><br/>
    "${escapeHtml(c.exact_text)}"
  </div>
</details>
`;
                });
                contentBuffer += '</div>\\n';
              }
              continue;
            }

            const delta = parsed?.choices?.[0]?.delta;
            if (!delta) continue;
            if (delta.reasoning_content) { reasoningBuffer += delta.reasoning_content; hasReasoning = true; }
            if (delta.content) contentBuffer += delta.content;
            const now = Date.now();
            if (now - lastUpdate > 50) {
              updateStreamingDOM(reasoningBuffer, contentBuffer, hasReasoning, reasoningStart);
              lastUpdate = now; scrollToBottom();
            }
          } catch { }
        }
      }
      updateStreamingDOM(reasoningBuffer, contentBuffer, hasReasoning, reasoningStart, true);
      scrollToBottom();
      const duration = hasReasoning ? formatDuration(Date.now() - reasoningStart) : null;
      const assistTimestamp = new Date().toISOString();
      const assistId = await IndexedDBService.saveChat(
        currentConversationId, 'assistant', contentBuffer, newUserId, assistTimestamp
      );
      const assistMsg = { id: assistId, role: 'assistant', content: contentBuffer, reasoning: reasoningBuffer, reasoningDuration: duration, parent_id: newUserId, timestamp: assistTimestamp };
      messages.push(assistMsg);
      currentLeafId = assistId;
      const wrapper = $('streaming-wrapper');
      if (wrapper) {
        wrapper.outerHTML = buildAssistantCard(assistMsg, messages.length - 1);
        if (window.hljs) $('chat-log')?.querySelectorAll('pre code').forEach(el => hljs.highlightElement(el));
      }
    } catch (err) {
      if (err.name !== 'AbortError') console.error(err);
    } finally {
      isStreaming = false; abortController = null; setStreamingUI(false);
    }
  }

  // ── Branch Switching ─────────────────────────────────────────────────────
  async function switchSibling(messageIndex, newSiblingPos) {
    if (!currentConversationId) return;
    const userMsg = messages[messageIndex];
    const siblings = await IndexedDBService.getChildren(userMsg.parent_id, currentConversationId);
    const newUser = siblings[newSiblingPos - 1];
    if (!newUser) return;
    const prefix = messages.slice(0, messageIndex);
    const suffix = await buildSuffix(newUser.id);
    messages = [...prefix, ...suffix];
    currentLeafId = messages[messages.length - 1]?.id ?? null;
    renderMessages();
    scrollToBottom();
  }

  // Called by branch prev/next buttons via inline onclick
  async function switchSiblingByEl(btn) {
    if (isStreaming) return;
    const index = parseInt(btn.dataset.index);
    const dir = parseInt(btn.dataset.dir); // -1 or +1
    const userMsg = messages[index];
    if (!userMsg) return;
    const siblings = await IndexedDBService.getChildren(userMsg.parent_id, currentConversationId);
    const currentPos = siblings.findIndex(s => s.id === userMsg.id);
    const newPos = currentPos + dir;
    if (newPos < 0 || newPos >= siblings.length) return;
    const prefix = messages.slice(0, index);
    const suffix = await buildSuffix(siblings[newPos].id);
    messages = [...prefix, ...suffix];
    currentLeafId = messages[messages.length - 1]?.id ?? null;
    renderMessages();
    scrollToBottom();
  }

  // Async: updates the branch nav indicators for a given user message
  async function updateBranchNav(msg, index) {
    if (!currentConversationId || msg.parent_id === undefined) return;
    const msgId = msg.id || 'u' + index;
    const navEl = $(`branch-nav-${msgId}`);
    const labelEl = $(`branch-label-${msgId}`);
    const prevBtn = $(`branch-prev-${msgId}`);
    const nextBtn = $(`branch-next-${msgId}`);
    if (!navEl) return;
    const siblings = await IndexedDBService.getChildren(msg.parent_id, currentConversationId);
    if (siblings.length <= 1) {
      navEl.style.visibility = 'hidden';
      return;
    }
    const currentPos = siblings.findIndex(s => s.id === msg.id);
    const pos = currentPos === -1 ? 1 : currentPos + 1;
    navEl.style.visibility = 'visible';
    if (labelEl) labelEl.textContent = `${pos} / ${siblings.length}`;
    if (prevBtn) prevBtn.disabled = pos <= 1;
    if (nextBtn) nextBtn.disabled = pos >= siblings.length;
  }

  async function buildSuffix(startId) {
    const suffix = [];
    let currentId = startId;
    while (currentId != null) {
      const current = await IndexedDBService.getChatById(currentId);
      if (!current) break;
      suffix.push({ ...current });
      const children = await IndexedDBService.getChildren(current.id, currentConversationId);
      if (children.length === 0) break;
      else if (children.length === 1) currentId = children[0].id;
      else currentId = children.reduce((a, b) => a.timestamp > b.timestamp ? a : b).id;
    }
    return suffix;
  }

  // ── File Attachments ─────────────────────────────────────────────────────
  function addFiles(fileList) {
    for (const file of fileList) {
      attachedFiles.push({ file });
    }
    renderFileChips();
  }

  function removeFile(index) {
    attachedFiles.splice(index, 1);
    renderFileChips();
  }

  function clearFileAttachments() {
    attachedFiles = [];
    renderFileChips();
    const fi = $('file-input');
    if (fi) fi.value = '';
  }

  function renderFileChips() {
    const row = $('file-chips');
    if (!row) return;
    if (attachedFiles.length === 0) { row.innerHTML = ''; return; }
    row.innerHTML = attachedFiles.map((f, i) => `
      <div class="file-chip">
        <svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" stroke-width="2"><path d="M13 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><polyline points="13 2 13 9 20 9"/></svg>
        <span>${escapeHtml(f.file.name)}</span>
        <button onclick="ChatApp.removeFile(${i})" aria-label="Remove file">&times;</button>
      </div>`).join('');
  }

  // ── Utilities ─────────────────────────────────────────────────────────────
  function formatDuration(ms) {
    const totalSeconds = Math.floor(ms / 1000);
    const minutes = Math.floor(totalSeconds / 60);
    const seconds = totalSeconds % 60;
    return minutes > 0 ? `${minutes}m ${seconds}s` : `${seconds}s`;
  }

  async function copyText(btn, text) {
    try {
      await navigator.clipboard.writeText(text);
      btn.title = 'Copied!';
      btn.classList.add('copied');
      setTimeout(() => { btn.title = 'Copy'; btn.classList.remove('copied'); }, 1500);
    } catch { }
  }

  function copyMessageText(btn, index) {
    const text = messages[index]?.content;
    if (text) copyText(btn, text);
  }

  function toggleReasoning(btn) {
    const container = btn.closest('.reasoning-block, .reasoning-wrapper');
    const body = container?.querySelector('.reasoning-content');
    if (!body) return;
    const isOpen = !body.classList.contains('collapsed');
    body.classList.toggle('collapsed', isOpen);
    container.classList.toggle('expanded', !isOpen);
    btn.querySelector('.chevron')?.style && (btn.querySelector('.chevron').style.transform = isOpen ? '' : 'rotate(180deg)');
  }

  // ── User Profile ──────────────────────────────────────────────────────────
  async function loadUserProfile() {
    try {
      const res = await fetch('/api/user');
      if (res.ok) {
        const user = await res.json();
        renderUserProfile(user);
      }
    } catch (err) {
      console.error('Failed to load user profile', err);
    }
  }

  function renderUserProfile(user) {
    const footer = $('sidebar-footer');
    if (!footer) return;
    const initial = (user.name || 'U').charAt(0).toUpperCase();
    footer.innerHTML = `
      <div class="user-profile">
        <div class="avatar-circle">${initial}</div>
        <div class="user-info">
          <div class="user-name">${escapeHtml(user.name || '')}</div>
          <div class="user-email">${escapeHtml(user.email || '')}</div>
        </div>
        <div class="user-menu-container" style="position: relative; margin-left: auto;">
          <button class="user-menu-btn" title="Menu" style="background: transparent; border: none; color: var(--text-muted); cursor: pointer; padding: 6px; border-radius: 6px; display: flex; align-items: center; justify-content: center;" onclick="event.stopPropagation(); const ds = this.nextElementSibling.style; ds.display = (ds.display === 'none' ? 'block' : 'none');">
            <svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <circle cx="12" cy="12" r="1"></circle>
              <circle cx="12" cy="5" r="1"></circle>
              <circle cx="12" cy="19" r="1"></circle>
            </svg>
          </button>
          <div class="user-dropdown" style="display: none; position: absolute; bottom: 100%; right: 0; margin-bottom: 8px; background: var(--bg-card); border: 1px solid var(--border-mid); border-radius: 8px; padding: 4px; min-width: 140px; box-shadow: 0 4px 12px rgba(0,0,0,0.5); z-index: 100;">
            <a href="/developer" style="display: flex; align-items: center; gap: 8px; padding: 8px 12px; color: var(--text-primary); text-decoration: none; font-size: 13px; border-radius: 6px; transition: background 0.15s;" onmouseover="this.style.background='var(--bg-hover)'" onmouseout="this.style.background='transparent'">
              <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2"><polyline points="16 18 22 12 16 6"></polyline><polyline points="8 6 2 12 8 18"></polyline></svg>
              Developer
            </a>
            <div style="display: flex; align-items: center; gap: 8px; padding: 8px 12px; color: var(--text-primary); cursor: pointer; font-size: 13px; border-radius: 6px; transition: background 0.15s;" onmouseover="this.style.background='var(--bg-hover)'" onmouseout="this.style.background='transparent'" onclick="ChatApp.openPromptsManager()">
              <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm0 3c1.66 0 3 1.34 3 3s-1.34 3-3 3-3-1.34-3-3 1.34-3 3-3zm0 14.2c-2.5 0-4.71-1.28-6-3.22.03-1.99 4-3.08 6-3.08 1.99 0 5.97 1.09 6 3.08-1.29 1.94-3.5 3.22-6 3.22z"/></svg>
              System Prompts
            </div>
            <a href="/logout" style="display: flex; align-items: center; gap: 8px; padding: 8px 12px; color: #ef4444; text-decoration: none; font-size: 13px; border-radius: 6px; transition: background 0.15s;" onmouseover="this.style.background='rgba(239, 68, 68, 0.15)'" onmouseout="this.style.background='transparent'">
              <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2"><path d="M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4M10 17l5-5-5-5M15 12H3"/></svg>
              Sign Out
            </a>
          </div>
        </div>
      </div>
    `;
  }

  // Global click to close the dropdown menu
  document.addEventListener('click', () => {
    const dropdowns = document.querySelectorAll('.user-dropdown');
    dropdowns.forEach(d => d.style.display = 'none');
  });

  // ── Init ──────────────────────────────────────────────────────────────────
  async function init() {
    await loadSidebar();
    await loadUserProfile();
    renderEmptyState();
    setupInputListeners();
    setupPasteListener();
    setupSidebarToggle();
    initIngestionUI();
    initSystemPromptUI();
  }

  function setupInputListeners() {
    const input = $('chat-input');
    const sendBtn = $('send-btn');
    const stopBtn = $('stop-btn');
    const fileBtn = $('attach-btn');
    const fileInput = $('file-input');

    sendBtn?.addEventListener('click', () => {
      const text = input?.value ?? '';
      input && (input.value = '');
      sendMessage(text);
    });

    stopBtn?.addEventListener('click', () => stopStreaming());

    input?.addEventListener('keydown', e => {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        if (!isStreaming) {
          const text = input.value;
          input.value = '';
          sendMessage(text);
        }
      }
    });

    // Auto-resize textarea
    input?.addEventListener('input', () => {
      input.style.height = 'auto';
      input.style.height = Math.min(input.scrollHeight, 200) + 'px';
    });

    fileBtn?.addEventListener('click', () => fileInput?.click());
    fileInput?.addEventListener('change', e => addFiles(e.target.files));

    // Edit modal
    $('edit-confirm')?.addEventListener('click', () => confirmEdit());
    $('edit-cancel')?.addEventListener('click', () => cancelEdit());
    $('edit-input')?.addEventListener('keydown', e => {
      if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); confirmEdit(); }
      if (e.key === 'Escape') cancelEdit();
    });
    $('edit-modal')?.addEventListener('click', e => {
      if (e.target === $('edit-modal')) cancelEdit();
    });

    $('new-chat-btn')?.addEventListener('click', () => newConversation());
  }

  function setupPasteListener() {
    const input = $('chat-input');
    input?.addEventListener('paste', e => {
      const items = e.clipboardData?.items;
      if (!items) return;
      for (const item of items) {
        if (item.type.startsWith('image/')) {
          e.preventDefault();
          const file = item.getAsFile();
          if (file) addFiles([file]);
        }
      }
    });
  }

  function setupSidebarToggle() {
    $('sidebar-toggle')?.addEventListener('click', () => {
      document.querySelector('.sidebar')?.classList.toggle('open');
    });
  }

  // ── Document Ingestion ───────────────────────────────────────────────────
  function initIngestionUI() {
    const toggleBtn = $('right-sidebar-toggle');
    const rightSidebar = $('right-sidebar');
    const closeBtn = $('right-sidebar-close');
    const fileInput = $('rag-file-input');

    if (toggleBtn && rightSidebar) {
      rightSidebar.classList.add('open');
      toggleBtn.addEventListener('click', () => rightSidebar.classList.toggle('open'));
    }
    if (closeBtn && rightSidebar) {
      closeBtn.addEventListener('click', () => rightSidebar.classList.remove('open'));
    }

    loadDocuments();

    if (fileInput) {
      fileInput.addEventListener('change', async (e) => {
        if (!e.target.files.length) return;
        const isGlobal = $('upload-global-flag')?.checked || false;
        await startIngestion(e.target.files, isGlobal);
        fileInput.value = ''; // resets
      });
    }
  }

  function initSystemPromptUI() {
    $('system-prompt-btn')?.addEventListener('click', (e) => {
      e.stopPropagation();
      const menu = $('system-prompt-menu');
      if (menu) menu.style.display = menu.style.display === 'none' ? 'block' : 'none';
    });

    $('system-prompt-menu')?.addEventListener('click', (e) => e.stopPropagation());

    $('manage-prompts-btn')?.addEventListener('click', () => {
      const menu = $('system-prompt-menu');
      if (menu) menu.style.display = 'none';
      openPromptsManager();
    });

    $('add-prompt-btn')?.addEventListener('click', () => {
      showPromptEditForm();
    });

    $('prompt-edit-cancel')?.addEventListener('click', () => {
      hidePromptEditForm();
    });

    $('prompt-edit-save')?.addEventListener('click', async () => {
      await saveSystemPrompt();
    });

    $('prompts-modal-close')?.addEventListener('click', () => {
      $('system-prompts-modal').classList.remove('open');
    });

    loadSystemPrompts();
  }

  // ── System Prompts ────────────────────────────────────────────────────────
  async function loadSystemPrompts() {
    try {
      const res = await fetch('/v1/system-prompts');
      if (res.ok) {
        systemPrompts = await res.json();
        renderSystemPromptOptions();
      }
    } catch (err) {
      console.error('Failed to load system prompts', err);
    }
  }

  function renderSystemPromptOptions() {
    const list = $('system-prompt-list');
    if (!list) return;

    let html = `
      <div class="prompt-option ${selectedSystemPromptId === 'none' ? 'active' : ''}" 
           data-id="none" onclick="ChatApp.selectSystemPrompt('none')">
        <span>None (Default)</span>
      </div>`;

    systemPrompts.forEach(p => {
      html += `
        <div class="prompt-option ${selectedSystemPromptId == p.id ? 'active' : ''}" 
             data-id="${p.id}" onclick="ChatApp.selectSystemPrompt(${p.id})">
          <span>${escapeHtml(p.name)}</span>
        </div>`;
    });
    list.innerHTML = html;
  }

  function selectSystemPrompt(id) {
    selectedSystemPromptId = id;
    renderSystemPromptOptions();
    const menu = $('system-prompt-menu');
    if (menu) menu.style.display = 'none';

    // Update button color if prompt is active
    const btn = $('system-prompt-btn');
    if (btn) {
      if (id !== 'none') {
        btn.style.color = 'var(--accent)';
        btn.style.borderColor = 'var(--accent)';
      } else {
        btn.style.color = '';
        btn.style.borderColor = '';
      }
    }
  }

  function openPromptsManager() {
    renderPromptsManager();
    $('system-prompts-modal')?.classList.add('open');
    hidePromptEditForm();
  }

  function renderPromptsManager() {
    const list = $('prompts-manager-list');
    if (!list) return;

    if (systemPrompts.length === 0) {
      list.innerHTML = '<div style="text-align: center; color: var(--text-muted); padding: 20px;">No custom system prompts created yet.</div>';
      return;
    }

    list.innerHTML = systemPrompts.map(p => `
      <div class="prompt-item">
        <div class="prompt-item-header">
          <div class="prompt-item-name">${escapeHtml(p.name)}</div>
          <div class="prompt-item-actions">
            <button class="prompt-action-btn" onclick="ChatApp.editPromptUI(${p.id})" title="Edit">
              <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2"><path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/></svg>
            </button>
            <button class="prompt-action-btn delete" onclick="ChatApp.deletePromptUI(${p.id})" title="Delete">
              <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2"><polyline points="3 6 5 6 21 6"/><path d="M19 6l-1 14H6L5 6"/><path d="M10 11v6"/><path d="M14 11v6"/><path d="M9 6V4h6v2"/></svg>
            </button>
          </div>
        </div>
        <div class="prompt-item-content">${escapeHtml(p.content || '').replace(/\n/g, '<br>')}</div>
      </div>
    `).join('');
  }

  function showPromptEditForm(id = null) {
    const form = $('prompt-edit-form');
    const footer = $('prompts-modal-footer');
    if (!form || !footer) return;

    form.style.display = 'block';
    footer.style.display = 'none';

    if (id) {
      const p = systemPrompts.find(x => x.id == id);
      if (p) {
        $('prompt-name-input').value = p.name;
        $('prompt-content-input').value = p.content;
        form.dataset.editId = id;
      }
    } else {
      $('prompt-name-input').value = '';
      $('prompt-content-input').value = '';
      delete form.dataset.editId;
    }
    $('prompt-name-input').focus();
  }

  function hidePromptEditForm() {
    const form = $('prompt-edit-form');
    const footer = $('prompts-modal-footer');
    if (form) form.style.display = 'none';
    if (footer) footer.style.display = 'flex';
  }

  async function saveSystemPrompt() {
    const name = $('prompt-name-input').value.trim();
    const content = $('prompt-content-input').value.trim();
    const editId = $('prompt-edit-form').dataset.editId;

    if (!name || !content) {
      alert('Please provide both name and content.');
      return;
    }

    const method = editId ? 'PATCH' : 'POST';
    const url = editId ? `/v1/system-prompts/${editId}` : '/v1/system-prompts';

    try {
      const res = await fetch(url, {
        method,
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, content })
      });

      if (res.ok) {
        await loadSystemPrompts();
        renderPromptsManager();
        hidePromptEditForm();
      } else {
        const err = await res.json();
        alert('Error: ' + err.error);
      }
    } catch (e) {
      console.error(e);
      alert('Failed to save prompt.');
    }
  }

  function editPromptUI(id) {
    showPromptEditForm(id);
  }

  async function deletePromptUI(id) {
    if (!confirm('Are you sure you want to delete this system prompt?')) return;
    try {
      const res = await fetch(`/v1/system-prompts/${id}`, { method: 'DELETE' });
      if (res.ok) {
        if (selectedSystemPromptId == id) selectSystemPrompt('none');
        await loadSystemPrompts();
        renderPromptsManager();
      }
    } catch (e) {
      console.error(e);
    }
  }


  async function startIngestion(files, isGlobal) {
    const formData = new FormData();
    for (let i = 0; i < files.length; i++) {
      formData.append('files', files[i]);
    }
    formData.append('is_global', isGlobal);

    try {
      const response = await fetch('/v1/ingest', { method: 'POST', body: formData });
      if (!response.ok) throw new Error(`HTTP error! status: ${response.status}`);
      const data = await response.json();

      const jobId = data.job_id;
      if (jobId) {
        const fileNames = Array.from(files).map(f => f.name);
        listenToJobProgress(jobId, fileNames);
      }
    } catch (err) {
      console.error('Ingestion failed:', err);
      alert('Failed to start ingestion: ' + err.message);
    }
  }

  function listenToJobProgress(jobId, files) {
    const list = $('upload-progress-list');
    if (!list) return;

    // Create item in DOM
    const li = document.createElement('li');
    li.className = 'progress-item';
    li.id = `job-${jobId}`;
    const titles = escapeHtml(files.join(', '));
    li.innerHTML = `
      <div class="progress-item-title">${titles}</div>
      <div class="progress-item-stage" id="job-stage-${jobId}">Starting...</div>
      <div class="progress-bar-bg"><div class="progress-bar-fill" id="job-fill-${jobId}"></div></div>
    `;
    list.prepend(li);

    const source = new EventSource(`/v1/ingest/progress?job_id=${jobId}`);
    source.onmessage = function (event) {
      try {
        const data = JSON.parse(event.data);
        const stageEl = $(`job-stage-${jobId}`);
        const fillEl = $(`job-fill-${jobId}`);

        if (stageEl) stageEl.textContent = data.stage || data.status;
        if (fillEl && data.progress !== undefined) fillEl.style.width = `${data.progress}%`;

        if (data.status === 'completed' || data.status === 'error') {
          source.close();
          if (stageEl) {
            stageEl.textContent = data.status === 'completed' ? 'Completed!' : 'Error occurred.';
            stageEl.style.color = data.status === 'completed' ? '#4ade80' : '#ef4444';
          }
          if (data.status === 'completed') {
            setTimeout(() => loadDocuments(), 1000);
          }
        }
      } catch (err) { }
    };
    source.onerror = function (err) {
      console.error('EventSource failed', err);
      source.close();
    };
  }

  async function loadDocuments() {
    const list = $('my-documents-list');
    if (!list) return;

    try {
      const response = await fetch('/v1/documents');
      if (!response.ok) throw new Error('Failed to fetch documents');
      const data = await response.json();

      const currentUserId = data.user_id;

      list.innerHTML = '';
      if (!data.documents || data.documents.length === 0) {
        list.innerHTML = '<div style="font-size: 12px; color: whitesmoke; padding: 10px 0;">No documents uploaded.</div>';
        return;
      }

      data.documents.forEach(doc => {
        const li = document.createElement('li');
        li.className = 'progress-item';

        let statusHtml = '';
        if (doc.status === 'processing') {
          statusHtml = '<div class="progress-item-stage" style="color: #fbbf24;">Processing...</div>';
        } else if (doc.status === 'error') {
          statusHtml = '<div class="progress-item-stage" style="color: #ef4444;">Error</div>';
        } else {
          statusHtml = '<div class="progress-item-stage" style="color: #4ade80;">Ready</div>';
        }

        const date = new Date(doc.created_at).toLocaleDateString();
        const isChecked = activeDocumentNames.has(doc.document_name) ? 'checked' : '';
        const sharedStatus = doc.is_global ? 'Global' : (doc.shared_with_user_ids?.length > 0 ? `Shared (${doc.shared_with_user_ids.length} users)` : 'Private');

        li.innerHTML = `
          <div class="progress-item-title" title="${escapeHtml(doc.document_name)}">${escapeHtml(doc.document_name)}</div>
          ${statusHtml}
          <div class="progress-item-meta">Uploaded: ${date}</div>
          <div class="progress-item-meta">Status: ${sharedStatus}</div>
          
          <div class="doc-actions-row">
            ${doc.user_id === currentUserId ? `
              <button class="doc-share-btn" onclick='ChatApp.openShareModal(${doc.id}, ${doc.is_global}, ${JSON.stringify(doc.shared_with_user_ids || [])})'>
                Share
              </button>
            ` : `
              <div style="margin-right: auto; font-size: 11px; color: var(--text-muted); font-style: italic;">
                Shared Workspace
              </div>
            `}

            ${doc.status === 'completed' ? `
              <button class="doc-action-btn" title="View Snapshot" onclick="ChatApp.viewDocumentSnapshot(${doc.id}, '${escapeHtml(doc.document_name).replace(/'/g, "\\'")}')">
                <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2">
                  <path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"></path>
                  <circle cx="12" cy="12" r="3"></circle>
                </svg>
              </button>
            ` : ''}

            ${doc.user_id === currentUserId ? `
              <button class="doc-action-btn doc-delete-btn-new" title="Delete Document" onclick="ChatApp.deleteDocument(${doc.id})">
                <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2">
                  <polyline points="3 6 5 6 21 6"></polyline>
                  <path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path>
                </svg>
              </button>
            ` : ''}

            ${doc.status === 'completed' ? `
              <div class="doc-chat-toggle">
                <span>Chat</span>
                <label class="switch">
                  <input type="checkbox" class="doc-toggle" data-doc="${escapeHtml(doc.document_name)}" ${isChecked}>
                  <span class="slider"></span>
                </label>
              </div>
            ` : ''}
          </div>
        `;

        const toggle = li.querySelector('.doc-toggle');
        if (toggle) {
          toggle.onchange = (e) => ChatApp.toggleDocumentChat(doc.document_name, e.target.checked);
        }

        list.appendChild(li);
      });
    } catch (err) {
      console.error('Error loading documents:', err);
    }
  }

  let allUsers = [];

  async function fetchAllUsers() {
    if (allUsers.length > 0) return;
    try {
      const res = await fetch('/api/users');
      if (res.ok) allUsers = await res.json();
    } catch (e) {
      console.error('Failed to fetch users', e);
    }
  }

  let currentShareDocId = null;

  async function openShareModal(docId, isGlobal, sharedWithIds) {
    currentShareDocId = docId;
    const modal = document.getElementById('share-modal');
    $('share-global-flag').checked = isGlobal;
    $('share-search-input').value = '';

    await fetchAllUsers();
    renderShareUsers(sharedWithIds || []);
    
    // UI logic for toggling global vs specific
    const toggleGlobal = () => {
      const isG = $('share-global-flag').checked;
      $('share-search-input').disabled = isG;
      document.querySelectorAll('#share-users-list .user-checkbox').forEach(cb => cb.disabled = isG);
    };
    $('share-global-flag').onchange = toggleGlobal;
    $('share-search-input').oninput = () => renderShareUsers(getCheckedUsers());

    toggleGlobal();
    modal.classList.add('open');
  }

  function getCheckedUsers() {
    const checked = [];
    document.querySelectorAll('#share-users-list .user-checkbox:checked').forEach(cb => {
      checked.push(parseInt(cb.value));
    });
    return checked;
  }

  function renderShareUsers(selectedIds) {
    const query = $('share-search-input').value.toLowerCase();
    const list = $('share-users-list');
    const isG = $('share-global-flag').checked;
    
    let html = '';
    const filtered = allUsers.filter(u => u.name.toLowerCase().includes(query) || (u.job_title && u.job_title.toLowerCase().includes(query)));
    
    if (filtered.length === 0) {
      list.innerHTML = '<div style="font-size: 12px; color: var(--text-muted);">No users found.</div>';
      return;
    }

    filtered.forEach(u => {
      const checked = selectedIds.includes(u.id) ? 'checked' : '';
      const disabled = isG ? 'disabled' : '';
      html += `
        <label style="display: flex; align-items: center; gap: 8px; font-size: 13px; color: var(--text-primary); cursor: pointer; padding: 4px;">
          <input type="checkbox" class="user-checkbox" value="${u.id}" ${checked} ${disabled}>
          <div>
            <div>${escapeHtml(u.name)}</div>
            <div style="font-size: 11px; color: var(--text-muted);">${escapeHtml(u.job_title || '')}</div>
          </div>
        </label>
      `;
    });
    list.innerHTML = html;
  }

  document.getElementById('share-cancel')?.addEventListener('click', () => {
    document.getElementById('share-modal').classList.remove('open');
    currentShareDocId = null;
  });

  document.getElementById('share-confirm')?.addEventListener('click', async () => {
    if (!currentShareDocId) return;
    const isGlobal = $('share-global-flag').checked;
    const sharedIds = getCheckedUsers();
    
    try {
      const res = await fetch(`/v1/documents/${currentShareDocId}/share`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ is_global: isGlobal, shared_with_user_ids: sharedIds })
      });
      if (!res.ok) throw new Error('Failed to update sharing');
      document.getElementById('share-modal').classList.remove('open');
      loadDocuments();
    } catch (e) {
      console.error(e);
      alert('Error updating share settings: ' + e.message);
    }
  });

  async function updateDocumentGlobalStatus(id, isGlobal) {
    try {
      const response = await fetch(`/v1/documents/${id}`, {
        method: 'PATCH',
        headers: {
          'Content-Type': 'application/json'
        },
        body: JSON.stringify({ is_global: isGlobal })
      });
      if (!response.ok) throw new Error('Failed to update document global status');
    } catch (err) {
      console.error('Error updating document:', err);
      alert('Failed to update document: ' + err.message);
      // Revert checkbox state
      const checkbox = document.getElementById(`global-doc-${id}`);
      if (checkbox) checkbox.checked = !isGlobal;
    }
  }

  async function deleteDocument(id) {
    if (!confirm('Are you sure you want to delete this document? This will remove it from the RAG knowledge base.')) return;
    try {
      const response = await fetch(`/v1/documents/${id}`, { method: 'DELETE' });
      if (!response.ok) throw new Error('Failed to delete document');
      await loadDocuments();
    } catch (err) {
      console.error('Error deleting document:', err);
      alert('Failed to delete document: ' + err.message);
    }
  }

  function toggleDocumentChat(docName, isEnabled) {
    if (isEnabled) activeDocumentNames.add(docName);
    else activeDocumentNames.delete(docName);

    const input = $('chat-input');
    const fileBtn = $('attach-btn');
    const hasDocs = activeDocumentNames.size > 0;

    if (fileBtn) fileBtn.style.display = hasDocs ? 'none' : 'flex';
    if (input) {
      input.placeholder = hasDocs
        ? `Chatting with ${activeDocumentNames.size} document(s)...`
        : 'Message...';
    }
  }

  function viewDocumentSnapshot(id, name) {
    const modal = $('snapshot-modal');
    const title = $('snapshot-modal-title')?.firstElementChild;
    const content = $('snapshot-content');
    if (!modal || !title || !content) return;

    title.textContent = `Snapshot: ${name}`;
    content.innerHTML = `<div hx-ext="sse" sse-connect="/v1/documents/${id}/snapshot" sse-swap="chunk" sse-close="close" hx-swap="beforeend"></div>`;
    
    // Activate HTMX inside the content div
    htmx.process(content);
    
    modal.classList.add('open');
  }

  return {
    init,
    deleteDocument,
    updateDocumentGlobalStatus,
    openShareModal,
    toggleDocumentChat,
    sendMessage,
    stopStreaming,
    switchConversation,
    newConversation,
    deleteConversation,
    startEditMessage,
    confirmEdit,
    cancelEdit,
    switchSibling,
    switchSiblingByEl,
    removeFile,
    toggleReasoning,
    copyText,
    copyMessageText,
    selectSystemPrompt,
    editPromptUI,
    deletePromptUI,
    viewDocumentSnapshot,
  };
})();

document.addEventListener('DOMContentLoaded', () => ChatApp.init());
