// Package realtime delivers server-pushed events to browsers and other
// clients over Server-Sent Events (see SSEHandler) or WebSockets (see
// realtime/websocket), with the safety properties an application needs before
// it can expose a long-lived connection:
//
//   - Subscriptions are authorized. A Channel declares the permission a
//     principal must hold; a topic that matches no declared channel, or whose
//     channel the principal may not use, is denied. There is no anonymous or
//     "public" channel.
//   - Topics are tenant-scoped by default. The tenant is part of the routing
//     key, so tenant A can never receive tenant B's events, even when both
//     subscribe to the same topic name. A global topic needs Channel.Global
//     on the subscribe side and the Global publish option on the publish side.
//   - Resources are bounded: payload size, per-connection queues, replay
//     memory, connections per subject and in total, and subscriptions per
//     connection and in total. A slow consumer is disconnected; it never
//     blocks publishers and never grows memory.
//   - Resume is explicit. Clients that reconnect with Last-Event-ID receive the
//     retained events they missed, or a forge.resync signal when the retained
//     window cannot cover the gap.
//   - Several replicas share events through a Transport (realtime/postgres
//     provides one). Without a transport the broker is in-process only.
//
// The package depends only on the standard library and Forge core packages.
package realtime
