// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useRef, useState } from 'react'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import hljs from 'highlight.js'
import 'highlight.js/styles/github-dark.css'
import { streamReply, checkImageAvailable, DEFAULT_IMAGE } from './backend.js'
import { signOut } from './auth.js'
import { THEME_TEXT, currentTheme, nextTheme, setTheme } from './theme.js'

marked.setOptions({ breaks: true })

function AvatarShape({ user }) {
  const who = user ? 'user' : 'assistant'
  return (
    <svg viewBox="0 0 100 100" className={`avatar-shape ${who}`} aria-hidden="true">
      {/* Fill: the closed silhouette (the cuts as diagonal edges) */}
      <polygon className="avatar-fill" points="50,2 100,50 84,66 50,98 0,50 16,34" />
      {/* Outline: two open segments, with gaps at the cuts (top left
          and bottom right); the corners top right and bottom left are
          drawn through. Colours from the theme (styles.css). */}
      <polyline className="avatar-line" points="16,34 50,2 100,50" />
      <polyline className="avatar-line" points="84,66 50,98 0,50" />
    </svg>
  )
}

// The bolt's colours come from the theme (styles.css): the gradient's five
// stops, the glitch copy behind it and an outline.
function Bolt() {
  return (
    <svg viewBox="0 0 24 24" className="bolt" aria-hidden="true">
      <defs>
        <linearGradient id="boltgrad" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0%" className="bolt-stop-1" />
          <stop offset="25%" className="bolt-stop-2" />
          <stop offset="50%" className="bolt-stop-3" />
          <stop offset="75%" className="bolt-stop-4" />
          <stop offset="100%" className="bolt-stop-5" />
        </linearGradient>
      </defs>
      <path className="bolt-glitch" d="M13 2 3 14h7l-1 8L19 10h-7z" transform="translate(0.9 0.7)" />
      <path className="bolt-main" d="M13 2 3 14h7l-1 8L19 10h-7z" fill="url(#boltgrad)" />
    </svg>
  )
}

function prettyJSON(s) {
  try {
    return JSON.stringify(JSON.parse(s), null, 2)
  } catch {
    return s
  }
}

function parseArgs(call) {
  try {
    return JSON.parse(call.arguments || '{}')
  } catch {
    return {}
  }
}

const trunc = (s, n = 90) =>
  typeof s === 'string' && s.length > n ? s.slice(0, n - 1) + '…' : s

// What the chip's header shows after the tool's name.
function toolHeaderSummary(call) {
  const args = parseArgs(call)
  switch (call.name) {
    case 'bash':
      return trunc(args.command)
    case 'write':
    case 'edit':
    case 'read':
      return args.path
    case 'ls':
      return args.path || '.'
    case 'rg':
      return args.pattern
        ? `/${trunc(args.pattern, 40)}/` + (args.path ? ` in ${args.path}` : '')
        : args.path
    case 'fd':
      return trunc([args.pattern, args.path].filter(Boolean).join(' ')) || null
    case 'webfetch':
      return trunc(args.url)
    default:
      return null
  }
}

// The sections an opened chip shows.
function toolSections(call) {
  const args = parseArgs(call)
  const sections = []
  if (call.name === 'bash') {
    sections.push({ label: 'command', value: args.command })
  } else if (call.name === 'write') {
    sections.push({ label: 'content', value: args.content })
  } else if (!['read', 'ls', 'webfetch'].includes(call.name)) {
    sections.push({ label: 'params', value: prettyJSON(call.arguments) })
  }
  sections.push({ label: 'output', value: call.output })
  return sections.filter((s) => s.value)
}

// The model's view of the conversation. Assistant turns go back exactly as
// the backend emitted them (tool_calls + tool results included): replaying
// only the rendered text would show the model a history of answers with
// results but no tool calls, which teaches it to claim tool use instead of
// doing it. pendingText is text streamed after the last history event, i.e.
// a turn that was stopped or failed before its round completed.
function wireHistory(messages) {
  return messages.flatMap((m) =>
    m.role === 'user'
      ? [{ role: 'user', content: m.content }]
      : [...m.wire, ...(m.pendingText ? [{ role: 'assistant', content: m.pendingText }] : [])],
  )
}

// Merge a tool result into its chip. It is matched by seq, the number the
// backend gives each call of this answer, not by the provider's id: that
// repeats from round to round (bash:0, bash:1, ...), and the result would
// land in the chip of the same name from an earlier round while its own
// stayed "running" for ever.
function settleToolCall(calls, call) {
  return calls.map((c) => (c.seq === call.seq ? { ...c, ...call, pending: false } : c))
}

