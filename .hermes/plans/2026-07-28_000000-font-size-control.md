# Chat Font Size Control — Implementation Plan

> **For Hermes:** Use subagent-driven-development skill to implement this plan task-by-task.

**Goal:** Add a simple − / + font size control to the topbar that adjusts chat message text size and persists across sessions.

**Architecture:** A CSS custom property (`--chat-font-size`) controls the font size of message bubbles and markdown content. Two buttons in the topbar increment/decrement this value via JavaScript, which writes to `document.documentElement.style` and persists the choice to `localStorage`. The current size is displayed between the buttons as a small label.

**Tech Stack:** Vanilla HTML/CSS/JS (no new dependencies). The app already uses `:root` CSS custom properties, `localStorage`, and inline DOM event handlers — this feature follows the exact same patterns.

**Design choices to confirm with user:**

---

## Option A: Topbar placement (recommended)

Place the font size controls in the topbar, to the right of the title, before the right sidebar toggle.

```
[☰] [Chat]  ...spacer...  [− 14.5 +]  [📄]
```

**Pros:** Always visible, easy to find, follows the "controls in topbar" pattern used by many chat apps.
**Cons:** Takes space in an already somewhat busy topbar. On very narrow mobile (< 360px) might need to reduce the label.

## Option B: Input toolbar placement

Place the controls in the input toolbar row (next to the attach/web/image buttons).

```
[📎] [🌐 Web] [🖼 Image]     ...spacer...     [− 14.5 +]  [🎤] [↑]
```

**Pros:** Keeps topbar clean. Closer to where the user reads text.
**Cons:** Less discoverable. Input toolbar is already dense on mobile.

## Option C: Floating FAB

A small floating action button in the bottom-right corner of the chat area that opens a compact popover.

**Pros:** Zero topbar/toolbar clutter. Very mobile-friendly.
**Cons:** More complex. Less obvious to users. Over-engineered for a simple feature.

---

## Recommended: Option A (Topbar)

It's the simplest, most discoverable, and matches the user's stated preference for the topbar. On mobile (< 400px), we can hide the numeric label and show only the − and + buttons to save space.

### Sizing behavior

| Setting | Base font | Markdown body | Code blocks | Notes |
|---------|----------|---------------|-------------|-------|
| Min (12px) | 12px | 12px | scales proportionally | Compact reading |
| Default | 14.5px | 14.5px | 13px | Current behavior |
| Max (20px) | 20px | 20px | scales proportionally | Accessibility / large text |

All other font sizes (headings, code blocks, reasoning, etc.) scale proportionally via `em`/`rem` or explicit multiplier — but for simplicity, phase 1 only adjusts the primary chat text.

### Persistence

- Key: `chat-font-size` in `localStorage`
- On page load: read from localStorage, apply to `:root`, update label
- On button click: update `:root`, update localStorage, update label

### Edge cases

- **Min/max clamping:** At 12px, the minus button disables. At 20px, the plus button disables.
- **Multiple tabs:** No sync needed — each tab reads localStorage on load.
- **Streaming messages:** No special handling needed — the CSS variable applies live to all `.msg-bubble` and `.markdown-body` elements.

---

## Implementation Tasks

### Task 1: Add CSS custom property and control styles

**Files:** `public/chat.css`

Add to `:root`:
```css
--chat-font-size: 14.5px;
```

Change `.msg-bubble` font-size:
```css
font-size: var(--chat-font-size);
```

Change `.markdown-body` font-size:
```css
font-size: var(--chat-font-size);
```

Add new styles for the font size control group:
```css
/* ── Font Size Controls ──────────────── */
.font-size-control {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    flex-shrink: 0;
}

.font-size-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 28px;
    height: 28px;
    background: transparent;
    border: 1px solid var(--border-mid);
    color: var(--text-secondary);
    cursor: pointer;
    border-radius: var(--radius-sm);
    font-size: 16px;
    font-family: var(--font);
    font-weight: 500;
    line-height: 1;
    transition: all var(--transition);
}

.font-size-btn:hover {
    background: var(--bg-hover);
    color: var(--text-primary);
    border-color: var(--text-muted);
}

.font-size-btn:disabled {
    opacity: 0.35;
    cursor: not-allowed;
}

.font-size-value {
    font-size: 12px;
    color: var(--text-muted);
    min-width: 36px;
    text-align: center;
    user-select: none;
}
```

