// Real Yggdrasil addresses and Ed25519 signatures for tests (address.AddrForKey in yggdrasil-go).
const crypto = require('crypto');
function addrForKey(pk) {
  const buf = Buffer.from(pk).map(b => ~b & 255);
  let ones = 0, done = false, bits = 0, nb = 0; const tmp = [];
  for (let i = 0; i < 256; i++) {
    const bit = (buf[i >> 3] >> (7 - (i & 7))) & 1;
    if (!done && bit) { ones++; continue; }
    if (!done) { done = true; continue; }
    bits = ((bits << 1) | bit) & 255; if (++nb === 8) { nb = 0; tmp.push(bits); }
  }
  const a = Buffer.alloc(16); a[0] = 2; a[1] = ones; Buffer.from(tmp).copy(a, 2, 0, 14);
  const p = []; for (let i = 0; i < 16; i += 2) p.push(a.readUInt16BE(i).toString(16));
  return new URL('http://[' + p.join(':') + ']/').hostname.slice(1, -1);
}
// keypair(name): the same key on every run (seeded from the name); keypair(): a random one.
function keypair(name) {
  const { publicKey, privateKey } = name == null ? crypto.generateKeyPairSync('ed25519') : (() => {
    const privateKey = crypto.createPrivateKey({ key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), crypto.createHash('sha256').update('yggchat-test-' + name).digest()]), format: 'der', type: 'pkcs8' });
    return { privateKey, publicKey: crypto.createPublicKey(privateKey) };
  })();
  const pk = publicKey.export({ format: 'der', type: 'spki' }).subarray(-32);
  const seed = privateKey.export({ format: 'der', type: 'pkcs8' }).subarray(-32);
  return { pk: pk.toString('hex'), priv: Buffer.concat([seed, pk]).toString('hex'), addr: addrForKey(pk), privateKey, publicKey };
}
const pubKeyObj = hex => crypto.createPublicKey({ key: Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), Buffer.from(hex, 'hex')]), format: 'der', type: 'spki' });
const signHex = (kp, msg) => crypto.sign(null, Buffer.from(msg), kp.privateKey).toString('hex');
const verify = (addr, pkHex, msg, sgB64) => {
  if (!/^[0-9a-fA-F]{64}$/.test(pkHex) || addrForKey(Buffer.from(pkHex, 'hex')) !== addr) return false;
  try { return crypto.verify(null, Buffer.from(msg), pubKeyObj(pkHex), Buffer.from(sgB64, 'base64')); } catch { return false; }
};
const addrOK = (addr, pkHex) => /^[0-9a-fA-F]{64}$/.test(pkHex) && addrForKey(Buffer.from(pkHex, 'hex')) === addr;
module.exports = { addrForKey, keypair, signHex, verify, addrOK };
