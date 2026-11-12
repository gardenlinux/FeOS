// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The backend's client: streams chat replies from POST /api/v1/chat (SSE).
// Interface: streamReply(messages) -> AsyncGenerator of
// delta | tool_start | tool | history | sandbox items. `messages` is the
// OpenAI wire history (see wireHistory in App.jsx), not the UI message
// model.

import { signIn } from './auth.js'

// Default sandbox image, the same as ferund's default; an empty field also
// gets ferund's default. Written without a tag: the check and the pull
// both resolve it to :latest.
export const DEFAULT_IMAGE = 'ghcr.io/maltej/debian-ai'

// The session ended (expired, or signed out elsewhere): sign in again.
function signedOut(resp) {
  if (resp.status !== 401) return false
  signIn()
  return true
}

// sandboxRef is the conversation's sandbox reference, as the last
// sandbox item handed it out; empty for a new conversation.
export async function* streamReply(
  messages,
  { signal, conversationId, sandboxRef, image, theme } = {},
) {
  const resp = await fetch('/api/v1/chat', {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'X-Conversation-Id': conversationId,
    },
    body: JSON.stringify({
      messages,
      conversation_id: conversationId,
      sandbox_ref: sandboxRef,
      image,
      // the backend gives the Vice City theme a note in the system prompt
      theme,
    }),
    signal,
  })
  if (signedOut(resp)) {
    throw new Error('Signed out; signing in again…')
  }
  if (!resp.ok) {
    throw new Error(`Backend error: HTTP ${resp.status}`)
  }

  const reader = resp.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''

  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })

    const lines = buffer.split('\n')
    buffer = lines.pop() ?? ''

    for (const line of lines) {
      if (!line.startsWith('data:')) continue
      let event
      try {
        event = JSON.parse(line.slice(5).trim())
      } catch {
        continue
      }
      if (event.type === 'delta') {
        yield { type: 'delta', content: event.content }
      } else if (event.type === 'tool_start') {
        yield {
          type: 'tool_start',
          call: { seq: event.seq, id: event.id, name: event.name, arguments: event.arguments },
        }
      } else if (event.type === 'tool') {
        yield {
          type: 'tool',
          call: {
            seq: event.seq,
            id: event.id,
            name: event.name,
            arguments: event.arguments,
            output: event.output,
          },
        }
      } else if (event.type === 'history') {
        // The exact wire messages (assistant tool_calls + tool results, or
        // the final assistant text) the backend appended for this round.
        yield { type: 'history', messages: event.messages }
      } else if (event.type === 'sandbox') {
        // The conversation's sandbox, signed by the backend: ferund mints
        // sandbox IDs and the backend keeps no database, so the
        // conversation keeps it and sends it back with every request.
        yield { type: 'sandbox', conversationId: event.conversation_id, ref: event.ref }
      } else if (event.type === 'error') {
        throw new Error(event.content)
      } else if (event.type === 'done') {
        return
      }
    }
  }
}

// Is the image there? The backend does an anonymous OCI manifest lookup
// (metadata only, nothing is pulled). Public images only; private
// registries look like "not found".
// -> { ok, message?, warning? }
export async function checkImageAvailable(image, { signal } = {}) {
  const resp = await fetch('/api/v1/image-check', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ image }),
    signal,
  })
  if (signedOut(resp)) {
    throw new Error('Signed out; signing in again…')
  }
  if (!resp.ok) {
    throw new Error(`image check failed: HTTP ${resp.status}`)
  }
  return resp.json()
}
