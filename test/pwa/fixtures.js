// Shared fixture: a store with a contact, a stranger request, a group we host and one we're in.
const ME = '200:1111:2222:3333:4444:5555:6666:7777';
const A = '201:aaaa:aaaa:aaaa:aaaa:aaaa:aaaa:aaaa';   // accepted contact
const B = '202:bbbb:bbbb:bbbb:bbbb:bbbb:bbbb:bbbb';   // member of our groups, not a contact
const C = '203:cccc:cccc:cccc:cccc:cccc:cccc:cccc';   // admin in G1
const S = '204:dddd:dddd:dddd:dddd:dddd:dddd:dddd';   // stranger
const G1 = '00112233aabbccdd', G2 = '1122334455667788';
const now = () => Math.floor(Date.now() / 1000);
function store() {
  const t = now();
  return {
    key: '11'.repeat(64), peers: ['wss://x.test/'], nick: 'me', nostrOn: false,
    contacts: { [A]: { addr: A, nick: 'alice', req: false, shared: true, hasprof: true, pk: 'aa'.repeat(32) } },
    messages: [
      { id: 'm1', peer: A, from: A, text: 'hello', ts: t - 100, out: false },
      { id: 'm2', peer: A, from: ME, text: 'yo', ts: t - 90, out: true, status: 'sent' },
      { id: 'p1', peer: 'g:' + G1, from: B, text: 'group hi', ts: t - 80, out: false },
    ],
    groups: {
      [G1]: { id: G1, name: 'g1', members: [ME, A, B, C].sort(), owner: ME, admins: [C], v: 3, sv: 2, set: { hi: true } },
      [G2]: { id: G2, name: 'g2', members: [ME, A, B].sort(), owner: A, v: 5, sv: 1, set: {} },
    },
    blocked: {}, ctl: [], joins: {}, gone: {}, early: {},
  };
}
module.exports = { ME, A, B, C, S, G1, G2, store, now };
