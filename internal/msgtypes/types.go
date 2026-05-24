// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package msgtypes

import "time"

// EventCode is an enum for event types.
type EventCode int

// Event codes enum.
const (
	Grade EventCode = iota
	Exam
	Reading
	FinalGrade
	NationalExam
)

// Message structure holds alert subject and description as well as grades fields, as well as corresponding username.
type Message struct {
	Timestamp      time.Time // event timestamp
	QueuedAt       time.Time // time the message first entered the failed-message queue; zero value for non-queued/legacy entries
	Username       string    // username (SSO/SAML)
	Subject        string    // subject
	Descriptions   []string  // descriptions for fields
	Fields         []string  // fields with actual grades/exams and remarks
	SkipRecipients []string  // recipients already notified; skip on retry to prevent duplicates
	Code           EventCode // type of event (grade, exam, reading or final grade)
}
