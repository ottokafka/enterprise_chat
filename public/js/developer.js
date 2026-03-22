const apiKeysContainer = document.getElementById('api-keys-container');
const loadingKeys = document.getElementById('loading-keys');
const generateKeyBtn = document.getElementById('generate-key-btn');
const newKeyModal = document.getElementById('new-key-modal');
const newKeyInput = document.getElementById('new-key-input');
const copyKeyBtn = document.getElementById('copy-key-btn');

// State
let apiKeys = [];

async function fetchApiKeys() {
  try {
    const res = await fetch('/fetch-api-key');
    if (!res.ok) throw new Error('Failed to fetch keys');
    const data = await res.json();
    apiKeys = data.results || [];
    renderKeys();
    updatePlaceholders();
  } catch (err) {
    console.error(err);
    loadingKeys.textContent = 'Error loading API keys.';
  }
}

async function generateKey() {
  generateKeyBtn.disabled = true;
  generateKeyBtn.textContent = 'Generating...';
  try {
    const res = await fetch('/create-api-key', { method: 'POST' });
    if (!res.ok) throw new Error('Failed to create key');
    const data = await res.json();

    // Add to list visually
    const newEntry = { id: data.id.toString(), key: data.key, created_at: new Date().toLocaleDateString('en-GB').replace(/\//g, '-') };
    apiKeys.push(newEntry);
    renderKeys();
    updatePlaceholders();

    // Show modal
    newKeyInput.value = data.key;
    newKeyModal.classList.add('open');
  } catch (err) {
    console.error(err);
    alert('Failed to generate key');
  } finally {
    generateKeyBtn.disabled = false;
    generateKeyBtn.textContent = 'Generate New Key';
  }
}

async function deleteKey(id) {
  if (!confirm('Are you sure you want to delete this API key? Systems using it will immediately lose access.')) return;

  try {
    const res = await fetch('/delete-api-key', {
      method: 'DELETE',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: parseInt(id) })
    });
    if (!res.ok) throw new Error('Failed to delete key');

    apiKeys = apiKeys.filter(k => k.id !== id.toString());
    renderKeys();
    updatePlaceholders();
  } catch (err) {
    console.error(err);
    alert('Failed to delete key');
  }
}

function renderKeys() {
  loadingKeys.style.display = 'none';
  if (apiKeys.length === 0) {
    apiKeysContainer.innerHTML = '<div style="font-size: 13px; color: var(--text-muted);">No API keys generated yet.</div>';
    return;
  }

  apiKeysContainer.innerHTML = apiKeys.map(k => {
    let kValue = k.key;
    if (!kValue.startsWith('sk-')) kValue = 'sk-' + kValue;
    
    return `
      <div class="api-key-item">
        <div>
          <div class="api-key-value" title="${kValue}">${kValue}</div>
          <div class="api-key-meta">Created on: ${k.created_at}</div>
        </div>
        <button class="btn btn-danger" onclick="deleteKey('${k.id}')">Delete</button>
      </div>
    `;
  }).join('');
}

const codeTemplates = new Map();

function updatePlaceholders() {
  if (codeTemplates.size === 0) {
    document.querySelectorAll('pre code.example-code').forEach(block => {
      codeTemplates.set(block, block.innerHTML);
    });
  }

  let activeKey = 'API_KEY';
  if (apiKeys.length > 0) {
    activeKey = apiKeys[apiKeys.length - 1].key;
    if (!activeKey.startsWith('sk-')) {
      activeKey = 'sk-' + activeKey;
    }
  }

  if (window.hljs) {
    document.querySelectorAll('pre code.example-code').forEach(block => {
      const template = codeTemplates.get(block);
      if (template) {
        // Use a temporary div to parse template and update spans
        const temp = document.createElement('div');
        temp.innerHTML = template;
        temp.querySelectorAll('.api-key-placeholder').forEach(el => {
          el.textContent = activeKey;
        });
        block.innerHTML = temp.innerHTML;
        hljs.highlightElement(block);
      }
    });
  } else {
    // Fallback if highlight.js not loaded
    document.querySelectorAll('.api-key-placeholder').forEach(el => {
      el.textContent = activeKey;
    });
  }
}

function closeModal() {
  newKeyModal.classList.remove('open');
  newKeyInput.value = '';
}

// Listeners
generateKeyBtn.addEventListener('click', generateKey);

copyKeyBtn.addEventListener('click', async () => {
  try {
    await navigator.clipboard.writeText(newKeyInput.value);
    copyKeyBtn.textContent = 'Copied!';
    setTimeout(() => copyKeyBtn.textContent = 'Copy', 2000);
  } catch (e) {
    console.error(e);
  }
});

// Init
document.addEventListener('DOMContentLoaded', () => {
  // We'll call fetch which will in turn update placeholders and trigger highlighting
  fetchApiKeys();
});
