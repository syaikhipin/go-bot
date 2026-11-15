import { useEffect, useState } from 'react'
import { Pressable, StyleSheet, Text, View } from 'react-native'
import { ApiError, Controller, type Schedule, type ScheduleCreate, type ScheduleKind, type SchedulePatch, type ScheduleTarget, type ScheduleTiming, type SessionRow } from './api'
import { useLocale, type Language } from './locale'
import { Button, Field, Hint, SheetScroll, accent, edge, glass, s, wash } from './ui'

export const timingLabels: Record<ScheduleKind, string> = {
  at: '指定时间', after: '延时一次', every: '固定间隔', daily: '每天', weekly: '每周', cron: 'Cron', rrule: 'RRULE',
}
export const targetLabels: Record<ScheduleTarget['kind'], string> = {
  session: '已有会话', new_once: '首次新建，之后复用', new: '每次新建会话',
}

export function scheduleError(error: unknown) {
  if (error instanceof ApiError) {
    switch (error.code) {
      case 'schedule_conflict': return '任务刚有变化。请重新打开任务后再修改。'
      case 'schedule_execution_pending': return '任务仍在排队、执行或投递间隔内。'
      case 'schedule_not_found': return '此任务已删除。'
      case 'schedule_input_invalid': return error.detail?.split('\n').at(-1) || '请检查任务指令、时间规则和投递目标。'
    }
  }
  return '任务操作未完成，请重试。'
}

export function WeekdayLabels(language: Language) {
  return Array.from({ length: 7 }, (_, index) => new Intl.DateTimeFormat(language, { weekday: 'short', timeZone: 'UTC' })
    .format(new Date(Date.UTC(2026, 0, 5 + index))))
}

const defaultZone = () => Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
const localParts = (at: Date) => {
  const pad = (value: number) => String(value).padStart(2, '0')
  return { date: [at.getFullYear(), pad(at.getMonth() + 1), pad(at.getDate())].join('-'),
    time: [pad(at.getHours()), pad(at.getMinutes()), pad(at.getSeconds())].join(':') }
}

