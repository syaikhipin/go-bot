import { applySessionEvent, applyStreamEvent, emptyTranscript, type Transcript } from './transcript.ts'
import type { SessionEvent, TranscriptPage } from '../api/controller.ts'

export type CachedTranscript = {
  transcript: Transcript
  before: number | null
  cursor: number | null
}

const maxCachedChats = 20

// A cache belongs to one signed-in workspace. Each entry keeps only the
// pages the user has opened, plus the events received by the live stream.
export class TranscriptCache {
  private readonly chats = new Map<string, CachedTranscript>()

  get(id: string) { return this.chats.get(id) }

  keep(id: string, entry: CachedTranscript) {
    this.chats.delete(id)
    this.chats.set(id, entry)
    if (this.chats.size > maxCachedChats) {
      const oldest = this.chats.keys().next().value
      if (oldest !== undefined) this.chats.delete(oldest)
    }
  }

  delete(id: string) { this.chats.delete(id) }
}

export function cacheTranscript(page: TranscriptPage): CachedTranscript {
  const history = page.events.reduce(applySessionEvent, emptyTranscript())
  return {
    before: page.before,
    cursor: page.throughSeq,
    transcript: { ...history, seenThrough: page.throughSeq ?? -1, queue: page.queue, questions: page.questions ?? [] },
  }
}

export function updateCachedTranscript(entry: CachedTranscript, kind: string, data: unknown): Transcript {
  if (kind === 'session') entry.cursor = Math.max(entry.cursor ?? -1, (data as SessionEvent).seq)
  entry.transcript = applyStreamEvent(entry.transcript, kind, data)
  return entry.transcript
}

export function prependTranscriptPage(entry: CachedTranscript, page: TranscriptPage) {
  const history = page.events.reduce(applySessionEvent, emptyTranscript())
  const current = entry.transcript
  const ids = new Set(current.messages.map(message => message.id))
  entry.before = page.before
  entry.transcript = {
    ...current,
    messages: [...history.messages.filter(message => !ids.has(message.id)), ...current.messages],
    toolResults: { ...history.toolResults, ...current.toolResults },
  }
}
