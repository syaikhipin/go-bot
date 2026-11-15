import { useEffect, useRef, useState } from 'react'
import { AppState } from 'react-native'
import { emptyTranscript } from '../../frontend/src/chat/transcript'
import { cacheTranscript, prependTranscriptPage, updateCachedTranscript, type TranscriptCache } from '../../frontend/src/chat/transcriptCache'
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
    let stream: AbortController | undefined
    let timer: ReturnType<typeof setTimeout> | undefined
    let entry = cache.get(id)
    let wait = 500
    let running = false

    function again() {
      if (life.signal.aborted || AppState.currentState === 'background') return
      setConnecting(true)
      timer = setTimeout(follow, wait)
      wait = Math.min(wait * 2, 5000)
    }

    async function follow() {
      if (running || !entry || life.signal.aborted || AppState.currentState === 'background') return
      const current = entry
      clearTimeout(timer)
      running = true
      stream = new AbortController()
      try {
        await api.stream(id, current.cursor, stream.signal, () => {
          if (life.signal.aborted) return
          setConnecting(false)
          wait = 500
        }, (kind, data) => {
          if (life.signal.aborted || stream?.signal.aborted || cache.get(id) !== current) return
          setState(updateCachedTranscript(current, kind, data))
        })
        again()
      } catch (e) {
        if (life.signal.aborted) return
        if (e instanceof ApiError && e.status === 404) {
          cache.delete(id)
          entry = undefined
          setState(emptyTranscript())
          setBefore(null)
          setConnecting(false)
          setUnavailable(true)
        } else again()
      } finally { running = false }
    }

    const listener = AppState.addEventListener('change', status => {
      if (status === 'background') { clearTimeout(timer); stream?.abort() }
      if (status === 'active') void follow()
    })
    setError('')
    if (entry) {
      cache.keep(id, entry)
      void follow()
    } else {
      setLoading(true)
      api.transcript(id, null, life.signal).then(page => {
        if (life.signal.aborted) return
        entry = cacheTranscript(page)
        cache.keep(id, entry)
        setBefore(entry.before)
        setState(entry.transcript)
        void follow()
      }).catch(e => {
        if (life.signal.aborted) return
        if (e instanceof ApiError && e.status === 404) setUnavailable(true)
        else setError(problem(e, '无法读取会话。'))
      }).finally(() => {
        if (!life.signal.aborted) setLoading(false)
      })
    }

    return () => { life.abort(); stream?.abort(); clearTimeout(timer); listener.remove() }
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
