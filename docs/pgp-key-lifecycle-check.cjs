// Design probe, not a product implementation. Run after npm ci in frontend:
// node docs/pgp-key-lifecycle-check.cjs
const assert = require('node:assert/strict');
const { createRequire } = require('node:module');
const { resolve } = require('node:path');
const pgp = createRequire(resolve(__dirname, '../frontend/package.json'))('openpgp');

(async () => {
  const makeKey = () => pgp.generateKey({ type: 'ecc', curve: 'curve25519Legacy', userIDs: [{ email: 'lifecycle@example.invalid' }], format: 'object' });
  const old = await makeKey();
  const current = await makeKey();
  const plaintext = 'historical mail must survive retirement';
  for (const wildcard of [false, true]) {
    const ciphertext = await pgp.encrypt({ message: await pgp.createMessage({ text: plaintext }), encryptionKeys: old.publicKey, wildcard });
    const read = () => pgp.readMessage({ armoredMessage: ciphertext });
    await assert.rejects(async () => pgp.decrypt({ message: await read(), decryptionKeys: current.privateKey }));
    const message = await read();
    const ids = message.getEncryptionKeyIDs().map(id => id.toHex());
    assert(ids.some(id => wildcard ? /^0+$/.test(id) : old.privateKey.getKeyIDs().some(keyId => keyId.toHex() === id)));
    const opened = await pgp.decrypt({ message, decryptionKeys: [current.privateKey, old.privateKey] });
    assert.equal(opened.data, plaintext);
    const revoked = await old.privateKey.revoke({ flag: pgp.enums.reasonForRevocation.keyCompromised });
    assert.equal((await pgp.decrypt({ message: await read(), decryptionKeys: revoked })).data, plaintext);
    await assert.rejects(async () => pgp.encrypt({ message: await pgp.createMessage({ text: plaintext }), encryptionKeys: revoked.toPublic() }), /revoked/i);
  }
  const certified = await current.publicKey.signPrimaryUser([old.privateKey]);
  const certifications = await certified.verifyPrimaryUser([old.publicKey]);
  assert(certifications.some(result => result.valid && result.keyID.toHex() === old.privateKey.getKeyID().toHex()));
  const changed = await pgp.reformatKey({ privateKey: current.privateKey, userIDs: [{ email: 'lifecycle@example.invalid' }, { email: 'alias@example.invalid' }], format: 'object' });
  assert.equal(changed.privateKey.getFingerprint(), current.privateKey.getFingerprint());
  assert.deepEqual(changed.privateKey.getKeyIDs().map(id => id.toHex()), current.privateKey.getKeyIDs().map(id => id.toHex()));
  assert(changed.publicKey.getUserIDs().some(id => id.includes('alias@example.invalid')));
  console.log('PASS: historical and wildcard decryption, revoked-key decryption, transition certification, UID addition without key replacement');
})().catch(error => { console.error(error); process.exitCode = 1; });