function ToolCallChip({ call }) {
  const summary = toolHeaderSummary(call)
  if (call.pending) {
    return (
      <div className="tool-call pending">
        <span className="tool-spinner" aria-hidden="true" />
        <span className="tool-name">{call.name}</span>
        {summary && <span className="tool-args">{summary}</span>}
        <span className="tool-pending-label">running</span>
      </div>
    )
  }
  return (
    <details className="tool-call">
      <summary>
        <span className="tool-name">{call.name}</span>
        {summary && <span className="tool-args">{summary}</span>}
      </summary>
      <div className="tool-body">
        {toolSections(call).map((s) => (
          <div key={s.label}>
            <div className="tool-label">{s.label}</div>
            <pre className={call.name === 'webfetch' ? 'wrap' : ''}>{s.value}</pre>
          </div>
        ))}
      </div>
    </details>
  )
}

function useTyped(lines, speed = 50, resetKey = 0) {
  const total = lines.join('').length
  const [n, setN] = useState(0)
  // start the animation over when resetKey changes (a new chat)
  useEffect(() => {
    setN(0)
  }, [resetKey])
  useEffect(() => {
    if (n >= total) return
    const t = setTimeout(() => setN(n + 1), speed)
    return () => clearTimeout(t)
  }, [n, total, speed])
  let remaining = n
  const typed = lines.map((l) => {
    const part = remaining > 0 ? l.slice(0, remaining) : ''
    remaining -= l.length
    return part
  })
  const activeLine = n >= total ? -1 : typed[0].length < lines[0].length ? 0 : 1
  return { typed, activeLine }
}

function renderMarkdown(text) {
  return DOMPurify.sanitize(marked.parse(text))
}