export function ScheduleEditor({ api, task, onSaved, onCancel }: {
  api: Controller; task?: Schedule; onSaved: () => void; onCancel: () => void
}) {
  const { t, language } = useLocale()
  const record = task?.record
  const planned = localParts(record ? new Date(record.scheduledAt) : new Date(Date.now() + 3600000))
  const [title, setTitle] = useState(record?.title ?? '')
  const [prompt, setPrompt] = useState(record?.prompt ?? '')
  const [kind, setKind] = useState<ScheduleKind>(record?.kind === 'after' ? 'at' : record?.kind ?? 'at')
  const [zone, setZone] = useState(record?.timeZone ?? defaultZone)
  const [date, setDate] = useState(planned.date)
  const [time, setTime] = useState(record?.time ?? planned.time)
  const [seconds, setSeconds] = useState(String(record?.everySeconds ?? record?.afterSeconds ?? 3600))
  const [weekdays, setWeekdays] = useState<number[]>(record?.weekdays ?? [1])
  const [expression, setExpression] = useState(record?.expression ?? '')
  const [rrule, setRrule] = useState(record?.rrule ?? '')
  const [target, setTarget] = useState<ScheduleTarget['kind']>(task?.target?.kind ?? 'new_once')
  const [sessionId, setSessionId] = useState(task?.target?.sessionId ?? '')
  const [timingChanged, setTimingChanged] = useState(!task)
  const [targetChanged, setTargetChanged] = useState(!task)
  const [cwd, setCwd] = useState(task?.target?.create?.cwd ?? '')
  const [workspaces, setWorkspaces] = useState<{ path: string; name: string }[]>([])
  const [sessions, setSessions] = useState<SessionRow[]>([])
  const [cursor, setCursor] = useState<string>()
  const [sessionError, setSessionError] = useState(false)
  const [loadingSessions, setLoadingSessions] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function loadSessions(next?: string) {
    setLoadingSessions(true)
    setSessionError(false)
    try {
      const page = await api.sessions(next)
      setSessions(previous => next ? [...previous, ...page.sessions] : page.sessions)
      setCursor(page.nextCursor)
    } catch {
      setSessionError(true)
    } finally {
      setLoadingSessions(false)
    }
  }

  useEffect(() => {
    void loadSessions()
    const controller = new AbortController()
    if (target !== 'session') {
      void api.hostFiles('workspaces', 0, controller.signal).then(page => {
        if (!controller.signal.aborted) {
          setWorkspaces(page.files.filter(item => item.isDir).map(item => ({ path: page.rootPath + '/' + item.path, name: item.name })))
        }
      }).catch(() => {})
    }
    return () => controller.abort()
  }, [api, target])

  function timing(): ScheduleTiming {
    const clock = time.trim().length === 5 ? time.trim() + ':00' : time.trim()
    const local = { time: clock, time_zone: zone.trim() }
    switch (kind) {
      case 'at': return { kind, at: { date: date.trim(), ...local } }
      case 'after': return { kind, after_seconds: Number(seconds) }
      case 'every': return { kind, every_seconds: Number(seconds) }
      case 'daily': return { kind, daily: local }
      case 'weekly': return { kind, weekly: { ...local, weekdays: weekdays.slice().sort((a, b) => a - b) } }
      case 'cron': return { kind, cron: { expression: expression.trim(), time_zone: local.time_zone } }
      case 'rrule': return { kind, rrule: rrule.trim() }
    }
  }

  async function save() {
    setError('')
    setBusy(true)
    const destination: ScheduleTarget = target === 'session' ? { kind: target, sessionId } : { kind: target, ...(cwd.trim() ? { create: { cwd: cwd.trim() } } : {}) }
    try {
      if (task) {
        const patch: SchedulePatch = {}
        if (title.trim() !== record?.title) patch.title = title.trim()
        if (prompt.trim() !== record?.prompt) patch.prompt = prompt.trim()
        if (timingChanged) patch.timing = timing()
        if (targetChanged) patch.target = destination
        if (Object.keys(patch).length) await api.editSchedule(task.record.id, task.updatedAt ?? null, patch)
      } else {
        const input: ScheduleCreate = { title: title.trim(), prompt: prompt.trim(), timing: timing(), target: destination }
        await api.createSchedule(input)
      }
      onSaved()
    } catch (cause) {
      setError(scheduleError(cause))
    } finally {
      setBusy(false)
    }
  }

  const kinds: ScheduleKind[] = ['at', 'after', 'every', 'daily', 'weekly', 'cron', 'rrule']
  const targets: ScheduleTarget['kind'][] = ['session', 'new_once', 'new']
  const timeInput = (value: string) => { setTime(value); setTimingChanged(true) }
  return <SheetScroll>
    <Field label={t('任务名称')} value={title} onChangeText={setTitle} maxLength={120} />
    <Field label={t('任务指令')} value={prompt} onChangeText={setPrompt} multiline />
    <Text style={s.caption}>{t('时间规则')}</Text>
    <View style={styles.choices}>{kinds.map(value => <Choice key={value} label={t(timingLabels[value])} selected={kind === value}
      onPress={() => { if (kind !== value) { setKind(value); setTimingChanged(true) } }} />)}</View>
    {kind === 'at' && <Field label={t('日期')} placeholder="YYYY-MM-DD" value={date} onChangeText={value => { setDate(value); setTimingChanged(true) }} />}
    {(kind === 'at' || kind === 'daily' || kind === 'weekly') && <Field label={t('时间')} placeholder="HH:mm" value={time} onChangeText={timeInput} />}
    {(kind === 'at' || kind === 'daily' || kind === 'weekly' || kind === 'cron') && <Field label={t('时区')} placeholder="Asia/Shanghai"
      value={zone} onChangeText={value => { setZone(value); setTimingChanged(true) }} />}
    {kind === 'weekly' && <View style={styles.choices}>{WeekdayLabels(language).map((day, index) => <Choice key={index} label={day} selected={weekdays.includes(index + 1)}
      onPress={() => { setWeekdays(previous => previous.includes(index + 1) ? previous.filter(value => value !== index + 1) : [...previous, index + 1]); setTimingChanged(true) }} />)}</View>}
    {(kind === 'after' || kind === 'every') && <Field label={t('间隔秒数')} keyboardType="number-pad" value={seconds}
      onChangeText={value => { setSeconds(value); setTimingChanged(true) }} />}
    {kind === 'cron' && <Field label={t('Cron 表达式')} placeholder="0 9 * * 1-5" value={expression}
      onChangeText={value => { setExpression(value); setTimingChanged(true) }} />}
    {kind === 'rrule' && <Field label={t('RRULE 规则')} multiline placeholder={'DTSTART;TZID=Asia/Shanghai:20270101T090000\nRRULE:FREQ=DAILY'}
      value={rrule} onChangeText={value => { setRrule(value); setTimingChanged(true) }} />}
    <Text style={s.caption}>{t('投递目标')}</Text>
    <View style={styles.choices}>{targets.map(value => <Choice key={value} label={t(targetLabels[value])} selected={target === value}
      onPress={() => { if (target !== value) { setTarget(value); setTargetChanged(true) } }} />)}</View>
    {target === 'session' && <>
      {sessions.map(row => <Choice key={row.sessionId} label={row.title || t('未命名会话')} selected={row.sessionId === sessionId}
        onPress={() => { setSessionId(row.sessionId); setTargetChanged(true) }} />)}
      {loadingSessions && <Hint>{t('正在读取会话…')}</Hint>}
      {sessionError && <Button label={t('重新加载')} onPress={() => void loadSessions()} />}
      {cursor && <Button label={t('加载更多')} disabled={loadingSessions} onPress={() => void loadSessions(cursor)} />}
      {!loadingSessions && !sessionError && !sessions.length && <Hint>{t('还没有会话。')}</Hint>}
    </>}
    {target !== 'session' && <>
      <Field label={t('工作目录')} value={cwd} placeholder={t('Host 默认目录')}
        onChangeText={value => { setCwd(value); setTargetChanged(true) }} />
      <View style={styles.choices}>
        {[...new Map([...workspaces.map(row => [row.path, row.name] as const),
          ...sessions.filter(row => row.cwd).map(row => [row.cwd!, row.title || row.cwd!] as const)]).entries()]
          .slice(0, 6).map(([path, name]) => <Choice key={path} label={name} selected={path === cwd}
            onPress={() => { setCwd(path); setTargetChanged(true) }} />)}
      </View>
    </>}
    <Hint error>{error && t(error)}</Hint>
    <Button label={t(task ? '保存修改' : '创建任务')} primary busy={busy}
      disabled={!title.trim() || !prompt.trim() || (target === 'session' && !sessionId)} onPress={() => void save()} />
    <Button label={t('取消')} disabled={busy} onPress={onCancel} />
  </SheetScroll>
}

export function Choice({ label, selected, onPress }: { label: string; selected: boolean; onPress: () => void }) {
  return <Pressable accessibilityRole="button" accessibilityState={{ selected }} onPress={onPress}
    style={({ pressed }) => [styles.choice, selected && styles.selected, pressed && s.pressed]}>
    <Text numberOfLines={1} style={[s.caption, selected && { color: accent }]}>{label}</Text>
  </Pressable>
}

const styles = StyleSheet.create({
  choices: { flexDirection: 'row', flexWrap: 'wrap', gap: 7 },
  choice: { minHeight: 38, borderWidth: 1, borderColor: edge, borderRadius: 13, backgroundColor: glass, justifyContent: 'center', paddingHorizontal: 11 },
  selected: { backgroundColor: wash, borderColor: 'transparent' },
})
