// @license
// Copyright (C) 2022  Dinko Korunic
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

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
	Timestamp    time.Time // event timestamp
	Username     string    // username (SSO/SAML)
	Subject      string    // subject
	Descriptions []string  // descriptions for fields
	Fields       []string  // fields with actual grades/exams and remarks
	Code         EventCode // type of event (grade, exam, reading or final grade)
}