Add mobile refinement inside the `@media (max-width: 700px)` block:
```css
.font-size-value {
    display: none;  /* hide label on mobile, just show − + */
}
```

Add extra-narrow mobile refinement (optional):
```css
@media (max-width: 380px) {
    .font-size-btn {
        width: 24px;
        height: 24px;
        font-size: 14px;
    }
}
```

### Task 2: Add HTML controls to the topbar

**Files:** `public/chat.html`

In the topbar div (lines 168-188), insert the font size control group between the spacer div and the right sidebar toggle:

```html
<!-- Font size control -->
<div class="font-size-control" id="font-size-control">
  <button class="font-size-btn" id="font-size-minus" aria-label="Decrease font size" title="Smaller text">−</button>
  <span class="font-size-value" id="font-size-value">14.5</span>
  <button class="font-size-btn" id="font-size-plus" aria-label="Increase font size" title="Larger text">+</button>
</div>
```

Insert it right before the `<button class="right-sidebar-toggle">` (after the spacer div at line 177).

### Task 3: Add JavaScript logic

**Files:** `public/js/chat.js`

Add constants at the top of the ChatApp IIFE (near line 27, after the dictation constants):
```js
const FONT_SIZE_MIN = 12;
const FONT_SIZE_MAX = 20;
const FONT_SIZE_STEP = 0.5;
const FONT_SIZE_DEFAULT = 14.5;
const FONT_SIZE_STORAGE_KEY = 'chat-font-size';
```

Add functions inside the ChatApp object (returned in the public API):
```js
function getFontSize() {
  const stored = localStorage.getItem(FONT_SIZE_STORAGE_KEY);
  if (stored !== null) {
    const parsed = parseFloat(stored);
    if (!isNaN(parsed)) return Math.min(FONT_SIZE_MAX, Math.max(FONT_SIZE_MIN, parsed));
  }
  return FONT_SIZE_DEFAULT;
}

function applyFontSize(size) {
  document.documentElement.style.setProperty('--chat-font-size', size + 'px');
  const label = $('font-size-value');
  if (label) label.textContent = size;
  const minusBtn = $('font-size-minus');
  const plusBtn = $('font-size-plus');
  if (minusBtn) minusBtn.disabled = size <= FONT_SIZE_MIN;
  if (plusBtn) plusBtn.disabled = size >= FONT_SIZE_MAX;
  localStorage.setItem(FONT_SIZE_STORAGE_KEY, size);
}

function changeFontSize(delta) {
  const current = getFontSize();
  const next = Math.min(FONT_SIZE_MAX, Math.max(FONT_SIZE_MIN, current + delta));
  if (next !== current) applyFontSize(next);
}

// Export
return {
  // ... existing exports ...
  increaseFontSize: () => changeFontSize(FONT_SIZE_STEP),
  decreaseFontSize: () => changeFontSize(-FONT_SIZE_STEP),
};
```

Add initialization call at the bottom of the ChatApp IIFE (before the final `})();`):
```js
// Init font size from storage
document.addEventListener('DOMContentLoaded', () => {
  applyFontSize(getFontSize());
});
```

Wire up button handlers in `chat.html` via onclick attributes (matching the existing pattern used in the HTML file):
```html
<button class="font-size-btn" id="font-size-minus" aria-label="Decrease font size"
  onclick="ChatApp.decreaseFontSize()" title="Smaller text">−</button>
<button class="font-size-btn" id="font-size-plus" aria-label="Increase font size"
  onclick="ChatApp.increaseFontSize()" title="Larger text">+</button>
```

### Task 4: Verify

1. Open the chat page — verify "14.5" appears between − and + in the topbar
2. Click + — verify text grows to 15px, label updates to "15"
3. Click − — verify text shrinks back to 14.5px
4. Click − until 12px — verify minus button disables
5. Click + until 20px — verify plus button disables
6. Refresh the page — verify font size persists (read from localStorage)
7. Resize to mobile width (≤ 700px) — verify only − and + show (no label), buttons are tappable
8. Send a message — verify the message bubble and markdown content use the custom font size

---

## Risks & tradeoffs

- **No risk to streaming:** CSS variable changes apply instantly to all rendered messages without touching the DOM.
- **Code blocks:** Currently hardcoded at 13px. They won't scale. Acceptable for phase 1 — the user only asked about chat text.
- **Headings in markdown:** Currently use fixed sizes. They won't scale. Acceptable for phase 1.
- **localStorage quota:** Font size value is trivial (~4 bytes), no risk.
