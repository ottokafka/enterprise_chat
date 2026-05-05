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
  let webSearchEnabled = false; // true when Web Search toggle is active
  let mcpEnabled = false;       // true when MCP endpoint (/v1/mcp) is active

  // ── DOM Helpers ───────────────────────────────────────────────────────────
  const $ = id => document.getElementById(id);

  function scrollToBottom(force = false) {
    const log = $('chat-log');
    if (!log) return;
    const threshold = 100; // pixels from bottom
    const isAtBottom = log.scrollHeight - log.scrollTop - log.clientHeight < threshold;
    if (force || isAtBottom) {
      log.scrollTop = log.scrollHeight;
    }
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
       <p class="empty-sub">Supported files: .jpg, .jpeg, .png, .webp, .gif, .heic, .pdf, .xlsx, .csv, .docx, .doc, .txt, .md</p>
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
    enhanceCodeBlocks(log);
    scrollToBottom(true);
    // Async: populate branch nav for each user message
    messages.forEach((msg, i) => { if (msg.role === 'user') updateBranchNav(msg, i); });
  }

  function enhanceCodeBlocks(logEl) {
    if (!logEl) return;
    logEl.querySelectorAll('pre').forEach(pre => {
      if (pre.dataset.enhanced) return;
      const codeEl = pre.querySelector('code');
      if (!codeEl) return;
      pre.dataset.enhanced = 'true';
      pre.style.position = 'relative';

      const btnContainer = document.createElement('div');
      btnContainer.className = 'code-actions';
      btnContainer.style.position = 'absolute';
      btnContainer.style.top = '6px';
      btnContainer.style.right = '6px';
      btnContainer.style.display = 'flex';
      btnContainer.style.gap = '6px';
      btnContainer.style.zIndex = '10';

      const isHtml = codeEl.classList.contains('language-html') || codeEl.classList.contains('language-xml');

      if (isHtml) {
        const previewBtn = document.createElement('button');
        previewBtn.className = 'msg-edit-btn';
        previewBtn.innerHTML = `<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"></path><circle cx="12" cy="12" r="3"></circle></svg> Preview`;
        previewBtn.title = "Preview HTML";
        previewBtn.style.background = '#2d2d2d';
        previewBtn.style.color = '#e0e0e0';
        previewBtn.style.border = '1px solid #444';
        previewBtn.style.borderRadius = '4px';
        previewBtn.style.padding = '4px 8px';
        previewBtn.style.cursor = 'pointer';
        previewBtn.style.display = 'flex';
        previewBtn.style.alignItems = 'center';
        previewBtn.style.gap = '4px';
        previewBtn.style.fontSize = '12px';
        previewBtn.onclick = () => {
          const form = document.createElement('form');
          form.method = 'POST';
          form.action = '/preview';
          form.target = '_blank';
          const input = document.createElement('input');
          input.type = 'hidden';
          input.name = 'html';
          input.value = codeEl.textContent;
          form.appendChild(input);
          document.body.appendChild(form);
          form.submit();
          document.body.removeChild(form);
        };
        btnContainer.appendChild(previewBtn);
      }

      const copyBtn = document.createElement('button');
      copyBtn.className = 'msg-edit-btn';
      copyBtn.innerHTML = `<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg> Copy`;
      copyBtn.title = "Copy Code";
      copyBtn.style.background = '#2d2d2d';
      copyBtn.style.color = '#e0e0e0';
      copyBtn.style.border = '1px solid #444';
      copyBtn.style.borderRadius = '4px';
      copyBtn.style.padding = '4px 8px';
      copyBtn.style.cursor = 'pointer';
      copyBtn.style.display = 'flex';
      copyBtn.style.alignItems = 'center';
      copyBtn.style.gap = '4px';
      copyBtn.style.fontSize = '12px';
      copyBtn.onclick = () => {
        navigator.clipboard.writeText(codeEl.textContent);
        const originalHtml = copyBtn.innerHTML;
        copyBtn.innerHTML = `<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><polyline points="20 6 9 17 4 12"></polyline></svg> Copied!`;
        setTimeout(() => copyBtn.innerHTML = originalHtml, 2000);
      };
      btnContainer.appendChild(copyBtn);

      pre.appendChild(btnContainer);
    });
  }

  function buildUserCard(msg, index) {
    const msgId = msg.id || 'u' + index;
    let attachmentsHtml = '';
    if (msg.attachments && msg.attachments.length > 0) {
      const imagesHtml = msg.attachments
        .filter(a => a.dataUrl)
        .map(a => `<img src="${a.dataUrl}" class="chat-attached-image" onclick="ChatApp.openImageModal(this.src)" alt="${escapeHtml(a.name)}" />`)
        .join('');
      if (imagesHtml) {
        attachmentsHtml = `<div class="msg-attachments" style="display: flex; gap: 8px; flex-wrap: wrap;">${imagesHtml}</div>`;
      }
    }
    return `
      <div class="msg-wrapper msg-user" data-index="${index}" id="msg-${msgId}">
        <div class="msg-bubble user-bubble">
          <div class="msg-content">${escapeHtml(msg.content)}</div>
          ${attachmentsHtml}
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
    const isSearch = reasoning.includes('[search]');

    const reasoningBlock = reasoning ? `
      <div class="reasoning-block">
        <button class="reasoning-toggle" onclick="ChatApp.toggleReasoning(this)">
          <svg class="chevron" viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2"><polyline points="6 9 12 15 18 9"/></svg>
          <span>${duration ? (isSearch ? `Searched for ${duration}` : `Thought for ${duration}`) : (isSearch ? 'Searching...' : 'Thinking...')}</span>
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

    const attachments = attachedFiles.map(af => ({
      name: af.file.name,
      type: af.file.type,
      dataUrl: af.dataUrl
    }));

    // Save user message to IndexedDB
    const userId = await IndexedDBService.saveChat(
      currentConversationId, 'user', displayContent, parentId, timestamp, attachments
    );

    const userMsg = { id: userId, role: 'user', content: displayContent, attachments: attachments, parent_id: parentId, timestamp };
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
      scrollToBottom(true);
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
        if (webSearchEnabled) {
          formData.append('web_search', 'true');
        }
        requestConfig = {
          method: 'POST',
          body: formData,
          signal: abortController.signal,
        };
      }

      const endpoint = activeDocumentNames.size > 0
          ? '/v1/rag'
          : mcpEnabled ? '/v1/mcp' : '/v1/chat/completions';
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
              const isSearch = reasoningBuffer.includes('[search]');
              updateStreamingDOM(reasoningBuffer, contentBuffer, hasReasoning, reasoningStart, false, isSearch);
              lastUpdate = now;
              scrollToBottom();
            }
          } catch { }
        }
      }

      // Final update
      const isSearch = reasoningBuffer.includes('[search]');
      updateStreamingDOM(reasoningBuffer, contentBuffer, hasReasoning, reasoningStart, true, isSearch);
      scrollToBottom();

      // Finalize: save assistant message to IndexedDB
      const duration = hasReasoning ? formatDuration(Date.now() - reasoningStart) : null;
      const assistTimestamp = new Date().toISOString();
      const assistId = await IndexedDBService.saveChat(
        currentConversationId, 'assistant', contentBuffer, userId, assistTimestamp, []
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
        enhanceCodeBlocks(log);
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

  function updateStreamingDOM(reasoning, content, hasReasoning, reasoningStart, isFinal = false, isSearch = false) {
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
        label.textContent = isSearch ? `Searched for ${formatDuration(Date.now() - reasoningStart)}` : `Thought for ${formatDuration(Date.now() - reasoningStart)}`;
      } else if (label) {
        label.textContent = isSearch ? 'Searching...' : 'Thinking...';
      }

      // Auto-expand reasoning if Web Search is enabled and it's a search
      if (webSearchEnabled && isSearch && wrapper && body) {
        if (!isFinal) {
          if (body.classList.contains('collapsed')) {
            body.classList.remove('collapsed');
            wrapper.classList.add('expanded');
            const chevron = wrapper.querySelector('.chevron');
            if (chevron) chevron.style.transform = 'rotate(180deg)';
          }
        } else {
          // Collapse on completion
          body.classList.add('collapsed');
          wrapper.classList.remove('expanded');
          const chevron = wrapper.querySelector('.chevron');
          if (chevron) chevron.style.transform = '';
        }
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
        adjustChatInputHeight();
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
    if (stopBtn) stopBtn.style.display = streaming ? 'flex' : 'none';
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
    const oldMsg = messages[index];
    const parentId = index === 0 ? null : messages[index - 1].id;
    const timestamp = new Date().toISOString();
    const newUserId = await IndexedDBService.saveChat(
      currentConversationId, 'user', newContent, parentId, timestamp, oldMsg.attachments
    );
    const prefix = messages.slice(0, index);
    const newUserMsg = { id: newUserId, role: 'user', content: newContent, attachments: oldMsg.attachments, parent_id: parentId, timestamp };
    messages = [...prefix, newUserMsg];
    renderMessages();
    scrollToBottom(true);
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
    scrollToBottom(true);

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

    // Re-attach files from the edited message
    const lastMsg = messages[messages.length - 1];
    if (lastMsg && lastMsg.attachments && lastMsg.attachments.length > 0) {
      for (const att of lastMsg.attachments) {
        if (att.dataUrl) {
          try {
            const res = await fetch(att.dataUrl);
            const blob = await res.blob();
            const file = new File([blob], att.name || 'attachment', { type: att.type || blob.type });
            formData.append('files', file, file.name);
          } catch (e) {
            console.error('Failed to convert attachment to file', e);
          }
        }
      }
    }

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
        if (webSearchEnabled) {
          formData.append('web_search', 'true');
        }
        requestConfig = {
          method: 'POST',
          body: formData,
          signal: abortController.signal,
        };
      }

      const endpoint = activeDocumentNames.size > 0
          ? '/v1/rag'
          : mcpEnabled ? '/v1/mcp' : '/v1/chat/completions';
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
              const isSearch = reasoningBuffer.includes('[search]');
              updateStreamingDOM(reasoningBuffer, contentBuffer, hasReasoning, reasoningStart, false, isSearch);
              lastUpdate = now; scrollToBottom();
            }
          } catch { }
        }
      }
      const isSearch = reasoningBuffer.includes('[search]');
      updateStreamingDOM(reasoningBuffer, contentBuffer, hasReasoning, reasoningStart, true, isSearch);
      scrollToBottom();
      const duration = hasReasoning ? formatDuration(Date.now() - reasoningStart) : null;
      const assistTimestamp = new Date().toISOString();
      const assistId = await IndexedDBService.saveChat(
        currentConversationId, 'assistant', contentBuffer, newUserId, assistTimestamp, []
      );
      const assistMsg = { id: assistId, role: 'assistant', content: contentBuffer, reasoning: reasoningBuffer, reasoningDuration: duration, parent_id: newUserId, timestamp: assistTimestamp };
      messages.push(assistMsg);
      currentLeafId = assistId;
      const wrapper = $('streaming-wrapper');
      if (wrapper) {
        wrapper.outerHTML = buildAssistantCard(assistMsg, messages.length - 1);
        const log = $('chat-log');
        if (window.hljs) log?.querySelectorAll('pre code').forEach(el => hljs.highlightElement(el));
        enhanceCodeBlocks(log);
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
    scrollToBottom(true);
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
    scrollToBottom(true);
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
  function fileToBase64(file) {
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(reader.result);
      reader.onerror = error => reject(error);
      reader.readAsDataURL(file);
    });
  }

  async function addFiles(fileList) {
    for (const file of fileList) {
      let dataUrl = null;
      if (file.type.startsWith('image/')) {
        try {
          dataUrl = await fileToBase64(file);
        } catch (err) {
          console.error('Failed to read file as data url', err);
        }
      }
      attachedFiles.push({ file, dataUrl });
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
    row.innerHTML = attachedFiles.map((f, i) => {
      if (f.dataUrl) {
        return `
          <div class="image-preview-chip">
            <img src="${f.dataUrl}" onclick="ChatApp.openImageModal(this.src)" alt="${escapeHtml(f.file.name)}" />
            <button class="remove-btn" onclick="ChatApp.removeFile(${i})" title="Remove">
              <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2"><line x1="18" y1="6" x2="6" y2="18"></line><line x1="6" y1="6" x2="18" y2="18"></line></svg>
            </button>
          </div>`;
      }
      return `
        <div class="file-chip">
          <svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" stroke-width="2"><path d="M13 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><polyline points="13 2 13 9 20 9"/></svg>
          <span>${escapeHtml(f.file.name)}</span>
          <button onclick="ChatApp.removeFile(${i})" aria-label="Remove file">&times;</button>
        </div>`;
    }).join('');
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

  function openImageModal(src) {
    const modal = $('image-modal');
    const content = $('image-modal-content');
    if (modal && content) {
      content.src = src;
      modal.classList.add('open');
    }
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
    setupChatLogListeners();
    setupPasteListener();
    setupSidebarToggle();
    setupSidebarClickOutside();
    initIngestionUI();
    initSystemPromptUI();
  }

  function setupChatLogListeners() {
    const log = $('chat-log');
    if (!log) return;
    log.addEventListener('click', e => {
      // Check if clicked element is an image within .markdown-body
      if (e.target.tagName === 'IMG' && e.target.closest('.markdown-body')) {
        openImageModal(e.target.src);
      }
    });
  }

  function setupInputListeners() {
    const input = $('chat-input');
    const sendBtn = $('send-btn');
    const stopBtn = $('stop-btn');
    const fileBtn = $('attach-btn');
    const fileInput = $('file-input');

    sendBtn?.addEventListener('click', () => {
      const text = input?.value ?? '';
      if (input) {
        input.value = '';
        adjustChatInputHeight();
      }
      sendMessage(text);
    });

    stopBtn?.addEventListener('click', () => stopStreaming());

    // Web Search toggle
    $('web-search-toggle')?.addEventListener('click', () => {
      webSearchEnabled = !webSearchEnabled;
      const btn = $('web-search-toggle');
      btn?.classList.toggle('active', webSearchEnabled);
      btn?.setAttribute('aria-pressed', String(webSearchEnabled));
    });

    input?.addEventListener('keydown', e => {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        if (!isStreaming) {
          const text = input.value;
          input.value = '';
          adjustChatInputHeight();
          sendMessage(text);
        }
      }
    });

    // Auto-resize textarea
    input?.addEventListener('input', () => {
      adjustChatInputHeight();
    });

    fileBtn?.addEventListener('click', () => fileInput?.click());
    fileInput?.addEventListener('change', async e => await addFiles(e.target.files));

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
    input?.addEventListener('paste', async e => {
      const items = e.clipboardData?.items;
      if (!items) return;

      const filesToAdd = [];
      for (const item of items) {
        if (item.type.startsWith('image/')) {
          e.preventDefault();
          const file = item.getAsFile();
          if (file) filesToAdd.push(file);
        }
      }
      if (filesToAdd.length > 0) {
        await addFiles(filesToAdd);
      }
    });
  }

  function setupSidebarToggle() {
    $('sidebar-toggle')?.addEventListener('click', () => {
      document.querySelector('.sidebar')?.classList.toggle('open');
    });
    $('left-sidebar-close')?.addEventListener('click', () => {
      document.querySelector('.sidebar')?.classList.remove('open');
    });
  }

  function adjustChatInputHeight() {
    const input = $('chat-input');
    if (!input) return;

    // Reset height to measure accurate scrollHeight without losing focus or page scroll
    input.style.height = '24px';
    
    // Set new height based on scrollHeight, with a reasonable max limit
    const newHeight = Math.min(input.scrollHeight, 400); 
    input.style.height = newHeight + 'px';
    
    // Ensure the cursor remains visible if the text exceeds the max height
    if (input.scrollHeight > newHeight) {
      input.scrollTop = input.scrollHeight;
    }
  }

  function setupSidebarClickOutside() {
    document.addEventListener('click', (e) => {
      if (window.innerWidth > 700) return;

      const sidebar = document.querySelector('.sidebar');
      const rightSidebar = document.querySelector('.right-sidebar');
      const leftToggle = $('sidebar-toggle');
      const rightToggle = $('right-sidebar-toggle');

      // 1. Handle left sidebar
      if (sidebar && sidebar.classList.contains('open')) {
        const isOutside = !sidebar.contains(e.target) && (!leftToggle || !leftToggle.contains(e.target));
        if (isOutside) sidebar.classList.remove('open');
      }

      // 2. Handle right sidebar
      if (rightSidebar && rightSidebar.classList.contains('open')) {
        const isOutside = !rightSidebar.contains(e.target) && (!rightToggle || !rightToggle.contains(e.target));
        if (isOutside) rightSidebar.classList.remove('open');
      }
    });
  }

  // ── Document Ingestion ───────────────────────────────────────────────────
  function initIngestionUI() {
    const toggleBtn = $('right-sidebar-toggle');
    const rightSidebar = $('right-sidebar');
    const closeBtn = $('right-sidebar-close');
    const fileInput = $('rag-file-input');

    if (toggleBtn && rightSidebar) {
      if (window.innerWidth > 700) {
        rightSidebar.classList.add('open');
      }
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
    $('sidebar-system-prompt-btn')?.addEventListener('click', (e) => {
      e.stopPropagation();
      const menu = $('sidebar-system-prompt-menu');
      if (menu) {
        const isHidden = menu.style.display === 'none';
        document.querySelectorAll('.user-dropdown').forEach(d => d.style.display = 'none');
        menu.style.display = isHidden ? 'block' : 'none';
      }
    });

    document.addEventListener('click', () => {
      const menu = $('sidebar-system-prompt-menu');
      if (menu) menu.style.display = 'none';
    });

    $('sidebar-system-prompt-menu')?.addEventListener('click', (e) => e.stopPropagation());

    $('manage-prompts-sidebar-btn')?.addEventListener('click', () => {
      const menu = $('sidebar-system-prompt-menu');
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
    const list = $('sidebar-system-prompt-menu');
    if (!list) return;

    let html = `
      <div class="prompt-option ${selectedSystemPromptId === 'none' ? 'active' : ''}" 
           data-id="none" onclick="ChatApp.selectSystemPrompt('none')"
           style="padding: 8px 12px; font-size: 13px; color: var(--text-primary); cursor: pointer; transition: background 0.15s;"
           onmouseover="this.style.background='var(--bg-hover)'" onmouseout="this.style.background='transparent'">
        None (Default)
      </div>`;

    systemPrompts.forEach(p => {
      html += `
        <div class="prompt-option ${selectedSystemPromptId == p.id ? 'active' : ''}" 
             data-id="${p.id}" onclick="ChatApp.selectSystemPrompt(${p.id})"
             style="padding: 8px 12px; font-size: 13px; color: var(--text-primary); cursor: pointer; transition: background 0.15s;"
             onmouseover="this.style.background='var(--bg-hover)'" onmouseout="this.style.background='transparent'">
          ${escapeHtml(p.name)}
        </div>`;
    });
    list.innerHTML = html;

    const label = $('sidebar-system-prompt-label');
    if (label) {
      if (selectedSystemPromptId === 'none') {
        label.textContent = 'None (Default)';
      } else {
        const p = systemPrompts.find(x => x.id == selectedSystemPromptId);
        label.textContent = p ? p.name : 'None (Default)';
      }
    }
  }

  function selectSystemPrompt(id) {
    selectedSystemPromptId = id;
    renderSystemPromptOptions();
    const menu = $('sidebar-system-prompt-menu');
    if (menu) menu.style.display = 'none';
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
      <div class="progress-item-title" title="${titles}">${titles}</div>
      <div class="progress-item-stage" id="job-stage-wrapper-${jobId}">
        <span id="job-stage-${jobId}">Starting...</span>
        <span class="progress-item-percentage" id="job-perc-${jobId}">0%</span>
      </div>
      <div class="progress-bar-bg"><div class="progress-bar-fill" id="job-fill-${jobId}"></div></div>
    `;
    list.prepend(li);

    const source = new EventSource(`/v1/ingest/progress?job_id=${jobId}`);
    source.onmessage = function (event) {
      try {
        const data = JSON.parse(event.data);
        const stageEl = $(`job-stage-${jobId}`);
        const percEl = $(`job-perc-${jobId}`);
        const fillEl = $(`job-fill-${jobId}`);

        if (stageEl) stageEl.textContent = data.stage || data.status;
        if (percEl && data.progress !== undefined) percEl.textContent = `${data.progress}%`;
        if (fillEl && data.progress !== undefined) {
          fillEl.style.width = `${data.progress}%`;
          // Dynamically adjust color as it nears completion
          if (data.progress > 90) {
            fillEl.style.background = 'linear-gradient(90deg, #10b981, #34d399)'; // Emerald
            fillEl.style.boxShadow = '0 0 12px rgba(16, 185, 129, 0.4)';
          }
        }

        if (data.status === 'completed' || data.status === 'error') {
          source.close();
          if (stageEl) {
            stageEl.textContent = data.status === 'completed' ? 'Completed!' : 'Error occurred.';
            stageEl.style.color = data.status === 'completed' ? '#4ade80' : '#ef4444';
          }
          if (percEl) {
            percEl.textContent = data.status === 'completed' ? '100%' : 'Failed';
            percEl.style.color = data.status === 'completed' ? '#4ade80' : '#ef4444';
          }
          if (fillEl && data.status === 'completed') {
            fillEl.style.width = '100%';
            fillEl.style.background = '#4ade80';
            fillEl.style.boxShadow = '0 0 16px rgba(74, 222, 128, 0.4)';
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
  let checkedShareUserIds = new Set();

  async function openShareModal(docId, isGlobal, sharedWithIds) {
    currentShareDocId = docId;
    checkedShareUserIds = new Set(sharedWithIds || []);
    const modal = document.getElementById('share-modal');
    $('share-global-flag').checked = isGlobal;
    $('share-search-input').value = '';

    await fetchAllUsers();
    renderShareUsers();

    // UI logic for toggling global vs specific
    const toggleGlobal = () => {
      const isG = $('share-global-flag').checked;
      $('share-search-input').disabled = isG;
      document.querySelectorAll('#share-users-list .user-checkbox').forEach(cb => cb.disabled = isG);
    };
    $('share-global-flag').onchange = toggleGlobal;
    $('share-search-input').oninput = () => renderShareUsers();

    toggleGlobal();
    modal.classList.add('open');
  }

  function handleShareUserToggle(checkbox) {
    const id = parseInt(checkbox.value);
    if (checkbox.checked) {
      checkedShareUserIds.add(id);
    } else {
      checkedShareUserIds.delete(id);
    }
  }

  function getCheckedUsers() {
    return Array.from(checkedShareUserIds);
  }

  function renderShareUsers() {
    const query = $('share-search-input').value.toLowerCase();
    const list = $('share-users-list');
    const isG = $('share-global-flag').checked;

    let html = '';
    
    // Always show checked users first, then filtered unchecked users
    const checkedUsers = allUsers.filter(u => checkedShareUserIds.has(u.id));
    const uncheckedFiltered = allUsers.filter(u => !checkedShareUserIds.has(u.id) && (u.name.toLowerCase().includes(query) || (u.job_title && u.job_title.toLowerCase().includes(query))));
    
    const displayUsers = [...checkedUsers, ...uncheckedFiltered];

    if (displayUsers.length === 0) {
      list.innerHTML = '<div style="font-size: 12px; color: var(--text-muted);">No users found.</div>';
      return;
    }

    displayUsers.forEach(u => {
      const checked = checkedShareUserIds.has(u.id) ? 'checked' : '';
      const disabled = isG ? 'disabled' : '';
      html += `
        <label style="display: flex; align-items: center; gap: 8px; font-size: 13px; color: var(--text-primary); cursor: pointer; padding: 4px;">
          <input type="checkbox" class="user-checkbox" value="${u.id}" ${checked} ${disabled} onchange="ChatApp.handleShareUserToggle(this)">
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

  // ── MCP Integration ──────────────────────────────────────────────────────
  /**
   * Toggle MCP endpoint mode on/off.
   * When enabled, chat completions are routed to /v1/mcp instead of
   * /v1/chat/completions. The MCP server speaks JSON-RPC 2.0 over HTTP.
   */
  function toggleMCP(enabled) {
    mcpEnabled = enabled !== undefined ? enabled : !mcpEnabled;
    console.log('[MCP] endpoint mode:', mcpEnabled ? '/v1/mcp' : '/v1/chat/completions');
    return mcpEnabled;
  }

  /**
   * Low-level MCP tool call helper.
   * Sends a JSON-RPC 2.0 `tools/call` request to /v1/mcp and returns the
   * parsed result content array.
   *
   * @param {string} toolName  - Name of the registered MCP tool
   * @param {object} args      - Tool input arguments (matched against tool schema)
   * @returns {Promise<Array>} - Array of MCP content items (TextContent etc.)
   */
  async function callMCPTool(toolName, args) {
    // MCP Streamable HTTP transport: initialize with a POST that establishes a session
    const initRes = await fetch('/v1/mcp', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Accept': 'application/json, text/event-stream' },
      body: JSON.stringify({
        jsonrpc: '2.0',
        id: 1,
        method: 'initialize',
        params: {
          protocolVersion: '2025-03-26',
          capabilities: {},
          clientInfo: { name: 'enterprise-chat-ui', version: '1.0.0' },
        },
      }),
    });

    if (!initRes.ok) throw new Error(`MCP init failed: ${initRes.status}`);
    const sessionId = initRes.headers.get('mcp-session-id');
    if (!sessionId) throw new Error('No MCP session ID returned');

    // Send initialized notification
    await fetch('/v1/mcp', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'mcp-session-id': sessionId,
      },
      body: JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized', params: {} }),
    });

    // Call the tool
    const callRes = await fetch('/v1/mcp', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Accept': 'application/json, text/event-stream',
        'mcp-session-id': sessionId,
      },
      body: JSON.stringify({
        jsonrpc: '2.0',
        id: 2,
        method: 'tools/call',
        params: { name: toolName, arguments: args },
      }),
    });

    if (!callRes.ok) throw new Error(`MCP tools/call failed: ${callRes.status}`);
    const data = await callRes.json();
    if (data.error) throw new Error(`MCP tool error: ${JSON.stringify(data.error)}`);
    return data.result?.content ?? [];
  }

  /**
   * Smoke-test the calculator MCP tool.
   * Injects the result as an assistant message in the current chat log.
   *
   * @param {string} operation - 'add' | 'subtract' | 'multiply' | 'divide'
   * @param {number} a         - First operand
   * @param {number} b         - Second operand
   */
  async function testMCPCalculator(operation = 'add', a = 6, b = 7) {
    const log = $('chat-log');
    if (log) {
      log.insertAdjacentHTML('beforeend', `
        <div class="msg-wrapper msg-assistant" id="mcp-test-result">
          <div class="msg-bubble assistant-bubble">
            <div class="msg-content">⏳ Calling MCP calculator tool…</div>
          </div>
        </div>`);
      scrollToBottom(true);
    }
    try {
      const content = await callMCPTool('calculator', { operation, a, b });
      const text = content.map(c => c.text || JSON.stringify(c)).join('\n');
      const el = $('mcp-test-result');
      if (el) el.innerHTML = `
        <div class="msg-bubble assistant-bubble">
          <div class="msg-content markdown-body">
            <strong>🔧 MCP Calculator Result</strong><br/>
            <code>${escapeHtml(text)}</code>
          </div>
        </div>`;
    } catch (err) {
      const el = $('mcp-test-result');
      if (el) el.innerHTML = `<div class="msg-bubble assistant-bubble"><div class="msg-content">❌ MCP error: ${escapeHtml(err.message)}</div></div>`;
      console.error('[MCP] testMCPCalculator error:', err);
    }
  }

  return {
    init,
    deleteDocument,
    updateDocumentGlobalStatus,
    openShareModal,
    handleShareUserToggle,
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
    openImageModal,
    // MCP
    toggleMCP,
    callMCPTool,
    testMCPCalculator,
  };
})();

document.addEventListener('DOMContentLoaded', () => ChatApp.init());