// Who is signed in: the avatar, which opens a menu with the name and the
// way out. A click outside or Escape closes it.
function UserMenu({ user }) {
  const [open, setOpen] = useState(false)
  const ref = useRef(null)
  useEffect(() => {
    if (!open) return
    const outside = (e) => {
      if (!ref.current?.contains(e.target)) setOpen(false)
    }
    const escape = (e) => {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('pointerdown', outside)
    document.addEventListener('keydown', escape)
    return () => {
      document.removeEventListener('pointerdown', outside)
      document.removeEventListener('keydown', escape)
    }
  }, [open])
  return (
    <div className="user-menu" ref={ref}>
      <button
        className={`user-button${open ? ' open' : ''}`}
        onClick={() => setOpen((o) => !o)}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={`Signed in as ${user.login}`}
      >
        {user.picture ? (
          <img className="user-avatar" src={user.picture} alt="" />
        ) : (
          <span className="user-avatar user-initial">{user.login.slice(0, 1).toUpperCase()}</span>
        )}
      </button>
      {open && (
        <div className="user-dropdown" role="menu">
          <div className="user-name">{user.login}</div>
          {user.email && <div className="user-email">{user.email}</div>}
          <button className="user-signout" role="menuitem" onClick={signOut}>
            Sign out
          </button>
        </div>
      )}
    </div>
  )
}

let nextId = 1

// Remember the last sandbox image in the browser: it survives reloads and
// later days, until another one is entered.
const IMAGE_STORAGE_KEY = 'fechat.image'

function loadStoredImage() {
  try {
    return localStorage.getItem(IMAGE_STORAGE_KEY) || DEFAULT_IMAGE
  } catch {
    return DEFAULT_IMAGE
  }
}

export default function App({ user }) {
  const [messages, setMessages] = useState([])
  const [input, setInput] = useState('')
  const [streaming, setStreaming] = useState(false)
  const [chatEpoch, setChatEpoch] = useState(0)
  // The conversation's sandbox image: editable until the first message
  // (messages.length === 0), immutable after it.
  const [image, setImage] = useState(loadStoredImage)
  const [theme, setThemeState] = useState(currentTheme)
  // imageCheck: { state: 'idle'|'checking'|'ok'|'error', message }
  const [imageCheck, setImageCheck] = useState({ state: 'idle', message: '' })
  const lastCheckedImageRef = useRef('')
  const imageCheckSeq = useRef(0)
  const bottomRef = useRef(null)
  const chatRef = useRef(null)
  const textareaRef = useRef(null)

  // The textarea grows with its content up to 200px, then scrolls inside
  useEffect(() => {
    const el = textareaRef.current
    if (!el) return
    el.style.height = 'auto'
    const next = Math.min(el.scrollHeight, 200)
    el.style.height = `${next}px`
    el.style.overflowY = el.scrollHeight > 200 ? 'auto' : 'hidden'
  }, [input])
  const abortRef = useRef(null)
  const conversationIdRef = useRef(crypto.randomUUID())
  // The conversation's sandbox reference: the backend hands it out with
  // the first tool call, and every later request sends it back.
  const sandboxRefRef = useRef('')
  const atBottomRef = useRef(true)

  // Auto-scroll: follow while the user is at the bottom. Any scrolling up
  // (wheel or touch) stops following AT ONCE, synchronously in the event,
  // so that fast stream chunks do not snap back before 'scroll' fires.
  useEffect(() => {
    const onScroll = () => {
      if (
        window.innerHeight + window.scrollY >=
        document.documentElement.scrollHeight - 60
      ) {
        atBottomRef.current = true
      }
    }
    const onWheel = (e) => {
      if (e.deltaY < 0) atBottomRef.current = false
    }
    const onTouch = () => {
      atBottomRef.current = false
    }
    window.addEventListener('scroll', onScroll, { passive: true })
    window.addEventListener('wheel', onWheel, { passive: true })
    window.addEventListener('touchmove', onTouch, { passive: true })
    onScroll()
    return () => {
      window.removeEventListener('scroll', onScroll)
      window.removeEventListener('wheel', onWheel)
      window.removeEventListener('touchmove', onTouch)
    }
  }, [])
  const words = THEME_TEXT[theme]
  // a new chat or another theme types the greeting anew
  const { typed, activeLine } = useTyped([words.greeting], 50, `${chatEpoch}/${theme}`)

  useEffect(() => {
    if (atBottomRef.current) {
      bottomRef.current?.scrollIntoView({ behavior: 'auto' })
    }
  }, [messages])

  // Tie the app's height to the visual viewport (an opening keyboard), so
  // the page is not scrolled away but the chat area shrinks. Scroll the
  // chat by the height's delta as well: what sat at the composer bar before
  // then sits right above the keyboard.
  useEffect(() => {
    const vv = window.visualViewport
    if (!vv) return
    let lastHeight = vv.height
    const update = () => {
      const delta = vv.height - lastHeight
      lastHeight = vv.height
      document.documentElement.style.setProperty('--app-height', `${vv.height}px`)
      if (delta !== 0) {
        // after the layout (next frame), move on by the delta
        requestAnimationFrame(() => {
          const el = chatRef.current
          if (el) el.scrollTop -= delta
        })
      }
    }
    update()
    vv.addEventListener('resize', update)
    return () => vv.removeEventListener('resize', update)
  }, [])

  // Measure the topbar's and the composer's height -> --topbar-h and
  // --composer-h (the padding and the mask fade of the full-screen scroller)
  useEffect(() => {
    const root = document.documentElement
    const topbar = document.querySelector('.topbar')
    const composer = document.querySelector('.composer')
    if (!topbar || !composer || typeof ResizeObserver === 'undefined') return
    const measure = () => {
      root.style.setProperty('--topbar-h', `${topbar.offsetHeight}px`)
      root.style.setProperty('--composer-h', `${composer.offsetHeight}px`)
    }
    const ro = new ResizeObserver(measure)
    ro.observe(topbar)
    ro.observe(composer)
    measure()
    return () => ro.disconnect()
  }, [])

  // Add syntax highlighting and copy buttons after every render (streaming)
  useEffect(() => {
    document.querySelectorAll('.markdown pre code:not([data-highlighted])').forEach((el) => {
      try {
        hljs.highlightElement(el)
      } catch {
        // incomplete code while streaming: the next chunk tries again
        el.removeAttribute('data-highlighted')
      }
    })
    document.querySelectorAll('.markdown pre').forEach((pre) => {
      if (pre.querySelector(':scope > .copy-btn')) return
      const btn = document.createElement('button')
      btn.className = 'copy-btn'
      btn.textContent = 'copy'
      btn.onclick = async (e) => {
        e.stopPropagation()
        const text = pre.querySelector('code')?.innerText ?? pre.innerText
        try {
          await navigator.clipboard.writeText(text)
        } catch {
          const ta = document.createElement('textarea')
          ta.value = text
          document.body.appendChild(ta)
          ta.select()
          document.execCommand('copy')
          ta.remove()
        }
        btn.textContent = 'copied!'
        btn.classList.add('copied')
        setTimeout(() => {
          btn.textContent = 'copy'
          btn.classList.remove('copied')
        }, 1500)
      }
      pre.appendChild(btn)
    })
  }, [messages])

  const typingDone = activeLine === -1
  useEffect(() => {
    if (!typingDone) return
    const t = setTimeout(() => textareaRef.current?.focus(), 200)
    return () => clearTimeout(t)
  }, [typingDone])

  // Check that the image is there, on Enter or when the field loses focus.
  // A ref that was checked already is not checked again, unless its check
  // failed (a passing network error can be retried).
  async function checkImage() {
    const ref = image.trim()
    if (!ref) {
      setImageCheck({ state: 'idle', message: '' })
      return
    }
    if (ref === lastCheckedImageRef.current && imageCheck.state !== 'error') return
    const seq = ++imageCheckSeq.current
    lastCheckedImageRef.current = ref
    setImageCheck({ state: 'checking', message: '' })
    try {
      const res = await checkImageAvailable(ref)
      if (seq !== imageCheckSeq.current) return // stale (the field was edited meanwhile)
      if (res.ok) setImageCheck({ state: 'ok', message: res.warning || '' })
      else setImageCheck({ state: 'error', message: res.message || 'not available' })
    } catch (err) {
      if (err.name === 'AbortError') return
      if (seq !== imageCheckSeq.current) return
      setImageCheck({ state: 'error', message: err.message })
    }
  }

  async function send() {
    const content = input.trim()
    if (!content || streaming) return

    const userMsg = { id: nextId++, role: 'user', content }
    // content/toolCalls are what the UI renders; wire/pendingText are what
    // goes back to the model (see wireHistory).
    const assistantMsg = {
      id: nextId++,
      role: 'assistant',
      content: '',
      toolCalls: [],
      wire: [],
      pendingText: '',
    }
    const history = [...messages, userMsg]

    setMessages([...history, assistantMsg])
    setInput('')
    setStreaming(true)
    textareaRef.current?.focus()
    // follow one's own message
    atBottomRef.current = true

    const controller = new AbortController()
    abortRef.current = controller

    const conversationId = conversationIdRef.current
    try {
      for await (const item of streamReply(wireHistory(history), {
        signal: controller.signal,
        conversationId,
        sandboxRef: sandboxRefRef.current,
        image: image.trim(),
        theme,
      })) {
        if (item.type === 'sandbox') {
          // only for the conversation still open (not after "new chat")
          if (
            item.conversationId === conversationId &&
            conversationIdRef.current === conversationId
          ) {
            sandboxRefRef.current = item.ref
          }
        } else if (item.type === 'tool_start') {
          setMessages((prev) =>
            prev.map((m) =>
              m.id === assistantMsg.id
                ? {
                    ...m,
                    // The text before the tool calls and the text that
                    // continues after them go into the same content string:
                    // insert a paragraph break so that "...result." and
                    // "Done ..." do not run together. Once per answer (later
                    // tool_starts of the round see the trailing \n).
                    content:
                      m.content && !m.content.endsWith('\n')
                        ? m.content + '\n\n'
                        : m.content,
                    toolCalls: [...(m.toolCalls ?? []), { ...item.call, pending: true }],
                  }
                : m,
            ),
          )
        } else if (item.type === 'tool') {
          setMessages((prev) =>
            prev.map((m) =>
              m.id === assistantMsg.id
                ? { ...m, toolCalls: settleToolCall(m.toolCalls ?? [], item.call) }
                : m,
            ),
          )
        } else if (item.type === 'history') {
          setMessages((prev) =>
            prev.map((m) =>
              m.id === assistantMsg.id
                ? { ...m, wire: [...m.wire, ...item.messages], pendingText: '' }
                : m,
            ),
          )
        } else {
          setMessages((prev) =>
            prev.map((m) =>
              m.id === assistantMsg.id
                ? {
                    ...m,
                    content: m.content + item.content,
                    pendingText: m.pendingText + item.content,
                  }
                : m,
            ),
          )
        }
      }
    } catch (err) {
      if (err.name === 'AbortError') return
      // A TypeError is a fetch broken off at the connection (the phone
      // changed cells, the screen locked, ...), which would otherwise show
      // only the browser's raw message ("Network error").
      const msg =
        err instanceof TypeError
          ? 'Connection lost — please send again.'
          : err.message
      setMessages((prev) =>
        prev.map((m) =>
          m.id === assistantMsg.id ? { ...m, content: m.content + `\n\n⚠️ ${msg}` } : m,
        ),
      )
    } finally {
      setStreaming(false)
    }
  }

  // On touch devices (coarse pointer), drop the focus after sending so the
  // on-screen keyboard closes. The desktop keeps the focus.
  function closeTouchKeyboard() {
    if (window.matchMedia?.('(pointer: coarse)').matches) {
      textareaRef.current?.blur()
    }
  }

  function onKeyDown(e) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      send()
      closeTouchKeyboard()
    }
  }

  function stop() {
    abortRef.current?.abort()
  }

  // The bolt in the logo switches the theme; the name beside it starts a
  // new chat.
  function switchTheme() {
    const next = nextTheme(theme)
    setTheme(next)
    setThemeState(next)
  }

  function newChat() {
    // abort the running answer so it does not flow into the new chat
    abortRef.current?.abort()
    abortRef.current = null
    conversationIdRef.current = crypto.randomUUID()
    sandboxRefRef.current = ''
    setStreaming(false)
    setMessages([])
    setChatEpoch((e) => e + 1)
  }

  return (
    <div className="app">
      <header className="topbar">
        <div className="logo">
          <button
            className="logo-bolt"
            onClick={switchTheme}
            title={nextTheme(theme) === 'gta' ? 'Vice City' : 'Back to cyber'}
            aria-label="Switch theme"
          >
            <Bolt />
          </button>
          <button className="logo-name" onClick={newChat} title="New chat">
            {words.name}
          </button>
        </div>
        <div className="topbar-right">
          <div className="image-box">
            <input
              className={`image-input${imageCheck.state === 'ok' ? ' ok' : ''}${imageCheck.state === 'error' ? ' invalid' : ''}`}
              value={image}
              onChange={(e) => {
                setImage(e.target.value)
                try {
                  localStorage.setItem(IMAGE_STORAGE_KEY, e.target.value)
                } catch {
                  // private mode or full storage: the session's state will do
                }
                setImageCheck({ state: 'idle', message: '' })
              }}
              onBlur={checkImage}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault()
                  checkImage()
                  e.currentTarget.blur()
                }
              }}
              disabled={messages.length > 0}
              spellCheck={false}
              aria-label="Sandbox image"
              aria-describedby="image-tooltip"
            />
            <div className="image-tooltip" id="image-tooltip" role="tooltip">
              <div className="image-tooltip-title">Sandbox Image</div>
              OCI image for this chat's sandbox — the agent and its tools
              run inside.
              <br />
              Default: <code>{DEFAULT_IMAGE}</code>
            </div>
            <div
              className={`image-status ${imageCheck.state}`}
              title={imageCheck.message}
            >
              {imageCheck.state === 'checking' && 'checking…'}
              {imageCheck.state === 'error' && '✗ error'}
            </div>
          </div>
          {user.login && <UserMenu user={user} />}
        </div>
      </header>

      <main className="chat" ref={chatRef}>
        {messages.length === 0 ? (
          <div className="empty">
            <h1>
              {typed[0]}
              {activeLine === 0 && <span className="cursor">▍</span>}
            </h1>
          </div>
        ) : (
          <div className="messages">
            {messages.map((m) => (
              <div key={m.id} className={`message ${m.role}`}>
                <div className="avatar">
                  <AvatarShape user={m.role === 'user'} />
                  <span className={`avatar-inner ${m.role}`}>
                    {m.role === 'user' ? 'You' : <Bolt />}
                  </span>
                </div>
                {m.role === 'user' ? (
                  <div className="bubble">{m.content}</div>
                ) : (
                  <div className="bubble">
                    {m.toolCalls?.length > 0 && (
                      <div className="tool-calls">
                        {m.toolCalls.map((tc, i) => (
                          <ToolCallChip key={i} call={tc} />
                        ))}
                      </div>
                    )}
                    <div
                      className="markdown"
                      dangerouslySetInnerHTML={{ __html: renderMarkdown(m.content) }}
                    />
                    {streaming &&
                      m.id === messages[messages.length - 1].id && (
                        <span className="cursor">▍</span>
                      )}
                  </div>
                )}
              </div>
            ))}
            <div ref={bottomRef} />
          </div>
        )}
      </main>

      <footer className="composer">
        <textarea
          ref={textareaRef}
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder={words.placeholder}
          rows={1}
        />
        {streaming ? (
          <button className="stop" onClick={stop} title="Stop">
            ■
          </button>
        ) : (
          <button
            onClick={() => {
              send()
              closeTouchKeyboard()
            }}
            disabled={!input.trim()}
          >
            ➤
          </button>
        )}
      </footer>
    </div>
  )
}
