// Drives the Go hub with the socket_io_client package the Flutter app uses,
// configured like lib/main_controller.dart: websocket first, polling as
// fallback, cookie header, reconnection.
//   dart run dart_client.dart <url>
import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'package:socket_io_client/socket_io_client.dart' as io;

Future<T> within<T>(Future<T> f, String what) =>
    f.timeout(const Duration(seconds: 6), onTimeout: () => throw 'timeout: $what');

// Key order is not significant in JSON, so compare with sorted keys.
Object? canon(Object? v) {
  if (v is Map) {
    final keys = v.keys.map((k) => k.toString()).toList()..sort();
    return {for (final k in keys) k: canon(v[k])};
  }
  if (v is List) return v.map(canon).toList();
  return v;
}

void expectEq(Object? got, Object? want, String what) {
  if (jsonEncode(canon(got)) != jsonEncode(canon(want))) {
    throw '$what: got ${jsonEncode(got)} want ${jsonEncode(want)}';
  }
}

io.Socket open(String url, String token, {bool reconnection = true}) {
  return io.io(url, <String, dynamic>{
    'transports': <String>['websocket', 'polling'],
    'autoConnect': false,
    'forceNew': true,
    'reconnection': reconnection,
    'reconnectionDelay': 100,
    'reconnectionDelayMax': 200,
    'withCredentials': true,
    'auth': <String, dynamic>{'token': token},
    'extraHeaders': <String, String>{'Cookie': 'neoagent.sid=signed-value'},
  });
}

Future<void> connected(io.Socket s) {
  final c = Completer<void>();
  s.onConnect((_) { if (!c.isCompleted) c.complete(); });
  s.onConnectError((e) { if (!c.isCompleted) c.completeError('connect_error: $e'); });
  s.connect();
  return within(c.future, 'connect');
}

Future<dynamic> ack(io.Socket s, String event, List<Object?> args) {
  final c = Completer<dynamic>();
  s.emitWithAck(event, args.length == 1 ? args.first : args, ack: (dynamic r) => c.complete(r));
  return within(c.future, 'ack $event');
}

Future<void> main(List<String> args) async {
  final url = args.first;
  final a = open(url, 'secret:7');
  await connected(a);

  // ack round trip with JSON fidelity
  final payload = <String, dynamic>{'s': 'a <b> & "q" é', 'n': 3, 'f': 1.5, 'nul': null, 'list': [1, 'x', {'k': true}]};
  expectEq(await ack(a, 'echo', [payload]), payload, 'echo');

  final who = await ack(a, 'whoami', [<String, dynamic>{}]);
  expectEq(who['user'], '7', 'user');
  expectEq(who['cookie'], 'neoagent.sid=signed-value', 'cookie header');
  expectEq(who['id'], a.id, 'socket id');

  // binary in both directions plus a server push with an attachment
  final pushed = Completer<List<dynamic>>();
  a.on('pushbin', (dynamic d) { if (!pushed.isCompleted) pushed.complete(List<dynamic>.from(d as List)); });
  final reversed = await ack(a, 'bin', [Uint8List.fromList([1, 2, 3, 250])]);
  expectEq(List<int>.from(reversed as List), [250, 3, 2, 1], 'binary ack');
  final push = await within(pushed.future, 'pushbin');
  expectEq(push.length, 2, 'push args');

  // hub.EmitToUser reaches this client
  final hubEvent = Completer<dynamic>();
  a.on('hub:user', (dynamic d) => hubEvent.complete(d));
  await ack(a, 'hub-user', [<String, dynamic>{}]);
  expectEq(await within(hubEvent.future, 'hub:user'), {'for': 7}, 'hub:user');

  // server-initiated acknowledgement
  // The Dart client hands a handler one value: the argument, or a list of
  // the arguments followed by the acknowledgement callback.
  a.on('question', (dynamic d) {
    final parts = d as List;
    (parts.last as Function)('answer:${parts.first}');
  });
  expectEq(await ack(a, 'ask', ['ping']), 'answer:ping', 'server ack');

  // reconnection after the server drops the transport
  final reconnected = Completer<void>();
  final firstId = a.id;
  a.onConnect((_) { if (a.id != firstId && !reconnected.isCompleted) reconnected.complete(); });
  a.emit('drop');
  await within(reconnected.future, 'reconnect');
  expectEq(await ack(a, 'echo', ['again']), 'again', 'echo after reconnect');

  // refused by middleware
  final bad = open(url, 'nope', reconnection: false);
  final refused = Completer<String>();
  bad.onConnectError((e) { if (!refused.isCompleted) refused.complete(e.toString()); });
  bad.connect();
  final reason = await within(refused.future, 'connect_error');
  if (!reason.contains('unauthorized')) throw 'unexpected refusal: $reason';
  bad.dispose();

  // server-side disconnect is reported and not retried
  final gone = Completer<String>();
  a.onDisconnect((r) { if (!gone.isCompleted) gone.complete(r.toString()); });
  a.emit('bye');
  expectEq(await within(gone.future, 'disconnect'), 'io server disconnect', 'disconnect reason');
  await Future<void>.delayed(const Duration(milliseconds: 500));
  expectEq(a.connected, false, 'stays disconnected');
  a.dispose();
  stdout.writeln('ok');
  exit(0);
}
