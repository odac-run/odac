// Generates signing-fixtures.json by running the REAL Node MessageSigner
// (server/src/Hub/WebSocket.js) over a corpus of nasty payloads, per the
// "3.6 DECISION" verification-harness requirement in
// docs/migration/contracts/hub-protocol.md. The Go tests replay the corpus
// and compare canonical bytes + HMAC signatures byte-for-byte.
//
// Regenerate (from the repo root, node_modules present):
//   node odac/internal/hub/testdata/gen-fixtures.mjs
import {createRequire} from 'module'
import {writeFileSync} from 'fs'
import {dirname, join} from 'path'
import {fileURLToPath} from 'url'

const here = dirname(fileURLToPath(import.meta.url))
const require = createRequire(join(here, '../../../../package.json'))

// WebSocket.js touches the Odac Log registry at module scope.
global.Odac = {core: () => ({init: () => ({log() {}, error() {}})})}
const {MessageSigner} = require('./server/src/Hub/WebSocket')

const SECRET = 'fixture-secret-0123456789abcdef'

// One entry per razor edge called out in the contract: unicode, exotic
// numbers, integer-like keys, absent/present/falsy id, null data.
const cases = [
  {name: 'task-envelope', msg: {type: 'app.list', data: {result: true, message: null, data: []}, timestamp: 1752300000}},
  {name: 'with-id', msg: {id: 'req-42', type: 'command.response', data: {id: 'req-42', success: true, message: 'ok'}, timestamp: 1752300001}},
  {name: 'empty-id-is-falsy', msg: {id: '', type: 'ping', data: {}, timestamp: 1752300002}},
  {name: 'zero-id-is-falsy', msg: {id: 0, type: 'ping', data: {}, timestamp: 1752300003}},
  {name: 'numeric-id', msg: {id: 7, type: 'ping', data: {}, timestamp: 1752300004}},
  {name: 'null-data', msg: {type: 'noop', data: null, timestamp: 1752300005}},
  {name: 'string-data', msg: {type: 'noop', data: 'plain <html> & \'quotes\' "dquotes"', timestamp: 1752300006}},
  {name: 'bool-data', msg: {type: 'noop', data: true, timestamp: 1752300007}},
  {
    name: 'unicode',
    msg: {type: 'noop', data: {text: 'çğüşöı 中文 🙂 <tag> & sep line end', emoji: '👩‍👩‍👧‍👦'}, timestamp: 1752300008}
  },
  {
    name: 'control-chars',
    msg: {type: 'noop', data: {raw: 'a\b\t\n\f\rz '}, timestamp: 1752300009}
  },
  {
    name: 'numbers',
    msg: {
      type: 'noop',
      data: [0.1, 1e21, 1e-7, -0, 9007199254740993, 9007199254740991, 123.456, 1e20, 5e-324, 1.7976931348623157e308, 0.000001, 1e-6, 100, -42.5, 2e21, 3.5e-8],
      timestamp: 1752300010
    }
  },
  {
    name: 'integer-like-keys',
    msg: {
      type: 'noop',
      // V8 reorders 2/10/4294967294 ahead of everything, ascending; "01"
      // and "4294967295" are NOT array indices and keep insertion order.
      data: {10: 'a', name: 'x', 2: 'b', '01': 'y', 4294967294: 'z', 4294967295: 'w'},
      timestamp: 1752300011
    }
  },
  {
    name: 'nested',
    msg: {
      type: 'command',
      data: {action: 'app.create', payload: {name: 'shop', env: {A: '1', B: null}, list: [[1, [2.5, 'x']], {}]}},
      timestamp: 1752300012
    }
  }
]

const sign = []
const verify = []

for (const {name, msg} of cases) {
  const signature = MessageSigner.sign(msg, SECRET)

  // The exact payload object sign() builds (id only when truthy).
  const payloadObj = {}
  if (msg.id) payloadObj.id = msg.id
  payloadObj.type = msg.type
  payloadObj.data = msg.data
  payloadObj.timestamp = msg.timestamp

  sign.push({
    name,
    secret: SECRET,
    message_json: JSON.stringify(msg),
    payload_canonical: JSON.stringify(payloadObj),
    signature
  })

  // Wire fixture with the fields in a hostile order: the splice verifier
  // must reassemble id,type,data,timestamp regardless of wire order.
  const wire = {}
  wire.signature = signature
  wire.timestamp = msg.timestamp
  wire.data = msg.data
  wire.type = msg.type
  if ('id' in msg) wire.id = msg.id
  verify.push({name, secret: SECRET, wire: JSON.stringify(wire), timestamp: msg.timestamp})
}

// V8 number formatting table: literal → JSON.stringify(JSON.parse(literal)).
const numberLiterals = [
  '0', '-0', '1', '-1', '0.1', '123.456', '100', '1e2', '1e20', '1e21', '2e21', '1e-6',
  '0.000001', '1e-7', '3.5e-8', '9007199254740991', '9007199254740992', '9007199254740993',
  '5e-324', '1.7976931348623157e308', '-42.5', '299792458', '2.998e8', '1234567890123456789012',
  '0.30000000000000004', '9.999999999999999e20', '1.5e-5'
]
const numbers = {}
for (const lit of numberLiterals) {
  numbers[lit] = JSON.stringify(JSON.parse(lit))
}

writeFileSync(join(here, 'signing-fixtures.json'), JSON.stringify({sign, verify, numbers}, null, 2) + '\n')
console.log(`wrote ${sign.length} sign + ${verify.length} verify fixtures`)
