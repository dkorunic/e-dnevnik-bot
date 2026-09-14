// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

// Package format renders a msgtypes.Message into the plain-text, HTML and
// Slack-markup bodies the messengers send.
//
// One renderer backs all three (see formatSubject and formatGrades), taking the
// escaper as a required argument: sharing a renderer otherwise shares its
// escaping policy, which is how portal content once reached Slack's mrkdwn
// through the plain-text path.
//
// Formatters build into a local strings.Builder, never a pooled one: String()
// aliases the builder's buffer, so reuse would overwrite results callers still
// hold — and the messengers keep formatted bodies across retries and queue
// writes.
package format
