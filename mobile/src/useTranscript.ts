import { useEffect, useRef, useState } from 'react'
import { emptyTranscript } from '../../frontend/src/chat/transcript'
import { prependTranscriptPage, type TranscriptCache } from '../../frontend/src/chat/transcriptCache'
import { ApiError, Controller, problem } from './api'

export function useTranscript(api: Controller, id: string, cache: TranscriptCache) {
  const cached = id ? cache.get(id) : undefined
  const [state, setState] = useState(() => cached?.transcript ?? emptyTranscript())
  const [before, setBefore] = useState<number | null>(() => cached?.before ?? null)
  const [loading, setLoading] = useState(Boolean(id && !cached))
  const [older, setOlder] = useState(false)
  const [connecting, setConnecting] = useState(false)
  const [error, setError] = useState('')
  const [unavailable, setUnavailable] = useState(false)
  const [retry, setRetry] = useState(0)
  const historySignal = useRef<AbortSignal>(undefined)

  useEffect(() => {
    if (!id) return
    const life = new AbortController()
    historySignal.current = life.signal
    setOlder(false)
    setError('')
    setUnavailable(false)
    const update = () => {
      if (cache.isUnavailable(id)) { setUnavailable(true); return }
      const entry = cache.get(id)
      if (entry) { setState(entry.transcript); setBefore(entry.before) }
    }
    const unsubscribe = cache.subscribe(id, update)
    const connection = () => setConnecting(cache.connecting)
    const unsubscribeConnection = cache.subscribe('@connection', connection)
    connection()
    const entry = cache.get(id)
    if (entry) {
      cache.keep(id, entry)
      update()
      setLoading(false)
    } else {
      cache.beginLoad(id)
      setLoading(true)
      api.transcript(id, null, life.signal).then(page => {
        if (!life.signal.aborted) cache.finishLoad(id, page)
      }).catch(e => {
        if (life.signal.aborted) return
        cache.cancelLoad(id)
        if (e instanceof ApiError && e.status === 404) setUnavailable(true)
        else setError(problem(e, '无法读取会话。'))
      }).finally(() => { if (!life.signal.aborted) setLoading(false) })
    }
    return () => { life.abort(); unsubscribe(); unsubscribeConnection(); cache.cancelLoad(id) }
  }, [api, cache, id, retry])

  async function loadOlder() {
    const entry = cache.get(id)
    const signal = historySignal.current
    if (!entry || !signal || entry.before === null || older) return
    setOlder(true)
    setError('')
    try {
      const page = await api.transcript(id, entry.before, signal)
      if (signal.aborted || cache.get(id) !== entry) return
      prependTranscriptPage(entry, page)
      setBefore(entry.before)
      setState(entry.transcript)
    } catch (e) { if (!signal.aborted) setError(problem(e, '无法加载更早的消息。')) }
    finally { if (!signal.aborted) setOlder(false) }
  }
  return { state, loading, older, connecting, error, unavailable, before, loadOlder, reload: () => setRetry(value => value + 1) }
}
