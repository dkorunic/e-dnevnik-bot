// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

// Package format renders a msgtypes.Message as plain text, HTML or Slack markup.
//
// One renderer backs all three, taking the escaper as a required argument: a
// shared renderer otherwise shares its escaping policy, which is how portal
// content once reached Slack's mrkdwn through the plain-text path.
//
// Builders are local, never pooled — String() aliases the buffer, and the
// messengers keep rendered bodies across retries and queue writes.
package format
