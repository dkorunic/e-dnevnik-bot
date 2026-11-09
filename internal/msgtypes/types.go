// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package msgtypes

import "time"

// EventCode identifies an event's kind.
type EventCode int

// Appended to, never reordered: these ordinals are a persisted wire format in
// the CBOR queue, so inserting one makes older rows decode as the wrong type.
const (
	Grade EventCode = iota
	Exam
	Reading
	FinalGrade
	NationalExam
	ExamDigest  // weekly summary of next week's exams; bypasses dedup
	PortalDrift // portal pages no longer parse as expected; bypasses dedup
)

// EventCodeNames maps the config-facing name of each EventCode to its value.
var EventCodeNames = map[string]EventCode{
	"grade":        Grade,
	"exam":         Exam,
	"reading":      Reading,
	"finalgrade":   FinalGrade,
	"nationalexam": NationalExam,
	"examdigest":   ExamDigest,
	"portaldrift":  PortalDrift,
}

// Message is the pipeline's canonical event.
type Message struct {
	Timestamp      time.Time // event timestamp
	QueuedAt       time.Time // time the message first entered the failed-message queue; zero value for non-queued/legacy entries
	Username       string    // username (SSO/SAML)
	Subject        string    // subject
	Average        string    // grade subject's mean at scrape time; display only, never dedup identity
	Class          string    // set when Subject carries " / <class>" only while multi-class: dedup then accepts either form
	Descriptions   []string  // descriptions for fields
	Fields         []string  // fields with actual grades/exams and remarks
	SkipRecipients []string  // recipients already notified; skip on retry to prevent duplicates
	Code           EventCode // type of event
}
