#!/usr/bin/env node

// Run from an installed DeepSeek Harness checkout with:
//   node --import tsx/esm /path/to/verify-upstream.mjs CHECKOUT JOURNAL [EXPECTED_TEXT... | --tool-meta=CALL_ID:JSON | --tool-error=CALL_ID:JSON | --contains-text=FRAGMENT | --reasoning=EXPECTED_TEXT | --offloaded-images=N | --retained-images=N | --timed-questions=continued|settled]
// This checks the Go-produced v4 artifact with the upstream physical catalog
// and the upstream Session.fromRestore, rather than with Go's own decoder.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'

const [, , checkoutArg, journalArg, ...expectedTexts] = process.argv
if (!checkoutArg || !journalArg) {
  throw new Error('usage: verify-serveragent-v4-upstream.mjs CHECKOUT JOURNAL [EXPECTED_TEXT...]')
}

const checkout = resolve(checkoutArg)
const journal = readFileSync(resolve(journalArg), 'utf8')
assert.ok(journal.endsWith('\n'), 'v4 journal must be newline-terminated')
const [headerLine, ...eventLines] = journal.trimEnd().split('\n')
const header = JSON.parse(headerLine)
assert.equal(header.type, 'session')
assert.equal(header.version, 4, 'only current v4 is accepted')

const source = relativePath => pathToFileURL(join(checkout, relativePath)).href
const { sessionFormatCatalog } = await import(source('packages/session/session-format-catalog/src/index.ts'))
const { currentSessionMessageProjections } = await import(source('packages/session/session-format-catalog/src/message-projections.ts'))
const { Session, SessionId, SessionLogOffset } = await import(source('packages/core/session/src/index.ts'))
const reader = sessionFormatCatalog.createRestore(header, { recovery: 'strict', validation: 'current' })
for (const line of eventLines) reader.decodeRow(JSON.parse(line))
const artifact = reader.finish()
const restored = Session.fromRestore(
  SessionId(artifact.header.id), artifact.events, artifact.header,
  SessionLogOffset(artifact.inheritedEventCount), 'detached', currentSessionMessageProjections,
)
const messages = restored.deriveMessages()
const questionExpectation = expectedTexts.find(value => value.startsWith('--timed-questions='))
let questions
if (questionExpectation !== undefined) {
  const { foldUserQuestions } = await import(source('packages/interaction/user-questions/src/projection.ts'))
  questions = foldUserQuestions(artifact.events)
  const wanted = questionExpectation.slice('--timed-questions='.length)
  if (wanted === 'continued') {
    assert.equal(questions.active.length, 1, 'one continued timed question')
    assert.equal(questions.active[0].state, 'continued', 'timed question state')
    assert.equal(questions.settled.length, 0, 'no answer before late reply')
  } else if (wanted === 'settled') {
    assert.equal(questions.active.length, 0, 'late reply closed the question')
    assert.equal(questions.settled.length, 1, 'one settled timed question')
    assert.equal(questions.settled[0].answers[0].selected[0], 'B', 'late answer selection')
  } else {
    throw new Error(`unknown timed question expectation: ${wanted}`)
  }
}
const text = messages.flatMap(message => message.content)
  .filter(block => block.type === 'text').map(block => block.text)
const reasoning = messages.flatMap(message => message.content)
  .filter(block => block.type === 'reasoning').map(block => block.text)
const images = messages.flatMap(message => message.content)
  .filter(block => block.type === 'image')
for (const expected of expectedTexts) {
  if (expected.startsWith('--reasoning=')) {
    const value = expected.slice('--reasoning='.length)
    assert.ok(reasoning.includes(value), `restored reasoning omits ${JSON.stringify(value)}`)
  } else if (expected.startsWith('--contains-text=')) {
    const fragment = expected.slice('--contains-text='.length)
    assert.ok(fragment.length > 0, 'contains-text assertion needs a non-empty fragment')
    assert.ok(text.some(value => value.includes(fragment)), `restored text blocks omit ${JSON.stringify(fragment)}`)
  } else if (expected.startsWith('--offloaded-images=')) {
    const count = Number(expected.slice('--offloaded-images='.length))
    assert.ok(Number.isSafeInteger(count) && count >= 0, 'offloaded image assertion needs a non-negative count')
    assert.equal(images.filter(block => block.offloaded === true).length, count, 'offloaded image occurrence count')
  } else if (expected.startsWith('--retained-images=')) {
    const count = Number(expected.slice('--retained-images='.length))
    assert.ok(Number.isSafeInteger(count) && count >= 0, 'retained image assertion needs a non-negative count')
    assert.equal(images.filter(block => block.offloaded !== true).length, count, 'retained image occurrence count')
  } else if (expected.startsWith('--timed-questions=')) {
    continue
  } else if (expected.startsWith('--tool-meta=') || expected.startsWith('--tool-error=')) {
    const equal = expected.indexOf('=')
    const colon = expected.indexOf(':', equal + 1)
    assert.ok(colon > equal + 1, 'tool result assertion expects CALL_ID:JSON')
    const callId = expected.slice(equal + 1, colon)
    const field = expected.slice('--tool-'.length, equal)
    const result = artifact.events.find(event => event.type === 'tool/result' && event.data.message.toolCallId === callId)
    assert.ok(result, `restored tool result ${callId} is absent`)
    assert.deepEqual(result.data[field], JSON.parse(expected.slice(colon + 1)))
  } else {
    assert.ok(text.includes(expected), `restored messages omit ${JSON.stringify(expected)}`)
  }
}
process.stdout.write(JSON.stringify({
  formatVersion: artifact.header.version,
  sessionId: artifact.header.id,
  events: artifact.events.length,
  messageRoles: messages.map(message => message.role),
  ...(questions === undefined ? {} : { timedQuestions: { active: questions.active.length, settled: questions.settled.length } }),
}) + '\n')
