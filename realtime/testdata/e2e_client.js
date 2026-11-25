'use strict';
// Drives the Go hub with the real socket.io-client of the Node server.
//   node e2e_client.js <node_modules> <url> <mode>
// mode: websocket | polling | upgrade | reconnect. Prints one JSON object of
// observations; any failed expectation throws and exits non-zero.
const [, , modules, url, mode] = process.argv;
const { io } = require(`${modules}/socket.io-client`);
const assert = require('node:assert/strict');

const transports = {
  websocket: ['websocket'],
  polling: ['polling'],
  upgrade: ['polling', 'websocket'],
  reconnect: ['websocket', 'polling'],
}[mode];

const wait = (ms) => new Promise((r) => setTimeout(r, ms));
const once = (target, event, ms = 4000) =>
  new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`timeout waiting for ${event}`)), ms);
    target.once(event, (...args) => { clearTimeout(timer); resolve(args); });
  });
const connect = (token, opts = {}) => {
  const socket = io(url, {
    transports,
    forceNew: true,
    auth: { token },
    extraHeaders: { Cookie: 'neoagent.sid=signed-value' },
    reconnectionDelay: 50,
    reconnectionDelayMax: 100,
    ...opts,
  });
  socket.firstTransport = new Promise((res) => socket.io.once('open', () => {
    res(socket.io.engine.transport.name);
    socket.upgraded = new Promise((up) => socket.io.engine.once('upgrade', (t) => up(t.name)));
  }));
  return socket;
};

(async () => {
  const seen = {};
  const a = connect('secret:7');
  await once(a, 'connect');
  seen.transport = await a.firstTransport;
  if (mode === 'upgrade') {
    seen.upgradedTo = await a.upgraded;
    assert.equal(seen.upgradedTo, 'websocket');
  }

  // events with acks, JSON fidelity
  const payload = { s: 'a <b> & "q" é', n: 3, f: 1.5, nul: null, list: [1, 'x', { k: true }] };
  // the server echoes every argument back as separate ack arguments
  const echoed = await new Promise((res) => a.emit('echo', payload, 5, (...r) => res(r)));
  assert.deepEqual(echoed, [payload, 5]);

  // session cookie and auth reach the server
  const who = await new Promise((res) => a.emit('whoami', (r) => res(r)));
  assert.equal(who.user, '7');
  assert.equal(who.cookie, 'neoagent.sid=signed-value');
  assert.equal(who.rooms, 2); // own id room + user room
  seen.sidMatches = who.id === a.id;
  assert.ok(seen.sidMatches);

  // binary both ways, and a server push with an attachment
  const pushed = once(a, 'pushbin');
  const reversed = await new Promise((res) => a.emit('bin', Buffer.from([1, 2, 3, 250]), (r) => res(r)));
  assert.deepEqual([...Buffer.from(reversed)], [250, 3, 2, 1]);
  const [meta, bin] = await pushed;
  assert.deepEqual(meta, { n: 4 });
  assert.deepEqual([...Buffer.from(bin)], [250, 3, 2, 1]);

  // events keep their order
  const order = [];
  a.on('order', (n) => order.push(n));
  for (let i = 0; i < 50; i += 1) a.emit('order', i);
  await wait(300);
  assert.deepEqual(order, Array.from({ length: 50 }, (_, i) => i));
  a.off('order');

  // rooms, sender excluded
  const b = connect('secret:8');
  await once(b, 'connect');
  await new Promise((res) => b.emit('join', 'r1', res));
  await new Promise((res) => a.emit('join', 'r1', res));
  const gotB = once(b, 'room:msg');
  let senderHeard = false;
  a.on('room:msg', () => { senderHeard = true; });
  await new Promise((res) => a.emit('to-room', { room: 'r1', msg: { text: 'hi' } }, res));
  assert.deepEqual((await gotB)[0], { text: 'hi' });
  await wait(100);
  assert.equal(senderHeard, false);

  // Hub.EmitToUser / EmitToRoom (nil payload means no arguments)
  const userEvent = once(a, 'hub:user');
  const roomEvent = once(b, 'hub:room');
  await new Promise((res) => a.emit('hub-user', res));
  assert.deepEqual((await userEvent)[0], { for: 7 });
  assert.deepEqual(await roomEvent, []);

  // user 8 never hears user 7's events
  let leaked = false;
  b.on('hub:user', () => { leaked = true; });
  await new Promise((res) => a.emit('hub-user', res));
  await wait(100);
  assert.equal(leaked, false);

  // server-initiated ack
  a.once('question', (q, cb) => cb(`answer:${q}`));
  const asked = await new Promise((res) => a.emit('ask', 'ping', (r) => res(r)));
  assert.equal(asked, 'answer:ping');
  assert.equal(await new Promise((res) => a.emit('ask-silent', res)), true);

  // namespaces
  const admin = connect('secret:7', { path: '/socket.io/' });
  const adminNs = admin.io.socket('/admin', { auth: { token: 'secret:7' } });
  await once(adminNs, 'connect');
  assert.equal(await new Promise((res) => adminNs.emit('hello', res)), 'admin');
  const missing = admin.io.socket('/nope', { auth: { token: 'secret:7' } });
  const [missingErr] = await once(missing, 'connect_error');
  assert.equal(missingErr.message, 'Invalid namespace');
  admin.close();

  // refused by middleware
  const bad = connect('nope', { reconnection: false });
  const [badErr] = await once(bad, 'connect_error');
  assert.equal(badErr.message, 'unauthorized');
  bad.close();

  if (mode === 'reconnect') {
    const firstId = a.id;
    const dropped = once(a, 'disconnect');
    a.emit('drop');
    const [reason] = await dropped;
    seen.dropReason = reason;
    await once(a, 'connect');
    assert.notEqual(a.id, firstId);
    seen.reconnected = true;
    const again = await new Promise((res) => a.emit('echo', 'again', (r) => res(r)));
    assert.equal(again, 'again');
    // room membership starts over; the user room is back through the connection handler
    const roomsAfter = await new Promise((res) => a.emit('whoami', (r) => res(r.rooms)));
    assert.equal(roomsAfter, 2);
  }

  // server-side disconnect, no reconnection afterwards
  const gone = once(a, 'disconnect');
  a.emit('bye');
  seen.byeReason = (await gone)[0];
  assert.equal(seen.byeReason, 'io server disconnect');
  await wait(300);
  assert.equal(a.connected, false);

  // whole connection closed by the server
  const c = connect('secret:9', { reconnection: false });
  await once(c, 'connect');
  const closed = once(c, 'disconnect');
  c.emit('bye-all');
  seen.byeAllReason = (await closed)[0];
  assert.equal(seen.byeAllReason, 'io server disconnect');

  // client-initiated disconnect
  b.disconnect();
  console.log(JSON.stringify(seen));
  process.exit(0);
})().catch((err) => {
  console.error(err && err.stack || err);
  process.exit(1);
});
