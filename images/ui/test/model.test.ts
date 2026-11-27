// Tests of the chat cost hint (#570): the claude-proxy rule, longest matching prefix. Run: npm test (node --test).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { multiplierOf } from '../src/api.ts'

const m = { 'claude-opus': 1.67, 'claude-opus-5-5': 1.33, 'claude-sonnet': 1, 'claude-haiku': 0.33 }

test('alias counts as its family', () => {
  assert.equal(multiplierOf('opus', m), 1.67)
  assert.equal(multiplierOf('sonnet[1m]', m), 1)
})

test('longest prefix wins', () => {
  assert.equal(multiplierOf('claude-opus-5-5', m), 1.33)
  assert.equal(multiplierOf('claude-opus-5', m), 1.67)
})

test('no hint without a table, a model or a match', () => {
  assert.equal(multiplierOf('opus', undefined), undefined)
  assert.equal(multiplierOf('', m), undefined)
  assert.equal(multiplierOf('fable', m), undefined)
})
