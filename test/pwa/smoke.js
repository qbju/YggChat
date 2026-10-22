const { makeNode } = require('./harness');
(async () => {
  const n = makeNode(); await n.ready();
  console.log('addr', n.st.nick, (await n.api('state')).status);
  const e = n.pkt('201::1', 'null'); console.log('null pkt ->', e && e.message);
  n.pkt('201::1', { t: 'msg', id: 'abc', text: 'hi', ts: Math.floor(Date.now()/1000) });
  await n.settle(20);
  console.log(n.st.messages.length, n.sent.map(x=>x.w.t), n.errors.map(String));
  process.exit(0);
})();
